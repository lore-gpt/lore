#!/usr/bin/env bash
# import-smoke — prove the open-core import surface (acceptance
# criterion 8).
#
# A throwaway module OUTSIDE this repo (so Go's internal-package rules apply as
# they would for a real downstream consumer such as lore-gpt/cloud) must be able
# to import and build against core/, core/httpapi, and core/ext — while
# server/internal/* must stay unimportable. If a future refactor hides composable
# code under server/internal, the positive build breaks; if it leaks bootstrap
# glue into an importable path, the negative check breaks.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# On Git Bash/Cygwin, translate the POSIX path to a Windows path (C:/…) that the
# Go toolchain accepts in a replace directive. A no-op on Linux CI.
if command -v cygpath >/dev/null 2>&1; then
	repo="$(cygpath -m "$repo")"
fi
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cat >"$work/go.mod" <<EOF
module importsmoke

go 1.26

require github.com/lore-gpt/lore v0.0.0

replace github.com/lore-gpt/lore => $repo
EOF

# Positive surface: the composition entrypoints and extension points a downstream
# build wires together.
cat >"$work/main.go" <<'EOF'
package main

import (
	"github.com/lore-gpt/lore/core"
	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/httpapi"
)

// Reference each surface so the imports are load-bearing.
var (
	_ = core.NewServer
	_ = core.NewWorker
	_ = httpapi.New
	_ ext.PolicyEngine = ext.BasicScopePolicy{}
)

func main() {}
EOF

# Negative surface: bootstrap glue that must NOT be importable from outside the
# module. Kept in its own package so it can be built (and expected to fail)
# independently of the positive build.
mkdir -p "$work/probe"
cat >"$work/probe/probe.go" <<'EOF'
package probe

import _ "github.com/lore-gpt/lore/server/internal/config"
EOF

cd "$work"
go mod tidy

echo "==> building external consumer of core, core/httpapi, core/ext ..."
go build .
echo "OK: open-core surface is importable"

echo "==> verifying server/internal is NOT importable from outside the module ..."
if go build ./probe 2>/dev/null; then
	echo "FAIL: server/internal/config was importable from an external module" >&2
	exit 1
fi
echo "OK: server/internal is correctly non-importable"

echo "import-smoke passed"
