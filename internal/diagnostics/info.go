// Package diagnostics holds the SQL-backed implementation of every
// registered command beyond help: one file per command (info.go,
// health.go, ...), each reading one or more embedded .sql statements
// (see embed.go) and writing its rows through a model.Sink. No
// terminal formatting happens here - that is internal/output's job.
package diagnostics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"

	// Used only to recognize this package's own permission-denied SQL
	// error numbers (permissionSQLErrors, below) - that classifier is
	// not exported, and this package never reaches back into
	// sqlserver's pool to borrow it.
	//
	// Task 13 fix-2 (A10): this package's own set, {229, 230, 297, 300},
	// deliberately diverges from internal/sqlserver's own private
	// sqlErrorPermission, {229, 300} - the two are no longer "the same
	// way" and are not meant to be merged. sqlserver's classifier only
	// ever sees errors from SET LOCK_TIMEOUT, SELECT @@LOCK_TIMEOUT and
	// SERVERPROPERTY (session setup and version detection), none of
	// which require any permission at all, so they can never raise 230
	// (column-level denial) or 297 (catalog/DMV metadata-visibility
	// denial, design spec line 172's own obj table/size table case) -
	// only this package's queries, against sys.dm_db_partition_stats
	// and the catalog views, can.
	mssql "github.com/microsoft/go-mssqldb"
)

// InfoTable is the one table "info" ever emits: the resolved identity
// of the server and database this session is actually connected to,
// not the configuration values used to reach them (see
// internal/model.ContextInfo for those, filled by internal/cli from
// config.Profile). internal/cli's registry uses this exact TableSpec
// as "info"'s declared Command.Tables, so help's advertised schema and
// what Info actually writes can never drift apart.
var InfoTable = model.TableSpec{
	Name: "identity",
	Columns: []model.Column{
		{Name: "server", SQLType: "NVARCHAR"},
		{Name: "database", SQLType: "NVARCHAR"},
		{Name: "principal", SQLType: "NVARCHAR"},
		{Name: "product_version", SQLType: "NVARCHAR"},
		{Name: "edition", SQLType: "NVARCHAR"},
		{Name: "compatibility_level", SQLType: "INT"},
	},
}

// Info reports the server/database identity actually resolved by the
// engine - SERVERPROPERTY, DB_NAME(), USER_NAME(), and the connected
// database's compatibility level (design spec: "info": "Resolved
// server/database identity, product version, edition, compatibility
// level, and current principal; no connection string or secrets"). It
// never touches config.Profile or anything s itself was opened with:
// every value here comes back from the engine, on this connection,
// after connection - see sql/info.sql's own doc comment for why that
// distinction matters.
func Info(ctx context.Context, s *sqlserver.Session, dst model.Sink) error {
	row, err := queryOneRow(ctx, s.Conn, infoQuery)
	if err != nil {
		return err
	}
	if err := dst.Begin(InfoTable); err != nil {
		return err
	}
	if err := dst.Row(row); err != nil {
		return err
	}
	return dst.End(true, true)
}

// queryOneRow runs query (optionally parameterized, e.g. with
// sql.Named values), expected to return exactly one row, and scans it
// through output.ScanRow - the project's one sanctioned
// SQL-value-to-model.Cell conversion (see internal/output/cell.go).
// Every diagnostic in this package reaches this helper rather than
// scanning into typed Go variables by hand, which would silently
// re-implement (and risk re-deciding) that conversion's ambiguous
// cases - DECIMAL, MONEY, VARBINARY and UNIQUEIDENTIFIER all arrive
// from the driver as the same Go type, []byte, and only
// DatabaseTypeName can tell them apart.
//
// It is queryOneOptionalRow below, plus the one difference every
// caller of this function actually wants: a query that is only ever
// run against a system view guaranteed to hand back exactly one row
// (sys.database_query_store_options, info.sql's own SERVERPROPERTY
// SELECT, coverage.sql's unconditional MIN/MAX) treats zero rows as
// its own execution defect (code 5), never a caller-visible "not
// found". query.go's Query, looking up one query_id that may
// genuinely not exist, needs the opposite - see
// queryOneOptionalRow's own doc comment for why it is the one that
// does not make this call.
func queryOneRow(ctx context.Context, conn *sql.Conn, query string, args ...any) ([]model.Cell, error) {
	cells, found, err := queryOneOptionalRow(ctx, conn, query, args...)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &model.PublicError{Code: 5, Kind: "execution", Message: "diagnostic query returned no rows"}
	}
	return cells, nil
}

// queryOneOptionalRow runs query (optionally parameterized) and
// returns its one row, or found=false if it returned zero rows -
// never an error on zero rows by itself, unlike queryOneRow above.
// query.go's Query uses this directly: zero rows from its own
// identity lookup means the requested query_id does not exist, or is
// not visible to the current principal (code 8, not_found_or_not_visible),
// a fact only the caller - which alone knows what ID it asked for -
// can report correctly.
func queryOneOptionalRow(ctx context.Context, conn *sql.Conn, query string, args ...any) ([]model.Cell, bool, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, classifyQueryError(err, "diagnostic query failed")
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, false, classifyQueryError(err, "reading diagnostic row")
		}
		return nil, false, nil
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, false, &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("reading column types: %s", err.Error())}
	}
	cells, err := output.ScanRow(rows, types)
	if err != nil {
		return nil, false, &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("scanning diagnostic row: %s", err.Error())}
	}
	if err := rows.Err(); err != nil {
		return nil, false, classifyQueryError(err, "reading diagnostic rows")
	}
	return cells, true, nil
}

// permissionSQLErrors lists every SQL Server error number this
// package's own queries can raise that means "connected fine, but not
// permitted", never an execution failure: 229 and 230 (object- and
// column-level GRANT-style denial: "The … permission was denied on
// the object/column …"), 297 (catalog/DMV metadata-visibility denial:
// "The user does not have permission to perform this action" -
// measured, task 13 fix-1, against sys.dm_db_partition_stats once
// VIEW DATABASE STATE is revoked from a principal that still resolves
// the object: obj table and size table both fell through to code 5
// before this number was added), and 300 (the same object-level denial
// as 229, worded slightly differently by the engine). Message text
// for each of these four, read from sys.messages (English, id 1033),
// confirms all four are the same "permission denied" family; 262
// ("%ls permission denied in database") and 15151/15247 (system
// stored procedure wording) were checked too and excluded: this
// package never issues the DDL or sp_-style statements that raise
// them.
var permissionSQLErrors = map[int32]bool{229: true, 230: true, 297: true, 300: true}

// classifyQueryError turns a driver error from one of this package's
// own embedded queries into a *model.PublicError: a permissionSQLErrors
// number means "connected fine, but not permitted" (code 4, mirroring
// internal/sqlserver's own, private classifySQLError), everything else
// is an execution failure (code 5). message is a fixed, reformulated
// description, never the driver error's own text verbatim - this
// package's queries carry no secrets, but repeating that discipline
// here keeps it true everywhere in this codebase without a
// case-by-case judgment call.
func classifyQueryError(err error, message string) error {
	var sqlErr mssql.Error
	if errors.As(err, &sqlErr) {
		if permissionSQLErrors[sqlErr.Number] {
			return &model.PublicError{Code: 4, Kind: "permission", Message: "insufficient permission", SQLNumber: sqlErr.Number}
		}
		return &model.PublicError{Code: 5, Kind: "execution", Message: message, SQLNumber: sqlErr.Number}
	}
	return &model.PublicError{Code: 5, Kind: "execution", Message: message}
}
