package ops

import (
	"context"
	"strings"
	"testing"

	"dboss/internal/supervisor"
)

func TestDeployPreviewRefusesUnknownAndUnconnectedApps(t *testing.T) {
	runtime := &fakeRuntime{snapshots: []supervisor.Snapshot{{Name: "local", Dir: t.TempDir()}}}
	service := New(runtime, nil, nil, nil, nil, nil)
	for app, message := range map[string]string{"missing": "unknown app", "local": "no connected Git repository"} {
		if _, err := service.DeployPreview(context.Background(), app); err == nil || !strings.Contains(err.Error(), message) {
			t.Fatalf("preview %s = %v", app, err)
		}
	}
	if len(runtime.actions) != 0 {
		t.Fatalf("preview ran actions: %v", runtime.actions)
	}
}
