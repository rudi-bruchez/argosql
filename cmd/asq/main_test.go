//go:build !windows

package main

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// TestSignalContextCancelsOnSIGTERM pins the defect this test exists for:
// the binary used to register only os.Interrupt, so a supervisor's
// SIGTERM killed the process outright instead of canceling the context
// Run converts to exit code 130. Sending SIGTERM to this test process is
// safe precisely when the wiring is right: signalContext has registered
// a handler for it, so the signal cancels the context rather than
// terminating the test binary. With the handler missing, this test does
// not merely fail, the process dies, which is the same failure mode a
// real SIGTERM produced. The context-to-130 mapping itself is covered by
// internal/cli's TestExitCodeFor.
func TestSignalContextCancelsOnSIGTERM(t *testing.T) {
	ctx, stop := signalContext(context.Background())
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not cancel the context")
	}
}
