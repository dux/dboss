package console

import (
	"os/exec"
	"testing"
)

func TestConsoleComponentBehavior(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("console component behavior tests need bun")
	}
	if output, err := exec.Command(bun, "test", "app_card_test.js", "redeploy_dialog_test.js").CombinedOutput(); err != nil {
		t.Fatalf("console component behavior: %v\n%s", err, output)
	}
}
