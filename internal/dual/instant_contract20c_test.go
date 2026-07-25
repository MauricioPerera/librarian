package dual_test

// CONTRACT-20C T3 — the test that FIXES THE DEFECT, at the level where the
// comparison lives.
//
// The values are built on purpose, one per way the text comparison goes wrong,
// and they are the values compat actually hands back: it re-renders every
// timestamp it reads with time.RFC3339Nano, which TRIMS the trailing zeros of the
// fractional second.
//
//	real instant (stored)        what compat hands back
//	…25.123456780Z (earlier)     …25.12345678Z
//	…25.123456781Z (later)       …25.123456781Z   → 'Z'(0x5A) > '1'(0x31): the
//	                                                 LATER one sorts FIRST by text
//	…26.000000000Z (later)       …26Z
//	…25.500000000Z (earlier)     …25.5Z           → within the same second, 'Z'
//	                                                 (0x5A) > '.'(0x2E), so a whole
//	                                                 second sorts AFTER every
//	                                                 fraction of it
//
// Run against the code of `main` — where the key is dual.Asc/dual.Desc over the
// text — this test FAILS. The report pastes that failure.

import (
	"errors"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/dual"
)

// tsRow is a row whose only ordering key is a timestamp.
type tsRow struct {
	name      string
	createdAt string
}

// trimmed renders an instant the way compat renders it on the way out.
func trimmed(t *testing.T, stored string) string {
	t.Helper()
	instant, err := time.Parse(dual.TimestampLayout, stored)
	if err != nil {
		t.Fatalf("fixture %q: %v", stored, err)
	}
	return instant.UTC().Format(time.RFC3339Nano)
}

func names(rows []tsRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.name)
	}
	return out
}

func equalOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestInstantKeysOrderChronologically is the defect test. It sorts rows whose
// only key is a timestamp and requires the CHRONOLOGICAL sequence.
func TestInstantKeysOrderChronologically(t *testing.T) {
	// Oldest first. Written in the fixed-width form the application stores, then
	// trimmed exactly as compat trims it on read.
	chronological := []tsRow{
		{"a-second-25-no-fraction", "2026-07-25T12:00:25.000000000Z"},
		{"b-nanos-ending-in-zero", "2026-07-25T12:00:25.123456780Z"},
		{"c-nanos-ending-in-one", "2026-07-25T12:00:25.123456781Z"},
		{"d-second-25-half", "2026-07-25T12:00:25.500000000Z"},
		{"e-second-26", "2026-07-25T12:00:26.000000000Z"},
	}
	wantAsc := names(chronological)
	wantDesc := make([]string, len(wantAsc))
	for i := range wantAsc {
		wantDesc[i] = wantAsc[len(wantAsc)-1-i]
	}

	// Fed in an order that is NOT the answer, so a no-op sort cannot pass.
	rows := []tsRow{chronological[3], chronological[1], chronological[4], chronological[0], chronological[2]}
	for i := range rows {
		rows[i].createdAt = trimmed(t, rows[i].createdAt)
	}

	// ASCENDING — oldest first.
	//
	// TO REPRODUCE THE DEFECT OF `main`: replace dual.AscInstant(r.createdAt) with
	// dual.Asc(r.createdAt) here (and DescInstant with Desc below). That is the
	// byte-wise text comparison this contract replaces.
	if err := dual.SortByKeysE(rows, func(r tsRow) ([]dual.Key, error) {
		key, err := dual.AscInstant(r.createdAt)
		if err != nil {
			return nil, err
		}
		return []dual.Key{key}, nil
	}); err != nil {
		t.Fatalf("ascending sort: %v", err)
	}
	if got := names(rows); !equalOrder(got, wantAsc) {
		t.Fatalf("ascending order is NOT chronological:\n  got  %v\n  want %v", got, wantAsc)
	}

	// DESCENDING — newest first, the direction ListAPIKeys declares. Not free from
	// the ascending case: the direction is applied to the normalized key, so it is
	// checked on its own.
	if err := dual.SortByKeysE(rows, func(r tsRow) ([]dual.Key, error) {
		key, err := dual.DescInstant(r.createdAt)
		if err != nil {
			return nil, err
		}
		return []dual.Key{key}, nil
	}); err != nil {
		t.Fatalf("descending sort: %v", err)
	}
	if got := names(rows); !equalOrder(got, wantDesc) {
		t.Fatalf("descending order is NOT chronological:\n  got  %v\n  want %v", got, wantDesc)
	}
}

