package buildinfo

import "runtime/debug"

// Commit can be set at build time with -ldflags -X.
var Commit string

// SourceCommit returns the source revision recorded by the Go toolchain.
func SourceCommit() string {
	if Commit != "" {
		return Commit
	}
	if information, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range information.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}
	return "development"
}
