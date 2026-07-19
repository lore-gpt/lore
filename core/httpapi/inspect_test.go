package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestInspectLimit covers the limit query param: absent → default, valid → itself, over-max → capped, and
// present-but-invalid → an error (so the handler can 400 rather than silently coerce).
func TestInspectLimit(t *testing.T) {
	cases := []struct {
		raw     string
		want    int32
		wantErr bool
	}{
		{"", defaultInspectLimit, false},
		{"10", 10, false},
		{"200", 200, false},
		{"500", maxInspectLimit, false}, // capped, not an error
		{"0", 0, true},
		{"-3", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/v1/memories?limit="+tc.raw, nil)
		got, err := inspectLimit(r)
		if tc.wantErr {
			if err == nil {
				t.Errorf("limit=%q: err = nil, want an error", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("limit=%q: unexpected err %v", tc.raw, err)
		}
		if got != tc.want {
			t.Errorf("limit=%q: got %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestCursorRoundTrip proves an encoded keyset cursor decodes back to the exact (created_at, id) pair — down to
// nanosecond precision, so a page boundary is never skipped or repeated — and that empty is "no cursor" while a
// malformed cursor is a decode error (which the handler maps to a 400 rather than a silent full scan).
func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 34, 56, 123456789, time.UTC)
	id := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	gotAt, gotID, err := decodeCursor(encodeCursor(at, id))
	if err != nil {
		t.Fatalf("decode(encode(...)) err: %v", err)
	}
	if !gotAt.Time.Equal(at) {
		t.Errorf("created_at round-trip = %v, want %v", gotAt.Time, at)
	}
	if gotID.Bytes != id.Bytes {
		t.Errorf("id round-trip = %x, want %x", gotID.Bytes, id.Bytes)
	}

	// Empty is the first page: no cursor, no error.
	emptyAt, _, err := decodeCursor("")
	if err != nil {
		t.Errorf("decode(\"\") err = %v, want nil", err)
	}
	if emptyAt.Valid {
		t.Error("decode(\"\") produced a valid timestamp, want the zero (no-cursor) value")
	}

	// Malformed cursors are errors, not a silent first page.
	for _, bad := range []string{"not-base64!!", "Zm9v", "YmFyfG5vdC1hLXV1aWQ"} {
		if _, _, err := decodeCursor(bad); err == nil {
			t.Errorf("decodeCursor(%q) err = nil, want an error", bad)
		}
	}
}
