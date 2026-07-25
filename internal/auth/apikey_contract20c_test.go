package auth_test

// CONTRACT-20C T3 — the same defect, fixed on the REAL production path:
// auth.ListAPIKeys over a real SQLite database, through compat.
//
// internal/dual's test fixes the comparison in isolation; this one proves the
// listing users actually see comes back in chronological order. It matters
// because the value ListAPIKeys compares is not what was WRITTEN: compat
// re-renders every timestamp it reads with time.RFC3339Nano, which trims the
// trailing zeros of the fractional second. That trimming is invisible from the
// call site and is exactly what made the text comparison wrong.
//
// Against the code of `main` (dual.Desc over the text) this test FAILS; the
// report pastes that failure.

import (
	"context"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
)

// TestListAPIKeysOrdersByInstantNotByText mints three keys and forces their
// created_at to values crafted for the two ways a text comparison goes wrong
// within one second: a nanosecond field ending in zero (shorter once trimmed)
// and a whole second (no fractional part at all once trimmed).
func TestListAPIKeysOrdersByInstantNotByText(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	// stamped in chronological order, oldest first, in the fixed-width form the
	// application writes (dual.TimestampLayout).
	fixtures := []struct{ label, createdAt string }{
		{"oldest-whole-second", "2026-07-25T12:00:25.000000000Z"},
		{"middle-nanos-ending-in-zero", "2026-07-25T12:00:25.123456780Z"},
		{"newest-half-second", "2026-07-25T12:00:25.500000000Z"},
	}
	for _, f := range fixtures {
		if _, err := auth.MintAPIKey(ctx, db, f.label, roleID(t, db, "editor")); err != nil {
			t.Fatalf("mint %s: %v", f.label, err)
		}
		// A raw UPDATE is the only way to place an exact instant: MintAPIKey stamps
		// time.Now(). The column is TEXT on both engines, so this writes the same
		// bytes the application would have written at that instant.
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE api_keys SET created_at = ? WHERE label = ?`, f.createdAt, f.label); err != nil {
			t.Fatalf("stamp %s: %v", f.label, err)
		}
	}

	keys, err := auth.ListAPIKeys(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, 0, len(keys))
	for _, k := range keys {
		got = append(got, k.Label)
	}

	// ListAPIKeys is declared newest first.
	want := []string{"newest-half-second", "middle-nanos-ending-in-zero", "oldest-whole-second"}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListAPIKeys order is NOT chronological:\n  got  %v\n  want %v\n"+
				"  (canonicalized created_at, in returned order: %v)", got, want, createdStamps(keys))
		}
	}
}

// TestListAPIKeysRejectsAnUnparseableCreatedAt fixes the documented failure mode
// on the production path: a created_at that cannot be read as an instant is an
// ERROR naming the row, not a silent fallback to comparing text. The column is
// NOT NULL and compat refuses to canonicalize an unparseable timestamp on read,
// so this can only be a corrupted row — and that is what it reports.
func TestListAPIKeysRejectsAnUnparseableCreatedAt(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	if _, err := auth.MintAPIKey(ctx, db, "corrupted", roleID(t, db, "editor")); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE api_keys SET created_at = ? WHERE label = ?`, "not-a-timestamp", "corrupted"); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	if _, err := auth.ListAPIKeys(ctx, db); err == nil {
		t.Fatal("ListAPIKeys returned no error for an unparseable created_at")
	}
}

func createdStamps(keys []auth.APIKeyRecord) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.CreatedAt)
	}
	return out
}
