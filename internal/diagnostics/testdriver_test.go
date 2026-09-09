package diagnostics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// objCaptureSink is the model.Sink every table_test.go/code_test.go/
// indexes_test.go/size_test.go test uses to inspect what Table, Code,
// Indexes and Size actually wrote, without going through
// internal/artifacts or internal/cli. Distinct from top_test.go's own
// captureSink (whose File always errors) and query_test.go's own
// queryCaptureSink (which does not record Begin's own call order):
// beginOrder is this type's own addition, tracking the exact sequence
// of table names Begin was called with - task 13's own "wrong table
// order" break (dispatch cassure 4) is only catchable by a test that
// can see this sequence, not merely which tables exist.
type objCaptureSink struct {
	tables     []objCapturedTable
	cur        *objCapturedTable
	beginOrder []string
	notices    []model.Notice
	files      []objCapturedFile
}

type objCapturedTable struct {
	spec                                   model.TableSpec
	rows                                   [][]model.Cell
	collectionComplete, propertiesComplete bool
}

type objCapturedFile struct {
	kind, suffix string
	content      []byte
}

func (s *objCaptureSink) Begin(spec model.TableSpec) error {
	s.beginOrder = append(s.beginOrder, spec.Name)
	s.cur = &objCapturedTable{spec: spec}
	return nil
}
func (s *objCaptureSink) Row(row []model.Cell) error {
	if s.cur == nil {
		return fmt.Errorf("objCaptureSink: Row called with no open table")
	}
	s.cur.rows = append(s.cur.rows, row)
	return nil
}
func (s *objCaptureSink) End(collectionComplete, propertiesComplete bool) error {
	if s.cur == nil {
		return fmt.Errorf("objCaptureSink: End called with no open table")
	}
	s.cur.collectionComplete = collectionComplete
	s.cur.propertiesComplete = propertiesComplete
	s.tables = append(s.tables, *s.cur)
	s.cur = nil
	return nil
}
func (s *objCaptureSink) File(kind, suffix string, src io.Reader) (model.Artifact, error) {
	b, err := io.ReadAll(src)
	if err != nil {
		return model.Artifact{}, err
	}
	s.files = append(s.files, objCapturedFile{kind: kind, suffix: suffix, content: b})
	return model.Artifact{Kind: kind, Path: "(captured in memory)/" + kind + suffix, Bytes: int64(len(b)), Complete: true}, nil
}
func (s *objCaptureSink) Notice(n model.Notice) { s.notices = append(s.notices, n) }

func (s *objCaptureSink) table(name string) *objCapturedTable {
	for i := range s.tables {
		if s.tables[i].spec.Name == name {
			return &s.tables[i]
		}
	}
	return nil
}

func (s *objCaptureSink) noticeWithKind(kind string) *model.Notice {
	for i := range s.notices {
		if s.notices[i].Kind == kind {
			return &s.notices[i]
		}
	}
	return nil
}

// objQueryResponse matches one query this package's task-13 commands
// (Table, Code, Indexes, Size) can issue, by a substring unique to
// that query's own embedded SQL text, and answers it. fakeObjConn
// (below) tries each response in order and fails the test outright if
// none matches, rather than silently returning empty rows for a query
// no test anticipated - the same "no responder, no silent success"
// discipline query_test.go's fakeQueryConn and top_test.go's
// fakeTopConn already apply to their own commands.
type objQueryResponse struct {
	match  func(query string) bool
	handle func(args []driver.NamedValue) (driver.Rows, error)
}

// fakeObjConn is the one fake database/sql/driver.Conn every
// table_test.go/code_test.go/indexes_test.go/size_test.go test builds
// a *sqlserver.Session around: table.go, code.go, indexes.go and
// size.go all resolve a name first (sqlserver.Resolve's own
// resolveObjectQuery) and then issue exactly one of their own
// commands' queries, so every test in this package's task-13 suite
// needs the same resolve responder plus its own command-specific one.
type fakeObjConn struct {
	t         *testing.T
	responses []objQueryResponse
}

