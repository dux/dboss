//go:build linux && !amd64 && !arm64

package sandbox

// auditArch 0 means the socket filter has no rules for this architecture, so no sandbox runs.
const auditArch = 0
