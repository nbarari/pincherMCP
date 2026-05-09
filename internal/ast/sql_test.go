package ast

import (
	"testing"
)

// TestExtractSQL_TablesAndViewsAreClassKind pins the kind mapping:
// CREATE TABLE / CREATE VIEW both emit Class symbols.
func TestExtractSQL_TablesAndViewsAreClassKind(t *testing.T) {
	src := []byte(`-- migrations/001_init.sql
CREATE TABLE users (
    id   bigserial PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    user_id bigint REFERENCES users(id),
    token   text
);

CREATE OR REPLACE VIEW active_users AS
SELECT * FROM users WHERE deleted_at IS NULL;

CREATE MATERIALIZED VIEW user_stats AS
SELECT user_id, count(*) FROM events GROUP BY user_id;
`)
	r := extractSQL(src, "schema.sql")
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
	}
	for _, name := range []string{"users", "sessions", "active_users", "user_stats"} {
		if got[name] != "Class" {
			t.Errorf("expected %q kind=Class, got %q (all: %v)", name, got[name], got)
		}
	}
}

// TestExtractSQL_FunctionsAndProceduresAndTriggersAreFunctionKind
// pins that callable definitions all emit Function symbols.
func TestExtractSQL_FunctionsAndProceduresAndTriggersAreFunctionKind(t *testing.T) {
	src := []byte(`CREATE OR REPLACE FUNCTION calc_total(amount numeric) RETURNS numeric
LANGUAGE sql AS $$
    SELECT amount * 1.07
$$;

CREATE PROCEDURE archive_old_orders(cutoff date)
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM orders WHERE created_at < cutoff;
END;
$$;

CREATE TRIGGER update_timestamp
BEFORE UPDATE ON users
FOR EACH ROW EXECUTE FUNCTION set_updated_at();
`)
	r := extractSQL(src, "procs.sql")
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
	}
	for _, name := range []string{"calc_total", "archive_old_orders", "update_timestamp"} {
		if got[name] != "Function" {
			t.Errorf("expected %q kind=Function, got %q (all: %v)", name, got[name], got)
		}
	}
}

// TestExtractSQL_SchemaPrefixSplitIntoQualifiedName pins that
// `CREATE TABLE auth.users` produces:
//   - Name: "users" (bare leaf)
//   - QualifiedName: "auth.users" (full dotted path)
//
// This matches the YAML Setting symbol shape — agents searching for
// "users" find the leaf, agents querying by qualified_name get the
// full schema path.
func TestExtractSQL_SchemaPrefixSplitIntoQualifiedName(t *testing.T) {
	src := []byte(`CREATE TABLE auth.users (id bigserial);
CREATE FUNCTION public.calc_total(x int) RETURNS int LANGUAGE sql AS 'SELECT x*2';
`)
	r := extractSQL(src, "schema.sql")
	want := map[string]string{
		"users":      "auth.users",
		"calc_total": "public.calc_total",
	}
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.QualifiedName
	}
	for name, qn := range want {
		if got[name] != qn {
			t.Errorf("schema-qualified %q: QN = %q, want %q", name, got[name], qn)
		}
	}
}

// TestExtractSQL_QuotedIdentifiersStripped pins the dialect-specific
// quoting cases. Backticks (MySQL), double-quotes (Postgres/SQLite),
// and square brackets (MSSQL) are stripped from both Name and QN.
func TestExtractSQL_QuotedIdentifiersStripped(t *testing.T) {
	src := []byte(
		"CREATE TABLE `mysql_users` (id int);\n" +
			"CREATE TABLE \"pg_users\" (id int);\n" +
			"CREATE TABLE [mssql_users] (id int);\n",
	)
	r := extractSQL(src, "quoted.sql")
	got := map[string]bool{}
	for _, s := range r.Symbols {
		got[s.Name] = true
	}
	for _, want := range []string{"mysql_users", "pg_users", "mssql_users"} {
		if !got[want] {
			t.Errorf("quoted-identifier %q not in extracted symbols: %v", want, got)
		}
	}
}

// TestExtractSQL_CommentedOutDefinitionsSkipped pins the comment-aware
// extraction: `-- CREATE TABLE foo` and `/* CREATE TABLE bar */` must
// NOT produce phantom symbols. Common in migration files where prior
// versions of definitions are kept as documentation.
func TestExtractSQL_CommentedOutDefinitionsSkipped(t *testing.T) {
	src := []byte(`-- CREATE TABLE legacy_users (id int);  -- old schema, kept for reference

/* CREATE FUNCTION old_calc()
   RETURNS int LANGUAGE sql AS 'SELECT 0';
*/

CREATE TABLE current_users (id bigserial);
`)
	r := extractSQL(src, "with-comments.sql")
	got := map[string]bool{}
	for _, s := range r.Symbols {
		got[s.Name] = true
	}
	if got["legacy_users"] {
		t.Error("commented-out (-- prefix) CREATE TABLE should not emit a symbol")
	}
	if got["old_calc"] {
		t.Error("commented-out (/* */ block) CREATE FUNCTION should not emit a symbol")
	}
	if !got["current_users"] {
		t.Error("real CREATE TABLE outside any comment should emit a symbol")
	}
}

