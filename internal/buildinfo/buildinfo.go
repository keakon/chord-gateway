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
	"regexp"
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
// Plain `go build` still records useful VCS fields through Go's build info
// when the build is performed inside a Git checkout with buildvcs enabled, so
// Commit and Dirty remain populated even without ldflags.
var (
	Version   = ""
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

const (
	unknown           = "unknown"
	DefaultDevVersion = "v0.3.2-dev"
)

// current is the cached result of [computeCurrent]. The build identity does
// not change during a process's lifetime, so we read os.Stat / debug.ReadBuildInfo
// at most once per process.
var current = sync.OnceValue(computeCurrent)

var (
	semverTagPattern      = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?(?:\+[0-9A-Za-z][0-9A-Za-z.-]*)?$`)
	pseudoVersionSuffixRE = regexp.MustCompile(`(?:^|[.-])[0-9]{14}-[0-9a-fA-F]{12}$`)
)

// Current returns best-effort build metadata for the running binary. Explicit
// ldflags values take precedence over Go VCS fallback fields. The result is
// cached after the first call.
func Current() Info { return current() }

func computeCurrent() Info {
	metadata := readBuildMetadata()
	info := Info{
		Version:   resolvedVersion(Version, metadata.moduleVersion),
		Commit:    strings.TrimSpace(Commit),
		BuildTime: strings.TrimSpace(BuildTime),
		VCSTime:   strings.TrimSpace(metadata.settings["vcs.time"]),
		Dirty:     strings.TrimSpace(Dirty),
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
	if info.Commit == "" {
		info.Commit = strings.TrimSpace(metadata.settings["vcs.revision"])
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
		info.Dirty = strings.TrimSpace(metadata.settings["vcs.modified"])
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
// surfaces. It includes the version, a trailing `*` on the version when the
// working tree was modified at build time, and the short commit when known.
// A clean or unknown dirty state is omitted to keep the line concise.
func (i Info) Short() string {
	version := valueOrUnknown(i.Version)
	if i.Dirty == "true" {
		version += "*"
	}
	parts := []string{version}
	if commit := shortCommit(i.Commit); commit != "" && commit != unknown {
		parts = append(parts, commit)
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

type buildMetadata struct {
	moduleVersion string
	settings      map[string]string
}

func readBuildMetadata() buildMetadata {
	metadata := buildMetadata{settings: make(map[string]string)}
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi == nil {
		return metadata
	}
	metadata.moduleVersion = bi.Main.Version
	for _, setting := range bi.Settings {
		metadata.settings[setting.Key] = setting.Value
	}
	return metadata
}

func resolvedVersion(explicitVersion, moduleVersion string) string {
	if version := strings.TrimSpace(explicitVersion); version != "" {
		return version
	}
	if version := strings.TrimSpace(moduleVersion); isReleaseModuleVersion(version) {
		return version
	}
	return DefaultDevVersion
}

func isReleaseModuleVersion(version string) bool {
	baseVersion, _, _ := strings.Cut(version, "+")
	return semverTagPattern.MatchString(version) && !pseudoVersionSuffixRE.MatchString(baseVersion)
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
