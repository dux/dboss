package cli

import (
	"context"
	"testing"
	"time"
)

func TestRestartOnKeysRestartsOnlyOnR(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	keys := make(chan byte)
	restarts := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		restartOnKeys(ctx, keys, "web", func(string) {}, func() error {
			restarts <- struct{}{}
			return nil
		})
		close(done)
	}()
	for _, key := range []byte("xq\nrR") {
		keys <- key
	}
	for range 2 {
		select {
		case <-restarts:
		case <-time.After(time.Second):
			t.Fatal("r and R must each restart")
		}
	}
	// A closed stdin keeps the session running until it is stopped.
	close(keys)
	select {
	case <-done:
		t.Fatal("closed stdin ended the loop")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop kept running after the session stopped")
	}
	if len(restarts) != 0 {
		t.Fatalf("other keys restarted %d times", len(restarts))
	}
}
