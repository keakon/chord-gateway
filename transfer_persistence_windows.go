//go:build windows

package main

// Windows has no takeover endpoints. This store still reads restart guards.
func syncTransferDirectory(string) error { return nil }
