// Package buildinfo exposes a single source of truth for the running
// chord-gateway binary's identity (version, commit, dirty state, build/VCS
// time, Go toolchain, executable path/mtime). It is consumed by the CLI
// version output, startup logs, and the chord-binary metadata included in
// process spawn diagnostics.
//
// The values are computed once on first use and cached for the rest of the
// process lifetime; no field changes after the binary starts.
package buildinfo

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Build-time variables. Release/CI builds may override these via -ldflags, e.g.
//
//	-X github.com/keakon/chord-gateway/internal/buildinfo.Version=v0.1.0
//	-X github.com/keakon/chord-gateway/internal/buildinfo.Commit=$(git rev-parse HEAD)
//	-X github.com/keakon/chord-gateway/internal/buildinfo.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)
//	-X github.com/keakon/chord-gateway/internal/buildinfo.Dirty=false
//
// The historical `-X main.version=<version>` path is bridged from main.go
// to Version here during package init for backwards compatibility with
// existing CI workflows.
//
// Plain `go build` still records useful VCS fields through Go's build info
// when the build is performed inside a Git checkout with buildvcs enabled, so
// Commit and Dirty remain populated even without ldflags.
var (
	Version   = "dev"
	Commit    = ""
	BuildTime = ""
	Dirty     = ""
)

// Info describes the running gateway binary and the Go toolchain metadata
// embedded in it. BuildTime is only populated when explicitly injected by the
// build; VCSTime is the source revision time reported by Go build info and is
// not the same thing.
type Info struct {
	Version         string
	Commit          string
	BuildTime       string
	VCSTime         string
	Dirty           string // "true", "false", or "unknown"
	GoVersion       string
	GOOS            string
	GOARCH          string
	ExecutablePath  string
	ExecutableMTime string
}

// Field is a single key/value pair for diagnostics output. Field is a named
// type (rather than an anonymous struct) so callers can declare variables and
// helpers around the slice returned by [Info.Fields].
type Field struct {
	Key   string
	Value string
}

// BinaryMetadata describes a filesystem binary path for diagnostic logging.
type BinaryMetadata struct {
	Path  string
	MTime string
}

const unknown = "unknown"

// current is the cached result of [computeCurrent]. The build identity does
// not change during a process's lifetime, so we read os.Stat / debug.ReadBuildInfo
// at most once per process.
var current = sync.OnceValue(computeCurrent)

// Current returns best-effort build metadata for the running binary. Explicit
// ldflags values take precedence over Go VCS fallback fields. The result is
// cached after the first call.
func Current() Info { return current() }

func computeCurrent() Info {
	settings := readBuildSettings()
	info := Info{
		Version:   valueOrUnknown(Version),
		Commit:    strings.TrimSpace(Commit),
		BuildTime: strings.TrimSpace(BuildTime),
		VCSTime:   strings.TrimSpace(settings["vcs.time"]),
		Dirty:     strings.TrimSpace(Dirty),
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
	if info.Commit == "" {
		info.Commit = strings.TrimSpace(settings["vcs.revision"])
	}
	if info.Commit == "" {
		info.Commit = unknown
	}
	if info.BuildTime == "" {
		info.BuildTime = unknown
	}
	if info.VCSTime == "" {
		info.VCSTime = unknown
	}
	if info.Dirty == "" {
		info.Dirty = strings.TrimSpace(settings["vcs.modified"])
	}
	if info.Dirty == "" {
		info.Dirty = unknown
	}
	meta := MetadataForPath("")
	info.ExecutablePath = meta.Path
	info.ExecutableMTime = meta.MTime
	return info
}

// MetadataForPath returns path and mtime diagnostics for path. If path is empty,
// it inspects the current executable.
func MetadataForPath(path string) BinaryMetadata {
	if strings.TrimSpace(path) == "" {
		exe, err := os.Executable()
		if err != nil || strings.TrimSpace(exe) == "" {
			return BinaryMetadata{Path: unknown, MTime: unknown}
		}
		path = exe
	}
	info, err := os.Stat(path)
	if err != nil {
		return BinaryMetadata{Path: path, MTime: unknown}
	}
	mtime := info.ModTime()
	if mtime.IsZero() {
		return BinaryMetadata{Path: path, MTime: unknown}
	}
	return BinaryMetadata{Path: path, MTime: mtime.Format(time.RFC3339Nano)}
}

// Short returns a compact one-line identity intended for human-facing
// surfaces (e.g. a verbose CLI output). It includes the version, short commit
// (when known), and a `dirty` marker only when the working tree was modified
// at build time. A clean or unknown dirty state is omitted to keep the line
// concise.
func (i Info) Short() string {
	parts := []string{valueOrUnknown(i.Version)}
	if commit := shortCommit(i.Commit); commit != "" && commit != unknown {
		parts = append(parts, commit)
	}
	if i.Dirty == "true" {
		parts = append(parts, "dirty")
	}
	return strings.Join(parts, " ")
}

// Fields returns the full set of gateway diagnostic key/value pairs in stable
// order. Used by startup-style metadata so every field line has the same
// `key: value` shape and ordering across surfaces.
func (i Info) Fields() []Field {
	return []Field{
		{"gateway_version", valueOrUnknown(i.Version)},
		{"gateway_commit", valueOrUnknown(i.Commit)},
		{"gateway_build_time", valueOrUnknown(i.BuildTime)},
		{"gateway_vcs_time", valueOrUnknown(i.VCSTime)},
		{"gateway_dirty", valueOrUnknown(i.Dirty)},
		{"go_version", valueOrUnknown(i.GoVersion)},
		{"goos", valueOrUnknown(i.GOOS)},
		{"goarch", valueOrUnknown(i.GOARCH)},
		{"executable_path", valueOrUnknown(i.ExecutablePath)},
		{"executable_mtime", valueOrUnknown(i.ExecutableMTime)},
	}
}

// LogString returns a compact key=value list for startup logs. It includes
// only the fields that are meaningful at every startup (version, commit,
// dirty state, build/VCS time, Go toolchain). Long-tail metadata such as
// executable path and mtime is reserved for fuller diagnostic surfaces to
// keep the startup line a manageable length.
func (i Info) LogString() string {
	startupKeys := map[string]struct{}{
		"gateway_version":    {},
		"gateway_commit":     {},
		"gateway_dirty":      {},
		"gateway_build_time": {},
		"gateway_vcs_time":   {},
		"go_version":         {},
	}
	fields := i.Fields()
	parts := make([]string, 0, len(startupKeys))
	for _, field := range fields {
		if _, ok := startupKeys[field.Key]; !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%q", field.Key, field.Value))
	}
	return strings.Join(parts, " ")
}

func readBuildSettings() map[string]string {
	settings := make(map[string]string)
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi == nil {
		return settings
	}
	for _, setting := range bi.Settings {
		settings[setting.Key] = setting.Value
	}
	return settings
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if commit == "" || commit == unknown {
		return commit
	}
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

func valueOrUnknown(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return unknown
	}
	return value
}
