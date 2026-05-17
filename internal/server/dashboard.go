package server

import "strings"

// renderDashboard returns the dashboard HTML with the reverse-proxy basepath
// (e.g. "/pincher") substituted in. Pass "" for direct deployment.
//
// Post-#56: the dashboard's CSS and JS are served from separate endpoints
// (/v1/dashboard.css + /v1/dashboard.js) instead of inline. This lets the
// CSP drop 'unsafe-inline' from script-src and style-src — XSS that injects
// inline <script> content is now blocked by the browser even if it bypasses
// our esc() escape pipeline.
func renderDashboard(prefix string) string {
	return strings.ReplaceAll(dashboardTemplate, "__PINCHER_BASEPATH__", normalizeBasePath(prefix))
}

// renderDashboardJS returns the dashboard JavaScript with the reverse-proxy
// basepath substituted into the BP constant. Served from /v1/dashboard.js.
//
// #523: prefix is normalized before substitution so a trailing slash on
// the input doesn't survive into BP. Without normalization the JS
// wrapper rewrite `BP + '/v1/...'` produces `/foo//v1/...` (double
// slash) which reverse proxies route as a different path than the HTML
// <link>/<script> tags, silently breaking every dashboard data fetch.
func renderDashboardJS(prefix string) string {
	return strings.ReplaceAll(dashboardJSTemplate, "__PINCHER_BASEPATH__", normalizeBasePath(prefix))
}

// renderDashboardCSS returns the dashboard CSS. No basepath substitution
// needed — CSS has no URL references. Served from /v1/dashboard.css.
func renderDashboardCSS() string {
	return dashboardCSSContent
}

// dashboardTemplate is the self-contained stats dashboard served at GET /v1/dashboard.
// Loads CSS from /v1/dashboard.css and JS from /v1/dashboard.js; no inline blocks
// so the CSP can enforce script-src 'self' / style-src 'self'.
// __PINCHER_BASEPATH__ tokens are substituted at render time by renderDashboard.
const dashboardTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>pincherMCP · Dashboard</title>
  <link rel="stylesheet" href="__PINCHER_BASEPATH__/v1/dashboard.css">
</head>
<body>
<header>
  <svg width="36" height="36" viewBox="0 0 36 36" fill="none">
    <circle cx="18" cy="18" r="17" stroke="url(#hg)" stroke-width="2"/>
    <line x1="10" y1="10" x2="26" y2="26" stroke="#58a6ff" stroke-width="2.5" stroke-linecap="round"/>
    <line x1="26" y1="10" x2="10" y2="26" stroke="#a371f7" stroke-width="2.5" stroke-linecap="round"/>
    <circle cx="18" cy="18" r="4" fill="#58a6ff"/>
    <defs><linearGradient id="hg" x1="0" y1="0" x2="36" y2="36"><stop stop-color="#58a6ff"/><stop offset="1" stop-color="#a371f7"/></linearGradient></defs>
  </svg>
  <div>
    <h1>pincher<span>MCP</span> <span style="font-size:12px;font-weight:400" id="ver"></span></h1>
    <p>Codebase intelligence · Token savings dashboard</p>
  </div>
  <div style="margin-left:auto;display:flex;gap:8px;align-items:center">
    <span class="badge badge-green" id="health-badge">● checking…</span>
    <span class="badge badge-blue" id="last-refresh">—</span>
    <span class="badge badge-muted updated-ago" data-source="overview" title="Time since last auto-refresh">—</span>
    <select class="header-btn" id="refresh-select" title="Auto-refresh interval (#552)" data-action-change="onRefreshIntervalChange">
      <option value="5000">Refresh: 5s</option>
      <option value="30000" selected>Refresh: 30s</option>
      <option value="60000">Refresh: 1m</option>
      <option value="300000">Refresh: 5m</option>
      <option value="0">Refresh: off</option>
    </select>
    <button class="header-btn" id="theme-btn" title="Toggle theme (auto/light/dark)" data-action="cycleTheme">🌗</button>
    <button class="header-btn" id="auth-btn" title="Set HTTP bearer token (required when pincher is started with --http-key)" data-action="promptForKey">Auth</button>
  </div>
</header>

<div id="auth-notice" class="auth-notice" role="status">
  <span class="auth-notice-icon">🔓</span>
  <div class="auth-notice-body">
    <strong>No HTTP auth in place.</strong>
    Pincher is bound without <code>--http-key</code> — fine for localhost,
    but bind a bearer token if you ever expose this beyond loopback.
    <a href="https://github.com/kwad77/pincher/blob/master/docs/REFERENCE.md#http-rest-api" target="_blank" rel="noopener">Reference</a>.
  </div>
  <button class="auth-notice-dismiss" data-action="dismissAuthNotice" title="Dismiss">&#x2715;</button>
</div>

<nav class="tab-bar">
  <button class="tab-btn active" data-action="showTab" data-args='["overview"]'>Overview</button>
  <button class="tab-btn" data-action="showTab" data-args='["projects"]'>Projects</button>
  <button class="tab-btn" data-action="showTab" data-args='["search"]'>Search</button>
  <button class="tab-btn" data-action="showTab" data-args='["adrs"]'>ADRs</button>
  <button class="tab-btn" data-action="showTab" data-args='["sessions"]'>Sessions</button>
</nav>

<!-- OVERVIEW -->
<div id="tab-overview" class="tab-pane active">
<main>
  <div id="error-box"></div>
  <p class="section-title" id="session-title">Session</p>
  <div class="grid grid-4" id="session-cards"><div class="loading">Loading…</div></div>
  <p class="section-title">All-Time ROI</p>
  <div class="grid grid-4" id="alltime-cards"><div class="loading">Loading…</div></div>
  <div id="projection-banner"></div>
  <p class="section-title">Token Savings History</p>
  <div class="sparkline-card">
    <div class="sparkline-meta">
      <span class="sparkline-title">Tokens saved per session</span>
      <span class="sparkline-legend" id="sparkline-legend">—</span>
    </div>
    <svg id="sparkline-svg" viewBox="0 0 800 80" preserveAspectRatio="none">
      <text x="400" y="45" text-anchor="middle" fill="#8b949e" font-size="12">Loading…</text>
    </svg>
  </div>
  <p class="section-title">PreToolUse Hook (last 7 days)</p>
  <div class="grid grid-3" id="hook-stats-cards"><div class="loading">Loading…</div></div>
  <!-- #635 v0.67: per-tool breakdown panel — driven by
       /v1/tool-call-stats over schema v27 session_tool_calls.
       Visible immediately after onboarding once the first session
       lands a few tool calls. -->
  <p class="section-title">Tool Call Breakdown (last 7 days)</p>
  <div class="tool-breakdown-card">
    <div id="tool-breakdown-body"><div class="loading">Loading…</div></div>
  </div>
  <!-- #635 panel 2: per-complexity-tier breakdown. Same substrate
       different cut — answers "where is my call budget being spent"
       across lite / standard / heavy tiers. -->
  <p class="section-title">Calls by Complexity Tier (last 7 days)</p>
  <div class="tier-breakdown-card">
    <div id="tier-breakdown-body"><div class="loading">Loading…</div></div>
  </div>
  <!-- #635 panel 3: per-tool payload-size distribution. Surfaces
       outliers — tools whose max response is many multiples of their
       avg, the ones occasionally blowing up token bills. Sorted by
       max_bytes DESC to put the loud ones first. -->
  <p class="section-title">Response Payload Size by Tool (last 7 days)</p>
  <div class="payload-size-card">
    <div id="payload-size-body"><div class="loading">Loading…</div></div>
  </div>
</main>
</div>

<!-- PROJECTS -->
<div id="tab-projects" class="tab-pane">
<main>
  <p class="section-title">Indexed Projects</p>
  <div class="proj-toolbar">
    <input class="search-input" id="proj-filter" type="text" placeholder="Filter by name or path…" data-action-input="renderProjects"/>
    <label class="toolbar-check" title="Hide projects with zero symbols and zero edges">
      <input type="checkbox" id="proj-hide-empty" data-action-change="renderProjects"/> Hide empty
    </label>
    <button class="btn secondary" id="proj-cleanup-btn" data-action="cleanupEmpty">Remove all empty</button>
    <button class="btn secondary" title="Export current projects table as CSV (#551)" data-action="exportTable" data-args='["csv","projects"]'>CSV</button>
    <button class="btn secondary" title="Export current projects table as JSON (#551)" data-action="exportTable" data-args='["json","projects"]'>JSON</button>
    <span class="toolbar-count" id="proj-count">&nbsp;</span>
  </div>
  <div class="grid grid-2" id="projects-grid"><div class="loading">Loading…</div></div>
  <div class="detail-panel" id="proj-detail-panel">
    <div class="detail-panel-title">
      <span id="proj-detail-name">Project Details</span>
      <button class="detail-close" data-action="closeDetail" title="Close">&#x2715;</button>
    </div>
    <div id="proj-detail-body"><div class="loading">Loading…</div></div>
  </div>
</main>
</div>

<!-- SEARCH -->
<div id="tab-search" class="tab-pane">
<main>
  <p class="section-title">Symbol Search</p>
  <div class="search-bar">
    <input class="search-input" id="search-q" type="text" placeholder="Search symbols… (e.g. handleSearch, auth*, &quot;token validation&quot;)" data-action-enter="doSearch" data-action-input="debouncedSearch"/>
    <select class="search-select" id="search-kind">
      <option value="">All kinds</option>
      <option>Function</option><option>Method</option><option>Class</option>
      <option>Interface</option><option>Type</option><option>Variable</option>
    </select>
    <select class="search-select" id="search-proj"><option value="">All projects</option></select>
    <button class="search-btn" data-action="doSearch">Search</button>
  </div>
  <div id="search-results"></div>
</main>
</div>

<!-- ADRs -->
<div id="tab-adrs" class="tab-pane">
<main>
  <p class="section-title">Architecture Decision Records</p>
  <div class="adr-toolbar">
    <select class="adr-select" id="adr-proj" data-action-change="onADRProjectChange"><option value="">Select a project…</option></select>
    <button class="btn secondary" data-action="toggleADRForm">+ Add Entry</button>
  </div>
  <div class="adr-form" id="adr-form">
    <input id="adr-key" type="text" maxlength="256" placeholder="Key (e.g. PURPOSE, STACK, PATTERNS)"/>
    <textarea id="adr-val" maxlength="16384" placeholder="Value…"></textarea>
    <div id="adr-val-counter" class="adr-counter">0 / 16384</div>
    <div class="adr-form-actions">
      <button class="btn" data-action="saveADR">Save</button>
      <button class="btn secondary" data-action="toggleADRForm">Cancel</button>
    </div>
  </div>
  <div id="adr-list"><div class="empty">Select a project to view its ADRs.</div></div>
</main>
</div>

<!-- SESSIONS -->
<div id="tab-sessions" class="tab-pane">
<main>
  <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px">
    <p class="section-title" style="margin:0">Session History</p>
    <div>
      <button class="btn secondary" title="Export sessions as CSV (#551)" data-action="exportTable" data-args='["csv","sessions"]'>CSV</button>
      <button class="btn secondary" title="Export sessions as JSON (#551)" data-action="exportTable" data-args='["json","sessions"]'>JSON</button>
    </div>
  </div>
  <div id="sessions-table-wrap"><div class="loading">Loading…</div></div>
</main>
</div>

<div class="footer">pincherMCP · <a href="__PINCHER_BASEPATH__/v1/openapi.json" target="_blank">OpenAPI</a> · <a href="__PINCHER_BASEPATH__/v1/health" target="_blank">Health</a></div>
<div class="toast" id="toast"></div>

<script src="__PINCHER_BASEPATH__/v1/dashboard.js" defer></script>
</body>
</html>`

// dashboardCSSContent is the dashboard stylesheet, served from /v1/dashboard.css.
const dashboardCSSContent = `
*{box-sizing:border-box;margin:0;padding:0}
/* #549: theme variables. Default = dark (GitHub-dark palette).
   Light variant overrides via :root[data-theme="light"]. The auto
   mode is the system @media query; explicit attribute wins. */
