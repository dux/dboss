package humanize

import (
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	cases := map[int64]string{
		0:                          "0B",
		1023:                       "1023B",
		1024:                       "1.0K",
		1536:                       "1.5K",
		1024 * 1024:                "1.0M",
		1024 * 1024 * 1024 * 3 / 2: "1.5G",
	}
	for size, want := range cases {
		if got := Bytes(size); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", size, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	for _, test := range []struct {
		span time.Duration
		want string
	}{
		{0, "0m"},
		{30 * time.Second, "0m"},
		{10 * time.Minute, "10m"},
		{2*time.Hour + 10*time.Minute, "2h 10m"},
		{3*24*time.Hour + 4*time.Hour, "3d 4h"},
	} {
		if got := Duration(test.span); got != test.want {
			t.Errorf("Duration(%s) = %q, want %q", test.span, got, test.want)
		}
	}
}

func TestAgo(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"never", time.Time{}, "-"},
		{"just now", now.Add(-3 * time.Second), "just now"},
		{"seconds", now.Add(-42 * time.Second), "42s ago"},
		{"a minute", now.Add(-3 * time.Minute), "3min ago"},
		{"hours", now.Add(-2 * time.Hour), "2h ago"},
		{"days", now.Add(-5 * 24 * time.Hour), "5d ago"},
		{"months", now.Add(-60 * 24 * time.Hour), "2mo ago"},
		{"years", now.Add(-800 * 24 * time.Hour), "2y ago"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Ago(test.at); got != test.want {
				t.Fatalf("Ago = %q, want %q", got, test.want)
			}
		})
	}
}