// TestInstantKeysTieBreakOnTheNextKey: two rows written in the SAME instant but
// with different carrier text must compare EQUAL on the timestamp key, so the
// next key decides. Comparing the text would have let the carrier decide.
func TestInstantKeysTieBreakOnTheNextKey(t *testing.T) {
	rows := []tsRow{
		{"zzz", "2026-07-25T12:00:25Z"},                // trimmed whole second
		{"aaa", "2026-07-25T12:00:25.000000000Z"},      // fixed width, same instant
		{"mmm", "2026-07-25T14:00:25.000000000+02:00"}, // other zone, same instant
	}
	if err := dual.SortByKeysE(rows, func(r tsRow) ([]dual.Key, error) {
		key, err := dual.DescInstant(r.createdAt)
		if err != nil {
			return nil, err
		}
		return []dual.Key{key, dual.Asc(r.name)}, nil
	}); err != nil {
		t.Fatalf("sort: %v", err)
	}
	want := []string{"aaa", "mmm", "zzz"}
	if got := names(rows); !equalOrder(got, want) {
		t.Fatalf("equal instants did not fall through to the next key:\n  got  %v\n  want %v", got, want)
	}
}

// TestInstantKeysHandleTheCarrierVariants covers the rest of the red-team list: a
// non-UTC zone denotes the same instant as its UTC rendering, and the legacy
// space-separated form of the engine's CURRENT_TIMESTAMP parses on the new path.
func TestInstantKeysHandleTheCarrierVariants(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int // -1 a<b, 0 equal, +1 a>b
	}{
		{"offset vs utc, same instant", "2026-07-25T14:00:00+02:00", "2026-07-25T12:00:00Z", 0},
		{"offset earlier than utc", "2026-07-25T13:59:59+02:00", "2026-07-25T12:00:00Z", -1},
		{"legacy space form parses", "2026-07-24 07:46:36", "2026-07-24T07:46:36Z", 0},
		{"legacy space form is older", "2026-07-24 07:46:36", "2026-07-25T12:00:00Z", -1},
		{"trimmed vs fixed width, same instant", "2026-07-25T12:00:25Z", "2026-07-25T12:00:25.000000000Z", 0},
		{"trimmed half second is earlier", "2026-07-25T12:00:25.5Z", "2026-07-25T12:00:26Z", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := dual.InstantSortValue(c.a)
			if err != nil {
				t.Fatalf("%q: %v", c.a, err)
			}
			b, err := dual.InstantSortValue(c.b)
			if err != nil {
				t.Fatalf("%q: %v", c.b, err)
			}
			got := 0
			switch {
			case a < b:
				got = -1
			case a > b:
				got = 1
			}
			if got != c.want {
				t.Fatalf("compare(%q, %q) = %d, want %d (keys %q vs %q)", c.a, c.b, got, c.want, a, b)
			}
		})
	}
}

// TestInstantKeysFailVisiblyOnUnparseableText fixes the documented failure mode
// the contract demands: no silent fallback to a text comparison.
func TestInstantKeysFailVisiblyOnUnparseableText(t *testing.T) {
	for _, text := range []string{"", "not-a-timestamp", "2026-13-45T99:99:99Z", "1753440000"} {
		if _, err := dual.AscInstant(text); err == nil {
			t.Fatalf("AscInstant(%q) = nil error, want a visible failure", text)
		}
	}

	// And the failure must reach the CALLER of the sort for a ONE-element slice
	// too — the case a comparator-based design would never notice, because a
	// comparator is not called at all when there is nothing to compare.
	rows := []string{"2026-07-25T12:00:00Z"}
	err := dual.SortByKeysE(rows, func(string) ([]dual.Key, error) {
		key, err := dual.AscInstant("")
		if err != nil {
			return nil, err
		}
		return []dual.Key{key}, nil
	})
	if err == nil {
		t.Fatal("SortByKeysE swallowed the key failure on a single-element slice")
	}
}

// TestSortByKeysELeavesInputUntouchedOnError fixes the other half of the
// documented degradation: on failure nothing is reordered.
func TestSortByKeysELeavesInputUntouchedOnError(t *testing.T) {
	boom := errors.New("boom")
	rows := []string{"c", "a", "b"}
	err := dual.SortByKeysE(rows, func(s string) ([]dual.Key, error) {
		if s == "b" {
			return nil, boom
		}
		return []dual.Key{dual.Asc(s)}, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if !equalOrder(rows, []string{"c", "a", "b"}) {
		t.Fatalf("input was reordered despite the error: %v", rows)
	}
}