:root{
  --bg:#0d1117;--surface:#161b22;--border:#30363d;
  --text:#e6edf3;--muted:#8b949e;--accent:#58a6ff;
  --green:#3fb950;--purple:#a371f7;--orange:#f0883e;--red:#f85149;
}
:root[data-theme="light"]{
  --bg:#f6f8fa;--surface:#ffffff;--border:#d0d7de;
  --text:#1f2328;--muted:#656d76;--accent:#0969da;
  --green:#1a7f37;--purple:#8250df;--orange:#bc4c00;--red:#cf222e;
}
@media (prefers-color-scheme: light){
  :root:not([data-theme]){
    --bg:#f6f8fa;--surface:#ffffff;--border:#d0d7de;
    --text:#1f2328;--muted:#656d76;--accent:#0969da;
    --green:#1a7f37;--purple:#8250df;--orange:#bc4c00;--red:#cf222e;
  }
}
body{background:var(--bg);color:var(--text);font-family:ui-sans-serif,system-ui,-apple-system,sans-serif;min-height:100vh}
header{background:linear-gradient(135deg,#0d1117 0%,#1a1f2e 100%);border-bottom:1px solid var(--border);padding:20px 32px;display:flex;align-items:center;gap:16px}
header svg{flex-shrink:0}
header h1{font-size:22px;font-weight:700;letter-spacing:-.5px}
header h1 span{color:var(--accent)}
header p{color:var(--muted);font-size:13px;margin-top:3px}
.badge{display:inline-flex;align-items:center;gap:5px;padding:3px 10px;border-radius:20px;font-size:11px;font-weight:600;letter-spacing:.4px}
.badge-green{background:rgba(63,185,80,.15);color:var(--green);border:1px solid rgba(63,185,80,.3)}
.badge-blue{background:rgba(88,166,255,.12);color:var(--accent);border:1px solid rgba(88,166,255,.25)}
.badge-muted{background:transparent;color:var(--muted);border:1px solid var(--border);font-variant-numeric:tabular-nums}
.header-btn{background:none;border:1px solid var(--border);border-radius:20px;color:var(--muted);cursor:pointer;font-size:11px;font-weight:600;letter-spacing:.4px;padding:3px 12px;transition:all .15s;font-family:inherit}
.header-btn:hover{border-color:var(--accent);color:var(--accent)}
.header-btn.authed{border-color:var(--green);color:var(--green)}

/* ── Tab nav ── */
.tab-bar{background:var(--surface);border-bottom:1px solid var(--border);padding:0 32px;display:flex;gap:0}
.tab-btn{background:none;border:none;border-bottom:2px solid transparent;color:var(--muted);cursor:pointer;font-size:13px;font-weight:500;padding:12px 18px;transition:color .15s,border-color .15s}
.tab-btn:hover{color:var(--text)}
.tab-btn.active{color:var(--accent);border-bottom-color:var(--accent)}
.tab-pane{display:none}
.tab-pane.active{display:block}

main{max-width:1200px;margin:0 auto;padding:32px}
.section-title{font-size:11px;font-weight:600;letter-spacing:1px;text-transform:uppercase;color:var(--muted);margin-bottom:14px}
.grid{display:grid;gap:16px;margin-bottom:32px}
.grid-4{grid-template-columns:repeat(auto-fit,minmax(220px,1fr))}
.grid-2{grid-template-columns:repeat(auto-fit,minmax(340px,1fr))}
.card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:20px;position:relative;overflow:hidden}
.card::before{content:'';position:absolute;top:0;left:0;right:0;height:2px;background:linear-gradient(90deg,var(--accent),var(--purple))}
.card.green::before{background:linear-gradient(90deg,var(--green),var(--accent))}
.card.orange::before{background:linear-gradient(90deg,var(--orange),var(--red))}
.card.purple::before{background:linear-gradient(90deg,var(--purple),var(--accent))}
.card-label{font-size:11px;color:var(--muted);font-weight:500;letter-spacing:.3px;text-transform:uppercase;margin-bottom:8px}
.card-value{font-size:32px;font-weight:700;line-height:1;letter-spacing:-1px}
.card-value.blue{color:var(--accent)}
.card-value.green{color:var(--green)}
.card-value.orange{color:var(--orange)}
.card-value.purple{color:var(--purple)}
.card-sub{font-size:12px;color:var(--muted);margin-top:6px}

/* ── Sparkline ── */
.sparkline-card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:20px;margin-bottom:32px;position:relative}
.sparkline-tip{display:none;position:absolute;background:#0d1117e6;border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:11px;padding:6px 9px;pointer-events:none;white-space:nowrap;z-index:10;line-height:1.4}
.sparkline-card svg{width:100%;height:80px;display:block}
.sparkline-meta{display:flex;justify-content:space-between;align-items:center;margin-bottom:12px}
.sparkline-title{font-size:11px;font-weight:600;letter-spacing:1px;text-transform:uppercase;color:var(--muted)}
.sparkline-legend{font-size:11px;color:var(--muted)}

/* ── Tool-call breakdown table (#635 v0.67) ── */
.tool-breakdown-card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:0;margin-bottom:32px;overflow-x:auto}
.tool-breakdown-table{width:100%;border-collapse:collapse;font-size:13px}
.tool-breakdown-table th,.tool-breakdown-table td{padding:10px 14px;text-align:left;border-bottom:1px solid var(--border)}
.tool-breakdown-table th{font-size:11px;font-weight:600;letter-spacing:.5px;text-transform:uppercase;color:var(--muted);background:rgba(255,255,255,.02)}
.tool-breakdown-table tr:last-child td{border-bottom:none}
.tool-breakdown-table tbody tr:hover{background:rgba(88,166,255,.03)}
.tool-breakdown-table .num{text-align:right;font-variant-numeric:tabular-nums}
.tool-breakdown-table .num.green{color:var(--green)}
.tool-breakdown-table tfoot td{font-size:12px;color:var(--text);background:rgba(255,255,255,.025);border-top:2px solid var(--border)}
.tool-breakdown-table code{background:transparent;border:none;padding:0;color:var(--text);font-weight:500}

