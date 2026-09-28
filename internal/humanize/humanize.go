// Package humanize renders sizes and times for messages, logs and command output, so every
// surface shows the same "1.5K" or "3min ago" instead of its own formatting.
package humanize

import (
	"fmt"
	"time"
)

// Bytes renders a size in bytes with a short unit: 1023B, 1.5K, 2.0G.
func Bytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%dB", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(size)/float64(div), "KMGTPE"[exp])
}

// Duration renders a span as the two largest units: "10m", "2h 10m", "3d 4h". A zero span, or
// one under a minute, is "0m". It mirrors the console's Human.duration.
func Duration(span time.Duration) string {
	seconds := int64(span.Seconds())
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	minutes := (seconds % 3600) / 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// Ago renders how long ago at happened: "just now", "42s ago", "3min ago", "2h ago", "5d ago",
// "3mo ago" or "2y ago". A zero time, which is "never", returns "-".
func Ago(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	seconds := int64(time.Since(at).Seconds())
	switch {
	case seconds < 10:
		return "just now"
	case seconds < 60:
		return fmt.Sprintf("%ds ago", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dmin ago", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh ago", seconds/3600)
	case seconds < 30*86400:
		return fmt.Sprintf("%dd ago", seconds/86400)
	case seconds < 365*86400:
		return fmt.Sprintf("%dmo ago", seconds/86400/30)
	default:
		return fmt.Sprintf("%dy ago", seconds/86400/365)
	}
}
