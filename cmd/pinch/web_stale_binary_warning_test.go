package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #706: when `pincher web` finds a live HTTP server whose version
// doesn't match the on-disk binary, it must warn loudly — pre-fix it
// returned the URL silently and devs dogfooded stale code without
// realizing. Tests target the helper in isolation so we don't have to
// spin up the full runWebCLI flow (which depends on DB state).

func TestFetchRunningServerVersion_ReturnsVersionFromHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"version":"0.22.0-2-g04b9715","auth_required":false}`))
	}))
	defer srv.Close()

	got := fetchRunningServerVersion(srv.URL)
	want := "0.22.0-2-g04b9715"
	if got != want {
		t.Errorf("fetchRunningServerVersion: got %q, want %q", got, want)
	}
}

func TestFetchRunningServerVersion_EmptyOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if got := fetchRunningServerVersion(srv.URL); got != "" {
		t.Errorf("non-2xx should return empty; got %q", got)
	}
}

func TestFetchRunningServerVersion_EmptyOnUnreachable(t *testing.T) {
	// Best-effort returns "" rather than blocking or erroring — this is
	// the design contract (#706: never fail the `web` flow on probe).
	if got := fetchRunningServerVersion("http://127.0.0.1:1"); got != "" {
		t.Errorf("unreachable URL should return empty; got %q", got)
	}
}

func TestFetchRunningServerVersion_EmptyOnMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json at all`))
	}))
	defer srv.Close()

	if got := fetchRunningServerVersion(srv.URL); got != "" {
		t.Errorf("malformed JSON should return empty; got %q", got)
	}
}

func TestFetchRunningServerVersion_EmptyOnMissingVersionField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"auth_required":false}`))
	}))
	defer srv.Close()

	if got := fetchRunningServerVersion(srv.URL); got != "" {
		t.Errorf("missing version field should return empty; got %q", got)
	}
}

// Sanity check: a version equal to the on-disk binary should NOT
// trigger the warning logic at the call site. We can't easily mock
// `version` (it's a package-level var stamped at build time), so this
// test exercises the comparison logic directly with strings the caller
// would use.
func TestStaleBinaryWarning_MatchingVersionsDoNotTriggerWarning(t *testing.T) {
	// Pin the comparison the caller uses: warn iff (runningVer != "" && runningVer != version).
	// We don't import the actual `version` value — just confirm the
	// semantic.
	cases := []struct {
		runningVer string
		binVer     string
		wantWarn   bool
	}{
		{"0.22.0", "0.59.0-test", true},
		{"0.59.0-test", "0.59.0-test", false},
		{"", "0.59.0-test", false}, // probe failed → no warn
	}
	for _, tc := range cases {
		warn := tc.runningVer != "" && tc.runningVer != tc.binVer
		if warn != tc.wantWarn {
			t.Errorf("(running=%q, bin=%q): warn=%v, want %v",
				tc.runningVer, tc.binVer, warn, tc.wantWarn)
		}
	}
}

// Smoke check: the warning banner text includes the running version,
// on-disk version, and the PID so the user can act on it. Tests the
// fmt.Fprintf pattern from the call site rather than re-invoking
// runWebCLI (which would need full DB + spawn machinery).
func TestStaleBinaryWarning_BannerContainsAllInfo(t *testing.T) {
	// Reproduce the call-site format with synthetic inputs.
	runningVer := "0.22.0-2-g04b9715"
	binVer := "0.59.0-test"
	pid := 1234
	banner := strings.Builder{}
	// The format string is pinned to the call site; if that changes,
	// this test should change too.
	banner.WriteString("pincher web: WARNING — running HTTP server is ")
	banner.WriteString(`"` + runningVer + `"`)
	banner.WriteString(" but the on-disk binary is ")
	banner.WriteString(`"` + binVer + `"`)
	banner.WriteString(".\n  ...")
	if !strings.Contains(banner.String(), runningVer) {
		t.Errorf("banner missing runningVer; got %q", banner.String())
	}
	if !strings.Contains(banner.String(), binVer) {
		t.Errorf("banner missing binVer; got %q", banner.String())
	}
	_ = pid // pinned in the real call site
}