/* ── Tier breakdown panel (#635 panel 2) ── */
.tier-breakdown-card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:18px;margin-bottom:32px}
.tier-bar{display:flex;height:18px;border-radius:6px;overflow:hidden;background:#0b1018;border:1px solid var(--border);margin-bottom:14px}
.tier-bar-seg{transition:flex-basis .25s;min-width:2px}
.tier-bar-seg:not(:first-child){border-left:1px solid rgba(0,0,0,.25)}
.tier-breakdown-table{width:100%;border-collapse:collapse;font-size:13px;margin-top:4px}
.tier-breakdown-table th,.tier-breakdown-table td{padding:8px 12px;text-align:left;border-bottom:1px solid var(--border)}
.tier-breakdown-table th{font-size:11px;font-weight:600;letter-spacing:.5px;text-transform:uppercase;color:var(--muted)}
.tier-breakdown-table tr:last-child td{border-bottom:none}
.tier-breakdown-table .num{text-align:right;font-variant-numeric:tabular-nums}
.tier-breakdown-table .num.green{color:var(--green)}
.tier-swatch{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:8px;vertical-align:middle}
.payload-size-card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:0;margin-bottom:32px;overflow-x:auto}
.payload-size-table{width:100%;border-collapse:collapse;font-size:13px}
.payload-size-table th,.payload-size-table td{padding:10px 14px;text-align:left;border-bottom:1px solid var(--border)}
.payload-size-table th{font-size:11px;font-weight:600;letter-spacing:.5px;text-transform:uppercase;color:var(--muted);background:#0b1018}
.payload-size-table tr:last-child td{border-bottom:none}
.payload-size-table .num{text-align:right;font-variant-numeric:tabular-nums}
.payload-badge{display:inline-block;padding:2px 8px;border-radius:10px;font-size:11px;font-weight:600;letter-spacing:.3px}
.payload-tight{background:rgba(57,211,83,.12);color:var(--green)}
.payload-wide{background:rgba(187,128,9,.18);color:var(--orange)}
.payload-spike{background:rgba(248,81,73,.18);color:var(--red)}

/* ── Project cards ── */
.proj-card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:18px;transition:border-color .2s;position:relative}
.proj-card:hover{border-color:var(--accent)}
.proj-card.invalid{border-color:rgba(248,81,73,.4)}
.proj-card.invalid::before{content:'';position:absolute;top:0;left:0;right:0;height:2px;background:var(--red);border-radius:10px 10px 0 0}
.proj-card.stale{border-color:rgba(240,136,62,.4)}
.proj-card.stale::before{content:'';position:absolute;top:0;left:0;right:0;height:2px;background:var(--orange);border-radius:10px 10px 0 0}
.proj-card.indexing{pointer-events:none;opacity:.6}
.proj-card.indexing::after{content:'Indexing\2026';position:absolute;inset:0;display:flex;align-items:center;justify-content:center;font-size:13px;font-weight:600;color:var(--accent);background:rgba(13,17,23,.7);border-radius:10px;animation:pulse 1.2s ease-in-out infinite}
.proj-header{display:flex;justify-content:space-between;align-items:flex-start;margin-bottom:4px}
.proj-name{font-size:15px;font-weight:600}
.proj-actions{display:flex;gap:6px;flex-shrink:0}
.proj-btn{background:none;border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer;font-size:11px;padding:3px 8px;transition:all .15s}
.proj-btn:hover{border-color:var(--accent);color:var(--accent)}
.proj-btn.danger:hover{border-color:var(--red);color:var(--red)}
.proj-btn:disabled{opacity:.4;cursor:default}
.proj-path{font-size:11px;color:var(--muted);margin-bottom:12px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.proj-path.missing{color:var(--red)}
.proj-stats{display:flex;gap:16px;flex-wrap:wrap}
.proj-stat{text-align:center}
.proj-stat-val{font-size:18px;font-weight:700;color:var(--accent)}
.proj-stat-label{font-size:10px;color:var(--muted);text-transform:uppercase;letter-spacing:.5px}

/* ── Projects toolbar ── */
.proj-toolbar{display:flex;gap:10px;margin-bottom:18px;align-items:center;flex-wrap:wrap}
/* #550: keyboard-focused project card. Outline picks up the accent
   color so it's visible in both light and dark themes. */
.proj-card.kbd-focused{outline:2px solid var(--accent);outline-offset:2px}
.proj-toolbar .search-input{flex:1;min-width:200px}
.toolbar-check{color:var(--muted);font-size:12px;display:inline-flex;align-items:center;gap:6px;cursor:pointer;user-select:none;white-space:nowrap}
.toolbar-check input{accent-color:var(--accent);cursor:pointer}
.toolbar-count{color:var(--muted);font-size:12px;margin-left:auto;font-variant-numeric:tabular-nums}

/* ── Pills ── */
.pill{display:inline-block;padding:2px 8px;border-radius:12px;font-size:11px;background:rgba(88,166,255,.12);color:var(--accent);border:1px solid rgba(88,166,255,.2);font-family:ui-monospace,monospace}
.pill.warn{background:rgba(248,81,73,.1);color:var(--red);border-color:rgba(248,81,73,.3)}
.pill.stale-pill{background:rgba(240,136,62,.1);color:var(--orange);border-color:rgba(240,136,62,.3)}

/* ── Search tab ── */
.search-bar{display:flex;gap:10px;margin-bottom:24px}
.search-input{flex:1;background:var(--surface);border:1px solid var(--border);border-radius:8px;color:var(--text);font-size:14px;padding:10px 14px;outline:none;transition:border-color .15s}
.search-input:focus{border-color:var(--accent)}
.search-select{background:var(--surface);border:1px solid var(--border);border-radius:8px;color:var(--text);font-size:13px;padding:10px 12px;cursor:pointer}
.search-btn{background:var(--accent);border:none;border-radius:8px;color:#0d1117;cursor:pointer;font-size:13px;font-weight:600;padding:10px 20px;transition:opacity .15s}
.search-btn:hover{opacity:.85}
.result-card{background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:14px 16px;margin-bottom:10px}
.result-header{display:flex;align-items:center;justify-content:space-between;gap:10px;margin-bottom:2px}
.result-name{font-size:14px;font-weight:600;color:var(--accent)}
.copy-id-btn{background:none;border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer;font-size:11px;padding:3px 10px;transition:all .15s;flex-shrink:0;font-family:inherit}
.copy-id-btn:hover{border-color:var(--accent);color:var(--accent)}
.copy-id-btn.copied{border-color:var(--green);color:var(--green)}
.first-run{background:var(--surface);border:1px solid var(--accent);border-radius:10px;padding:28px;text-align:left;line-height:1.6;grid-column:1/-1}
.first-run code{background:#0d1117;border:1px solid var(--border);border-radius:4px;padding:2px 8px;font-family:ui-monospace,monospace;color:var(--accent);font-size:12px}
.result-meta{font-size:11px;color:var(--muted);margin-bottom:8px}
.result-snippet{background:#0d1117;border-radius:6px;font-family:ui-monospace,monospace;font-size:12px;line-height:1.5;overflow-x:auto;padding:10px 12px;white-space:pre;color:var(--text)}
.result-snippet mark,.result-name mark{background:#facc1530;color:#fde047;padding:0 2px;border-radius:2px}
.result-count{color:var(--muted);font-size:12px;margin-bottom:8px;font-variant-numeric:tabular-nums}

/* ── ADR tab ── */
.adr-toolbar{display:flex;gap:10px;margin-bottom:20px;align-items:center}
.adr-select{background:var(--surface);border:1px solid var(--border);border-radius:8px;color:var(--text);font-size:13px;padding:8px 12px;flex:1;max-width:400px}
.btn{background:var(--accent);border:none;border-radius:8px;color:#0d1117;cursor:pointer;font-size:13px;font-weight:600;padding:8px 16px;transition:opacity .15s}
.btn:hover{opacity:.85}
.btn.secondary{background:var(--surface);border:1px solid var(--border);color:var(--text)}
.btn.secondary:hover{border-color:var(--accent);opacity:1}
.btn.danger-btn{background:rgba(248,81,73,.15);border:1px solid rgba(248,81,73,.3);color:var(--red)}
.adr-row{background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:12px 16px;margin-bottom:8px;display:flex;gap:12px;align-items:flex-start}
.adr-key{font-family:ui-monospace,monospace;font-size:12px;color:var(--accent);font-weight:600;min-width:140px;padding-top:2px}
.adr-val{flex:1;font-size:13px;color:var(--text);white-space:pre-wrap;word-break:break-word}
.adr-del{background:none;border:none;color:var(--muted);cursor:pointer;font-size:14px;padding:2px 6px;border-radius:4px;transition:color .15s;flex-shrink:0}
.adr-del:hover{color:var(--red)}
.adr-form{background:var(--surface);border:1px solid var(--accent);border-radius:8px;padding:16px;margin-bottom:16px;display:none}
.adr-form.open{display:block}
.adr-form input,.adr-form textarea{background:#0d1117;border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:13px;padding:8px 10px;width:100%;margin-bottom:10px;outline:none;font-family:inherit}
.adr-counter{font-size:11px;color:var(--muted);text-align:right;margin-top:-6px;margin-bottom:8px}
.adr-counter.warn{color:#f59e0b}
.adr-counter.over{color:#ef4444;font-weight:600}

/* #553: ADR value as <pre> with pre-wrap so multi-line content
   keeps its line breaks but still wraps on long lines. */
.adr-val{background:#0d1117;border:1px solid var(--border);border-radius:6px;padding:8px 10px;font-size:12px;font-family:ui-monospace,monospace;line-height:1.5;color:var(--text);white-space:pre-wrap;word-wrap:break-word;margin-top:4px;max-height:400px;overflow-y:auto}

/* #541: skeleton loading placeholders. Pulse animation gives the
   user visual confirmation that something is loading without text. */
.sk-wrap{display:flex;flex-direction:column;gap:8px;padding:8px 0}
.sk-line{background:var(--surface);border-radius:4px;height:16px;width:100%;animation:sk-pulse 1.5s ease-in-out infinite}
.sk-line:nth-child(2n){width:80%}
.sk-line:nth-child(3n){width:65%}
.sk-card{background:var(--surface);border:1px solid var(--border);border-radius:8px;height:60px;animation:sk-pulse 1.5s ease-in-out infinite}
@keyframes sk-pulse{0%,100%{opacity:.4}50%{opacity:.7}}

/* #540: empty-state CTA card. Replaces the bare "No X yet" text. */
.empty-cta{background:var(--surface);border:1px dashed var(--border);border-radius:10px;padding:32px;text-align:center;margin:8px 0}
.empty-cta-icon{font-size:32px;margin-bottom:8px}
.empty-cta-title{font-size:15px;font-weight:600;color:var(--text);margin-bottom:6px}
.empty-cta-body{font-size:12px;color:var(--muted);margin-bottom:12px}
.empty-cta-cmd{display:inline-block;background:#0d1117;border:1px solid var(--border);border-radius:6px;padding:8px 14px;font-family:ui-monospace,monospace;font-size:12px;color:var(--text);text-align:left}

/* #543: confirm modal. Centered card on a translucent backdrop. */
.confirm-modal{display:none;position:fixed;inset:0;background:rgba(0,0,0,.5);z-index:1000;align-items:center;justify-content:center}
.confirm-modal.open{display:flex}
.confirm-modal-card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:20px 22px;max-width:420px;width:90%;box-shadow:0 10px 40px rgba(0,0,0,.4)}
.confirm-modal-title{font-size:14px;font-weight:600;color:var(--text);margin-bottom:6px}
.confirm-modal-body{font-size:13px;color:var(--muted);line-height:1.5;margin-bottom:16px}
.confirm-modal-actions{display:flex;gap:8px;justify-content:flex-end}
.btn.destructive{background:rgba(248,81,73,.15);color:var(--red);border:1px solid rgba(248,81,73,.4)}
.btn.destructive:hover{background:rgba(248,81,73,.25)}

/* #552: header refresh select picks up header-btn styling but needs
   a matching appearance reset since it's a <select>. */
#refresh-select{appearance:none;-webkit-appearance:none;background-image:none;padding-right:18px}
.adr-form textarea{min-height:80px;resize:vertical}
.adr-form input:focus,.adr-form textarea:focus{border-color:var(--accent)}
.adr-form-actions{display:flex;gap:8px}

/* ── Sessions table ── */
.sessions-table{width:100%;border-collapse:collapse;font-size:13px}
.sessions-table th{border-bottom:1px solid var(--border);color:var(--muted);font-size:11px;font-weight:600;letter-spacing:.5px;padding:8px 12px;text-align:left;text-transform:uppercase}
.sessions-table td{border-bottom:1px solid rgba(48,54,61,.5);padding:10px 12px;vertical-align:middle}
.sessions-table tbody tr:last-child td{border-bottom:none}
.sessions-table tr:hover td{background:rgba(22,27,34,.6)}
.sessions-table tfoot tr.sessions-total td{background:rgba(88,166,255,.05);border-top:1px solid var(--border);color:var(--text);font-weight:700;font-variant-numeric:tabular-nums;padding-top:12px;padding-bottom:12px}
.sessions-table tfoot tr.sessions-total:hover td{background:rgba(88,166,255,.08)}
.mono{font-family:ui-monospace,monospace;font-size:11px}

/* ── Projection banner ── */
.proj-banner{background:linear-gradient(135deg,rgba(63,185,80,.08) 0%,rgba(88,166,255,.08) 100%);border:1px solid rgba(63,185,80,.25);border-radius:10px;padding:20px 24px;margin-bottom:32px;display:flex;align-items:center;gap:24px;flex-wrap:wrap}
.proj-banner-icon{font-size:28px;flex-shrink:0}
.proj-banner-body{flex:1;min-width:200px}
.proj-banner-title{font-size:11px;font-weight:600;letter-spacing:1px;text-transform:uppercase;color:var(--muted);margin-bottom:4px}
.proj-banner-rate{font-size:28px;font-weight:700;color:var(--green);letter-spacing:-1px;line-height:1}
.proj-banner-sub{font-size:12px;color:var(--muted);margin-top:4px}
.proj-banner-pills{display:flex;gap:10px;flex-wrap:wrap;align-items:center}
.proj-banner-pill{background:rgba(63,185,80,.12);border:1px solid rgba(63,185,80,.25);border-radius:20px;color:var(--green);font-size:12px;font-weight:600;padding:4px 12px}
.proj-banner-pill.blue{background:rgba(88,166,255,.12);border-color:rgba(88,166,255,.25);color:var(--accent)}

/* ── Project detail panel ── */
.detail-panel{background:var(--surface);border:1px solid var(--accent);border-radius:10px;padding:20px;margin-top:16px;display:none}
.detail-panel.open{display:block}
.detail-panel-title{font-size:13px;font-weight:600;color:var(--accent);margin-bottom:14px;display:flex;align-items:center;justify-content:space-between}
.detail-close{background:none;border:none;color:var(--muted);cursor:pointer;font-size:16px;line-height:1;padding:0 4px}
.detail-close:hover{color:var(--text)}
.detail-section{margin-bottom:16px}
.detail-section-label{font-size:10px;font-weight:600;letter-spacing:.8px;text-transform:uppercase;color:var(--muted);margin-bottom:8px}
.detail-section-count{font-weight:400;letter-spacing:0;text-transform:none;color:var(--muted);margin-left:6px;font-size:11px}
.show-all-btn{background:none;border:1px solid var(--border);border-radius:4px;color:var(--accent);cursor:pointer;font-size:11px;margin-top:6px;padding:3px 9px}
.show-all-btn:hover{border-color:var(--accent);background:#161b22}
.lang-bar{display:flex;gap:6px;flex-wrap:wrap}
.lang-chip{background:rgba(163,113,247,.12);border:1px solid rgba(163,113,247,.25);border-radius:6px;color:var(--purple);font-size:11px;font-family:ui-monospace,monospace;padding:3px 8px}
.ep-list,.hotspot-list{list-style:none;margin:0;padding:0}
.ep-list li,.hotspot-list li{border-bottom:1px solid rgba(48,54,61,.4);font-size:12px;font-family:ui-monospace,monospace;padding:5px 0;color:var(--text)}
.ep-list li:last-child,.hotspot-list li:last-child{border-bottom:none}
.hotspot-calls{color:var(--muted);font-size:11px;margin-left:8px}

/* ── Progress bar ── */
.progress-wrap{background:rgba(48,54,61,.5);border-radius:4px;height:4px;margin-top:8px;overflow:hidden;display:none}
.progress-wrap.active{display:block}
.progress-bar{background:linear-gradient(90deg,var(--accent),var(--purple));height:100%;transition:width .3s;border-radius:4px}
.progress-label{font-size:11px;color:var(--muted);margin-top:4px;display:none}
.progress-label.active{display:block}

/* ── Misc ── */
.empty{color:var(--muted);font-size:13px;padding:24px;text-align:center}
.error{background:rgba(248,81,73,.1);border:1px solid rgba(248,81,73,.3);border-radius:8px;padding:16px;color:var(--red);font-size:13px;margin-bottom:16px}
.loading{color:var(--muted);font-size:13px;padding:8px 0;animation:pulse 1.5s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.4}}
.footer{text-align:center;padding:24px;color:var(--muted);font-size:12px;border-top:1px solid var(--border);margin-top:8px}
.footer a{color:var(--accent);text-decoration:none}
.toast{position:fixed;bottom:24px;right:24px;background:#161b22;border:1px solid var(--border);border-radius:8px;padding:10px 16px;font-size:13px;color:var(--text);opacity:0;transition:opacity .3s;pointer-events:none;z-index:100}
.toast.show{opacity:1}

/* ── Auth notice banner (#203) ─────────────────────────────────────────── */
/* Shown once per browser when /v1/health reports auth_required=false.
   Dismissed via localStorage so it never re-shows on reload until the
   user clears site data or starts pincher with --http-key. */
.auth-notice{display:none;align-items:flex-start;gap:12px;background:rgba(240,136,62,.08);border-bottom:1px solid rgba(240,136,62,.25);padding:10px 32px;font-size:12px;color:var(--text)}
.auth-notice.show{display:flex}
.auth-notice-icon{font-size:14px;line-height:1.3;flex-shrink:0}
.auth-notice-body{flex:1;line-height:1.5;color:var(--muted)}
.auth-notice-body strong{color:var(--text);font-weight:600}
.auth-notice-body code{background:var(--surface);padding:1px 5px;border-radius:3px;font-size:11px}
.auth-notice-body a{color:var(--accent);text-decoration:none}
.auth-notice-body a:hover{text-decoration:underline}
.auth-notice-dismiss{background:none;border:none;color:var(--muted);cursor:pointer;font-size:12px;padding:2px 6px;line-height:1;border-radius:3px;flex-shrink:0}
.auth-notice-dismiss:hover{color:var(--text);background:rgba(255,255,255,.04)}

/* ── Narrow viewport (#203) ────────────────────────────────────────────── */
/* Phone widths: collapse the header (logo+title can wrap, badges go to a
   second line), drop the page padding, force grids to single column,
   stack the project toolbar so the search input doesn't get crushed. */
@media (max-width:720px){
  header{padding:14px 16px;flex-wrap:wrap}
  header h1{font-size:18px}
  header > div:last-child{margin-left:0;width:100%;justify-content:flex-start;flex-wrap:wrap}
  .tab-bar{padding:0 8px;overflow-x:auto;flex-wrap:nowrap;-webkit-overflow-scrolling:touch}
  .tab-btn{padding:10px 12px;font-size:12px;flex-shrink:0}
  main{padding:16px}
  .grid-2,.grid-4{grid-template-columns:1fr !important}
  .auth-notice{padding:10px 16px}
  .proj-toolbar{flex-wrap:wrap}
  .proj-toolbar .search-input{flex-basis:100%;min-width:0}
  .search-bar{flex-wrap:wrap}
  .search-bar input,.search-bar select,.search-bar button{flex-basis:100%}
  .adr-toolbar{flex-direction:column;align-items:stretch}
  .adr-select{max-width:none}
  .adr-form{flex-direction:column}
  .adr-key{min-width:0}
  table{font-size:11px}
  .footer{padding:16px;font-size:11px}
}
`

// dashboardJSTemplate is the dashboard JavaScript, served from /v1/dashboard.js.
// __PINCHER_BASEPATH__ tokens are substituted at render time by renderDashboardJS.
const dashboardJSTemplate = `
// ── Reverse-proxy basepath ─────────────────────────────────────────────────
// When pincher is served behind a proxy at e.g. /pincher, the server injects
// the prefix here. All call sites still write fetch('/v1/...') — the wrapper
// below rewrites those to BP + '/v1/...' so the proxy sees the prefixed URL.
const BP = "__PINCHER_BASEPATH__";

// ── Auth fetch wrapper ─────────────────────────────────────────────────────
// Pincher's HTTP server optionally requires a bearer token (--http-key).
// The dashboard HTML itself loads without auth, but every data fetch it
// makes needs the token. We stash it in localStorage so users only set it
// once, then wrap window.fetch so every /v1/ call gets the header.
const AUTH_KEY_STORAGE = 'pincher_http_key';
let _authPrompting = false;
const _origFetch = window.fetch.bind(window);
window.fetch = async function(input, init) {
  init = init || {};
  init.headers = new Headers(init.headers || {});
  // Rewrite /v1/* → BP + /v1/* so the dashboard works behind a reverse proxy
  // without changing every fetch call site.
  if (BP && typeof input === 'string' && input.startsWith('/v1/')) {
    input = BP + input;
  }
  const key = localStorage.getItem(AUTH_KEY_STORAGE);
  const url = typeof input === 'string' ? input : (input && input.url) || '';
  const isV1 = url.startsWith('/v1/') || (BP && url.startsWith(BP + '/v1/'));
  if (key && isV1 && !init.headers.has('Authorization')) {
    init.headers.set('Authorization', 'Bearer ' + key);
  }
  const r = await _origFetch(input, init);
  if (r.status === 401 && !_authPrompting) {
    _authPrompting = true;
    const entered = promptForKey('Server requires a bearer token. Paste the value set via pincher --http-key:');
    _authPrompting = false;
    if (entered) {
      // Retry the original request with the new token.
      init.headers.set('Authorization', 'Bearer ' + entered);
      return _origFetch(input, init);
    }
  }
  return r;
};

function promptForKey(message) {
  const current = localStorage.getItem(AUTH_KEY_STORAGE) || '';
  const val = window.prompt(message || 'HTTP bearer token (leave blank to clear):', current);
  if (val === null) return null; // cancelled
  if (val === '') {
    localStorage.removeItem(AUTH_KEY_STORAGE);
    updateAuthBadge();
    showToast('Auth token cleared');
    return '';
  }
  localStorage.setItem(AUTH_KEY_STORAGE, val);
  updateAuthBadge();
  showToast('Auth token saved; reloading…');
  setTimeout(() => location.reload(), 400);
  return val;
}

// #549: theme toggle. Three states cycle on click — auto → light → dark → auto.
// Stored in localStorage so it survives reload. "auto" removes the
// data-theme attribute so the @media query takes over.
const THEME_STORAGE = 'pincher_theme';

function applyStoredTheme() {
  const v = localStorage.getItem(THEME_STORAGE);
  if (v === 'light' || v === 'dark') {
    document.documentElement.setAttribute('data-theme', v);
  } else {
    document.documentElement.removeAttribute('data-theme');
  }
  // Update the toggle icon to reflect current state.
  const btn = document.getElementById('theme-btn');
  if (btn) {
    btn.textContent = v === 'light' ? '☀️' : v === 'dark' ? '🌙' : '🌗';
  }
}

function cycleTheme() {
  const cur = localStorage.getItem(THEME_STORAGE) || 'auto';
  const next = cur === 'auto' ? 'light' : cur === 'light' ? 'dark' : 'auto';
  if (next === 'auto') {
    localStorage.removeItem(THEME_STORAGE);
  } else {
    localStorage.setItem(THEME_STORAGE, next);
  }
  applyStoredTheme();
  showToast('Theme: '+next);
}

function updateAuthBadge() {
  const btn = document.getElementById('auth-btn');
  if (!btn) return;
  const has = !!localStorage.getItem(AUTH_KEY_STORAGE);
  btn.textContent = has ? 'Auth ✓' : 'Auth';
  btn.classList.toggle('authed', has);
}

function dismissAuthNotice() {
  const banner = document.getElementById('auth-notice');
  if (banner) banner.classList.remove('show');
  localStorage.setItem('pincher.auth-notice.dismissed', '1');
}

// ── Utilities ──────────────────────────────────────────────────────────────
const fmt = n => n >= 1e6 ? (n/1e6).toFixed(1)+'M' : n >= 1e3 ? (n/1e3).toFixed(1)+'K' : String(n);
const fmtMs = ms => ms < 1 ? '<1ms' : ms+'ms';
// #635 panel 3: human-readable byte sizes for the payload-size table.
// Uses binary units since response sizes are about wire bytes, not
// human counting. "0 B" is preferred over "—" so users see the column
// is alive even on empty entries.
const fmtBytes = n => {
  if (n == null || isNaN(n)) return '—';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / (1024 * 1024)).toFixed(1) + ' MB';
};
const esc = s => String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
function timeAgo(iso) {
  if (!iso) return '—';
  const secs = Math.floor((Date.now() - new Date(iso)) / 1000);
  if (secs < 60) return 'just now';
  if (secs < 3600) return Math.floor(secs/60) + 'm ago';
  if (secs < 86400) return Math.floor(secs/3600) + 'h ago';
  return Math.floor(secs/86400) + 'd ago';
}
const STALE_HOURS = 24;

async function copyID(id, btn) {
  const original = btn.textContent;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(id);
    } else {
      // Fallback for http:// non-secure contexts (common for local dashboards)
      const ta = document.createElement('textarea');
      ta.value = id; ta.style.position = 'fixed'; ta.style.top = '-9999px';
      document.body.appendChild(ta); ta.select();
      document.execCommand('copy');
      ta.remove();
    }
    btn.textContent = 'Copied';
    btn.classList.add('copied');
    setTimeout(() => { btn.textContent = original; btn.classList.remove('copied'); }, 1400);
  } catch(e) {
    showToast('Copy failed: '+e.message, false);
  }
}

// #542: toast manager. Originally a single one-shot toast; now
// supports kind (success/error/info), configurable TTL, and an
// ARIA live region so screen readers announce updates. Backwards
// compatible — existing callers passing showToast(msg, ok) still
// work because the second arg is reinterpreted as kind=success/error.
function showToast(msg, kindOrOk='success', opts={}) {
  const kind = (kindOrOk === true || kindOrOk === 'success') ? 'success'
             : (kindOrOk === false || kindOrOk === 'error') ? 'error'
             : kindOrOk;
  const t = document.getElementById('toast');
  if (!t) return;
  t.setAttribute('role', 'status');
  t.setAttribute('aria-live', 'polite');
  t.textContent = msg;
  t.style.borderColor = kind === 'error' ? 'var(--red)'
                      : kind === 'info' ? 'var(--accent)'
                      : 'var(--green)';
  t.classList.add('show');
  const ttl = (opts && opts.ttl) || (kind === 'error' ? 4500 : 2800);
  clearTimeout(t._toastTimer);
  t._toastTimer = setTimeout(() => t.classList.remove('show'), ttl);
}

// #543: custom confirm dialog. Replaces window.confirm() — styled,
// non-blocking, focus-managed, ARIA role=dialog. Returns a Promise
// that resolves true on confirm, false on cancel/Esc. Idempotent:
// the modal element is created lazily on first call.
function showConfirmDialog(title, body, opts={}) {
  return new Promise(resolve => {
    let m = document.getElementById('confirm-modal');
    if (!m) {
      m = document.createElement('div');
      m.id = 'confirm-modal';
      m.className = 'confirm-modal';
      m.setAttribute('role', 'dialog');
      m.setAttribute('aria-modal', 'true');
      document.body.appendChild(m);
    }
    const destroyText = (opts && opts.destroyText) || 'Confirm';
    const cancelText = (opts && opts.cancelText) || 'Cancel';
    const destructive = !!(opts && opts.destructive);
    m.innerHTML = '<div class="confirm-modal-card">'+
      '<div class="confirm-modal-title">'+esc(title)+'</div>'+
      '<div class="confirm-modal-body">'+esc(body)+'</div>'+
      '<div class="confirm-modal-actions">'+
        '<button class="btn secondary" id="cm-cancel">'+esc(cancelText)+'</button>'+
        '<button class="btn '+(destructive?'destructive':'')+'" id="cm-ok">'+esc(destroyText)+'</button>'+
      '</div></div>';
    m.classList.add('open');
    const cancelBtn = m.querySelector('#cm-cancel');
    const okBtn = m.querySelector('#cm-ok');
    const close = (val) => {
      m.classList.remove('open');
      document.removeEventListener('keydown', onKey);
      resolve(val);
    };
    const onKey = (ev) => {
      if (ev.key === 'Escape') { ev.preventDefault(); close(false); }
    };
    cancelBtn.onclick = () => close(false);
    okBtn.onclick = () => close(true);
    m.onclick = (ev) => { if (ev.target === m) close(false); };
    document.addEventListener('keydown', onKey);
    // Initial focus on cancel — the safer default per accessibility
    // guidance for destructive actions.
    setTimeout(() => cancelBtn.focus(), 10);
  });
}

// #541: render a skeleton placeholder for a tab section. Pure DOM —
// caller decides which element to fill. Skeleton blocks pulse via
// CSS animation so the user perceives "loading" without text.
function skeletonRows(count, kind='line') {
  const cls = kind === 'card' ? 'sk-card' : 'sk-line';
  let h = '<div class="sk-wrap" aria-busy="true" aria-label="Loading">';
  for (let i = 0; i < count; i++) h += '<div class="' + cls + '"></div>';
  h += '</div>';
  return h;
}

// #540: empty-state CTA. Tab-specific copy + a single primary action.
// Centralizes the three reachable empty states (Projects, Sessions,
// ADRs) so the messaging stays consistent.
function emptyStateCTA(kind) {
  const data = {
    projects: { icon: '📦', title: 'No projects indexed yet',
      body: 'Index your first repo to see it here.',
      cmd: 'pincher index /path/to/your/repo' },
    sessions: { icon: '⏱', title: 'No sessions yet',
      body: 'Sessions appear after the MCP server runs and flushes its first counters.',
      cmd: '' },
    adrs: { icon: '📓', title: 'No ADR entries',
      body: 'Pick a project and add the first decision via the form above.',
      cmd: '' },
  }[kind] || { icon: '·', title: 'Empty', body: '', cmd: '' };
  return '<div class="empty-cta">'+
    '<div class="empty-cta-icon">'+data.icon+'</div>'+
    '<div class="empty-cta-title">'+esc(data.title)+'</div>'+
    '<div class="empty-cta-body">'+esc(data.body)+'</div>'+
    (data.cmd ? '<pre class="empty-cta-cmd">'+esc(data.cmd)+'</pre>' : '')+
    '</div>';
}

// ── Tab navigation ─────────────────────────────────────────────────────────
// #539: per-tab AbortController registry. Each load*() registers
// its in-flight controller so the next load on that tab (or a
// tab switch) can abort prior requests. Without this, fast tab
// switching pile up parallel fetches whose late-arriving responses
// can overwrite a newer tab's content.
const _tabControllers = {};

function _abortTab(tab) {
  const c = _tabControllers[tab];
  if (c) {
    try { c.abort(); } catch(_) {}
    delete _tabControllers[tab];
  }
}

// tabFetch: scoped fetch wrapper. Aborts any prior in-flight request
// on this tab, registers a new controller, and returns the response.
// Callers handle AbortError by returning early (the controller was
// superseded by a newer call, the result is no longer relevant).
async function tabFetch(tab, url, opts) {
  _abortTab(tab);
  const c = new AbortController();
  _tabControllers[tab] = c;
  opts = opts || {};
  opts.signal = c.signal;
  try {
    const r = await fetch(url, opts);
    return r;
  } finally {
    // Only clear if we're still the active controller — a newer call
    // may have replaced us between fetch() and now, in which case we
    // should NOT clear theirs.
    if (_tabControllers[tab] === c) delete _tabControllers[tab];
  }
}

// #538: surface fetch failure in the tab body itself, not just the
// global error banner. Each tab-pane has a "loading…" placeholder
// that pre-#538 stayed forever on failure; setTabError swaps it for
// the v0.25 error envelope's message + a Retry button.
function setTabError(elementID, message, retryFn) {
  const el = document.getElementById(elementID);
  if (!el) return;
  // Strip control chars + cap length so a malicious response can't
  // inject anything weird through esc().
  const safe = String(message || 'Request failed').slice(0, 500);
  let html = '<div class="error">'+esc(safe);
  if (retryFn) html += ' <button class="btn secondary" data-action="'+esc(retryFn)+'">Retry</button>';
  html += '</div>';
  el.innerHTML = html;
}

// extractErrMsg pulls the message off the v0.25 envelope. Falls back
// to a status code or the generic exception message.
async function extractErrMsg(r) {
  if (!r) return 'no response';
  try {
    const body = await r.json();
    if (body.error && body.error.message) return body.error.message;
    if (typeof body.error === 'string') return body.error; // pre-v0.25 fallback
  } catch(_) {}
  return 'HTTP '+r.status;
}

function showTab(name) {
  // #539: abort every other tab's in-flight fetches before switching.
  // The tab we're switching TO will get a fresh controller from its
  // load*() call below.
  Object.keys(_tabControllers).forEach(t => { if (t !== name) _abortTab(t); });

  document.querySelectorAll('.tab-pane').forEach(p => p.classList.remove('active'));
  document.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
  document.getElementById('tab-'+name).classList.add('active');
  document.querySelectorAll('.tab-btn').forEach(b => {
    if (b.textContent.toLowerCase().startsWith(name === 'adrs' ? 'adr' : name)) b.classList.add('active');
  });
  location.hash = name;
  if (name === 'sessions') loadSessions();
  if (name === 'adrs') loadADRProjects();
  if (name === 'search') document.getElementById('search-q').focus();
}

// ── Stat card helper ───────────────────────────────────────────────────────
function statCard(label, value, cls, sub, cardCls) {
  return '<div class="card '+(cardCls||'')+'"><div class="card-label">'+esc(label)+'</div><div class="card-value '+cls+'">'+esc(value)+'</div>'+(sub?'<div class="card-sub">'+esc(sub)+'</div>':'')+'</div>';
}

// ── Overview ───────────────────────────────────────────────────────────────
async function load() {
  document.getElementById('last-refresh').textContent = 'refreshing…';
  document.getElementById('error-box').innerHTML = '';

  // Kick off every independent fetch in parallel — sequential awaits were
  // adding ~2s to time-to-interactive. Each result is handled independently
  // so one failing endpoint doesn't block the rest.
  const [healthR, statsR] = await Promise.allSettled([
    fetch('/v1/health').then(r=>r.json()),
    fetch('/v1/stats').then(r=>r.json()),
  ]);

  if (healthR.status === 'fulfilled') {
    const h = healthR.value;
    const hb = document.getElementById('health-badge');
    hb.textContent = '● online'; hb.className = 'badge badge-green';
    document.getElementById('ver').textContent = h.version ? 'v'+h.version : '';
    // Auth notice (#203): when the server reports auth_required=false,
    // surface a small banner so users know this is an unauthenticated
    // pincher (loopback-only by default — non-loopback binds without
    // --http-key are refused server-side per #199, so this is purely
    // informational, not a security issue, but still worth flagging).
    // Dismissed banners stay dismissed via localStorage so the warning
    // doesn't annoy on repeat loads of an intentional dev setup.
    if (h.auth_required === false && !localStorage.getItem('pincher.auth-notice.dismissed')) {
      const banner = document.getElementById('auth-notice');
      if (banner) banner.classList.add('show');
    }
  } else {
    document.getElementById('health-badge').textContent = '● offline';
    document.getElementById('health-badge').style.color = 'var(--red)';
  }

  if (statsR.status === 'fulfilled') {
    const data = statsR.value;
    const s = data.session || {};
    const a = data.all_time || {};
    // Determine if the session data is from the current session or a stale one.
    // A session is "live" if last_seen was within the last 90 seconds.
    const lastSeenMs = s.last_seen ? Date.now() - new Date(s.last_seen) : Infinity;
    const isLive = lastSeenMs < 90000;
    const sessLabel = isLive ? 'Current Session' : 'Last Session';
    const sessAge = s.last_seen ? (isLive ? 'active '+timeAgo(s.last_seen) : 'ended '+timeAgo(s.last_seen)) : 'no data yet';
    document.getElementById('session-title').textContent = sessLabel;
    if ((a.calls||0) === 0 && (s.calls||0) === 0) {
      // First-run: skip the all-zero stat cards and show onboarding instead.
      document.getElementById('session-cards').innerHTML =
        '<div class="empty first-run">Welcome to pincher.<br/><br/>' +
        'Index your first repo with <code>pincher index /path/to/repo</code> ' +
        'and start using pincher tools in Claude Code — your stats will populate here.</div>';
      document.getElementById('alltime-cards').innerHTML = '';
    } else {
      document.getElementById('session-cards').innerHTML =
        statCard('Tool Calls', fmt(s.calls||0), 'blue', sessAge, '') +
        statCard('Tokens Saved', fmt(s.tokens_saved||0), 'green', 'vs reading full files', 'green') +
        statCard('Tokens Used', fmt(s.tokens_used||0), 'purple', 'context consumed', 'purple');
      document.getElementById('alltime-cards').innerHTML = (a.calls||0) > 0 ?
        statCard('Total Calls', fmt(a.calls||0), 'blue', 'all sessions', '') +
        statCard('Total Tokens Saved', fmt(a.tokens_saved||0), 'green', 'cumulative', 'green') +
        statCard('Tokens Used', fmt(a.tokens_used||0), 'purple', 'context consumed', 'purple') :
        '<div class="empty">No sessions recorded yet.</div>';
    }
  } else {
    document.getElementById('error-box').innerHTML = '<div class="error">Failed to load stats: '+esc(statsR.reason)+'</div>';
  }

  // Projects + sparkline + hook stats load concurrently in the background
  // — the overview is already interactive once stats resolve.
  loadProjects();
  loadSparkline();
  loadHookStats();
  loadToolBreakdown();
  loadTierBreakdown();
  loadPayloadSize();
  document.getElementById('last-refresh').textContent = 'updated ' + new Date().toLocaleTimeString();
}

// loadHookStats fetches /v1/hook-stats and renders the v0.37 headline
// conversion-rate panel (#628). Until the v0.36 hook accumulates at
// least a handful of intercepts, the panel renders an onboarding hint
// pointing the user at "pincher init --target=claude" instead of
// flapping percentages.
async function loadHookStats() {
  try {
    const data = await fetch('/v1/hook-stats').then(r => r.json());
    const redirects = data.redirects || 0;
    const taken = data.taken || 0;
    const resolved = data.resolved || 0;
    const overrides = data.overrides || 0;
    const pct = (redirects > 0) ? (data.conversion_pct || 0).toFixed(1) + '%' : '—';
    const sub = (redirects > 0)
      ? taken + ' of ' + redirects + ' redirects taken within 3 calls'
      : 'No PreToolUse intercepts in the last 7 days';
    const cardCls = (redirects === 0) ? '' : (data.conversion_pct >= 60 ? 'green' : 'purple');
    const headline = statCard('Read/Grep → pincher (7d)', pct, 'green', sub, cardCls);
    // #629: override rate isolates "agent saw the hint and rejected"
    // from "no signal yet". Distinct from 100%-conversion because it
    // excludes redirects with no subsequent calls observed.
    const overridePct = (resolved > 0) ? (data.override_pct || 0).toFixed(1) + '%' : '—';
    const overrideSub = (resolved > 0)
      ? overrides + ' of ' + resolved + ' resolved redirects rejected'
      : 'Awaiting first resolved redirect';
    const overrideCls = (resolved === 0) ? '' : (data.override_pct < 30 ? 'green' : 'purple');
    const overrideCard = statCard('Override rate (7d)', overridePct, 'purple', overrideSub, overrideCls);
    // #629: per-tool breakdown — Read vs Grep imbalance is an early
    // signal that one tier of decision logic needs rebalancing.
    const byTool = data.by_tool || {};
    const toolNames = Object.keys(byTool).sort();
    let breakdownVal, breakdownSub;
    if (toolNames.length === 0) {
      breakdownVal = '—';
      breakdownSub = 'No tool intercepts yet';
    } else {
      breakdownVal = toolNames.length + ' tool' + (toolNames.length === 1 ? '' : 's');
      breakdownSub = toolNames.map(n => n + ': ' + (byTool[n].redirects || 0) + 'r/' + (byTool[n].taken || 0) + 't').join(' · ');
    }
    const breakdownCard = statCard('By tool (7d)', breakdownVal, 'green', breakdownSub, '');
    let body = headline + overrideCard + breakdownCard;
    if (redirects === 0) {
      body += '<div class="empty" style="grid-column:1/-1">Install the PreToolUse hook to start capturing intercepts: <code>pincher init --target=claude</code>. Once the hook fires on indexed Read/Grep calls, the conversion rate populates here within ~1 day of normal usage.</div>';
    }
    document.getElementById('hook-stats-cards').innerHTML = body;
  } catch (e) {
    document.getElementById('hook-stats-cards').innerHTML =
      '<div class="error">Failed to load hook stats: ' + esc(String(e)) + '</div>';
  }
}

// #635 v0.67: per-tool aggregate panel. Fetches /v1/tool-call-stats
// (one row per tool over a trailing 7-day window) and renders a
// compact table with call count, average tokens used, cumulative
// tokens saved, and average saved-pct. Tools without a Read/Grep
// baseline (architecture/list/schema — they record NULL in
// tokens_saved_pct) show "—" rather than 0% so the user doesn't
// misread admin shapes as "no savings."
async function loadToolBreakdown() {
  try {
    const data = await fetch('/v1/tool-call-stats?window_seconds=604800&limit=20').then(r => r.json());
    const tallies = data.tallies || [];
    const body = document.getElementById('tool-breakdown-body');
    if (tallies.length === 0) {
      body.innerHTML = '<div class="empty">No tool calls recorded in the last 7 days. Make a few search/symbol/trace calls via your MCP client and refresh.</div>';
      return;
    }
    // Sum totals across all tools for the footer summary — lets the
    // user see "I made N calls saving X tokens this week" at a glance.
    let totalCalls = 0;
    let totalSaved = 0;
    for (const t of tallies) {
      totalCalls += (t.call_count || 0);
      totalSaved += (t.sum_tokens_saved || 0);
    }
    let html = '<table class="tool-breakdown-table"><thead><tr>' +
      '<th>Tool</th>' +
      '<th class="num">Calls</th>' +
      '<th class="num">Avg tokens used</th>' +
      '<th class="num">Tokens saved</th>' +
      '<th class="num">Avg saved %</th>' +
      '</tr></thead><tbody>';
    for (const t of tallies) {
      const savedPct = (t.avg_tokens_saved_pct && t.avg_tokens_saved_pct > 0)
        ? t.avg_tokens_saved_pct.toFixed(1) + '%'
        : '—';
      const savedTotal = (t.sum_tokens_saved && t.sum_tokens_saved > 0)
        ? fmt(t.sum_tokens_saved)
        : '—';
      html += '<tr>' +
        '<td><code>' + esc(t.tool) + '</code></td>' +
        '<td class="num">' + fmt(t.call_count) + '</td>' +
        '<td class="num">' + fmt(Math.round(t.avg_tokens_used)) + '</td>' +
        '<td class="num green">' + savedTotal + '</td>' +
        '<td class="num">' + savedPct + '</td>' +
        '</tr>';
    }
    html += '</tbody>';
    html += '<tfoot><tr>' +
      '<td><strong>Total (7d)</strong></td>' +
      '<td class="num"><strong>' + fmt(totalCalls) + '</strong></td>' +
      '<td class="num">—</td>' +
      '<td class="num green"><strong>' + fmt(totalSaved) + '</strong></td>' +
      '<td class="num">—</td>' +
      '</tr></tfoot>';
    html += '</table>';
    body.innerHTML = html;
  } catch (e) {
    document.getElementById('tool-breakdown-body').innerHTML =
      '<div class="error">Failed to load tool breakdown: ' + esc(String(e)) + '</div>';
  }
}

// #635 panel 2: per-complexity-tier breakdown. Shows a horizontal
// stacked bar (lite + standard + heavy proportions) plus a small
// table with per-tier counts and savings. Read at a glance:
// "am I burning my budget on heavy synthesis tools or lite reads?"
async function loadTierBreakdown() {
  try {
    const data = await fetch('/v1/tool-tier-stats?window_seconds=604800').then(r => r.json());
    const tallies = data.tallies || [];
    const body = document.getElementById('tier-breakdown-body');
    if (tallies.length === 0) {
      body.innerHTML = '<div class="empty">No tool calls recorded in the last 7 days.</div>';
      return;
    }
    // Tier color map — keeps the bar segments and table swatches
    // visually consistent. Same accent palette as the rest of the
    // dashboard: blue for lite, purple for standard, orange for
    // heavy (matches the "warmer == more expensive" intuition).
    const tierColors = { lite: 'var(--accent)', standard: 'var(--purple)', heavy: 'var(--orange)' };
    let totalCalls = 0;
    for (const t of tallies) totalCalls += (t.call_count || 0);

    // Stacked-bar segments. Render in a fixed lite → standard → heavy
    // order regardless of result-set ordering so the visual is stable
    // across sessions. Tiers absent from the data render as 0-width.
    const tierOrder = ['lite', 'standard', 'heavy'];
    const byTier = {};
    for (const t of tallies) byTier[t.tier] = t;

    let barHTML = '<div class="tier-bar">';
    for (const tier of tierOrder) {
      const t = byTier[tier];
      if (!t || t.call_count === 0) continue;
      const pct = (t.call_count / totalCalls * 100).toFixed(1);
      const color = tierColors[tier] || 'var(--muted)';
      barHTML += '<div class="tier-bar-seg" style="flex-basis:' + pct + '%;background:' + color + '" title="' + esc(tier) + ': ' + pct + '%"></div>';
    }
    barHTML += '</div>';

    let tableHTML = '<table class="tier-breakdown-table"><thead><tr>' +
      '<th>Tier</th>' +
      '<th class="num">Calls</th>' +
      '<th class="num">Share</th>' +
      '<th class="num">Avg tokens used</th>' +
      '<th class="num">Tokens saved</th>' +
      '</tr></thead><tbody>';
    for (const tier of tierOrder) {
      const t = byTier[tier];
      if (!t) continue;
      const color = tierColors[tier] || 'var(--muted)';
      const pct = (t.call_count / totalCalls * 100).toFixed(1);
      const savedTotal = (t.sum_tokens_saved && t.sum_tokens_saved > 0)
        ? fmt(t.sum_tokens_saved)
        : '—';
      tableHTML += '<tr>' +
        '<td><span class="tier-swatch" style="background:' + color + '"></span>' + esc(tier) + '</td>' +
        '<td class="num">' + fmt(t.call_count) + '</td>' +
        '<td class="num">' + pct + '%</td>' +
        '<td class="num">' + fmt(Math.round(t.avg_tokens_used)) + '</td>' +
        '<td class="num green">' + savedTotal + '</td>' +
        '</tr>';
    }
    tableHTML += '</tbody></table>';

    body.innerHTML = barHTML + tableHTML;
  } catch (e) {
    document.getElementById('tier-breakdown-body').innerHTML =
      '<div class="error">Failed to load tier breakdown: ' + esc(String(e)) + '</div>';
  }
}

// #635 panel 3: per-tool payload-size distribution. Surfaces outliers
// — tools whose max response is many multiples of their average are
// the occasional bill-blowers. Ratio column flags ≥5× as "spike," ≥2×
// as "wide," anything else as "tight." Sorted by max_bytes DESC server-
// side so loud tools naturally appear at the top.
async function loadPayloadSize() {
  try {
    const data = await fetch('/v1/tool-payload-stats?window_seconds=604800&limit=20').then(r => r.json());
    const tallies = data.tallies || [];
    const body = document.getElementById('payload-size-body');
    if (tallies.length === 0) {
      body.innerHTML = '<div class="empty">No tool calls recorded in the last 7 days.</div>';
      return;
    }
    let html = '<table class="payload-size-table"><thead><tr>' +
      '<th>Tool</th>' +
      '<th class="num">Calls</th>' +
      '<th class="num">Min</th>' +
      '<th class="num">Avg</th>' +
      '<th class="num">Max</th>' +
      '<th class="num">Total</th>' +
      '<th>Spread</th>' +
      '</tr></thead><tbody>';
    for (const t of tallies) {
      // ratio of max:avg — outlier signal. Guard avg=0 so we don't /0.
      const ratio = (t.avg_bytes && t.avg_bytes > 0) ? (t.max_bytes / t.avg_bytes) : 0;
      let label = 'tight';
      let cls = 'payload-tight';
      if (ratio >= 5) { label = 'spike ' + ratio.toFixed(1) + '×'; cls = 'payload-spike'; }
      else if (ratio >= 2) { label = 'wide ' + ratio.toFixed(1) + '×'; cls = 'payload-wide'; }
      else if (ratio > 0) { label = 'tight ' + ratio.toFixed(1) + '×'; }
      html += '<tr>' +
        '<td><code>' + esc(t.tool) + '</code></td>' +
        '<td class="num">' + fmt(t.call_count) + '</td>' +
        '<td class="num">' + fmtBytes(t.min_bytes) + '</td>' +
        '<td class="num">' + fmtBytes(Math.round(t.avg_bytes)) + '</td>' +
        '<td class="num">' + fmtBytes(t.max_bytes) + '</td>' +
        '<td class="num">' + fmtBytes(t.sum_bytes) + '</td>' +
        '<td><span class="payload-badge ' + cls + '">' + label + '</span></td>' +
        '</tr>';
    }
    html += '</tbody></table>';
    body.innerHTML = html;
  } catch (e) {
    document.getElementById('payload-size-body').innerHTML =
      '<div class="error">Failed to load payload-size panel: ' + esc(String(e)) + '</div>';
  }
}

// #555: per-data-point lookup state. Stored on a module-level variable
// so the mousemove handler can find the right session without re-
// fetching. Cleared by loadSparkline on each refresh.
let _sparklineData = null;

async function loadSparkline() {
  try {
    const data = await fetch('/v1/sessions').then(r=>r.json());
    const sessions = (data.sessions||[]).slice().reverse();
    const svg = document.getElementById('sparkline-svg');
    const legend = document.getElementById('sparkline-legend');
    if (!sessions.length) { svg.innerHTML = '<text x="400" y="45" text-anchor="middle" fill="#8b949e" font-size="12">No sessions yet</text>'; _sparklineData = null; return; }
    const vals = sessions.map(s => s.tokens_saved||0);
    const maxVal = Math.max(...vals, 1);
    const W=800, H=80, pad=4;
    const xStep = vals.length < 2 ? W : (W-pad*2)/(vals.length-1);
    const pts = vals.map((v,i) => (pad+i*xStep).toFixed(1)+','+(H-pad-((v/maxVal)*(H-pad*2))).toFixed(1)).join(' ');
    const f = pad.toFixed(1)+','+H, l = (pad+(vals.length-1)*xStep).toFixed(1)+','+H;
    const lx = pad+(vals.length-1)*xStep, ly = H-pad-((vals[vals.length-1]/maxVal)*(H-pad*2));
    svg.innerHTML = '<defs><linearGradient id="sg" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#58a6ff" stop-opacity=".3"/><stop offset="100%" stop-color="#58a6ff" stop-opacity="0"/></linearGradient></defs>'+
      '<polygon points="'+f+' '+pts+' '+l+'" fill="url(#sg)"/>'+
      '<polyline points="'+pts+'" fill="none" stroke="#58a6ff" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>'+
      '<circle cx="'+lx.toFixed(1)+'" cy="'+ly.toFixed(1)+'" r="3" fill="#58a6ff"/>';
    const total = vals.reduce((a,b)=>a+b,0);
    legend.textContent = fmt(total)+' total \u00b7 '+sessions.length+' sessions \u00b7 peak '+fmt(maxVal)+'/session';
    // #555: cache the per-point coordinate\u2192session lookup for the
    // tooltip mousemove handler. Recomputed each load so a refresh
    // gets the fresh window.
    _sparklineData = { sessions, W, H, pad, xStep };
    _attachSparklineTooltip(svg);
  } catch(e) {
    document.getElementById('sparkline-svg').innerHTML = '<text x="400" y="45" text-anchor="middle" fill="#8b949e" font-size="12">Could not load history</text>';
    _sparklineData = null;
  }
}

// #555: tooltip wiring. Idempotent \u2014 listeners are bound on the SVG
// element which gets recycled on each loadSparkline call (innerHTML
// replacement on the same node), so re-attaching every load doesn't
// leak listeners. The tooltip element is a single <div> appended to
// the sparkline-card.
function _attachSparklineTooltip(svg) {
  const card = svg.closest('.sparkline-card');
  if (!card) return;
  let tip = card.querySelector('.sparkline-tip');
  if (!tip) {
    tip = document.createElement('div');
    tip.className = 'sparkline-tip';
    card.appendChild(tip);
  }
  // Avoid double-binding by replacing the old onmousemove handler.
  svg.onmousemove = (ev) => {
    if (!_sparklineData) return;
    const rect = svg.getBoundingClientRect();
    const ratio = (ev.clientX - rect.left) / rect.width;
    const xSvg = ratio * _sparklineData.W;
    const idx = Math.round((xSvg - _sparklineData.pad) / _sparklineData.xStep);
    const clamped = Math.max(0, Math.min(_sparklineData.sessions.length - 1, idx));
    const s = _sparklineData.sessions[clamped];
    if (!s) return;
    tip.innerHTML = '<b>'+esc(timeAgo(s.started_at))+'</b><br>'+
      fmt(s.tokens_saved||0)+' tokens saved \u00b7 '+(s.calls||0)+' calls';
    tip.style.display = 'block';
    // Position right of cursor; flip left if it would clip viewport.
    const tipW = tip.offsetWidth;
    let x = ev.clientX - rect.left + 12;
    if (x + tipW > rect.width) x = ev.clientX - rect.left - tipW - 12;
    tip.style.left = x + 'px';
    tip.style.top = (ev.clientY - rect.top - 30) + 'px';
  };
  svg.onmouseleave = () => { tip.style.display = 'none'; };
  // Touch support \u2014 tap shows tooltip near the touch point; tap
  // outside hides via the body listener.
  svg.ontouchstart = (ev) => {
    if (!ev.touches || !ev.touches[0]) return;
    svg.onmousemove({ clientX: ev.touches[0].clientX, clientY: ev.touches[0].clientY });
  };
}

// ── Projects ───────────────────────────────────────────────────────────────
let _allProjects = [];

async function loadProjects() {
  try {
    const r = await tabFetch('projects', '/v1/projects');
    if (!r.ok) {
      setTabError('projects-grid', 'Failed to load projects: '+(await extractErrMsg(r)), 'loadProjects');
      return;
    }
    const data = await r.json();
    _allProjects = data.projects || [];
    renderProjects();
  } catch(e) {
    if (e.name === 'AbortError') return; // #539: superseded by newer call
    setTabError('projects-grid', 'Failed to load projects: '+e.message, 'loadProjects');
  }
}

function renderProjects() {
  const grid = document.getElementById('projects-grid');
  const countEl = document.getElementById('proj-count');
  const filterInput = document.getElementById('proj-filter');
  const hideEmptyInput = document.getElementById('proj-hide-empty');
  const cleanupBtn = document.getElementById('proj-cleanup-btn');

  const filter = (filterInput ? filterInput.value : '').toLowerCase().trim();
  const hideEmpty = hideEmptyInput ? hideEmptyInput.checked : false;

  if (!_allProjects.length) {
    grid.innerHTML = emptyStateCTA('projects'); // #540
    if (countEl) countEl.textContent = '';
    if (cleanupBtn) cleanupBtn.disabled = true;
    return;
  }

  const emptyCount = _allProjects.filter(p => (p.symbol_count||p.SymCount||p.sym_count||0)===0 && (p.EdgeCount||p.edge_count||0)===0).length;
  if (cleanupBtn) {
    cleanupBtn.disabled = emptyCount === 0;
    cleanupBtn.textContent = emptyCount > 0 ? 'Remove '+emptyCount+' empty' : 'Remove all empty';
  }

  const shown = _allProjects.filter(p => {
    const name=(p.Name||p.name||'').toLowerCase();
    const path=(p.Path||p.path||'').toLowerCase();
    const syms=p.symbol_count||p.SymCount||p.sym_count||0, edges=p.EdgeCount||p.edge_count||0;
    if (hideEmpty && syms===0 && edges===0) return false;
    if (filter && !name.includes(filter) && !path.includes(filter)) return false;
    return true;
  });

  if (countEl) countEl.textContent = shown.length+' / '+_allProjects.length+' shown';

  if (!shown.length) {
    grid.innerHTML = '<div class="empty">No projects match the current filter.</div>';
    return;
  }

  grid.innerHTML = shown.map(p => {
    const id=p.ID||p.id||'', name=p.Name||p.name||'—', path=p.Path||p.path||'';
    const syms=p.symbol_count||p.SymCount||p.sym_count||0, edges=p.EdgeCount||p.edge_count||0, files=p.FileCount||p.file_count||0;
    const ts=p.IndexedAt||p.indexed_at||'';
    const isEmpty=syms===0&&edges===0;
      const ageHours=ts?(Date.now()-new Date(ts))/3600000:0;
      const isStale=!isEmpty&&ageHours>STALE_HOURS;
      const cardCls=isEmpty?' invalid':isStale?' stale':'';
      const pillCls=isEmpty?' warn':isStale?' stale-pill':'';
      const statusMsg=isEmpty?' \u26a0 no data \u2014 may be a ghost project':isStale?' \u26a0 index is '+Math.floor(ageHours)+'h old \u2014 consider re-indexing':'';
      return '<div class="proj-card'+cardCls+'" id="pcard-'+esc(id)+'">'+
        '<div class="proj-header"><div class="proj-name">'+esc(name)+'</div>'+
        '<div class="proj-actions">'+
        // SECURITY: data-action attributes + global click delegation in init.
        // JSON.stringify into an HTML attribute would break on bare ", so we
        // pass a JSON ARRAY through esc() \u2014 the array is parsed by the
        // delegation handler at click time. No inline JS, so script-src
        // 'self' (without 'unsafe-inline') applies.
        '<button class="proj-btn" data-action="openDetail" data-args="'+esc(JSON.stringify([id, name]))+'">&#x2699; Details</button>'+
        '<button class="proj-btn" data-action="reindex" data-args="'+esc(JSON.stringify([id]))+'">\u27f3 Re-index</button>'+
        '<button class="proj-btn danger" data-action="deleteProject" data-args="'+esc(JSON.stringify([id, name]))+'">\u2715 Remove</button>'+
        '</div></div>'+
        '<div class="proj-path'+(isEmpty||isStale?' missing':'')+'" title="'+esc(path)+'">'+esc(path)+esc(statusMsg)+'</div>'+
        '<div class="proj-stats">'+
        '<div class="proj-stat"><div class="proj-stat-val">'+fmt(files)+'</div><div class="proj-stat-label">Files</div></div>'+
        '<div class="proj-stat"><div class="proj-stat-val" style="color:var(--purple)">'+fmt(syms)+'</div><div class="proj-stat-label">Symbols</div></div>'+
        '<div class="proj-stat"><div class="proj-stat-val" style="color:var(--green)">'+fmt(edges)+'</div><div class="proj-stat-label">Edges</div></div>'+
        '</div>'+
        '<div class="progress-wrap" id="prog-'+esc(id)+'"><div class="progress-bar" id="progbar-'+esc(id)+'" style="width:0%"></div></div>'+
        '<div class="progress-label" id="proglabel-'+esc(id)+'"></div>'+
        (ts?'<div style="margin-top:10px" title="'+esc(new Date(ts).toLocaleString())+'"><span class="pill'+pillCls+'">indexed '+timeAgo(ts)+'</span></div>':'')+
      '</div>';
  }).join('');
}

async function cleanupEmpty() {
  const emptyCount = _allProjects.filter(p => (p.symbol_count||p.SymCount||p.sym_count||0)===0 && (p.EdgeCount||p.edge_count||0)===0).length;
  if (!emptyCount) { showToast('No empty projects to remove.'); return; }
  const ok = await showConfirmDialog(
    'Remove '+emptyCount+' empty project'+(emptyCount>1?'s':'')+'?',
    'Only projects with zero symbols and zero edges are affected. Source files are NOT deleted.',
    {destroyText: 'Remove', destructive: true});
  if (!ok) return;
  try {
    const r = await fetch('/v1/projects/empty', {method:'DELETE'});
    if (!r.ok) throw new Error(await r.text());
    const d = await r.json();
    showToast('Removed '+(d.deleted||0)+' empty project'+((d.deleted||0)!==1?'s':'')+'.');
    loadProjects();
    populateSearchProjects();
  } catch(e) { showToast('Cleanup failed: '+e.message, false); }
}

async function reindex(id, btn) {
  btn.disabled=true; btn.textContent='…';
  const card=btn.closest('.proj-card');
  const progWrap=document.getElementById('prog-'+id);
  const progBar=document.getElementById('progbar-'+id);
  const progLabel=document.getElementById('proglabel-'+id);
  if(card) card.classList.add('indexing');
  if(progWrap){ progWrap.classList.add('active'); }
  if(progLabel){ progLabel.classList.add('active'); progLabel.textContent='Starting…'; }

  // Poll progress every 600ms while indexing
  let pct=0, ticker=setInterval(()=>{
    pct=Math.min(pct+Math.random()*8+2, 92);
    if(progBar) progBar.style.width=pct.toFixed(0)+'%';
    fetch('/v1/index-progress',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({project:id})})
      .then(r=>r.json()).then(d=>{
        const p=d.result||d;
        if(p.files_done!=null&&p.files_total>0){
          pct=Math.round(p.files_done/p.files_total*100);
          if(progBar) progBar.style.width=pct+'%';
          if(progLabel) progLabel.textContent=p.files_done+' / '+p.files_total+' files';
        }
      }).catch(()=>{});
  }, 600);

  try {
    const d = await fetch('/v1/index',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:id})}).then(r=>r.json());
    clearInterval(ticker);
    if(progBar) progBar.style.width='100%';
    if(progLabel) progLabel.textContent='Done';
    const res=d.result||d;
    showToast('Re-indexed: '+(res.symbols||0)+' symbols, '+(res.edges||0)+' edges');
    setTimeout(()=>{ if(progWrap) progWrap.classList.remove('active'); if(progLabel) progLabel.classList.remove('active'); }, 1500);
    loadProjects();
  } catch(e) {
    clearInterval(ticker);
    showToast('Re-index failed: '+e.message, false);
    if(card) card.classList.remove('indexing');
    if(progWrap) progWrap.classList.remove('active');
    if(progLabel) progLabel.classList.remove('active');
    btn.disabled=false; btn.textContent='\u27f3';
  }
}

async function deleteProject(id, name) {
  const ok = await showConfirmDialog(
    'Remove project "'+name+'"?',
    'This deletes all symbols, edges, and graph data. Source files are NOT deleted.',
    {destroyText: 'Remove', destructive: true});
  if (!ok) return;
  try {
    const r=await fetch('/v1/projects',{method:'DELETE',headers:{'Content-Type':'application/json'},body:JSON.stringify({id})});
    if(!r.ok) throw new Error(await r.text());
    showToast('Project "'+name+'" removed.');
    loadProjects();
  } catch(e) { showToast('Delete failed: '+e.message, false); }
}

// ── Search ─────────────────────────────────────────────────────────────────
async function populateSearchProjects() {
  try {
    const data = await fetch('/v1/projects').then(r=>r.json());
    const sel = document.getElementById('search-proj');
    (data.projects||[]).forEach(p => {
      const o=document.createElement('option');
      o.value=p.ID||p.id||''; o.textContent=p.Name||p.name||o.value;
      sel.appendChild(o);
    });
  } catch(e) {}
}

// #547: debounce wrapper. Wraps a function so rapid invocations
// collapse to a single trailing call after the wait period of quiet.
// Used on the search input so typing "supervisor" sends 1 fetch, not 10.
function debounce(fn, wait) {
  let t = null;
  return function(...args) {
    if (t) clearTimeout(t);
    t = setTimeout(() => { t = null; fn.apply(this, args); }, wait);
  };
}

// #548: highlight match terms in a snippet. Splits the query into
// tokens (FTS5-style — whitespace, then strip leading/trailing
// non-word chars), escapes the snippet for HTML, then wraps each
// case-insensitive match in <mark>. Pure-string substitution after
// escape — never touches innerHTML on raw user input, so no XSS
// surface beyond what esc() already gates.
function highlightSnippet(snippet, query) {
  const escaped = esc(snippet || '');
  if (!query) return escaped;
  // Drop FTS5 operators that aren't part of the literal text the user
  // wants to see highlighted (quotes, asterisks, OR/AND, parens).
  const cleaned = String(query).replace(/["*()]|\bOR\b|\bAND\b/g, ' ');
  const tokens = cleaned.split(/\s+/).filter(t => t.length >= 2);
  if (!tokens.length) return escaped;
  // Sort tokens longest-first so a substring match doesn't shadow a
  // longer one (e.g. "func" before "function").
  tokens.sort((a, b) => b.length - a.length);
  // Build a single regex from all tokens with alternation. Escape
  // regex special chars in each token so a typo'd .+ doesn't blow
  // up the regex compile.
  const re = new RegExp('(' + tokens.map(t =>
    t.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  ).join('|') + ')', 'gi');
  return escaped.replace(re, '<mark>$1</mark>');
}

async function doSearch() {
  const q=document.getElementById('search-q').value.trim();
  if(!q){showToast('Enter a search query',false);return;}
  const kind=document.getElementById('search-kind').value;
  const proj=document.getElementById('search-proj').value;
  const out=document.getElementById('search-results');
  out.innerHTML='<div class="loading">Searching…</div>';
  try {
    const body={query:q};
    if(kind) body.kind=kind;
    if(proj) body.project=proj;
    const r=await tabFetch('search','/v1/search',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
    if(!r.ok){setTabError('search-results','Search failed: '+(await extractErrMsg(r)),'doSearch');return;}
    const data=await r.json();
    const results=(data.result||data).results||[];
    const total=(data.result||data).total;
    if(!results.length){out.innerHTML='<div class="empty">No results for "'+esc(q)+'"</div>';return;}
    // #532: surface total + has_more in a header row.
    let header='';
    if(typeof total==='number' && total>results.length){
      header='<div class="result-count">Showing '+results.length+' of '+total+(((data.result||data).has_more)?'+':'')+'</div>';
    }
    out.innerHTML=header+results.map(r=>
      '<div class="result-card">'+
      '<div class="result-header">'+
        '<div class="result-name">'+highlightSnippet(r.name||'', q)+'</div>'+
        // SECURITY: esc() around JSON.stringify — see project-card buttons.
        (r.id?'<button class="copy-id-btn" title="Copy symbol ID" data-action="copyID" data-args="'+esc(JSON.stringify([r.id]))+'">Copy ID</button>':'')+
      '</div>'+
      '<div class="result-meta">'+
        '<span class="pill">'+esc(r.kind||'')+'</span> &nbsp;'+
        esc(r.file_path||'')+(r.start_line?' :'+r.start_line:'')+
        (r.language?' &nbsp;<span class="pill">'+esc(r.language)+'</span>':'')+
      '</div>'+
      // #548: highlight query terms in snippet/signature.
      (r.snippet?'<div class="result-snippet">'+highlightSnippet(r.snippet, q)+'</div>':
        r.signature?'<div class="result-snippet">'+highlightSnippet(r.signature, q)+'</div>':'')+
      '</div>'
    ).join('');
  } catch(e) {
    if(e.name==='AbortError')return;
    setTabError('search-results','Search failed: '+e.message,'doSearch');
  }
}

// #547: debounced search-as-you-type. Bound below in init via
// addEventListener('input', debouncedSearch). Pre-fix Enter was the
// only way to trigger search; debounce makes type-to-search viable
// without spamming the server.
const debouncedSearch = debounce(() => {
  // Skip empty / very-short queries — they BM25-rank to noise and
  // wasting a request to render "no results" trains the user that
  // type-as-you-go is broken.
  const q = document.getElementById('search-q');
  if (q && q.value.trim().length >= 2) doSearch();
}, 200);

// ── ADRs ───────────────────────────────────────────────────────────────────
const ADR_LAST_PROJECT = 'pincher_adr_last_project';

async function loadADRProjects() {
  try {
    // Kick off stats + projects in parallel — stats surfaces the live
    // session project so the dropdown can default to "where you're
    // actually working" without a second roundtrip.
    const [projR, statsR] = await Promise.allSettled([
      fetch('/v1/projects').then(r=>r.json()),
      fetch('/v1/stats').then(r=>r.json()),
    ]);
    const sel=document.getElementById('adr-proj');
    const cur=sel.value;
    while(sel.options.length>1) sel.remove(1);
    const projects = projR.status === 'fulfilled' ? (projR.value.projects||[]) : [];
    projects.forEach(p=>{
      const o=document.createElement('option');
      o.value=p.ID||p.id||''; o.textContent=p.Name||p.name||o.value;
      sel.appendChild(o);
    });
    // Priority: 1) current UI value (user just picked), 2) last-used project
    // from localStorage, 3) session project from /v1/stats, 4) first project
    // in the list. Keeps users from re-picking every visit.
    const sessionProject = statsR.status === 'fulfilled' ? statsR.value.session_project : '';
    const remembered = localStorage.getItem(ADR_LAST_PROJECT) || '';
    const valid = id => id && projects.some(p => (p.ID||p.id) === id);
    if (cur && valid(cur)) {
      sel.value = cur;
    } else if (valid(remembered)) {
      sel.value = remembered;
    } else if (valid(sessionProject)) {
      sel.value = sessionProject;
    } else if (projects.length) {
      sel.value = projects[0].ID || projects[0].id || '';
    }
    if(sel.value) loadADRs();
  } catch(e) {}
}

function onADRProjectChange() {
  const v = document.getElementById('adr-proj').value;
  if (v) localStorage.setItem(ADR_LAST_PROJECT, v);
  loadADRs();
}

async function loadADRs() {
  const proj=document.getElementById('adr-proj').value;
  const list=document.getElementById('adr-list');
  if(!proj){list.innerHTML='<div class="empty">Select a project to view its ADRs.</div>';return;}
  list.innerHTML=skeletonRows(4, 'card'); // #541
  try {
    const r=await tabFetch('adrs','/v1/adr',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'list',project:proj})});
    if(!r.ok){setTabError('adr-list','Failed to load ADRs: '+(await extractErrMsg(r)),'loadADRs');return;}
    const data=await r.json();
    const entries=(data.result||data).entries||[];
    if(!entries.length){list.innerHTML='<div class="empty">No ADR entries yet. Add the first one above.</div>';return;}
    list.innerHTML=entries.map(e=>
      '<div class="adr-row">'+
      '<div class="adr-key">'+esc(e.key||'')+'</div>'+
      // #553: pre-wrap so multi-line values + code snippets render
      // with their original line breaks. Still text-only — no
      // markdown parser, no innerHTML on raw values.
      '<pre class="adr-val">'+esc(e.value||'')+'</pre>'+
      // SECURITY: see openDetail/reindex/deleteProject — esc() around
      // JSON.stringify keeps the value safe inside an HTML attribute.
      '<button class="adr-del" title="Delete" data-action="deleteADR" data-args="'+esc(JSON.stringify([e.key||'']))+'">&#x2715;</button>'+
      '</div>'
    ).join('');
  } catch(e) {
    if(e.name==='AbortError')return; // #539: superseded by newer call
    setTabError('adr-list','Failed to load ADRs: '+e.message,'loadADRs');
  }
}

function toggleADRForm() {
  const f=document.getElementById('adr-form');
  f.classList.toggle('open');
  if(f.classList.contains('open')) {
    document.getElementById('adr-key').focus();
    // #534: rebind the value-counter listener every open. Idempotent —
    // listeners on the element get reattached on the same node, and
    // the previous one was already removed when the textarea cleared.
    const v=document.getElementById('adr-val');
    const c=document.getElementById('adr-val-counter');
    if(v && c) {
      const update=()=>{
        const len=v.value.length, max=16384;
        c.textContent=len+' / '+max;
        c.classList.toggle('warn', len>max*0.85 && len<=max);
        c.classList.toggle('over', len>max);
      };
      v.removeEventListener('input', v._counterUpdate);
      v._counterUpdate=update;
      v.addEventListener('input', update);
      update();
    }
  } else {
    document.getElementById('adr-key').value='';
    document.getElementById('adr-val').value='';
  }
}

async function saveADR() {
  const proj=document.getElementById('adr-proj').value;
  const key=document.getElementById('adr-key').value.trim();
  const val=document.getElementById('adr-val').value.trim();
  if(!proj){showToast('Select a project first',false);return;}
  if(!key||!val){showToast('Key and value are required',false);return;}
  // #534: client-side preflight on the same bounds the backend enforces.
  // Saves the round-trip when the user pasted a transcript by mistake.
  if(key.length>256){showToast('Key too long ('+key.length+' / 256)',false);return;}
  if(val.length>16384){showToast('Value too long ('+val.length+' / 16384)',false);return;}
  try {
    const r=await fetch('/v1/adr',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'set',project:proj,key,value:val})});
    if(!r.ok) {
      // v0.25 error envelope: {error: {code, message, details?}}.
      const body=await r.json().catch(()=>({}));
      const msg=(body.error && body.error.message) || ('HTTP '+r.status);
      showToast('Save failed: '+msg, false);
      return;
    }
    showToast('ADR saved.');
    toggleADRForm();
    loadADRs();
  } catch(e) { showToast('Save failed: '+e.message, false); }
}

async function deleteADR(key) {
  const ok = await showConfirmDialog(
    'Delete ADR entry "'+key+'"?',
    'This is permanent — the value is removed from the project.',
    {destroyText: 'Delete', destructive: true});
  if (!ok) return;
  const proj=document.getElementById('adr-proj').value;
  try {
    await fetch('/v1/adr',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'delete',project:proj,key})});
    showToast('Entry "'+key+'" deleted.');
    loadADRs();
  } catch(e) { showToast('Delete failed: '+e.message, false); }
}

