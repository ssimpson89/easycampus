// Package version exposes build identity (semver tag, commit, date)
// injected by GoReleaser through -ldflags at release-build time.
//
// Development builds (go build / go run / make build) leave the
// defaults in place, so the binary still reports a meaningful "dev"
// banner instead of an empty string.
package version

// These values are overridden by GoReleaser ldflags for release builds.
// See .goreleaser.yaml; the matching -X path is
//
//	easycampus/internal/version.{Version,Commit,Date}
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is the JSON-friendly snapshot consumed by /api/health and the
// startup banner. Omitting empty Commit/Date keeps dev builds tidy.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`
}

// Current returns the build identity as it stands at link time. Pure
// function — no globals are mutated.
func Current() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
	}
}
