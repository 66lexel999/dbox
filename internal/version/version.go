// Package version is the single source of truth for the app's build identity.
// Version and ManifestURL are overridden at build time via -ldflags -X so the
// release script can stamp them without editing source:
//
//	-ldflags "-X myidm/internal/version.Version=1.1.0
//	          -X myidm/internal/version.ManifestURL=https://you.example/latest.json"
package version

// Version is the running build's semantic version ("1.2.0"). "dev" for a plain
// `go build` with no ldflags (update checks are skipped for "dev").
var Version = "dev"

// ManifestURL points at the static latest.json describing the newest release.
// Empty (the default) disables the in-app update check entirely. Set it to your
// GitHub Pages / Netlify URL (see DEPLOY.md), e.g.:
//
//	https://<user>.github.io/dbox/latest.json
var ManifestURL = ""

// IsDev reports a build that shouldn't self-update (unstamped local build).
func IsDev() bool { return Version == "dev" || Version == "" }