// ── Sessions ───────────────────────────────────────────────────────────────
async function loadSessions() {
  const wrap=document.getElementById('sessions-table-wrap');
  wrap.innerHTML=skeletonRows(8, 'line'); // #541
  try {
    const r=await tabFetch('sessions','/v1/sessions');
    if(!r.ok){setTabError('sessions-table-wrap','Failed to load sessions: '+(await extractErrMsg(r)),'loadSessions');return;}
    const data=await r.json();
    const sessions=data.sessions||[];
    if(!sessions.length){wrap.innerHTML=emptyStateCTA('sessions');return;} // #540
    // Running totals for a footer row so users see the cumulative number
    // without reaching for a spreadsheet. No cost column — we don't know
    // the user's model or pricing (#476 SAVINGS_HONESTY).
    const totalCalls=sessions.reduce((a,s)=>a+(s.calls||0),0);
    const totalSaved=sessions.reduce((a,s)=>a+(s.tokens_saved||0),0);
    const totalUsed=sessions.reduce((a,s)=>a+(s.tokens_used||0),0);
    wrap.innerHTML='<table class="sessions-table"><thead><tr>'+
      '<th>Started</th><th>Last Seen</th><th>Calls</th><th>Tokens Saved</th><th>Tokens Used</th>'+
      '</tr></thead><tbody>'+
      sessions.map(s=>
        '<tr>'+
        '<td title="'+esc(s.session_id)+'">'+timeAgo(s.started_at)+'</td>'+
        '<td>'+timeAgo(s.last_seen)+'</td>'+
        '<td>'+fmt(s.calls||0)+'</td>'+
        '<td style="color:var(--green)">'+fmt(s.tokens_saved||0)+'</td>'+
        '<td>'+fmt(s.tokens_used||0)+'</td>'+
        '</tr>'
      ).join('')+
      '</tbody><tfoot><tr class="sessions-total">'+
      '<td colspan="2">Total across '+sessions.length+' session'+(sessions.length!==1?'s':'')+'</td>'+
      '<td>'+fmt(totalCalls)+'</td>'+
      '<td style="color:var(--green)">'+fmt(totalSaved)+'</td>'+
      '<td>'+fmt(totalUsed)+'</td>'+
      '</tr></tfoot></table>';
  } catch(e) {
    if(e.name==='AbortError')return; // #539: superseded by newer call
    setTabError('sessions-table-wrap','Failed to load sessions: '+e.message,'loadSessions');
  }
}

