// Package build carries the identity a release stamps into the binary.
//
// It exists in wave 1 so the module has something real to vet, lint and test
// before the wire lands (#2010). Everything here is deliberately inert: the
// companion's behaviour is not this issue's scope.
package build

// Name is the product name reported by every surface.
const Name = "uffici-companion"

// DevVersion is the version reported by a binary built outside a release.
const DevVersion = "0.0.0-dev"

// version is overridden at link time by the release build:
//
//	go build -ldflags "-X github.com/uffici-ai/uffici-companion/internal/build.version=1.2.3"
var version string

// Version reports the release version, or DevVersion for an unstamped build.
func Version() string {
	if version == "" {
		return DevVersion
	}
	return version
}

// commit and buildDate are stamped at link time by the release build, beside
// version. They are reported by `uffici --version` so a customer can tie a
// running binary to a signed release.
var (
	commit    string
	buildDate string
)

// Commit reports the revision this binary was built from, or "unknown".
func Commit() string {
	if commit == "" {
		return "unknown"
	}
	return commit
}

// Date reports when this binary was built, or "unknown".
func Date() string {
	if buildDate == "" {
		return "unknown"
	}
	return buildDate
}
