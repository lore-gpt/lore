// Package core is the open-core import surface of the Lore server.
//
// Everything a downstream binary (including a closed-source one) needs to
// compose a server lives under core/ and its subpackages. The OSS binary wires
// these together in server/cmd/lore.
package core

// Version is the single source of truth for the Lore build version. It is
// surfaced by the GET /healthz endpoint, the `lore version` subcommand, and the
// `lore --version` flag.
//
// It is a var (not a const) precisely so the release build can overwrite it at
// link time with the git tag via -ldflags "-X
// github.com/lore-gpt/lore/core.Version=<tag>"; a const cannot be overwritten by
// -X. The default marks an untagged local/dev build.
var Version = "0.0.0-dev"