// ── Cost projection ────────────────────────────────────────────────────────
// #544: minimum data window before showing the projection banner.
// Below this the math extrapolates noise \u2014 typical bug: 2 sessions
// over <1 day produced "1M tokens/mo" or NaN. 7 days is the floor
// where the rate stops being mostly-randomness.
const PROJECTION_MIN_DAYS = 7;
// #544: cap the displayed monthly rate. Above this the number is
// either wrong (math bug) or so big it strains credibility \u2014 better
// to show the cap with a "~" indicator than a misleading 999B.
const PROJECTION_MAX_TOKENS_PER_MONTH = 100_000_000;

// computeProjection extracts the math from loadProjection so it can
// be unit-tested with synthetic session arrays. Returns null when
// the data isn't sufficient to project (insufficient days, zero
// savings, NaN/Infinity guard). Pure function \u2014 no DOM access.
function computeProjection(sessions) {
  if (!Array.isArray(sessions) || sessions.length < 2) return null;
  const totalSaved = sessions.reduce((a, s) => a + (s.tokens_saved || 0), 0);
  const totalCalls = sessions.reduce((a, s) => a + (s.calls || 0), 0);
  if (totalSaved === 0) return null;
  const dates = sessions.map(s => new Date(s.started_at)).filter(d => !isNaN(d));
  if (!dates.length) return null;
  const oldest = Math.min(...dates.map(d => d.getTime()));
  const newest = Math.max(...dates.map(d => d.getTime()));
  const days = (newest - oldest) / 86400000;
  if (!isFinite(days) || days < PROJECTION_MIN_DAYS) {
    return { needsMoreData: true, days: Math.max(0, days), totalSaved, totalCalls };
  }
  let dailyTokens = totalSaved / days;
  let monthlyTokens = dailyTokens * 30;
  // NaN/Infinity guard: cap or null.
  if (!isFinite(dailyTokens) || !isFinite(monthlyTokens)) return null;
  if (monthlyTokens > PROJECTION_MAX_TOKENS_PER_MONTH) {
    monthlyTokens = PROJECTION_MAX_TOKENS_PER_MONTH;
    dailyTokens = monthlyTokens / 30;
  }
  return {
    needsMoreData: false,
    days: Math.round(days),
    sessionCount: sessions.length,
    dailyTokens: Math.round(dailyTokens),
    monthlyTokens: Math.round(monthlyTokens),
    totalSaved,
    totalCalls,
    capped: monthlyTokens >= PROJECTION_MAX_TOKENS_PER_MONTH,
  };
}

