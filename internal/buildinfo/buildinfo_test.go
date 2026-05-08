package buildinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentIncludesFallbackMetadata(t *testing.T) {
	info := Current()
	if info.Version == "" {
		t.Fatal("Version is empty")
	}
	if info.Commit == "" {
		t.Fatal("Commit is empty")
	}
	if info.BuildTime == "" {
		t.Fatal("BuildTime is empty")
	}
	if info.VCSTime == "" {
		t.Fatal("VCSTime is empty")
	}
	if info.Dirty == "" {
		t.Fatal("Dirty is empty")
	}
	if info.GoVersion == "" || info.GOOS == "" || info.GOARCH == "" {
		t.Fatalf("toolchain metadata incomplete: %+v", info)
	}
	if info.ExecutablePath == "" || info.ExecutableMTime == "" {
		t.Fatalf("executable metadata incomplete: %+v", info)
	}
}

func TestCurrentIsCached(t *testing.T) {
	// Two consecutive calls must return identical structs without re-reading
	// os.Stat / debug.ReadBuildInfo. ExecutableMTime in particular is the
	// easiest field to observe a change on if caching breaks.
	a := Current()
	b := Current()
	if a != b {
		t.Fatalf("Current() not cached: %+v vs %+v", a, b)
	}
}

func TestFieldsIncludeGatewayDiagnosticsKeys(t *testing.T) {
	info := Info{
		Version:         "dev",
		Commit:          "abcdef1234567890",
		BuildTime:       "unknown",
		VCSTime:         "2026-05-05T00:00:00Z",
		Dirty:           "true",
		GoVersion:       "go-test",
		GOOS:            "testos",
		GOARCH:          "testarch",
		ExecutablePath:  "/tmp/chord-gateway",
		ExecutableMTime: "2026-05-05T01:00:00Z",
	}
	fields := info.Fields()
	got := make(map[string]string, len(fields))
	for _, field := range fields {
		got[field.Key] = field.Value
	}
	for _, key := range []string{
		"gateway_version",
		"gateway_commit",
		"gateway_build_time",
		"gateway_vcs_time",
		"gateway_dirty",
		"go_version",
		"goos",
		"goarch",
		"executable_path",
		"executable_mtime",
	} {
		if got[key] == "" {
			t.Fatalf("missing diagnostics key %q in %#v", key, got)
		}
	}
}

func TestFieldsReturnsNamedType(t *testing.T) {
	// Compile-time guarantee that callers can declare variables of the
	// returned slice element type — i.e. the type is exported as Field,
	// not an anonymous struct.
	f := Field{Key: "k", Value: "v"}
	if f.Key != "k" || f.Value != "v" {
		t.Fatalf("Field literal mismatch: %+v", f)
	}

	info := Info{Version: "dev"}
	fields := info.Fields()
	if len(fields) == 0 {
		t.Fatal("Fields() returned empty slice")
	}
	// Assigning to []Field also documents the contract.
	var _ []Field = fields
}

func TestFieldsSubstitutesUnknownForEmpty(t *testing.T) {
	info := Info{} // every value is the zero string
	for _, field := range info.Fields() {
		if field.Value != "unknown" {
			t.Fatalf("field %q = %q, want %q", field.Key, field.Value, "unknown")
		}
	}
}

func TestShortIncludesDirtyOnlyWhenTrue(t *testing.T) {
	cases := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "dirty true",
			info: Info{Version: "dev", Commit: "abcdef1234567890", Dirty: "true"},
			want: "dev abcdef123456 dirty",
		},
		{
			name: "dirty false omitted",
			info: Info{Version: "dev", Commit: "abcdef1234567890", Dirty: "false"},
			want: "dev abcdef123456",
		},
		{
			name: "dirty unknown omitted",
			info: Info{Version: "dev", Commit: "abcdef1234567890", Dirty: "unknown"},
			want: "dev abcdef123456",
		},
		{
			name: "no commit",
			info: Info{Version: "v1.0.0", Commit: "unknown", Dirty: "false"},
			want: "v1.0.0",
		},
		{
			name: "short commit not truncated",
			info: Info{Version: "v1.0.0", Commit: "abc123", Dirty: "true"},
			want: "v1.0.0 abc123 dirty",
		},
		{
			name: "empty version becomes unknown",
			info: Info{Version: "", Commit: "", Dirty: ""},
			want: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.Short(); got != tc.want {
				t.Fatalf("Short() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLogStringIsCompact(t *testing.T) {
	info := Info{
		Version:         "v1.0.0",
		Commit:          "abc123",
		BuildTime:       "2026-05-05T00:00:00Z",
		VCSTime:         "2026-05-04T00:00:00Z",
		Dirty:           "false",
		GoVersion:       "go-test",
		GOOS:            "testos",
		GOARCH:          "testarch",
		ExecutablePath:  "/tmp/chord-gateway-very-long-path/chord-gateway",
		ExecutableMTime: "2026-05-05T01:00:00Z",
	}
	got := info.LogString()

	// LogString should include the small set of fields useful at every
	// startup, and *exclude* the long-tail metadata reserved for diagnostics.
	for _, want := range []string{
		`gateway_version="v1.0.0"`,
		`gateway_commit="abc123"`,
		`gateway_dirty="false"`,
		`gateway_build_time="2026-05-05T00:00:00Z"`,
		`gateway_vcs_time="2026-05-04T00:00:00Z"`,
		`go_version="go-test"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("LogString() = %q, missing %q", got, want)
		}
	}
	for _, banned := range []string{"executable_path", "executable_mtime", "goos", "goarch"} {
		if strings.Contains(got, banned) {
			t.Fatalf("LogString() should not include %q (reserved for diagnostics): %s", banned, got)
		}
	}
}

func TestShortCommitTruncation(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"", ""},
		{"unknown", "unknown"},
		{"abc", "abc"},
		{"abcdef123456", "abcdef123456"},  // exactly 12 chars: not truncated
		{"abcdef1234567", "abcdef123456"}, // 13 chars: truncate to 12
		{"abcdef1234567890abcdef", "abcdef123456"},
	}
	for _, tc := range cases {
		if got := shortCommit(tc.in); got != tc.out {
			t.Fatalf("shortCommit(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

func TestMetadataForPathIncludesMTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(path, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := MetadataForPath(path)
	if meta.Path != path {
		t.Fatalf("Path = %q, want %q", meta.Path, path)
	}
	if meta.MTime == "" || meta.MTime == "unknown" {
		t.Fatalf("MTime = %q, want timestamp", meta.MTime)
	}
}

func TestMetadataForPathFallsBackToExecutable(t *testing.T) {
	meta := MetadataForPath("")
	// We can't predict the exact path, but it must be set (either to the
	// actual executable or to "unknown" if os.Executable failed). The MTime
	// should mirror that decision.
	if meta.Path == "" {
		t.Fatal("Path empty for default lookup")
	}
	if meta.MTime == "" {
		t.Fatal("MTime empty for default lookup")
	}
}

func TestMetadataForPathMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	meta := MetadataForPath(missing)
	if meta.Path != missing {
		t.Fatalf("Path = %q, want %q", meta.Path, missing)
	}
	if meta.MTime != "unknown" {
		t.Fatalf("MTime = %q, want %q", meta.MTime, "unknown")
	}
}
