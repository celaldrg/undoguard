// Package version reports the undoguard version.
package version

import "runtime/debug"

// Version is set at build time with
// -ldflags "-X github.com/celaldrg/undoguard/internal/version.Version=v1.0.0".
var Version = "dev"

// String returns the version: the one set at build time, else the module
// version recorded by go install, else "dev".
func String() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}
