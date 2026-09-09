package output

import (
	"database/sql"
	"fmt"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// ScanRow is the only place in this project that turns one row of
// *sql.Rows into a []model.Cell. Every diagnostic that reads a row reaches
// this function rather than calling rows.Scan itself: types must decide
// each ambiguous conversion (see cell.go), and there is exactly one place
// that decision is allowed to live.
//
// rows must already be positioned on a row (rows.Next() returned true);
// types must be rows.ColumnTypes() for the same query, in the same order.
func ScanRow(rows *sql.Rows, types []*sql.ColumnType) ([]model.Cell, error) {
	raw := make([]any, len(types))
	dest := make([]any, len(types))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, fmt.Errorf("output: scanning row: %w", err)
	}

	cells := make([]model.Cell, len(types))
	for i, v := range raw {
		cell, err := convertCell(v, types[i])
		if err != nil {
			return nil, err
		}
		cells[i] = cell
	}
	return cells, nil
}
