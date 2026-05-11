package server

import (
	"bytes"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #521 (umbrella #519): HTML snapshot test for /v1/dashboard. Pre-fix
// the dashboard had ~1400 LOC of inline HTML with no test coverage —
// CSP header drift, basepath substitution regressions, accidental
// inline <script> reintroduction, link-rel changes were all invisible
// until a user noticed in production. This pins the entire response
// envelope (HTML body) byte-for-byte, with two snapshots so the
// reverse-proxy basepath path is also covered.
//
// To regenerate after intentional dashboard changes:
//
//	go test ./internal/server -run TestDashboardHTMLSnapshot -update-dashboard-snapshot
//
// Same shape as the corpus snapshot tests: review the diff in PR.

var updateDashboardSnapshot = flag.Bool("update-dashboard-snapshot", false,
	"overwrite testdata/dashboard/*.html with the current renderer output")

func TestDashboardHTMLSnapshot(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		fixture string
	}{
		{
			name:    "no-basepath",
			path:    "/v1/dashboard",
			fixture: "testdata/dashboard/no_basepath.html",
		},
		{
			name:    "with-basepath",
			path:    "/pincher/v1/dashboard",
			fixture: "testdata/dashboard/with_basepath.html",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			// trustProxy is required for the with-basepath case since
			// X-Forwarded-Prefix is the only signal a reverse-proxied
			// request carries. Without it the prefix detection no-ops.
			if tc.name == "with-basepath" {
				srv.trustProxy = true
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tc.path, nil)
			if tc.name == "with-basepath" {
				r.Header.Set("X-Forwarded-Prefix", "/pincher")
			}
			srv.ServeHTTP(w, r)

			if w.Code != 200 {
				t.Fatalf("dashboard %s: status %d, want 200", tc.name, w.Code)
			}
			got := w.Body.Bytes()

			if *updateDashboardSnapshot {
				if err := os.MkdirAll(filepath.Dir(tc.fixture), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(tc.fixture, got, 0o644); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
				t.Logf("updated %s (%d bytes)", tc.fixture, len(got))
				return
			}

			want, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatalf("read fixture %s: %v\n"+
					"Run with -update-dashboard-snapshot to generate.", tc.fixture, err)
			}
			if !bytes.Equal(normalizeNewlines(got), normalizeNewlines(want)) {
				t.Errorf("dashboard %s drifted from %s.\n"+
					"If intentional, regenerate:\n"+
					"  go test ./internal/server -run TestDashboardHTMLSnapshot -update-dashboard-snapshot\n"+
					"Review the diff in PR.", tc.name, tc.fixture)
			}
		})
	}
}

// normalizeNewlines folds CRLF → LF so the snapshot doesn't break on
// Windows checkouts (git's autocrlf rewrites checked-in fixtures on
// Windows; the live HTTP response is always LF).
func normalizeNewlines(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// TestDashboardHTMLSnapshot_NoInlineScript is the explicit guard that
// motivates the snapshot test: CSP forbids inline <script> blocks but
// the server-side template doesn't enforce it. If a future edit
// reintroduces an inline script, the browser will silently block it
// and half the dashboard will stop working — easy to miss without
// this test.
func TestDashboardHTMLSnapshot_NoInlineScript(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/dashboard", nil)
	srv.ServeHTTP(w, r)
	body := string(w.Body.Bytes())
	// Allow `<script src=...>` (external) but reject `<script>` (inline).
	for _, bad := range []string{"<script>", "<script type=\"text/javascript\">"} {
		if strings.Contains(body, bad) {
			t.Errorf("dashboard HTML contains %q — CSP blocks inline scripts; load via /v1/dashboard.js instead", bad)
		}
	}
}
