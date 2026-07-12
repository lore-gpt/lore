// Command sqlc-tenant-lint is the first belt of tenant isolation: a compile-time / CI guard
// that fails the build when a query touching a tenant-scoped table is written without a
// project_id filter. It is the developer-facing complement to the database's Row-Level
// Security (migration 0008) — RLS is the runtime backstop that always holds, this catches the
// mistake earlier, at review time, and works even where RLS is bypassed (the owner/migration
// connection).
//
// It is deliberately a HEURISTIC, not a full SQL parser. For each named query it strips `--`
// comments and '...' string literals (so a ';' or a table name inside them cannot mislead),
// splits the block into statements on ';', and for every statement that mentions a tenant table
// requires that the statement also CONSTRAINS project_id — a `project_id = ...` predicate
// (WHERE/ON) or project_id among an INSERT's columns (the new row carries its tenant). Merely
// selecting project_id in the projection does not count: `SELECT project_id FROM events WHERE
// id = $1` is not tenant-scoped. That can still be fooled by sufficiently exotic SQL, but the
// cost of a false negative is bounded — RLS enforces isolation at runtime — and the check stays
// simple. A query that is legitimately cross-tenant (creating the tenant row itself, a global
// admin/metrics read) opts out with a marker that REQUIRES a written justification:
//
//	-- name: CountAllEvents :one
//	-- lore:tenant-exempt: global by design; runs under a bypass role, not the app role
//	SELECT count(*) FROM events;
//
// The marker must be its own comment line, only exempts the -- name: block it sits in, and an
// empty reason is itself a violation.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// tenantTables are the project-scoped tables under Row-Level Security (migration 0008). Keep
// this list in sync with the RLS policies: a query touching one of these must be project-scoped.
// organizations is intentionally absent — it sits above the tenant boundary.
var tenantTables = []string{
	"projects", "api_keys", "runs", "events", "memories", "embeddings",
	"memory_versions", "memory_scopes", "claims", "entities", "entity_links", "pack_logs",
}

var (
	tenantRe = regexp.MustCompile(`(?i)\b(` + strings.Join(tenantTables, "|") + `)\b`)
	nameRe   = regexp.MustCompile(`(?m)^--\s*name:\s*(\S+)`)
	// The exemption marker must be its own comment line (anchored), so the phrase appearing in
	// incidental prose does not silently disable the check.
	exemptRe = regexp.MustCompile(`(?im)^[ \t]*--[ \t]*lore:tenant-exempt:(.*)$`)

	// A statement is tenant-SCOPED if it constrains project_id in a predicate (project_id = / IN /
	// IS ...) or lists project_id among an INSERT's columns (so the inserted row carries its
	// tenant). Selecting project_id in the projection is not scoping — that is the by-id trap.
	scopePredRe = regexp.MustCompile(`(?i)\bproject_id\s*(=|<>|!=|>=|<=|<|>|\bin\b|\bis\b)`)
	insertColRe = regexp.MustCompile(`(?is)\binsert\s+into\s+\w+\s*\([^)]*\bproject_id\b`)
)

type violation struct {
	file  string
	query string
	msg   string
}

// lintContent checks one .sql file's named queries. Each query is the text from its `-- name:`
// line to the next `-- name:` (or EOF). An exemption marker on its own comment line exempts the
// whole block (an empty reason is a violation); otherwise each statement touching a tenant table
// must constrain project_id.
func lintContent(file, content string) []violation {
	var out []violation
	locs := nameRe.FindAllStringSubmatchIndex(content, -1)
	for i, loc := range locs {
		end := len(content)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := content[loc[0]:end]
		name := content[loc[2]:loc[3]]

		if m := exemptRe.FindStringSubmatch(block); m != nil {
			if strings.TrimSpace(m[1]) == "" {
				out = append(out, violation{file, name,
					"-- lore:tenant-exempt marker requires a written reason (found an empty one)"})
			}
			continue // a deliberate, justified exemption
		}

		for _, stmt := range strings.Split(stripSQL(block), ";") {
			table := tenantRe.FindString(stmt)
			if table == "" {
				continue
			}
			if !scopePredRe.MatchString(stmt) && !insertColRe.MatchString(stmt) {
				out = append(out, violation{file, name, fmt.Sprintf(
					"touches tenant table %q but does not scope by project_id (no `project_id = ...` filter, no project_id in an INSERT column list) and has no `-- lore:tenant-exempt: <reason>` marker", table)})
			}
		}
	}
	return out
}

// stripSQL blanks out `--` line comments and '...' single-quoted string literals (replacing their
// bytes with spaces so line breaks are preserved), so a ';' or a tenant-table name appearing
// inside a comment or a literal cannot fool the statement split or the table/scope detection.
func stripSQL(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inLine, inStr := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inLine:
			b.WriteByte(blankExceptNewline(c))
			if c == '\n' {
				inLine = false
			}
		case inStr:
			b.WriteByte(' ')
			if c == '\'' {
				inStr = false
			}
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			inLine = true
			b.WriteByte(' ')
		case c == '\'':
			inStr = true
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func blankExceptNewline(c byte) byte {
	if c == '\n' {
		return c
	}
	return ' '
}

// lint checks a set of file name -> content pairs, returning violations in a stable order.
func lint(files map[string]string) []violation {
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	var out []violation
	for _, f := range names {
		out = append(out, lintContent(f, files[f])...)
	}
	return out
}

// fprintf writes a best-effort diagnostic line; a failure to write to the output stream is
// unrecoverable for a lint tool and not worth propagating.
func fprintf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func main() {
	dir := "core/store/queries"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	os.Exit(run(dir, os.Stdout, os.Stderr))
}

// run lints every *.sql file under dir and returns a process exit code: 0 clean, 1 violations,
// 2 an operational error (no files, unreadable file). It is separated from main so tests can
// assert the exit path without spawning a process.
func run(dir string, out, errOut io.Writer) int {
	matches, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		fprintf(errOut, "sqlc-tenant-lint: %v\n", err)
		return 2
	}
	if len(matches) == 0 {
		fprintf(errOut, "sqlc-tenant-lint: no .sql files under %s\n", dir)
		return 2
	}
	files := make(map[string]string, len(matches))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			fprintf(errOut, "sqlc-tenant-lint: %v\n", err)
			return 2
		}
		files[m] = string(b)
	}
	violations := lint(files)
	for _, v := range violations {
		fprintf(errOut, "%s: query %s: %s\n", v.file, v.query, v.msg)
	}
	if len(violations) > 0 {
		fprintf(errOut, "sqlc-tenant-lint: %d violation(s)\n", len(violations))
		return 1
	}
	fprintf(out, "sqlc-tenant-lint: %d query file(s) clean — every tenant query is scoped or exempt\n", len(matches))
	return 0
}
