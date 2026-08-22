// Package version carries the build identity injected by CI through -ldflags.
// A plain `go build` leaves the placeholder values, which the updater treats as
// "always outdated" so local builds never silently skip an update check.
package version

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// Info returns the build identity as the flat map served by /api/version.
func Info() map[string]string {
	return map[string]string{
		"version":    Version,
		"commit":     Commit,
		"build_time": BuildTime,
	}
}