func (c *fakeObjConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("fakeObjConn: Prepare not supported, use QueryContext")
}
func (c *fakeObjConn) Close() error { return nil }
func (c *fakeObjConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakeObjConn: Begin not supported")
}
func (c *fakeObjConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	for _, r := range c.responses {
		if r.match(query) {
			return r.handle(args)
		}
	}
	// Returned as a driver error, never t.Fatalf: FailNow (which
	// Fatalf calls) must run on the test's own goroutine only, and
	// this method is called from inside database/sql's own call
	// stack - measured to hang the whole test binary rather than
	// fail it cleanly when this called t.Fatalf directly.
	c.t.Logf("fakeObjConn: no responder matched query:\n%s", query)
	return nil, errors.New("fakeObjConn: no responder matched this query, see test log")
}

type fakeObjDriver struct{}

func (d *fakeObjDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("fakeObjDriver: Open not supported, use the connector")
}

type fakeObjConnector struct{ conn *fakeObjConn }

func (c *fakeObjConnector) Connect(ctx context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *fakeObjConnector) Driver() driver.Driver                            { return &fakeObjDriver{} }

// newFakeObjSession builds a *sqlserver.Session backed by conn, the
// same fake-driver plumbing query_test.go's newFakeQuerySession and
// top_test.go's newFakeTopSession already build for their own commands.
func newFakeObjSession(t *testing.T, conn *fakeObjConn) *sqlserver.Session {
	t.Helper()
	conn.t = t
	db := sql.OpenDB(&fakeObjConnector{conn: conn})
	t.Cleanup(func() { db.Close() })
	sqlConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { sqlConn.Close() })
	return &sqlserver.Session{Conn: sqlConn, Major: 16}
}

// resolveFoundResponse matches sqlserver.Resolve's own resolveObjectQuery
// (objects.go) - identified by its unique JOIN to sys.schemas - and
// answers it with one resolved row: obj.ID/obj.Schema/obj.Name/obj.Type.
// Resolve scans this directly with (*sql.Row).Scan, never through
// output.ScanRow, so the column type names below are never inspected;
// they exist only because driver.Rows must report some type name.
func resolveFoundResponse(id int64, schema, name, typ string) objQueryResponse {
	return objQueryResponse{
		match: func(q string) bool { return strings.Contains(q, "sys.schemas AS s ON s.schema_id") },
		handle: func(args []driver.NamedValue) (driver.Rows, error) {
			return &fakeStaticRows{
				cols:  []string{"object_id", "schema_name", "object_name", "type"},
				types: []string{"INT", "NVARCHAR", "NVARCHAR", "CHAR"},
				data:  [][]driver.Value{{id, schema, name, typ}},
			}, nil
		},
	}
}

// resolveNotFoundResponse matches the same resolveObjectQuery, but
// answers zero rows - sqlserver.Resolve's own not_found_or_not_visible
// path (code 8), exercised here to prove that every one of this
// package's task-13 commands checks resolution BEFORE issuing any
// other query (design spec line 172).
func resolveNotFoundResponse() objQueryResponse {
	return objQueryResponse{
		match: func(q string) bool { return strings.Contains(q, "sys.schemas AS s ON s.schema_id") },
		handle: func(args []driver.NamedValue) (driver.Rows, error) {
			return &fakeStaticRows{
				cols:  []string{"object_id", "schema_name", "object_name", "type"},
				types: []string{"INT", "NVARCHAR", "NVARCHAR", "CHAR"},
				data:  nil,
			}, nil
		},
	}
}

// tableRowResponse matches table.sql (identified by its unique
// is_memory_optimized projection) and answers it with one row:
// rowCount/memOpt are driver.Value, so either may be passed as a bare
// Go nil to mean SQL NULL.
func tableRowResponse(rowCount, memOpt driver.Value) objQueryResponse {
	return objQueryResponse{
		match: func(q string) bool { return strings.Contains(q, "is_memory_optimized") },
		handle: func(args []driver.NamedValue) (driver.Rows, error) {
			return &fakeStaticRows{
				cols:  []string{"row_count", "is_memory_optimized"},
				types: []string{"BIGINT", "BIT"},
				data:  [][]driver.Value{{rowCount, memOpt}},
			}, nil
		},
	}
}

// tableErrResponse matches table.sql and answers it with err - used to
// simulate the permission denial size.go/table.go's own
// tableHeaderRowCount converts into rowCountReasonPermission (via
// classifyQueryError).
func tableErrResponse(err error) objQueryResponse {
	return objQueryResponse{
		match:  func(q string) bool { return strings.Contains(q, "is_memory_optimized") },
		handle: func(args []driver.NamedValue) (driver.Rows, error) { return nil, err },
	}
}

