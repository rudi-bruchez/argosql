package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResultJSONSnakeCaseAndPositionalCells(t *testing.T) {
	result := Result{
		SchemaVersion: 1,
		OK:            true,
		Context: ContextInfo{
			Server:                "localhost",
			CertificateValidation: "skipped",
			CollectedAt:           time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC),
		},
		Tables: []TableResult{
			{
				Spec: TableSpec{Name: "t", Columns: []Column{{Name: "a", SQLType: "int"}}},
				Rows: [][]Cell{
					{"abc", int64(42), nil, true, 3.5},
				},
				State: Completeness{RowsCollected: 7, CollectionComplete: true, PropertiesComplete: true},
			},
		},
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	for _, field := range []string{`"schema_version"`, `"rows_collected"`, `"certificate_validation"`} {
		if !strings.Contains(got, field) {
			t.Fatalf("expected field %s in %s", field, got)
		}
	}

	// A row is an array of naked values in column order. This is the whole
	// reason Cell is a bare type: the assertion below fails for any shape
	// that wraps a value in an object or reorders the row.
	if !strings.Contains(got, `["abc",42,null,true,3.5]`) {
		t.Fatalf("expected positional row encoding, got %s", got)
	}
}
