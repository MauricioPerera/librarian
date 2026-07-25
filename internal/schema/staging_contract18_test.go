package schema_test

// CONTRACT-18 — the enforcement of the STAGING table namespace.
//
// The rebuild of a dynamic type parks its rows in a transient table named
// schema.StagingTableName. That name must be one NOTHING else in the project
// can produce, or a rebuild could drop a real table. Two claims are made, and
// each gets a test rather than a comment:
//
//	(a) no CODE table uses the prefix — code tables are Go literals that pass
//	    through no runtime validator, so only a test over Build() can guarantee
//	    it (the same reasoning CONTRACT-17 used for `cpt_`);
//	(b) no DYNAMIC table can ever produce it — that one is structural (the `_`
//	    in `cpt_`), and the test pins the structure instead of trusting prose.

import (
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
)

// TestNoCodeTableUsesStagingPrefix is claim (a). Without it the staging prefix
// would be a convention; with it, it is a guarantee.
func TestNoCodeTableUsesStagingPrefix(t *testing.T) {
	for _, table := range schema.Build().Tables {
		if strings.HasPrefix(table.Name, schema.StagingTablePrefix) {
			t.Fatalf(`code table %q starts with %q, which is RESERVED for the transient table of a content-type rebuild.

CONSEQUENCE: editing the fields of ANY dynamic content type would CREATE and then
DROP this code table inside the rebuild transaction — silently destroying it and
every row in it.

FIX: rename the code table so it does not start with %q. Never "solve" this by
changing the prefix.`,
				table.Name, schema.StagingTablePrefix, schema.StagingTablePrefix)
		}
	}
	t.Logf("ENFORCEMENT OK: none of the %d code tables uses the %q prefix", len(schema.Build().Tables), schema.StagingTablePrefix)
}

// TestStagingPrefixIsDisjointFromDynamicTables is claim (b): the staging family
// and the dynamic family cannot intersect, because a dynamic table is always
// "cpt_" + name and therefore always has '_' as its 4th character, while a
// staging name has 'm'. The nastiest candidate — a type literally called
// `mp_...` — is checked explicitly.
func TestStagingPrefixIsDisjointFromDynamicTables(t *testing.T) {
	for _, name := range []string{"mp_eventos", "mp_rebuild", "rebuild", "eventos", "cpt_mp_x", "m", "mp"} {
		if err := schema.ValidateTypeName(name); err != nil {
			t.Fatalf("test premise broken: %q is not even a legal type name: %v", name, err)
		}
		table := schema.DynamicTableName(name)
		if strings.HasPrefix(table, schema.StagingTablePrefix) {
			t.Fatalf("type %q produces the table %q, which collides with the staging namespace %q",
				name, table, schema.StagingTablePrefix)
		}
	}
	// The structural reason, pinned: the dynamic prefix ends in '_' and the
	// staging prefix does not, so no concatenation of the former can start with
	// the latter.
	if !strings.HasSuffix(schema.DynamicTablePrefix, "_") {
		t.Fatalf("DynamicTablePrefix %q no longer ends in '_' — the disjointness argument with %q is broken",
			schema.DynamicTablePrefix, schema.StagingTablePrefix)
	}
	if strings.HasPrefix(schema.StagingTablePrefix, schema.DynamicTablePrefix) {
		t.Fatalf("staging prefix %q starts with the dynamic prefix %q", schema.StagingTablePrefix, schema.DynamicTablePrefix)
	}
	t.Logf("DISJOINT OK: dynamic tables are %q+name, staging is %q; %q is always a legal identifier (%d bytes)",
		schema.DynamicTablePrefix, schema.StagingTablePrefix, schema.StagingTableName, len(schema.StagingTableName))
}

// TestStagingTableNameIsAlwaysALegalIdentifier pins the reason the staging name
// is a CONSTANT and not derived from the type name: a derived name would be
// 6 + up to 28 bytes and would blow the identifier budget for exactly the types
// whose names sit at the top of the allowed range.
func TestStagingTableNameIsAlwaysALegalIdentifier(t *testing.T) {
	if len(schema.StagingTableName) > schema.MaxIdentifierLength {
		t.Fatalf("staging table name %q is %d bytes, over the %d-byte budget",
			schema.StagingTableName, len(schema.StagingTableName), schema.MaxIdentifierLength)
	}
	if _, err := schema.QuoteInternalIdentifier(schema.StagingTableName); err != nil {
		t.Fatalf("staging table name is not quotable: %v", err)
	}
	longest := strings.Repeat("a", schema.MaxTypeNameLength)
	derived := schema.StagingTablePrefix + longest
	if len(derived) <= schema.MaxIdentifierLength {
		t.Fatalf("premise broken: a DERIVED staging name for the longest legal type (%d bytes) would fit the budget after all", len(derived))
	}
	t.Logf("CONSTANT OK: %q fits (%d bytes); a derived name for the longest legal type would be %d bytes (> %d)",
		schema.StagingTableName, len(schema.StagingTableName), len(derived), schema.MaxIdentifierLength)
}
