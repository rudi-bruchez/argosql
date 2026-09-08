package model

import (
	"errors"
	"fmt"
)

// PublicError is the error surfaced to the CLI boundary: it carries the exit
// code the process must use, a stable machine-readable kind, a human message,
// and the originating SQL Server error number when there is one.
type PublicError struct {
	Code      int    `json:"code"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	SQLNumber int32  `json:"sql_number,omitempty"`
}

func (e *PublicError) Error() string {
	if e.SQLNumber != 0 {
		return fmt.Sprintf("%s: %s (sql error %d)", e.Kind, e.Message, e.SQLNumber)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// ExitCode maps an error to the process exit code: nil succeeds with 0, an
// error wrapping a *PublicError (at any depth, via errors.As) preserves that
// error's Code, and anything else falls back to 5 (execution/timeout).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var public *PublicError
	if errors.As(err, &public) {
		return public.Code
	}
	return 5
}

// FallbackError is the last-resort envelope used when Result itself does not
// fit the output budget. It is a dedicated type rather than a degenerate
// Result: Result cannot produce this literal, and omitempty on Result's
// fields would make "ok":false disappear, which the spec requires to always
// be present here.
type FallbackError struct {
	SchemaVersion int          `json:"schema_version"`
	OK            bool         `json:"ok"`
	Error         *PublicError `json:"error"`
}
