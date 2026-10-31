package im

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Segmenting an outbound message renders budget probes of the remaining text,
// so its cost grows with message length and platform budget. These benchmarks
// keep the two shipping adapter shapes honest: markdown escaping counted in
// UTF-16 units (discord) and escaping counted in bytes (wecom).

func benchEscapeMarkdown(source string) string {
	var out strings.Builder
	for _, r := range source {
		switch r {
		case '\\', '`', '*', '_', '{', '}', '[', ']', '(', ')', '#', '+', '-', '.', '!', '|', '~', '<', '>':
			out.WriteByte('\\')
			out.WriteRune(r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func benchSample(bytes int, cjk bool) string {
	unit := "The quick brown fox writes a report, then lists `items` and <tags>.\n"
	if cjk {
		unit = "这是一段用于核对分段成本的中文正文，包含 *强调*、<标签> 与常见标点。\n"
	}
	var out strings.Builder
	for out.Len() < bytes {
		out.WriteString(unit)
	}
	return out.String()
}

func BenchmarkIMTextSegments(b *testing.B) {
	adapters := []struct {
		name    string
		budget  int
		render  func(string) string
		measure func(string) int
	}{
		{"utf16budget", 1900, benchEscapeMarkdown, UTF16Units},
		{"bytebudget", 1024, benchEscapeMarkdown, func(s string) int { return len(s) }},
	}
	for _, size := range []int{4 << 10, 16 << 10, MaxTextBytes} {
		for _, adapter := range adapters {
			for _, cjk := range []bool{false, true} {
				text := benchSample(size, cjk)
				b.Run(fmt.Sprintf("%s/%dKiB/cjk=%v", adapter.name, size>>10, cjk), func(b *testing.B) {
					s, err := NewIMTextSender(IMTextSendOptions{
						Platform: adapter.name, Budget: adapter.budget, Render: adapter.render, Measure: adapter.measure,
						SendSegment: func(context.Context, string, string) error { return nil },
					})
					if err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.SetBytes(int64(len(text)))
					for b.Loop() {
						segments, err := s.Segments(text)
						if err != nil {
							b.Fatal(err)
						}
						if len(segments) == 0 {
							b.Fatal("no segments")
						}
					}
				})
			}
		}
	}
}
