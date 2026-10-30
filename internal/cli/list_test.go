package cli

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestListWatchStopsOnCancel stands in for Ctrl+C: the signal context is built
// on the command's, so cancelling it has to end the loop without an error.
func TestListWatchStopsOnCancel(t *testing.T) {
	dbPath = seed(t)
	listWatch = true
	t.Cleanup(func() { listWatch = false })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetContext(ctx)

	done := make(chan error, 1)
	go func() { done <- runList(cmd, nil) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("--watch on a cancelled context: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("--watch did not return after the context was cancelled")
	}
}

func TestListWatchRejectsJSON(t *testing.T) {
	if _, err := run(t, "list", "--db", seed(t), "--watch", "--json"); err == nil {
		t.Error("--watch with --json: want an error")
	}
}
