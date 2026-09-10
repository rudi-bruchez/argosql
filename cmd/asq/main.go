// Command asq is a SQL Server diagnostic CLI: its entire job here is to
// wire the process's standard streams and signals to internal/cli.Run
// and exit with the code it returns.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rudi-bruchez/argosql/internal/cli"
)

func main() {
	ctx, stop := signalContext(context.Background())
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// signalContext cancels its returned context on SIGINT or SIGTERM instead
// of letting the default handler kill the process outright: Run needs a
// live, canceled context to tell a user interruption (exit 130) apart
// from its own deadline expiring (see internal/cli/run.go's exitCodeFor).
//
// SIGTERM is not optional here. A process launched by a supervisor or a
// test harness receives SIGTERM before any SIGKILL, and the integration
// harness registers exactly this pair; catching only os.Interrupt left
// the binary killed without the 130 conversion or the cleanup a canceled
// context drives.
func signalContext(parent context.Context) (context.Context, func()) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