async function loadProjection() {
  const el = document.getElementById('projection-banner');
  if (!el) return;
  try {
    const data = await fetch('/v1/sessions').then(r => r.json());
    const p = computeProjection(data.sessions || []);
    if (!p) { el.innerHTML = ''; return; }
    if (p.needsMoreData) {
      el.innerHTML = '<div class="proj-banner proj-banner-pending">'+
        '<div class="proj-banner-icon">\u23f3</div>'+
        '<div class="proj-banner-body">'+
          '<div class="proj-banner-title">Projection unavailable</div>'+
          '<div class="proj-banner-sub">Need '+PROJECTION_MIN_DAYS+'+ days of session history to project (currently '+
            (p.days < 1 ? '<1 day' : Math.round(p.days)+' days')+'). Keep using pincher \u2014 the rate stabilizes after a week.</div>'+
        '</div></div>';
      return;
    }
    el.innerHTML = '<div class="proj-banner">'+
      '<div class="proj-banner-icon">\ud83d\udcc8</div>'+
      '<div class="proj-banner-body">'+
        '<div class="proj-banner-title">Projected Savings Rate</div>'+
        '<div class="proj-banner-rate">'+(p.capped?'\u2265':'~')+fmt(p.monthlyTokens)+' tokens/mo</div>'+
        '<div class="proj-banner-sub">Based on '+p.sessionCount+' sessions over '+p.days+' days \u00b7 ~'+fmt(p.dailyTokens)+' tokens/day'+(p.capped?' \u00b7 capped':'')+'</div>'+
      '</div>'+
      '<div class="proj-banner-pills">'+
        '<div class="proj-banner-pill">'+fmt(p.totalSaved)+' tokens saved</div>'+
        '<div class="proj-banner-pill blue">'+p.totalCalls+' tool calls</div>'+
      '</div>'+
    '</div>';
  } catch(e) { document.getElementById('projection-banner').innerHTML = ''; }
}

