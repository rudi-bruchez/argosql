package diagnostics

import (
	"errors"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"

	mssql "github.com/microsoft/go-mssqldb"
)

// TestClassifyQueryErrorRecognizesPermissionFamily is task 13 fix-1's
// own A1 target: the classifier used to recognize only SQL error
// numbers 229 and 300 as permission denials, not 230 or 297 - 297
// measured directly against a real engine (tests/integration's own
// TestObjTableSizeTableSurviveMissingViewDatabaseState) as the number
// sys.dm_db_partition_stats actually raises once VIEW DATABASE STATE
// (and, on 2022, its two narrower siblings) are revoked from a
// principal that still resolves the object; obj table and size table
// both fell through to a plain code-5 execution failure instead of
// design spec line 172's required behavior. 230 (column-level denial)
// is included for the same family, on the same reasoning, though this
// package's own queries do not currently probe at column granularity.
// A number outside this family (4060, "cannot open database") must
// still classify as an ordinary execution failure, never permission.
func TestClassifyQueryErrorRecognizesPermissionFamily(t *testing.T) {
	for _, num := range []int32{229, 230, 297, 300} {
		err := classifyQueryError(mssql.Error{Number: num, Message: "denied"}, "message")
		var pub *model.PublicError
		if !errors.As(err, &pub) {
			t.Fatalf("SQL error %d: want *model.PublicError, got %#v", num, err)
		}
		if pub.Code != 4 || pub.Kind != "permission" {
			t.Fatalf("SQL error %d: want code 4/permission, got code %d/%s", num, pub.Code, pub.Kind)
		}
	}

	err := classifyQueryError(mssql.Error{Number: 4060, Message: "cannot open database"}, "message")
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("SQL error 4060: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 5 || pub.Kind != "execution" {
		t.Fatalf("SQL error 4060 (outside the permission family): want code 5/execution, got code %d/%s", pub.Code, pub.Kind)
	}
}
