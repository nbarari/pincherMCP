package cypher

import (
	"testing"
)

// #927: collectUnknownOrderByWarnings used to warn even when the
// ORDER BY target was a RETURN-clause alias. The actual sort worked
// correctly (the alias resolution paths in buildResult handle both
// property aliases and aggregate aliases), but the warning told the
// user their sort was "silently dropped" — misleading enough that
// the user couldn't trust the result.

func TestOrderByAliasWarning_AggregateAliasNoWarning(t *testing.T) {
	q := &queryAST{
		orderBy: "cnt",
		returnVars: []returnVar{
			{variable: "n", property: "file_path"},
			{fn: "COUNT", variable: "n", alias: "cnt"},
		},
	}
	warnings := collectUnknownOrderByWarnings(q)
	if len(warnings) > 0 {
		t.Errorf("ORDER BY on aggregate alias 'cnt' should not warn; got %v", warnings)
	}
}

func TestOrderByAliasWarning_PropertyAliasNoWarning(t *testing.T) {
	q := &queryAST{
		orderBy: "c",
		returnVars: []returnVar{
			{variable: "n", property: "name"},
			{variable: "n", property: "complexity", alias: "c"},
		},
	}
	warnings := collectUnknownOrderByWarnings(q)
	if len(warnings) > 0 {
		t.Errorf("ORDER BY on property alias 'c' should not warn; got %v", warnings)
	}
}

// The warning should still fire for a genuinely unknown column with
// no alias coverage.
func TestOrderByAliasWarning_UnknownColumnStillWarns(t *testing.T) {
	q := &queryAST{
		orderBy: "bogus_column",
		returnVars: []returnVar{
			{variable: "n", property: "name"},
		},
	}
	warnings := collectUnknownOrderByWarnings(q)
	if len(warnings) != 1 {
		t.Errorf("ORDER BY on genuinely unknown column should warn; got %v", warnings)
	}
}

// Property whitelist hits stay non-warning (existing behavior, not
// changed by #927).
func TestOrderByAliasWarning_WhitelistedPropertyNoWarning(t *testing.T) {
	q := &queryAST{
		orderBy: "n.complexity",
		returnVars: []returnVar{
			{variable: "n", property: "name"},
			{variable: "n", property: "complexity"},
		},
	}
	warnings := collectUnknownOrderByWarnings(q)
	if len(warnings) > 0 {
		t.Errorf("ORDER BY on whitelisted property should not warn; got %v", warnings)
	}
}