// ── Project deep-dive ──────────────────────────────────────────────────────
let _detailOpenId=null;
async function openDetail(id, name) {
  const panel=document.getElementById('proj-detail-panel');
  const body=document.getElementById('proj-detail-body');
  const title=document.getElementById('proj-detail-name');
  // Toggle off if same project clicked again
  if(_detailOpenId===id && panel.classList.contains('open')){ closeDetail(); return; }
  _detailOpenId=id;
  panel.classList.add('open');
  // #554: encode project ID into hash so the URL is shareable.
  // history.replaceState avoids polluting browser history with each
  // detail open/close — refresh + back behave naturally.
  history.replaceState(null, '', '#projects/'+encodeURIComponent(id));
  title.textContent=name+' — Architecture';
  body.innerHTML='<div class="loading">Loading architecture…</div>';
  panel.scrollIntoView({behavior:'smooth',block:'nearest'});
  try {
    const data=await fetch('/v1/architecture',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({project:id})}).then(r=>r.json());
    const d=data.result||data;
    const langs=d.languages||[];
    const eps=d.entry_points||[];
    const hotspots=d.hotspot_functions||d.hotspots||[];
    // #533: render entry-points + hotspots with a "Show all" toggle
    // when the list exceeds the default cap (8 / 10). Pre-fix the
    // truncation was silent \u2014 users had no idea more existed.
    const renderTruncatable = (label, items, defaultCap, expandedCap, toggleId, fmtItem) => {
      if (!items.length) return '';
      const expanded = _detailExpanded[toggleId] === true;
      const cap = expanded ? expandedCap : defaultCap;
      const shown = items.slice(0, cap);
      const total = items.length;
      const showToggle = total > defaultCap;
      const headerCount = showToggle ? ' <span class="detail-section-count">'+shown.length+' of '+total+'</span>' : '';
      const toggle = showToggle ? '<button class="show-all-btn" data-action="toggleDetailExpanded" data-args="'+esc(JSON.stringify([toggleId]))+'">'+(expanded?'Show fewer':'Show all '+total)+'</button>' : '';
      return '<div class="detail-section"><div class="detail-section-label">'+label+headerCount+'</div>'+
        '<ul class="'+(label==='Hotspot Functions'?'hotspot-list':'ep-list')+'">'+
        shown.map(fmtItem).join('')+
        '</ul>'+toggle+'</div>';
    };
    body.innerHTML=
      (langs.length?'<div class="detail-section"><div class="detail-section-label">Languages</div>'+
        '<div class="lang-bar">'+langs.map(l=>'<span class="lang-chip">'+esc(l.language||l)+(l.file_count?' \u00b7 '+l.file_count+' files':'')+'</span>').join('')+'</div></div>':'')+
      renderTruncatable('Entry Points', eps, 8, 50, 'eps_'+id,
        e=>'<li>'+esc(e.name||e)+(e.file?' <span style="color:var(--muted)">'+esc(e.file)+'</span>':'')+'</li>') +
      renderTruncatable('Hotspot Functions', hotspots, 10, 50, 'hotspots_'+id,
        h=>'<li>'+esc(h.name||h)+'<span class="hotspot-calls">'+(h.call_count!=null?h.call_count+' calls':'')+'</span></li>') +
      (!langs.length&&!eps.length&&!hotspots.length?'<div class="empty">No architecture data available. Re-index the project first.</div>':'');
  } catch(e) { body.innerHTML='<div class="error">Failed to load architecture: '+esc(e.message)+'</div>'; }
}

