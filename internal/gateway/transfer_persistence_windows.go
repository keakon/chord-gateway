//go:build windows

package gateway

// Windows has no takeover endpoints. This store still reads restart guards.
func syncTransferDirectory(string) error { return nil }
