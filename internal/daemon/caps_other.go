//go:build !linux

package daemon

// dropAmbientCaps is Linux only: no other system has ambient capabilities.
func dropAmbientCaps() error { return nil }
