//go:build dualengine

package auth_test

// CONTRACT-20C T2 — the SQL `ORDER BY`, MEASURED instead of reasoned about.
//
// An ORDER BY inside the engine cannot parse: it compares the TEXT that is
// stored. librarian's `timestamp` columns hold a MIXTURE today —
//
//	legacy   "2026-07-24 07:46:36"            written by the engine's
//	                                          CURRENT_TIMESTAMP, before the
//	                                          application started writing the value
//	trimmed  "2026-07-25T12:00:25.5Z"         time.RFC3339Nano, which drops the
//	                                          trailing zeros of the fraction
//	fixed    "2026-07-25T12:00:25.500000000Z" dual.TimestampLayout
//
// — so the question the contract asks is what sequence that mixture actually
// produces, on each engine, and whether it is chronological. This file answers it
// by building the table, running the ORDER BY on a REAL SQLite file and a REAL
// PostgreSQL 17 server, and comparing the returned sequence to the known
// chronological one. Nothing here is inferred from collation rules.
//
// Run it with:
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineTimestampOrderBy -count=1 -v ./internal/auth
//
// It also FIXES the condition that makes the mixture safe: scenarios A and C
// ASSERT a chronological result, so if anybody breaks that condition — by writing
// a legacy-shaped value after the cutover, or by reverting the writer to the
// trimmed format — this test goes red. Scenario B pins what the breakage looks
// like.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// tsProbeRow is one fixture row: a label, the exact TEXT stored in the column,
// and the position that row has in the TRUE chronological order (0 = oldest).
type tsProbeRow struct {
	label  string
	stored string
	rank   int
}

// tsProbeScenario is one measurement: a set of rows plus whether its ORDER BY is
// expected to come back chronological.
type tsProbeScenario struct {
	name             string
	why              string
	rows             []tsProbeRow
	wantChronologica bool
}

// TestDualEngineTimestampOrderBy is the T2 measurement.
func TestDualEngineTimestampOrderBy(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this measurement is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	scenarios := []tsProbeScenario{
		{
			name: "A-production-shape",
			why: "legacy rows written by the engine's CURRENT_TIMESTAMP, all strictly OLDER " +
				"than every row the application wrote afterwards — the shape production has today",
			rows: []tsProbeRow{
				{"legacy-1", "2026-07-24 07:46:36", 0},
				{"legacy-2", "2026-07-24 09:12:00", 1},
				{"app-1", "2026-07-25T12:00:25.000000000Z", 2},
				{"app-2", "2026-07-25T12:00:25.123456780Z", 3},
				{"app-3", "2026-07-25T12:00:25.500000000Z", 4},
			},
			wantChronologica: true,
		},
		{
			name: "B1-legacy-row-newer-on-a-LATER-DAY",
			why: "the condition of A violated, but on a later DATE: the shapes differ only " +
				"from the 11th character on (the ' ' / 'T' separator), so the date decides " +
				"first and the mixture still comes out right. MEASURED, not assumed",
			rows: []tsProbeRow{
				{"app-1", "2026-07-25T12:00:25.000000000Z", 0},
				{"app-2", "2026-07-25T12:00:25.500000000Z", 1},
				{"legacy-newer-next-day", "2026-07-26 08:00:00", 2},
			},
			wantChronologica: true,
		},
		{
			name: "B2-legacy-row-newer-on-the-SAME-DAY",
			why: "the condition of A violated on the SAME date, which is where it actually " +
				"breaks: with equal date prefixes the comparison reaches the separator, and " +
				"' ' (0x20) sorts before 'T' (0x54), so a legacy row is forced to look OLDEST " +
				"no matter what time it carries. This is what an INSERT that does not come " +
				"from the application produces — the column DEFAULT is still the engine's " +
				"CURRENT_TIMESTAMP",
			rows: []tsProbeRow{
				{"app-1", "2026-07-25T12:00:25.000000000Z", 0},
				{"app-2", "2026-07-25T12:00:25.500000000Z", 1},
				{"legacy-newer-same-day", "2026-07-25 23:00:00", 2},
			},
			wantChronologica: false,
		},
		{
			name: "C-application-rows-only-FIXED-width",
			why: "only rows written by the application in dual.TimestampLayout — the format " +
				"CONTRACT-20C makes universal",
			rows: []tsProbeRow{
				{"t0", "2026-07-25T12:00:25.000000000Z", 0},
				{"t1", "2026-07-25T12:00:25.123456780Z", 1},
				{"t2", "2026-07-25T12:00:25.123456781Z", 2},
				{"t3", "2026-07-25T12:00:25.500000000Z", 3},
				{"t4", "2026-07-25T12:00:26.000000000Z", 4},
			},
			wantChronologica: true,
		},
		{
			name: "D-application-rows-only-TRIMMED",
			why: "the same instants written with a bare time.RFC3339Nano — what internal/auth " +
				"wrote before this contract, and what a reverted writer would produce again",
			rows: []tsProbeRow{
				{"t0", "2026-07-25T12:00:25Z", 0},
				{"t1", "2026-07-25T12:00:25.12345678Z", 1},
				{"t2", "2026-07-25T12:00:25.123456781Z", 2},
				{"t3", "2026-07-25T12:00:25.5Z", 3},
				{"t4", "2026-07-25T12:00:26Z", 4},
			},
			wantChronologica: false,
		},
	}

	engines := []struct {
		name  string
		store *compat.Store
	}{
		{"sqlite", sqliteStore},
		{"postgres", pgStore},
	}

	for _, scenario := range scenarios {
		chronological := chronologicalLabels(t, scenario.rows)
		perEngine := map[string][]string{}
		for _, engine := range engines {
			got := measureOrderBy(t, engine.store, scenario)
			perEngine[engine.name] = got
			t.Logf("%s | %-8s | ORDER BY created_at ASC -> %v", scenario.name, engine.name, got)

			isChronological := strings.Join(got, ",") == strings.Join(chronological, ",")
			if isChronological != scenario.wantChronologica {
				t.Errorf("%s on %s: chronological=%t, want %t\n  why: %s\n  got        %v\n  true order %v",
					scenario.name, engine.name, isChronological, scenario.wantChronologica,
					scenario.why, got, chronological)
			}
		}
		if strings.Join(perEngine["sqlite"], ",") != strings.Join(perEngine["postgres"], ",") {
			t.Errorf("%s: the two engines disagree\n  sqlite  : %v\n  postgres: %v",
				scenario.name, perEngine["sqlite"], perEngine["postgres"])
		}
		t.Logf("%s | truth    | chronological           -> %v", scenario.name, chronological)
	}
}

