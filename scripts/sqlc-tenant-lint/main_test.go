package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintContent(t *testing.T) {
	cases := []struct {
		name    string
		sql     string
		wantN   int
		wantSub string // substring expected in the (single) violation message, if wantN == 1
	}{
		{
			name:  "scoped query passes",
			sql:   "-- name: GetMemory :one\nSELECT id FROM memories WHERE project_id = $1 AND id = $2;",
			wantN: 0,
		},
		{
			name:  "insert carrying project_id passes",
			sql:   "-- name: InsertClaim :one\nINSERT INTO claims (memory_id, project_id) VALUES ($1, $2) RETURNING id;",
			wantN: 0,
		},
		{
			name:  "insert-select carrying project_id in the column list passes",
			sql:   "-- name: InsertEvent :one\nINSERT INTO events (project_id, run_id) SELECT r.project_id, r.id FROM runs r WHERE r.id = $1 RETURNING id, project_id;",
			wantN: 0,
		},
		{
			name:  "non-tenant table is not checked",
			sql:   "-- name: InsertOrg :one\nINSERT INTO organizations (name) VALUES ($1) RETURNING id;",
			wantN: 0,
		},
		{
			name:    "tenant query without project_id fails",
			sql:     "-- name: GetEvent :one\nSELECT id FROM events WHERE id = $1;",
			wantN:   1,
			wantSub: "does not scope by project_id",
		},
		{
			name:    "selecting project_id without filtering by it is not scoping",
			sql:     "-- name: GetEvent :one\nSELECT id, project_id FROM events WHERE id = $1;",
			wantN:   1,
			wantSub: "does not scope by project_id",
		},
		{
			name:  "exemption with a reason passes",
			sql:   "-- name: CountAllEvents :one\n-- lore:tenant-exempt: global by design; runs under a bypass role\nSELECT count(*) FROM events;",
			wantN: 0,
		},
		{
			name:    "exemption with an empty reason fails",
			sql:     "-- name: CountAllEvents :one\n-- lore:tenant-exempt:\nSELECT count(*) FROM events;",
			wantN:   1,
			wantSub: "requires a written reason",
		},
		{
			name:    "exemption with only whitespace reason fails",
			sql:     "-- name: CountAllEvents :one\n-- lore:tenant-exempt:   \nSELECT count(*) FROM events;",
			wantN:   1,
			wantSub: "requires a written reason",
		},
		{
			// A second, unscoped statement after a scoped one must still be caught.
			name:    "multi-statement: an unscoped tenant statement after a scoped one fails",
			sql:     "-- name: X :exec\nUPDATE runs SET status = 's' WHERE project_id = $1;\nUPDATE events SET seq = 0;",
			wantN:   1,
			wantSub: "does not scope by project_id",
		},
		{
			name:  "multi-statement: all statements scoped passes",
			sql:   "-- name: X :exec\nUPDATE runs SET status = 's' WHERE project_id = $1;\nUPDATE events SET seq = 0 WHERE project_id = $1;",
			wantN: 0,
		},
		{
			// A ';' inside a string literal must not truncate the statement and drop the filter.
			name:  "semicolon inside a string literal is not a statement boundary",
			sql:   "-- name: X :one\nSELECT id FROM events WHERE note = 'a;b' AND project_id = $1;",
			wantN: 0,
		},
		{
			// The marker phrase in ordinary prose must not exempt the block.
			name:    "the marker phrase in prose does not exempt",
			sql:     "-- name: X :one\n-- note: we removed the -- lore:tenant-exempt: line earlier\nSELECT id FROM events WHERE id = $1;",
			wantN:   1,
			wantSub: "does not scope by project_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lintContent("test.sql", tc.sql)
			if len(got) != tc.wantN {
				t.Fatalf("got %d violations, want %d: %+v", len(got), tc.wantN, got)
			}
			if tc.wantN == 1 && !strings.Contains(got[0].msg, tc.wantSub) {
				t.Errorf("violation %q does not contain %q", got[0].msg, tc.wantSub)
			}
		})
	}
}

// TestMarkerIsBlockScoped proves an exemption marker attached to one query does not leak onto a
// following query that shares the file: the second, unscoped query still fails.
func TestMarkerIsBlockScoped(t *testing.T) {
	sql := "-- name: CountAllEvents :one\n" +
		"-- lore:tenant-exempt: global by design\n" +
		"SELECT count(*) FROM events;\n\n" +
		"-- name: GetEvent :one\n" +
		"SELECT id FROM events WHERE id = $1;\n"
	got := lintContent("test.sql", sql)
	if len(got) != 1 {
		t.Fatalf("got %d violations, want 1 (the marker must not exempt the second query): %+v", len(got), got)
	}
	if got[0].query != "GetEvent" {
		t.Errorf("violation is on %q, want GetEvent", got[0].query)
	}
}

// TestLintOrdersByFile locks the stable, file-sorted output contract of lint().
func TestLintOrdersByFile(t *testing.T) {
	files := map[string]string{
		"z.sql": "-- name: Z :one\nSELECT id FROM events WHERE id = $1;",
		"a.sql": "-- name: A :one\nSELECT id FROM claims WHERE id = $1;",
	}
	got := lint(files)
	if len(got) != 2 {
		t.Fatalf("want 2 violations, got %d: %+v", len(got), got)
	}
	if got[0].file != "a.sql" || got[1].file != "z.sql" {
		t.Errorf("violations not sorted by file: %q then %q", got[0].file, got[1].file)
	}
}

// TestRun covers the exit-code contract the CI gate depends on: 0 clean, 1 violations, 2 no files.
func TestRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write("ok.sql", "-- name: G :one\nSELECT id FROM memories WHERE project_id = $1;")
	if code := run(dir, io.Discard, io.Discard); code != 0 {
		t.Errorf("clean dir exit = %d, want 0", code)
	}

	write("bad.sql", "-- name: B :one\nSELECT id FROM events WHERE id = $1;")
	if code := run(dir, io.Discard, io.Discard); code != 1 {
		t.Errorf("dir with a violation exit = %d, want 1", code)
	}

	if code := run(t.TempDir(), io.Discard, io.Discard); code != 2 {
		t.Errorf("empty dir exit = %d, want 2", code)
	}
}
