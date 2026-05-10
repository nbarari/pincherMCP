package cypher

import (
	"strings"
	"testing"
)

// #294: when an agent reaches for a SQL/Cypher operator that pinchQL
// doesn't support, the parse error should nudge them toward the supported
// spelling instead of just saying "unsupported operator".
func TestParse_UnsupportedOperator_LIKE_Hint(t *testing.T) {
	tokens := tokenize("MATCH (n:Function) WHERE n.name LIKE '%handle%' RETURN n.name")
	p := &parser{tokens: tokens}
	_, err := p.parseQuery()
	if err == nil {
		t.Fatal("expected parse error for LIKE")
	}
	msg := err.Error()
	if !strings.Contains(msg, "LIKE") {
		t.Errorf("error %q must mention the offending operator", msg)
	}
	if !strings.Contains(msg, "CONTAINS") {
		t.Errorf("error %q must suggest CONTAINS", msg)
	}
}

func TestParse_UnsupportedOperator_StartsWithUnderscore_Hint(t *testing.T) {
	tokens := tokenize("MATCH (n:Function) WHERE n.name STARTS_WITH 'handle' RETURN n.name")
	p := &parser{tokens: tokens}
	_, err := p.parseQuery()
	if err == nil {
		t.Fatal("expected parse error for STARTS_WITH")
	}
	msg := err.Error()
	if !strings.Contains(msg, "STARTS WITH") {
		t.Errorf("error %q must suggest STARTS WITH (no underscore)", msg)
	}
}

func TestParse_UnsupportedOperator_NotEquals_Hint(t *testing.T) {
	// `!=` is two punct tokens (`!` then `=`); the parser must still
	// produce a hint pointing at the supported `<>`.
	tokens := tokenize("MATCH (n:Function) WHERE n.name != 'foo' RETURN n.name")
	p := &parser{tokens: tokens}
	_, err := p.parseQuery()
	if err == nil {
		t.Fatal("expected parse error for !=")
	}
	msg := err.Error()
	if !strings.Contains(msg, "<>") {
		t.Errorf("error %q must suggest <>", msg)
	}
}

func TestParse_UnsupportedOperator_REGEXP_Hint(t *testing.T) {
	tokens := tokenize("MATCH (n:Function) WHERE n.name REGEXP '.*foo.*' RETURN n.name")
	p := &parser{tokens: tokens}
	_, err := p.parseQuery()
	if err == nil {
		t.Fatal("expected parse error for REGEXP")
	}
	if !strings.Contains(err.Error(), "=~") {
		t.Errorf("error %q must suggest =~", err.Error())
	}
}

// Negative — supported operators must still parse.
func TestParse_SupportedOperators_StillWork(t *testing.T) {
	cases := []string{
		"MATCH (n) WHERE n.name = 'foo' RETURN n.name",
		"MATCH (n) WHERE n.name <> 'foo' RETURN n.name",
		"MATCH (n) WHERE n.name CONTAINS 'foo' RETURN n.name",
		"MATCH (n) WHERE n.name STARTS WITH 'foo' RETURN n.name",
		"MATCH (n) WHERE n.name =~ '.*foo.*' RETURN n.name",
		"MATCH (n) WHERE n.start_line >= 100 RETURN n.name",
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			p := &parser{tokens: tokenize(q)}
			if _, err := p.parseQuery(); err != nil {
				t.Errorf("supported operator query failed: %v", err)
			}
		})
	}
}

// operatorHint table coverage — ensure every entry returns a non-empty
// hint. Mostly a guard against typos in future additions to the map.
func TestOperatorHint_AllEntriesNonEmpty(t *testing.T) {
	for _, op := range []string{"LIKE", "like", "REGEXP", "RLIKE", "STARTS_WITH", "ENDS", "MATCHES"} {
		hint, ok := operatorHint(op)
		if !ok || hint == "" {
			t.Errorf("operatorHint(%q) returned empty/false", op)
		}
	}
	if _, ok := operatorHint("BOGUS"); ok {
		t.Error("operatorHint(BOGUS) should return false")
	}
}
