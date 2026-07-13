// Package core is the open-core import surface of the Lore server.
//
// Everything a downstream binary (including a closed-source one) needs to
// compose a server lives under core/ and its subpackages. The OSS binary wires
// these together in server/cmd/lore.
package core

// Version is the single source of truth for the Lore build version. It is
// surfaced by the GET /healthz endpoint and the `lore version` subcommand.
//
// The release train (lockstep semver) overwrites this at build time
// via -ldflags; the default marks an untagged local/dev build.
const Version = "0.0.0-dev"
