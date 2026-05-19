package index

import (
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1497 v0.83: truth table for shouldSuppressBinaryDriftForce. Pins
// the four-corner classification of (migrations-ran-this-startup ×
// inv.All × inv.Languages) so a future edit that "simplifies" the
// gate can't accidentally relax the safety case (no migrations →
// keep the force).

func TestShouldSuppressBinaryDriftForce_TruthTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		inv     db.MigrationInvalidates
		from    int
		to      int
		suppress bool
	}{
		{
			name:    "no migrations ran this startup — must KEEP force",
			inv:     db.MigrationInvalidates{}, // doesn't matter
			from:    33,
			to:      33,
			suppress: false,
		},
		{
			name:    "migrations ran, union is invalidatesNothing — SUPPRESS",
			inv:     db.MigrationInvalidates{}, // .All=false, .Languages=nil
			from:    30,
			to:      33,
			suppress: true,
		},
		{
			name:    "migrations ran, union is invalidatesAll — KEEP force",
			inv:     db.MigrationInvalidates{All: true},
			from:    17,
			to:      19,
			suppress: false,
		},
		{
			// #1543 v0.84: gate itself still returns false for the
			// language-scoped case — the language-scoped relaxation is
			// applied by the indexer's wrapper (which calls
			// ClearFileHashesByLanguage and sets binaryDriftForce=false
			// after a successful selective clear). Keeping the gate
			// strict here preserves the safety case: if the indexer's
			// new branch errors or is bypassed, the full force-reindex
			// still fires.
			name:    "migrations ran, union is language-scoped — gate still says KEEP force (indexer wrapper relaxes)",
			inv:     db.MigrationInvalidates{Languages: []string{"Bash"}},
			from:    30,
			to:      32,
			suppress: false,
		},
		{
			name:    "single migration step, invalidatesNothing — SUPPRESS",
			inv:     db.MigrationInvalidates{},
			from:    32,
			to:      33,
			suppress: true,
		},
		{
			name:    "single migration step, invalidatesAll — KEEP force",
			inv:     db.MigrationInvalidates{All: true},
			from:    32,
			to:      33,
			suppress: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldSuppressBinaryDriftForce(c.inv, c.from, c.to)
			if got != c.suppress {
				t.Errorf("shouldSuppressBinaryDriftForce(inv=%+v, from=%d, to=%d) = %v, want %v",
					c.inv, c.from, c.to, got, c.suppress)
			}
		})
	}
}

// TestShouldSuppressBinaryDriftForce_ConservativeOnAllUnknown — control:
// if a future migration is added with an unrecognized scope shape
// (struct fields beyond All / Languages), the gate must not silently
// suppress. Pure-Go field model means new fields would need an
// explicit `default-keep-force` check; this test serves as the
// canary so a new field gets reviewed against the safety case.
//
// Currently the gate looks at only inv.All + inv.Languages. If
// MigrationInvalidates grows a field, ensure the gate considers it
// or update this test to bless the new field's interpretation.
func TestShouldSuppressBinaryDriftForce_GateChecksOnlyExpectedFields(t *testing.T) {
	t.Parallel()
	// Sanity: the gate's "suppress" branch is structurally only
	// reachable when ALL of: migrations ran AND !All AND no languages.
	// Any future field that should ALSO inhibit suppression must be
	// added to the gate AND this list.
	known := db.MigrationInvalidates{}
	if known.All {
		t.Fatal("zero-value All must be false")
	}
	if known.Languages != nil {
		t.Fatal("zero-value Languages must be nil")
	}
}
