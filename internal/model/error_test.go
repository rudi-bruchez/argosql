package model

import (
	"fmt"
	"testing"
)

func TestExitCode(t *testing.T) {
	for _, code := range []int{2, 3, 4, 5, 6, 7, 8, 130} {
		err := fmt.Errorf("context: %w", &PublicError{Code: code, Kind: "fixture"})
		if got := ExitCode(err); got != code {
			t.Fatalf("got %d want %d", got, code)
		}
	}
	if ExitCode(nil) != 0 {
		t.Fatal("nil must succeed")
	}
}
