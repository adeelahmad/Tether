// Package version carries build metadata stamped in with -ldflags:
//
//	-X github.com/adeelahmad/tether/internal/version.Version=v1.2.3
//	-X github.com/adeelahmad/tether/internal/version.Commit=abc1234
//	-X github.com/adeelahmad/tether/internal/version.Date=2026-10-11T00:00:00Z
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func init() {
	// `go install …@vX` builds carry module and VCS info even without
	// ldflags; use it as a fallback.
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch {
		case s.Key == "vcs.revision" && Commit == "none" && len(s.Value) >= 7:
			Commit = s.Value[:7]
		case s.Key == "vcs.time" && Date == "unknown":
			Date = s.Value
		}
	}
}

// String is the one-line version banner.
func String() string {
	return fmt.Sprintf("tether %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