// #533: per-detail-section expanded state. Keyed by toggle ID
// (e.g. "eps_<projectID>" or "hotspots_<projectID>") so multiple
// open detail panels each remember their own state. Cleared on
// closeDetail.
const _detailExpanded = {};

function toggleDetailExpanded(toggleId) {
  _detailExpanded[toggleId] = !_detailExpanded[toggleId];
  // Re-render the same detail panel by re-invoking openDetail with
  // the cached id+name. The toggleId always starts with "eps_" or
  // "hotspots_" \u2014 the suffix is the project id.
  const id = toggleId.replace(/^(eps_|hotspots_)/, '');
  const card = document.getElementById('pcard-'+id);
  const name = card ? card.querySelector('.proj-name') : null;
  if (id && name) {
    // openDetail toggles closed if same project clicked again \u2014 pre-empt
    // that by clearing _detailOpenId so the re-open is a fresh open.
    _detailOpenId = null;
    openDetail(id, name.textContent);
  }
}

function closeDetail() {
  _detailOpenId=null;
  document.getElementById('proj-detail-panel').classList.remove('open');
  // #554: drop the project ID from the hash so the URL reflects state.
  history.replaceState(null, '', '#projects');
}

// ── Event delegation ───────────────────────────────────────────────────────
// All interactive elements use data-action* attributes instead of inline
// onclick=/oninput= handlers. This keeps the CSP claim honest: we ship
// script-src 'self' (no 'unsafe-inline'), and inline event attributes
// would be silently blocked. Every handler runs through these four
// delegated listeners.
//
// Args are JSON-encoded in data-args. The clicked/changed element is
// appended as the LAST argument so functions like reindex(id, btnEl) and
// copyID(id, btnEl) keep working — they just receive the element via
// delegation instead of the inline 'this' reference. Functions that
// don't need the element ignore the trailing arg (JS truncates extras).
function _dispatchAction(el) {
  const action = el.getAttribute('data-action');
  const argsRaw = el.getAttribute('data-args');
  const args = argsRaw ? JSON.parse(argsRaw) : [];
  const fn = window[action];
  if (typeof fn === 'function') {
    fn.apply(null, args.concat(el));
  }
}
function _dispatchSimple(el, attr) {
  const fn = window[el.getAttribute(attr)];
  if (typeof fn === 'function') fn();
}
document.body.addEventListener('click', e => {
  const el = e.target.closest('[data-action]');
  if (el) {
    e.preventDefault();
    _dispatchAction(el);
  }
});
document.body.addEventListener('input', e => {
  const el = e.target.closest('[data-action-input]');
  if (el) _dispatchSimple(el, 'data-action-input');
});
document.body.addEventListener('change', e => {
  const el = e.target.closest('[data-action-change]');
  if (el) _dispatchSimple(el, 'data-action-change');
});
document.body.addEventListener('keydown', e => {
  if (e.key !== 'Enter') return;
  const el = e.target.closest('[data-action-enter]');
  if (el) _dispatchSimple(el, 'data-action-enter');
});

// #545 + #546: pollManager wraps setInterval with two enhancements:
// 1) tracks last-fetch time per source (#545) so the staleness
//    indicator can show "Updated Ns ago".
// 2) pauses the interval when the tab is hidden (#546). On visible
//    again, fires an immediate refresh AND restarts the interval.
// Returns nothing — the manager owns the timer and the visibility
// listener for the lifetime of the page.
const _lastRefresh = {};
const _pollers = []; // {label, fn, ms, timerId}

function pollManager(label, fn, ms) {
  const wrapped = async () => {
    try { await fn(); } catch(_) {}
    _lastRefresh[label] = Date.now();
  };
  // Initial fire — also seeds _lastRefresh so the indicator has a
  // value to render on first paint.
  wrapped();
  const entry = { label, fn: wrapped, ms, timerId: null };
  entry.timerId = setInterval(wrapped, ms);
  _pollers.push(entry);
}

document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    // #546: pause every poller. The label survives so we can resume.
    _pollers.forEach(p => {
      if (p.timerId !== null) { clearInterval(p.timerId); p.timerId = null; }
    });
  } else {
    // Resume: fire each poller once immediately for a fresh paint,
    // then restart the interval.
    _pollers.forEach(p => {
      if (p.timerId === null) {
        p.fn();
        p.timerId = setInterval(p.fn, p.ms);
      }
    });
  }
});

// #545: tick the staleness indicator every second. Single setInterval
// for all sources — re-renders all .updated-ago elements from
// _lastRefresh on each tick. Cheap; one DOM read + N short writes.
function _renderStaleness() {
  const now = Date.now();
  document.querySelectorAll('.updated-ago').forEach(el => {
    const src = el.getAttribute('data-source');
    const last = _lastRefresh[src];
    if (!last) { el.textContent = ''; return; }
    const sec = Math.round((now - last) / 1000);
    el.textContent = sec < 5 ? 'just now'
      : sec < 60 ? sec + 's ago'
      : sec < 3600 ? Math.round(sec / 60) + 'm ago'
      : Math.round(sec / 3600) + 'h ago';
  });
}
setInterval(_renderStaleness, 1000);

// ── Init ───────────────────────────────────────────────────────────────────
applyStoredTheme(); // #549: must run before first paint
updateAuthBadge();
populateSearchProjects();
// #552: configurable refresh interval (#545+#546 pollManager keeps
// the visibility-aware wrapping). applyRefreshInterval tears down
// existing _pollers and re-registers; ms === 0 leaves _pollers
// empty so the visibility-resume path does nothing either.
const REFRESH_STORAGE = 'pincher_refresh_ms';
function applyRefreshInterval(ms) {
  _pollers.forEach(p => { if (p.timerId !== null) clearInterval(p.timerId); });
  _pollers.length = 0;
  if (ms <= 0) return;
  pollManager('overview', load, ms);
  pollManager('projection', loadProjection, ms * 2);
}
function onRefreshIntervalChange() {
  const sel = document.getElementById('refresh-select');
  const ms = parseInt(sel.value, 10);
  if (isFinite(ms) && ms >= 0) {
    localStorage.setItem(REFRESH_STORAGE, String(ms));
    applyRefreshInterval(ms);
    showToast('Auto-refresh: '+(ms===0?'off':((ms>=60000?(ms/60000)+'m':(ms/1000)+'s'))));
  }
}
// Initial bind: read persisted choice (default 30s).
{
  const stored = parseInt(localStorage.getItem(REFRESH_STORAGE) || '30000', 10);
  const valid = [0, 5000, 30000, 60000, 300000].includes(stored) ? stored : 30000;
  const sel = document.getElementById('refresh-select');
  if (sel) sel.value = String(valid);
  applyRefreshInterval(valid);
}

// #554: deep links — restore tab + optional project ID from hash.
// Format: #<tab> or #<tab>/<projectID>. Replaces the v0.21 plain-tab
// hash routing without breaking the older shape (a bare "#projects"
// still works since the split returns ["projects", undefined]).
function _parseHash() {
  const raw = (location.hash || '').replace('#', '');
  const [tab, project] = raw.split('/');
  return { tab, project };
}
function _restoreFromHash() {
  const { tab, project } = _parseHash();
  if (['overview','projects','search','adrs','sessions'].includes(tab)) {
    showTab(tab);
  }
  // #554: if hash includes a project ID, open the detail panel for it
  // after the projects tab finishes loading. The card may not exist
  // yet on first paint, so retry once after a short delay.
  if (project && tab === 'projects') {
    setTimeout(() => {
      const card = document.getElementById('pcard-'+project);
      if (card) {
        const name = card.querySelector('.proj-name');
        if (name) openDetail(project, name.textContent);
      }
    }, 300);
  }
}
_restoreFromHash();
// Reflect detail-panel state into the hash so the URL is shareable.
window.addEventListener('hashchange', _restoreFromHash);

// #550: keyboard shortcuts. Don't fire while typing into an input.
//   /     → focus the search box (Search tab)
//   g s   → switch to search tab
//   g p   → switch to projects tab
//   g o   → switch to overview tab
//   g a   → switch to ADRs tab
//   g h   → switch to sessions tab (h = history)
//   Esc   → close detail panel + dismiss confirm modal
//   j/k   → next/prev project card in Projects tab
const SHORTCUT_HELP = [
  ['/', 'Focus search'],
  ['g s', 'Search tab'], ['g p', 'Projects tab'],
  ['g o', 'Overview'], ['g a', 'ADRs'], ['g h', 'Sessions'],
  ['j/k', 'Next/prev project'],
  ['Esc', 'Close panel/modal'],
];
let _kbdLeader = '';
let _kbdLeaderTimer = null;
let _kbdProjectCursor = -1;
function _isTypingTarget(t) {
  if (!t) return false;
  const tag = t.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || t.isContentEditable;
}
document.addEventListener('keydown', (ev) => {
  // Don't intercept when modal is open — Escape is its own handler.
  if (_isTypingTarget(ev.target) && ev.key !== 'Escape') return;
  if (ev.metaKey || ev.ctrlKey || ev.altKey) return;

  if (ev.key === '/') {
    ev.preventDefault();
    showTab('search');
    setTimeout(() => document.getElementById('search-q').focus(), 50);
    return;
  }
  if (ev.key === 'Escape') {
    // Close detail panel if open (modal closes itself via its own listener).
    if (document.getElementById('proj-detail-panel') &&
        document.getElementById('proj-detail-panel').classList.contains('open')) {
      closeDetail();
    }
    return;
  }
  if (_kbdLeader === 'g') {
    const map = { s: 'search', p: 'projects', o: 'overview', a: 'adrs', h: 'sessions' };
    if (map[ev.key]) { ev.preventDefault(); showTab(map[ev.key]); }
    _kbdLeader = '';
    return;
  }
  if (ev.key === 'g') {
    _kbdLeader = 'g';
    clearTimeout(_kbdLeaderTimer);
    _kbdLeaderTimer = setTimeout(() => { _kbdLeader = ''; }, 1500);
    return;
  }
  if (ev.key === 'j' || ev.key === 'k') {
    const cards = Array.from(document.querySelectorAll('#projects-grid .proj-card'));
    if (!cards.length) return;
    _kbdProjectCursor = ev.key === 'j'
      ? Math.min(_kbdProjectCursor + 1, cards.length - 1)
      : Math.max(_kbdProjectCursor - 1, 0);
    cards.forEach(c => c.classList.remove('kbd-focused'));
    const c = cards[_kbdProjectCursor];
    c.classList.add('kbd-focused');
    c.scrollIntoView({block: 'nearest'});
    ev.preventDefault();
  }
});

// #551: CSV/JSON export. Generates a Blob from the table data and
// triggers a download. No server roundtrip — uses the data already
// rendered. exportTable(elementID, filename, format) is generic
// so the same helper covers Projects + Sessions tables.
function exportTable(format, kind) {
  let rows = [];
  let header = [];
  let filename = kind+'.'+format;
  if (kind === 'projects') {
    header = ['id','name','path','file_count','symbol_count','edge_count','indexed_at'];
    rows = (_allProjects || []).map(p => [
      p.ID || p.id || '',
      p.Name || p.name || '',
      p.Path || p.path || '',
      p.FileCount || p.file_count || 0,
      p.symbol_count || p.SymCount || p.sym_count || 0,
      p.EdgeCount || p.edge_count || 0,
      p.IndexedAt || p.indexed_at || '',
    ]);
  } else if (kind === 'sessions') {
    header = ['session_id','started_at','last_seen','calls','tokens_saved','tokens_used'];
    // Re-fetch to get the live data — the sparkline cache only holds
    // a slice. Async export so we can await the fetch.
    return fetch('/v1/sessions').then(r=>r.json()).then(data => {
      const sessions = data.sessions || [];
      const rows2 = sessions.map(s => [s.session_id, s.started_at, s.last_seen, s.calls||0, s.tokens_saved||0, s.tokens_used||0]);
      _downloadExport(format, filename, header, rows2);
    });
  }
  _downloadExport(format, filename, header, rows);
}
function _downloadExport(format, filename, header, rows) {
  let body;
  if (format === 'csv') {
    const escCsv = v => {
      const s = String(v == null ? '' : v);
      if (/["\n,]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
      return s;
    };
    body = header.join(',') + '\n' + rows.map(r => r.map(escCsv).join(',')).join('\n');
  } else {
    body = JSON.stringify(rows.map(r => Object.fromEntries(header.map((h, i) => [h, r[i]]))), null, 2);
  }
  const blob = new Blob([body], { type: format === 'csv' ? 'text/csv' : 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url; a.download = filename;
  document.body.appendChild(a); a.click();
  document.body.removeChild(a);
  setTimeout(() => URL.revokeObjectURL(url), 1000);
  showToast('Exported '+rows.length+' rows to '+filename);
}
`
