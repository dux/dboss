package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/config"
)

func TestTokenPrintsTheWebhookToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.ServerFileName)
	run := func(contents string) (int, string, string) {
		writeFiles(t, dir, map[string]string{config.ServerFileName: contents})
		var out, errOut strings.Builder
		code := (CLI{Out: &out, Err: &errOut}).Run([]string{"token", "-c", path})
		return code, strings.TrimSpace(out.String()), errOut.String()
	}
	if code, out, _ := run("tokens:\n  dboss: s3cret\n"); code != 0 || out != (config.Tokens{Dboss: "s3cret"}.WebhookToken()) || out == "s3cret" {
		t.Fatalf("derived = %d %q", code, out)
	}
	if code, out, _ := run("tokens:\n  dboss: s3cret\n  webhook: hooks-only\n"); code != 0 || out != "hooks-only" {
		t.Fatalf("explicit = %d %q", code, out)
	}
	if code, _, errOut := run("apps: ./apps\n"); code == 0 || !strings.Contains(errOut, "neither tokens.dboss nor tokens.webhook") {
		t.Fatalf("none = %d %q", code, errOut)
	}
}
