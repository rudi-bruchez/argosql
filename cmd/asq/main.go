// Command asq is a SQL Server diagnostic CLI: its entire job here is to
// wire the process's standard streams and signals to internal/cli.Run
// and exit with the code it returns.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/rudi-bruchez/argosql/internal/cli"
)

func main() {
	// signal.NotifyContext cancels ctx on SIGINT/SIGTERM instead of
	// letting the default handler kill the process outright: Run needs
	// a live, canceled context to tell a user interruption (exit 130)
	// apart from its own deadline expiring (see internal/cli/run.go's
	// exitCodeFor).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
