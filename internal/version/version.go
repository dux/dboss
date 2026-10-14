// Package version identifies the running binary for metrics and `dboss version`.
package version

import "strings"

// Version is the build version: v<X.Y.Z> from .version, injected by `make build` and
// `make assets` with -ldflags "-X dboss/internal/version.Version=v1.4.4". A plain
// `go build` leaves it at "dev", which is what marks a from-source binary.
var Version = Dev

// Dev is the version a build without the ldflag reports.
const Dev = "dev"

// String returns the build version rendered as v<a>.<b>.<c>; see Format.
func String() string { return Format(Version) }

// Format renders an old v<count> tag as v<a>.<b>.<c>, where b and c are the last two digits of
// the count and a is everything before them: v123 -> v1.2.3, v1123 -> v11.2.3, and a shorter
// count is left-padded, so v5 is v0.0.5. That is the same mapping vstamp stamps into .version,
// so anything else (dev, a dotted version) is returned unchanged.
func Format(version string) string {
	count, ok := strings.CutPrefix(version, "v")
	if !ok || count == "" {
		return version
	}
	for _, digit := range count {
		if digit < '0' || digit > '9' {
			return version
		}
	}
	for len(count) < 3 {
		count = "0" + count
	}
	return "v" + count[:len(count)-2] + "." + count[len(count)-2:len(count)-1] + "." + count[len(count)-1:]
}
