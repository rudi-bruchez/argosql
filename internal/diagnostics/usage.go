package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// usageQuery is sql/usage.sql (see its own doc comment for what it reads
// and why) - embedded here, rather than in embed.go, for the same
// reason query.go and top.go embed their own sql/*.sql locally: it is
// this file's own concern alone.
//
//go:embed sql/usage.sql
var usageQuery string

// serverStartTimeQuery reads the one instance-wide fact "idx usage"
// reports for context (design spec: "server start time if permitted"):
// sys.dm_os_sys_info.sqlserver_start_time. It is a plain Go constant,
// not a go:embed file, for the same reason session.go's own
// selectMajorVersionQuery is: a single-line statement with nothing a
// dedicated .sql file would add.
//
// This DMV and sys.dm_db_index_usage_stats itself both require the
// exact same instance-level permission (VIEW SERVER STATE on 2019,
// VIEW SERVER PERFORMANCE STATE on 2022+ - measured against Microsoft's
// own documentation for both views), so in the ordinary case where that
// permission is simply absent, Usage's own main query already fails at
// code 4 before this one ever runs (see Usage's own doc comment and the
// design spec's Q/I/S matrix: "idx usage | ... | 4, instance permission
// absent | 0"). This separate read exists for the narrower case the
// same documentation also names: an operator can DENY SELECT on this
// one DMV specifically while leaving the instance-level permission (and
// therefore the main query) intact - the design spec's own "server
// start time if permitted" wording is for exactly that case, not for
// the I-bundle failure the matrix already covers.
const serverStartTimeQuery = "SELECT sqlserver_start_time FROM sys.dm_os_sys_info"

// UsageTable is the one table "idx usage" emits (design spec's declared
// table order: "idx usage: usage"). Counters and timestamps are NULL
// together when observation_status is 'never_observed' - a legitimately
// untouched index, never a fabricated zero (design spec: "Do not
// include a stale verdict in v0.1"). Rows are ordered by index_id
// ascending (design spec: "Rows use IDs/ordinals ascending unless
// ranking is specified" - usage is not a ranking), via sql/usage.sql's
// own ORDER BY.
var UsageTable = model.TableSpec{
	Name: "usage",
	Columns: []model.Column{
		{Name: "index_id", SQLType: "INT"},
		{Name: "name", SQLType: "NVARCHAR"},
		{Name: "seeks", SQLType: "BIGINT"},
		{Name: "scans", SQLType: "BIGINT"},
		{Name: "lookups", SQLType: "BIGINT"},
		{Name: "updates", SQLType: "BIGINT"},
		{Name: "last_seek", SQLType: "DATETIME"},
		{Name: "last_scan", SQLType: "DATETIME"},
		{Name: "last_lookup", SQLType: "DATETIME"},
		{Name: "last_update", SQLType: "DATETIME"},
		{Name: "observation_status", SQLType: "NVARCHAR"},
	},
}

// observationWindowNotice builds the Notice "idx usage" always emits on
// the "usage" table: the explicit observation-window limitation design
// spec names ("server start time if permitted, and explicit
// observation-window limitations") - counters reset at instance restart
// and, on some versions, at index rebuild, so a low count is evidence
// of a short window, never proof of low usage (design spec: "Index
// usage counters have reset and visibility limitations ... Missing
// usage rows are unknown/unobserved, not proof of no reads. Do not
// recommend dropping an index in v0.1"). startTime is nil, with
// startErr naming why, when the narrower permission above was absent.
func observationWindowNotice(startTime model.Cell, startErr error) model.Notice {
	const limitation = "counters reset at instance restart and, on some versions, at index rebuild; a low or missing count reflects an unobserved window, not evidence about how the index is read"
	var message string
	if startErr == nil {
		message = fmt.Sprintf("server started at %v; %s", startTime, limitation)
	} else {
		message = fmt.Sprintf("server start time unavailable (permission); %s", limitation)
	}
	return model.Notice{Kind: "observation_window", Message: message, Table: UsageTable.Name}
}

// Usage runs "idx usage <schema.name>": obj's seeks/scans/lookups/updates
// and last-use timestamps from sys.dm_db_index_usage_stats, plus server
// start time when a separate, narrower permission allows it (design
// spec: "idx usage": "Seeks/scans/lookups/updates, last-use timestamps,
// server start time if permitted, and explicit observation-window
// limitations").
//
// Resolution precedes everything else (design spec: "Permission checks
// follow target resolution for commands taking object names" - and its
// own worked example: "idx usage on an unresolved name returns 8 before
// checking its server permission"). A resolved object of the wrong type
// is rejected at code 2 immediately after, exactly like "idx list"/"obj
// table"/"size table".
//
// sys.dm_db_index_usage_stats itself requires the instance-level
// VIEW SERVER STATE/VIEW SERVER PERFORMANCE STATE permission - not
// merely the database-level bundle "I" already holds (measured against
// Microsoft's own documentation) - so this command's main query simply
// fails at code 4 (via classifyQueryError, inside queryRows) when that
// permission is absent, matching the design spec's own Q/I/S matrix
// exactly: Q fails earlier at code 8 (unresolved in the fixture that
// matrix describes), I fails here at code 4, S succeeds.
func Usage(ctx context.Context, s *sqlserver.Session, name string, dst model.Sink) error {
	obj, err := sqlserver.Resolve(ctx, s.Conn, name)
	if err != nil {
		return err
	}
	if !tableAllowedTypes[obj.Type] {
		return wrongObjectTypeError(obj, "idx usage", "tables")
	}

	if err := dst.Begin(UsageTable); err != nil {
		return err
	}
	if err := queryRows(ctx, s.Conn, usageQuery, dst, sql.Named("id", obj.ID)); err != nil {
		return err
	}

	startRow, startErr := queryOneRow(ctx, s.Conn, serverStartTimeQuery)
	if startErr != nil {
		var pub *model.PublicError
		if !errors.As(startErr, &pub) || pub.Kind != "permission" {
			// A genuine execution failure here is this command's own
			// failure, never silently treated as "permission absent".
			return startErr
		}
	}
	var startTime model.Cell
	complete := startErr == nil
	if complete {
		startTime = startRow[0]
	}
	dst.Notice(observationWindowNotice(startTime, startErr))
	return dst.End(true, complete)
}