// chronologicalLabels returns the labels in the TRUE chronological order, taken
// from the declared ranks rather than from any comparison of the stored text —
// the point being to have an oracle independent of what is under test.
func chronologicalLabels(t *testing.T, rows []tsProbeRow) []string {
	t.Helper()
	out := make([]string, len(rows))
	for _, r := range rows {
		if r.rank < 0 || r.rank >= len(rows) || out[r.rank] != "" {
			t.Fatalf("bad rank %d on %q", r.rank, r.label)
		}
		out[r.rank] = r.label
	}
	return out
}

// measureOrderBy creates a throwaway table with a TEXT created_at (the mapping
// compat's `timestamp` family gets on BOTH engines), inserts the fixture rows and
// returns the labels in the order the engine's own ORDER BY produces.
func measureOrderBy(t *testing.T, store *compat.Store, scenario tsProbeScenario) []string {
	t.Helper()
	ctx := context.Background()
	table := "ts_probe_" + strings.ReplaceAll(scenario.name, "-", "_")

	if _, err := store.DB.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
		t.Fatalf("drop %s: %v", table, err)
	}
	if _, err := store.DB.ExecContext(ctx,
		`CREATE TABLE `+table+` ("label" TEXT NOT NULL, "created_at" TEXT NOT NULL)`); err != nil {
		t.Fatalf("create %s: %v", table, err)
	}
	defer func() {
		if _, err := store.DB.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table); err != nil {
			t.Logf("cleanup %s: %v", table, err)
		}
	}()

	for _, row := range scenario.rows {
		statement := fmt.Sprintf(`INSERT INTO %s ("label", "created_at") VALUES (%s, %s)`,
			table, compat.Placeholder(store.Target.Engine, 1), compat.Placeholder(store.Target.Engine, 2))
		if _, err := store.DB.ExecContext(ctx, statement, row.label, row.stored); err != nil {
			t.Fatalf("insert %s into %s: %v", row.label, table, err)
		}
	}

	rows, err := store.DB.QueryContext(ctx,
		`SELECT "label" FROM `+table+` ORDER BY "created_at" ASC`)
	if err != nil {
		t.Fatalf("order by on %s: %v", table, err)
	}
	return scanLabels(t, rows)
}

func scanLabels(t *testing.T, rows *sql.Rows) []string {
	t.Helper()
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, label)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
