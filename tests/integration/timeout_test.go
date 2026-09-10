//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimeoutCleanup(t *testing.T) {
	if os.Getenv("ASQ_TIMEOUT_CHILD") == "1" {
		podmanRun(t, "asq-test-"+randomHex(6), os.Getenv("ASQ_TEST_IMAGE"), os.Getenv("ASQ_TIMEOUT_ENV_FILE"))
		t.Log("timeout fixture container created")
		time.Sleep(time.Minute)
		return
	}
	envPath := filepath.Join(t.TempDir(), "container.env")
	if err := os.WriteFile(envPath, []byte("ACCEPT_EULA=Y\nMSSQL_PID=Developer\nMSSQL_SA_PASSWORD="+randomPassword()+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Keep the parent run label so its outer wrapper also covers a parent timeout.
	runID := testRunID
	filter := "label=io.argosql.test=" + runID
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
		defer cancel()
		removeContainersByLabel(ctx, filter)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "run.sh", os.Args[0], "-test.run=^TestTimeoutCleanup$", "-test.timeout=5s", "-test.v")
	// Replace only these fixture variables, without logging the environment.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "ASQ_TEST_RUN_ID=") && !strings.HasPrefix(entry, "ASQ_TIMEOUT_CHILD=") && !strings.HasPrefix(entry, "ASQ_TIMEOUT_ENV_FILE=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "ASQ_TEST_RUN_ID="+runID, "ASQ_TIMEOUT_CHILD=1", "ASQ_TIMEOUT_ENV_FILE="+envPath)
	out, err := cmd.CombinedOutput()
	t.Logf("child === RUN count: %d", strings.Count(string(out), "=== RUN"))
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), "test timed out after") {
		t.Fatalf("expected testing timeout with exit 2, got %v (output omitted)", err)
	}
	if ids := podmanContainerIDs(t, filter); len(ids) != 0 {
		t.Fatalf("timeout left %d containers", len(ids))
	}
	// Require actual container creation before testing cleanup.
	if !strings.Contains(string(out), "timeout fixture container created") {
		t.Fatal("child did not create a container")
	}
}
