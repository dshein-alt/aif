// Package version holds the server's version and build id as a leaf package: no imports beyond
// the standard library, so a second binary can depend on it without pulling in internal/core
// (which depends on PostgreSQL/pgx).
package version

// Version of the Go port.
const Version = "0.5.1"

// BuildID is the running build identity (git sha or pkg hash), or "" if unknown. The build bakes
// it in via -ldflags "-X github.com/dshein-alt/aif/internal/version.BuildID=..."; empty means
// "cannot tell".
var BuildID = ""