// TestExtractSQL_DMLDoesNotProduceSymbols pins the deliberate
// out-of-scope cases: SELECT/INSERT/UPDATE/DELETE/ALTER/DROP/CREATE
// INDEX never emit symbols.
func TestExtractSQL_DMLDoesNotProduceSymbols(t *testing.T) {
	src := []byte(`SELECT * FROM users WHERE id = 1;
INSERT INTO orders (sku) VALUES ('abc');
UPDATE orders SET status = 'shipped' WHERE id = 1;
DELETE FROM sessions WHERE expires_at < now();
ALTER TABLE users ADD COLUMN email text;
DROP TABLE temp_data;
CREATE INDEX idx_users_email ON users(email);
`)
	r := extractSQL(src, "ops.sql")
	if len(r.Symbols) != 0 {
		t.Errorf("DML / ALTER / DROP / CREATE INDEX should produce 0 symbols, got %d: %v",
			len(r.Symbols), r.Symbols)
	}
}

// TestExtractSQL_AllSymbolsExportedByDefinition pins that every
// CREATE-emitted symbol carries IsExported=true. SQL DDL has no
// access-modifier concept; CREATE statements introduce
// public-by-definition entities.
func TestExtractSQL_AllSymbolsExportedByDefinition(t *testing.T) {
	src := []byte(`CREATE TABLE t1 (id int);
CREATE FUNCTION f1() RETURNS int LANGUAGE sql AS 'SELECT 1';
CREATE PROCEDURE p1() LANGUAGE sql AS 'SELECT 1';
`)
	r := extractSQL(src, "exp.sql")
	if len(r.Symbols) != 3 {
		t.Fatalf("expected 3 symbols, got %d", len(r.Symbols))
	}
	for _, s := range r.Symbols {
		if !s.IsExported {
			t.Errorf("symbol %q should be IsExported=true (CREATE = public by definition)", s.Name)
		}
	}
}

// TestExtractSQL_IfNotExistsAcrossKinds pins that `IF NOT EXISTS` is
// recognised on every CREATE keyword that supports it across the major
// dialects: TABLE (everyone), VIEW (some), FUNCTION (MariaDB),
// PROCEDURE (MariaDB), TRIGGER (SQLite + MariaDB). A single source
// file mixing the form against each kind verifies the regex doesn't
// silently drop matches when the optional modifier appears.
func TestExtractSQL_IfNotExistsAcrossKinds(t *testing.T) {
	src := []byte(`CREATE TABLE IF NOT EXISTS t1 (id int);
CREATE FUNCTION IF NOT EXISTS f1() RETURNS int LANGUAGE sql AS 'SELECT 1';
CREATE PROCEDURE IF NOT EXISTS p1() LANGUAGE sql AS 'SELECT 1';
CREATE TRIGGER IF NOT EXISTS tr1 BEFORE INSERT ON t1 FOR EACH ROW BEGIN END;
`)
	r := extractSQL(src, "ifnotexists.sql")
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
	}
	wants := map[string]string{
		"t1":  "Class",
		"f1":  "Function",
		"p1":  "Function",
		"tr1": "Function",
	}
	for name, kind := range wants {
		if got[name] != kind {
			t.Errorf("CREATE … IF NOT EXISTS %s: got kind=%q, want %q (all: %v)",
				name, got[name], kind, got)
		}
	}
}

// TestSQL_DetectedByExtension pins the registry detection for both
// .sql and .ddl extensions.
func TestSQL_DetectedByExtension(t *testing.T) {
	cases := map[string]string{
		"schema.sql":              "SQL",
		"migrations/001_init.sql": "SQL",
		"defs.ddl":                "SQL",
		"main.go":                 "Go", // sanity: don't claim Go files
	}
	for filename, want := range cases {
		got := DetectLanguage(filename)
		if got != want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", filename, got, want)
		}
	}
}

// TestSQL_RegisteredConfidenceIs0_85 pins the regex-tier confidence
// commitment.
func TestSQL_RegisteredConfidenceIs0_85(t *testing.T) {
	if got := RegisteredConfidence("SQL"); got != 0.85 {
		t.Errorf("RegisteredConfidence(SQL) = %v, want 0.85", got)
	}
}
