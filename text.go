package main

import "strings"

// text.go centralizes string truncation helpers used across the gateway.
//
// All helpers operate on runes so multi-byte UTF-8 sequences (Chinese
// characters, emoji) are never split mid-character.

const ellipsis = "…"

// truncateRunes shortens s to at most maxRunes runes, appending the supplied
// suffix when truncation occurs. If maxRunes <= len([]rune(suffix)) the suffix
// alone is returned (truncated to maxRunes); maxRunes <= 0 yields "".
func truncateRunes(s string, maxRunes int, suffix string) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	suffixRunes := []rune(suffix)
	if maxRunes <= len(suffixRunes) {
		return string(suffixRunes[:maxRunes])
	}
	return string(runes[:maxRunes-len(suffixRunes)]) + suffix
}

// truncate shortens s to fit within maxNotificationRunes. Adds an ASCII
// ellipsis ("...") to match historic IM-friendly output.
func truncate(s string) string {
	const ascii = "..."
	return truncateRunes(s, maxNotificationRunes, ascii)
}

// truncateLine collapses newlines to "\n" literals and truncates the result to
// maxRunes runes. Used to render single-line previews of tool arguments.
func truncateLine(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	return truncateRunes(s, maxRunes, ellipsis)
}

// truncateButtonLabel returns label trimmed and truncated to fit a Feishu card
// button (24 runes max).
func truncateButtonLabel(label string) string {
	return truncateRunes(strings.TrimSpace(label), 24, ellipsis)
}

// shortID renders an opaque identifier as <head>…<tail>, falling back to the
// raw value when it's already short enough.
func shortID(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= 12 {
		return s
	}
	return string(r[:8]) + ellipsis + string(r[len(r)-4:])
}

// truncateStderrTail returns at most max bytes from the end of s. Used to
// excerpt child-process stderr without unbounded memory growth. Operates on
// bytes (not runes) because the source is a raw byte buffer.
func truncateStderrTail(s string, max int) string {
	if max <= 0 {
		max = 2000
	}
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}
