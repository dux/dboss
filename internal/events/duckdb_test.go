package events

import (
	"strings"
	"testing"
)

func TestQueryAuditDetailIsOneCappedLine(t *testing.T) {
	if got := QueryAuditDetail("select *\n  from users\nwhere id = 1"); got != "select * from users where id = 1" {
		t.Fatalf("detail = %q", got)
	}
	long := QueryAuditDetail("select " + strings.Repeat("x", 500))
	if len(long) != queryAuditLimit+3 || !strings.HasSuffix(long, "...") {
		t.Fatalf("detail should be capped at %d chars plus an ellipsis, got %d", queryAuditLimit, len(long))
	}
}
