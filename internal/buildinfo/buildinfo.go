// Package buildinfo reports version and source metadata for Wormzy binaries.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

var (
	version   = "dev"
	commit    string
	buildDate string
)

// Info describes the source and toolchain used to build a Wormzy binary.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
	GoVersion string
	Modified  bool
}

// Current returns build metadata injected by the build or discovered from Go's
// embedded module and VCS settings.
func Current() Info {
	current := Info{
		Version:   strings.TrimSpace(version),
		Commit:    strings.TrimSpace(commit),
		BuildDate: strings.TrimSpace(buildDate),
		GoVersion: runtime.Version(),
	}

	goInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return current
	}
	if current.Version == "" || current.Version == "dev" {
		if goInfo.Main.Version != "" && goInfo.Main.Version != "(devel)" {
			current.Version = goInfo.Main.Version
		}
	}
	if goInfo.GoVersion != "" {
		current.GoVersion = goInfo.GoVersion
	}
	for _, setting := range goInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			if current.Commit == "" {
				current.Commit = setting.Value
			}
		case "vcs.modified":
			current.Modified = setting.Value == "true"
		}
	}
	return current
}

// VersionString returns the display version, including dirty-tree state.
func (i Info) VersionString() string {
	version := strings.TrimSpace(i.Version)
	if version == "" {
		version = "dev"
	}
	dirtySuffix := strings.HasSuffix(version, "-dirty") || strings.HasSuffix(version, "+dirty")
	if i.Modified && !dirtySuffix {
		version += "-dirty"
	}
	return version
}

// Format returns human-readable version details for a named binary.
func (i Info) Format(program string) string {
	name := strings.TrimSpace(program)
	if name == "" {
		name = "wormzy"
	}

	lines := []string{fmt.Sprintf("%s %s", name, i.VersionString())}
	if i.Commit != "" {
		lines = append(lines, "commit: "+i.Commit)
	}
	if i.BuildDate != "" {
		lines = append(lines, "built: "+i.BuildDate)
	}
	if i.GoVersion != "" {
		lines = append(lines, "go: "+i.GoVersion)
	}
	return strings.Join(lines, "\n")
}