// permProbeResponse matches sqlserver.Probe's own hasPermsByNameQuery
// (permissions.go) and answers it with one HAS_PERMS_BY_NAME result:
// 1 for Allowed, 0 for Denied, nil for the malformed-probe case
// (Unknown), matching permissionFromSQL's own three-way read of a
// nullable int.
func permProbeResponse(result driver.Value) objQueryResponse {
	return objQueryResponse{
		match: func(q string) bool { return strings.Contains(q, "HAS_PERMS_BY_NAME") },
		handle: func(args []driver.NamedValue) (driver.Rows, error) {
			return &fakeStaticRows{
				cols:  []string{""},
				types: []string{"INT"},
				data:  [][]driver.Value{{result}},
			}, nil
		},
	}
}

// columnsRowsResponse matches columns.sql (identified by its unique
// JOIN to sys.default_constraints) and streams rows back exactly as
// given - callers pass real column values (ordinal, name, type_name,
// max_length, precision, scale, is_nullable, is_identity, is_computed,
// default_definition, computed_definition).
func columnsRowsResponse(rows [][]driver.Value) objQueryResponse {
	return objQueryResponse{
		match: func(q string) bool { return strings.Contains(q, "sys.default_constraints") },
		handle: func(args []driver.NamedValue) (driver.Rows, error) {
			return &fakeStaticRows{
				cols: []string{"ordinal", "name", "type_name", "max_length", "precision", "scale",
					"is_nullable", "is_identity", "is_computed", "default_definition", "computed_definition"},
				types: []string{"INT", "NVARCHAR", "NVARCHAR", "SMALLINT", "TINYINT", "TINYINT",
					"BIT", "BIT", "BIT", "NVARCHAR", "NVARCHAR"},
				data: rows,
			}, nil
		},
	}
}

// indexesRowsResponse matches indexes.sql (identified by its unique
// STRING_AGG aggregation) and streams rows back exactly as given:
// index_id, name, type_desc, keys, includes, filter_definition,
// is_unique, is_disabled.
func indexesRowsResponse(rows [][]driver.Value) objQueryResponse {
	return objQueryResponse{
		match: func(q string) bool { return strings.Contains(q, "STRING_AGG") },
		handle: func(args []driver.NamedValue) (driver.Rows, error) {
			return &fakeStaticRows{
				cols:  []string{"index_id", "name", "type_desc", "keys", "includes", "filter_definition", "is_unique", "is_disabled"},
				types: []string{"INT", "NVARCHAR", "NVARCHAR", "NVARCHAR", "NVARCHAR", "NVARCHAR", "BIT", "BIT"},
				data:  rows,
			}, nil
		},
	}
}

// sizeRowsResponse matches size.sql (identified by its unique
// in_row_used_page_count projection) and streams rows back exactly as
// given: index_id, partition_number, allocation_type, used_pages,
// reserved_pages.
func sizeRowsResponse(rows [][]driver.Value) objQueryResponse {
	return objQueryResponse{
		match: func(q string) bool { return strings.Contains(q, "in_row_used_page_count") },
		handle: func(args []driver.NamedValue) (driver.Rows, error) {
			return &fakeStaticRows{
				cols:  []string{"index_id", "partition_number", "allocation_type", "used_pages", "reserved_pages"},
				types: []string{"INT", "INT", "NVARCHAR", "BIGINT", "BIGINT"},
				data:  rows,
			}, nil
		},
	}
}

// moduleRowResponse matches module.sql (identified by its unique
// sys.sql_modules join) and answers it with one row: found=false
// simulates the "obj disappeared" recheck (zero rows), matching
// design spec line 204's "Recheck disappearance during collection and
// report unavailable rather than treating it as an empty definition."
func moduleRowResponse(found bool, definition, isEncrypted driver.Value) objQueryResponse {
	return objQueryResponse{
		match: func(q string) bool { return strings.Contains(q, "sys.sql_modules") },
		handle: func(args []driver.NamedValue) (driver.Rows, error) {
			var data [][]driver.Value
			if found {
				data = [][]driver.Value{{definition, isEncrypted}}
			}
			return &fakeStaticRows{
				cols:  []string{"definition", "is_encrypted"},
				types: []string{"NVARCHAR", "BIT"},
				data:  data,
			}, nil
		},
	}
}
