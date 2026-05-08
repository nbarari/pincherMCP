package server

import "strings"

// renderDashboard returns the dashboard HTML with the reverse-proxy basepath
// (e.g. "/pincher") substituted in. Pass "" for direct deployment. The prefix
// flows into:
//   - window.PINCHER_BASEPATH (read by the fetch interceptor + auth check)
//   - footer anchor hrefs (plain HTML — can't use the interceptor)
func renderDashboard(prefix string) string {
	return strings.ReplaceAll(dashboardTemplate, "__PINCHER_BASEPATH__", prefix)
}

// dashboardTemplate is the self-contained stats dashboard served at GET /v1/dashboard.
// Fetches all data live from /v1/* endpoints; no external dependencies.
// __PINCHER_BASEPATH__ tokens are substituted at render time by renderDashboard.
const dashboardTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>pincherMCP · Dashboard</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0d1117;--surface:#161b22;--border:#30363d;
  --text:#e6edf3;--muted:#8b949e;--accent:#58a6ff;
  --green:#3fb950;--purple:#a371f7;--orange:#f0883e;--red:#f85149;
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
.sparkline-card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:20px;margin-bottom:32px}
.sparkline-card svg{width:100%;height:80px;display:block}
.sparkline-meta{display:flex;justify-content:space-between;align-items:center;margin-bottom:12px}
.sparkline-title{font-size:11px;font-weight:600;letter-spacing:1px;text-transform:uppercase;color:var(--muted)}
.sparkline-legend{font-size:11px;color:var(--muted)}

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
</style>
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
    <button class="header-btn" id="auth-btn" title="Set HTTP bearer token (required when pincher is started with --http-key)" onclick="promptForKey()">Auth</button>
  </div>
</header>

<nav class="tab-bar">
  <button class="tab-btn active" onclick="showTab('overview')">Overview</button>
  <button class="tab-btn" onclick="showTab('projects')">Projects</button>
  <button class="tab-btn" onclick="showTab('search')">Search</button>
  <button class="tab-btn" onclick="showTab('adrs')">ADRs</button>
  <button class="tab-btn" onclick="showTab('sessions')">Sessions</button>
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
</main>
</div>

<!-- PROJECTS -->
<div id="tab-projects" class="tab-pane">
<main>
  <p class="section-title">Indexed Projects</p>
  <div class="proj-toolbar">
    <input class="search-input" id="proj-filter" type="text" placeholder="Filter by name or path…" oninput="renderProjects()"/>
    <label class="toolbar-check" title="Hide projects with zero symbols and zero edges">
      <input type="checkbox" id="proj-hide-empty" onchange="renderProjects()"/> Hide empty
    </label>
    <button class="btn secondary" id="proj-cleanup-btn" onclick="cleanupEmpty()">Remove all empty</button>
    <span class="toolbar-count" id="proj-count">&nbsp;</span>
  </div>
  <div class="grid grid-2" id="projects-grid"><div class="loading">Loading…</div></div>
  <div class="detail-panel" id="proj-detail-panel">
    <div class="detail-panel-title">
      <span id="proj-detail-name">Project Details</span>
      <button class="detail-close" onclick="closeDetail()" title="Close">&#x2715;</button>
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
    <input class="search-input" id="search-q" type="text" placeholder="Search symbols… (e.g. handleSearch, auth*, &quot;token validation&quot;)" onkeydown="if(event.key==='Enter')doSearch()"/>
    <select class="search-select" id="search-kind">
      <option value="">All kinds</option>
      <option>Function</option><option>Method</option><option>Class</option>
      <option>Interface</option><option>Type</option><option>Variable</option>
    </select>
    <select class="search-select" id="search-proj"><option value="">All projects</option></select>
    <button class="search-btn" onclick="doSearch()">Search</button>
  </div>
  <div id="search-results"></div>
</main>
</div>

<!-- ADRs -->
<div id="tab-adrs" class="tab-pane">
<main>
  <p class="section-title">Architecture Decision Records</p>
  <div class="adr-toolbar">
    <select class="adr-select" id="adr-proj" onchange="onADRProjectChange()"><option value="">Select a project…</option></select>
    <button class="btn secondary" onclick="toggleADRForm()">+ Add Entry</button>
  </div>
  <div class="adr-form" id="adr-form">
    <input id="adr-key" type="text" placeholder="Key (e.g. PURPOSE, STACK, PATTERNS)"/>
    <textarea id="adr-val" placeholder="Value…"></textarea>
    <div class="adr-form-actions">
      <button class="btn" onclick="saveADR()">Save</button>
      <button class="btn secondary" onclick="toggleADRForm()">Cancel</button>
    </div>
  </div>
  <div id="adr-list"><div class="empty">Select a project to view its ADRs.</div></div>
</main>
</div>

<!-- SESSIONS -->
<div id="tab-sessions" class="tab-pane">
<main>
  <p class="section-title">Session History</p>
  <div id="sessions-table-wrap"><div class="loading">Loading…</div></div>
</main>
</div>

<div class="footer">pincherMCP · <a href="__PINCHER_BASEPATH__/v1/openapi.json" target="_blank">OpenAPI</a> · <a href="__PINCHER_BASEPATH__/v1/health" target="_blank">Health</a></div>
<div class="toast" id="toast"></div>

<script>
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

function updateAuthBadge() {
  const btn = document.getElementById('auth-btn');
  if (!btn) return;
  const has = !!localStorage.getItem(AUTH_KEY_STORAGE);
  btn.textContent = has ? 'Auth ✓' : 'Auth';
  btn.classList.toggle('authed', has);
}

// ── Utilities ──────────────────────────────────────────────────────────────
const fmt = n => n >= 1e6 ? (n/1e6).toFixed(1)+'M' : n >= 1e3 ? (n/1e3).toFixed(1)+'K' : String(n);
const fmtMs = ms => ms < 1 ? '<1ms' : ms+'ms';
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

function showToast(msg, ok=true) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.style.borderColor = ok ? 'var(--green)' : 'var(--red)';
  t.classList.add('show');
  setTimeout(() => t.classList.remove('show'), 2800);
}

// ── Tab navigation ─────────────────────────────────────────────────────────
function showTab(name) {
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
        statCard('Tokens Used', fmt(s.tokens_used||0), 'purple', 'context consumed', 'purple') +
        statCard('Cost Avoided', s.total_cost_avoided||'$0.0000', 'orange', 'estimated savings', 'orange');
      document.getElementById('alltime-cards').innerHTML = (a.calls||0) > 0 ?
        statCard('Total Calls', fmt(a.calls||0), 'blue', 'all sessions', '') +
        statCard('Total Tokens Saved', fmt(a.tokens_saved||0), 'green', 'cumulative', 'green') +
        statCard('Total Cost Avoided', a.total_cost_avoided||'$0.0000', 'orange', 'provable ROI', 'orange') +
        statCard('Tokens Used', fmt(a.tokens_used||0), 'purple', 'context consumed', 'purple') :
        '<div class="empty">No sessions recorded yet.</div>';
    }
  } else {
    document.getElementById('error-box').innerHTML = '<div class="error">Failed to load stats: '+esc(statsR.reason)+'</div>';
  }

  // Projects + sparkline load concurrently in the background — the overview
  // is already interactive once stats resolve.
  loadProjects();
  loadSparkline();
  document.getElementById('last-refresh').textContent = 'updated ' + new Date().toLocaleTimeString();
}

async function loadSparkline() {
  try {
    const data = await fetch('/v1/sessions').then(r=>r.json());
    const sessions = (data.sessions||[]).slice().reverse();
    const svg = document.getElementById('sparkline-svg');
    const legend = document.getElementById('sparkline-legend');
    if (!sessions.length) { svg.innerHTML = '<text x="400" y="45" text-anchor="middle" fill="#8b949e" font-size="12">No sessions yet</text>'; return; }
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
  } catch(e) {
    document.getElementById('sparkline-svg').innerHTML = '<text x="400" y="45" text-anchor="middle" fill="#8b949e" font-size="12">Could not load history</text>';
  }
}

// ── Projects ───────────────────────────────────────────────────────────────
let _allProjects = [];

async function loadProjects() {
  try {
    const data = await fetch('/v1/projects').then(r=>r.json());
    _allProjects = data.projects || [];
    renderProjects();
  } catch(e) {
    document.getElementById('projects-grid').innerHTML = '<div class="error">Failed to load projects: '+esc(e.message)+'</div>';
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
    grid.innerHTML = '<div class="empty">No projects indexed yet.<br/><br/>Run <code>pincher index /path/to/your/repo</code> to get started.</div>';
    if (countEl) countEl.textContent = '';
    if (cleanupBtn) cleanupBtn.disabled = true;
    return;
  }

  const emptyCount = _allProjects.filter(p => (p.SymCount||p.sym_count||0)===0 && (p.EdgeCount||p.edge_count||0)===0).length;
  if (cleanupBtn) {
    cleanupBtn.disabled = emptyCount === 0;
    cleanupBtn.textContent = emptyCount > 0 ? 'Remove '+emptyCount+' empty' : 'Remove all empty';
  }

  const shown = _allProjects.filter(p => {
    const name=(p.Name||p.name||'').toLowerCase();
    const path=(p.Path||p.path||'').toLowerCase();
    const syms=p.SymCount||p.sym_count||0, edges=p.EdgeCount||p.edge_count||0;
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
    const syms=p.SymCount||p.sym_count||0, edges=p.EdgeCount||p.edge_count||0, files=p.FileCount||p.file_count||0;
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
        // SECURITY: JSON.stringify makes the value safe as JS, but the
        // resulting string lives inside an HTML attribute \u2014 bare " inside
        // breaks out of the attribute. esc() escapes HTML special chars
        // so the attribute value stays intact while remaining valid JS
        // when the browser unescapes it for the onclick handler.
        '<button class="proj-btn" onclick="openDetail('+esc(JSON.stringify(id))+','+esc(JSON.stringify(name))+')">&#x2699; Details</button>'+
        '<button class="proj-btn" onclick="reindex('+esc(JSON.stringify(id))+',this)">\u27f3 Re-index</button>'+
        '<button class="proj-btn danger" onclick="deleteProject('+esc(JSON.stringify(id))+','+esc(JSON.stringify(name))+')">\u2715 Remove</button>'+
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
  const emptyCount = _allProjects.filter(p => (p.SymCount||p.sym_count||0)===0 && (p.EdgeCount||p.edge_count||0)===0).length;
  if (!emptyCount) { showToast('No empty projects to remove.'); return; }
  if (!confirm('Remove '+emptyCount+' empty project'+(emptyCount>1?'s':'')+' from the index?\n\nOnly projects with zero symbols and zero edges are affected.\nSource files are NOT deleted.')) return;
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
  if(!confirm('Remove project "'+name+'" from the index?\n\nThis deletes all symbols, edges, and graph data. Source files are NOT deleted.')) return;
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
    const data=await fetch('/v1/search',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(r=>r.json());
    const results=(data.result||data).results||[];
    if(!results.length){out.innerHTML='<div class="empty">No results for "'+esc(q)+'"</div>';return;}
    out.innerHTML=results.map(r=>
      '<div class="result-card">'+
      '<div class="result-header">'+
        '<div class="result-name">'+esc(r.name||'')+'</div>'+
        // SECURITY: esc() around JSON.stringify — see project-card buttons.
        (r.id?'<button class="copy-id-btn" title="Copy symbol ID" onclick="copyID('+esc(JSON.stringify(r.id))+',this)">Copy ID</button>':'')+
      '</div>'+
      '<div class="result-meta">'+
        '<span class="pill">'+esc(r.kind||'')+'</span> &nbsp;'+
        esc(r.file_path||'')+(r.start_line?' :'+r.start_line:'')+
        (r.language?' &nbsp;<span class="pill">'+esc(r.language)+'</span>':'')+
      '</div>'+
      (r.snippet?'<div class="result-snippet">'+esc(r.snippet)+'</div>':
        r.signature?'<div class="result-snippet">'+esc(r.signature)+'</div>':'')+
      '</div>'
    ).join('');
  } catch(e) { out.innerHTML='<div class="error">Search failed: '+esc(e.message)+'</div>'; }
}

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
  list.innerHTML='<div class="loading">Loading…</div>';
  try {
    const data=await fetch('/v1/adr',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'list',project:proj})}).then(r=>r.json());
    const entries=(data.result||data).entries||[];
    if(!entries.length){list.innerHTML='<div class="empty">No ADR entries yet. Add the first one above.</div>';return;}
    list.innerHTML=entries.map(e=>
      '<div class="adr-row">'+
      '<div class="adr-key">'+esc(e.key||'')+'</div>'+
      '<div class="adr-val">'+esc(e.value||'')+'</div>'+
      // SECURITY: see openDetail/reindex/deleteProject — esc() around
      // JSON.stringify keeps the value safe inside an HTML attribute.
      '<button class="adr-del" title="Delete" onclick="deleteADR('+esc(JSON.stringify(e.key||''))+')">&#x2715;</button>'+
      '</div>'
    ).join('');
  } catch(e) { list.innerHTML='<div class="error">Failed to load ADRs: '+esc(e.message)+'</div>'; }
}

function toggleADRForm() {
  const f=document.getElementById('adr-form');
  f.classList.toggle('open');
  if(f.classList.contains('open')) document.getElementById('adr-key').focus();
  else { document.getElementById('adr-key').value=''; document.getElementById('adr-val').value=''; }
}

async function saveADR() {
  const proj=document.getElementById('adr-proj').value;
  const key=document.getElementById('adr-key').value.trim();
  const val=document.getElementById('adr-val').value.trim();
  if(!proj){showToast('Select a project first',false);return;}
  if(!key||!val){showToast('Key and value are required',false);return;}
  try {
    await fetch('/v1/adr',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'set',project:proj,key,value:val})});
    showToast('ADR saved.');
    toggleADRForm();
    loadADRs();
  } catch(e) { showToast('Save failed: '+e.message, false); }
}

async function deleteADR(key) {
  if(!confirm('Delete ADR entry "'+key+'"?')) return;
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
  wrap.innerHTML='<div class="loading">Loading…</div>';
  try {
    const data=await fetch('/v1/sessions').then(r=>r.json());
    const sessions=data.sessions||[];
    if(!sessions.length){wrap.innerHTML='<div class="empty">No sessions recorded yet.</div>';return;}
    // Running totals for a footer row so users see the cumulative number
    // without reaching for a spreadsheet. Cost parsed from "$X.YYYY" strings.
    const parseDollar=s=>parseFloat(String(s).replace(/[^0-9.]/g,''))||0;
    const totalCalls=sessions.reduce((a,s)=>a+(s.calls||0),0);
    const totalSaved=sessions.reduce((a,s)=>a+(s.tokens_saved||0),0);
    const totalUsed=sessions.reduce((a,s)=>a+(s.tokens_used||0),0);
    const totalCost=sessions.reduce((a,s)=>a+parseDollar(s.cost_avoided),0);
    wrap.innerHTML='<table class="sessions-table"><thead><tr>'+
      '<th>Started</th><th>Last Seen</th><th>Calls</th><th>Tokens Saved</th><th>Tokens Used</th><th>Cost Avoided</th>'+
      '</tr></thead><tbody>'+
      sessions.map(s=>
        '<tr>'+
        '<td title="'+esc(s.session_id)+'">'+timeAgo(s.started_at)+'</td>'+
        '<td>'+timeAgo(s.last_seen)+'</td>'+
        '<td>'+fmt(s.calls||0)+'</td>'+
        '<td style="color:var(--green)">'+fmt(s.tokens_saved||0)+'</td>'+
        '<td>'+fmt(s.tokens_used||0)+'</td>'+
        '<td style="color:var(--orange)">'+esc(s.cost_avoided||'$0.0000')+'</td>'+
        '</tr>'
      ).join('')+
      '</tbody><tfoot><tr class="sessions-total">'+
      '<td colspan="2">Total across '+sessions.length+' session'+(sessions.length!==1?'s':'')+'</td>'+
      '<td>'+fmt(totalCalls)+'</td>'+
      '<td style="color:var(--green)">'+fmt(totalSaved)+'</td>'+
      '<td>'+fmt(totalUsed)+'</td>'+
      '<td style="color:var(--orange)">$'+totalCost.toFixed(4)+'</td>'+
      '</tr></tfoot></table>';
  } catch(e) { wrap.innerHTML='<div class="error">Failed to load sessions: '+esc(e.message)+'</div>'; }
}

// ── Cost projection ────────────────────────────────────────────────────────
async function loadProjection() {
  const el=document.getElementById('projection-banner');
  if(!el) return;
  try {
    const data=await fetch('/v1/sessions').then(r=>r.json());
    const sessions=data.sessions||[];
    if(sessions.length<2){el.innerHTML='';return;}
    // Parse cost strings like "$0.1234"
    const parseDollar=s=>parseFloat(String(s).replace(/[^0-9.]/g,''))||0;
    const totalCost=sessions.reduce((a,s)=>a+parseDollar(s.cost_avoided),0);
    const totalSaved=sessions.reduce((a,s)=>a+(s.tokens_saved||0),0);
    const totalCalls=sessions.reduce((a,s)=>a+(s.calls||0),0);
    // Date range from oldest to newest
    const dates=sessions.map(s=>new Date(s.started_at)).filter(d=>!isNaN(d));
    if(!dates.length||totalCost===0){el.innerHTML='';return;}
    const oldest=Math.min(...dates.map(d=>d.getTime()));
    const newest=Math.max(...dates.map(d=>d.getTime()));
    const days=Math.max((newest-oldest)/86400000, 1);
    const monthlyRate=(totalCost/days)*30;
    const dailyRate=totalCost/days;
    const rateStr=monthlyRate<0.01?('<$0.01/mo'):'$'+monthlyRate.toFixed(2)+'/mo';
    const dailyStr=dailyRate<0.001?('<$0.01/day'):'$'+dailyRate.toFixed(3)+'/day';
    el.innerHTML='<div class="proj-banner">'+
      '<div class="proj-banner-icon">\ud83d\udcc8</div>'+
      '<div class="proj-banner-body">'+
        '<div class="proj-banner-title">Projected Savings Rate</div>'+
        '<div class="proj-banner-rate">~'+rateStr+'</div>'+
        '<div class="proj-banner-sub">Based on '+sessions.length+' sessions over '+Math.round(days)+' days \u00b7 '+dailyStr+' avg</div>'+
      '</div>'+
      '<div class="proj-banner-pills">'+
        '<div class="proj-banner-pill">'+fmt(totalSaved)+' tokens saved</div>'+
        '<div class="proj-banner-pill blue">'+totalCalls+' tool calls</div>'+
      '</div>'+
    '</div>';
  } catch(e) { document.getElementById('projection-banner').innerHTML=''; }
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
  title.textContent=name+' — Architecture';
  body.innerHTML='<div class="loading">Loading architecture…</div>';
  panel.scrollIntoView({behavior:'smooth',block:'nearest'});
  try {
    const data=await fetch('/v1/architecture',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({project:id})}).then(r=>r.json());
    const d=data.result||data;
    const langs=d.languages||[];
    const eps=d.entry_points||[];
    const hotspots=d.hotspot_functions||d.hotspots||[];
    body.innerHTML=
      (langs.length?'<div class="detail-section"><div class="detail-section-label">Languages</div>'+
        '<div class="lang-bar">'+langs.map(l=>'<span class="lang-chip">'+esc(l.language||l)+(l.file_count?' \u00b7 '+l.file_count+' files':'')+'</span>').join('')+'</div></div>':'')+
      (eps.length?'<div class="detail-section"><div class="detail-section-label">Entry Points</div>'+
        '<ul class="ep-list">'+eps.slice(0,8).map(e=>'<li>'+esc(e.name||e)+(e.file?' <span style="color:var(--muted)">'+esc(e.file)+'</span>':'')+'</li>').join('')+'</ul></div>':'')+
      (hotspots.length?'<div class="detail-section"><div class="detail-section-label">Hotspot Functions</div>'+
        '<ul class="hotspot-list">'+hotspots.slice(0,10).map(h=>'<li>'+esc(h.name||h)+'<span class="hotspot-calls">'+(h.call_count!=null?h.call_count+' calls':'')+'</span></li>').join('')+'</ul></div>':'')+
      (!langs.length&&!eps.length&&!hotspots.length?'<div class="empty">No architecture data available. Re-index the project first.</div>':'');
  } catch(e) { body.innerHTML='<div class="error">Failed to load architecture: '+esc(e.message)+'</div>'; }
}

function closeDetail() {
  _detailOpenId=null;
  document.getElementById('proj-detail-panel').classList.remove('open');
}

// ── Init ───────────────────────────────────────────────────────────────────
updateAuthBadge();
load();
populateSearchProjects();
loadProjection();
setInterval(load, 30000);
setInterval(loadProjection, 60000);

// Restore tab from URL hash
const hash=(location.hash||'').replace('#','');
if(['overview','projects','search','adrs','sessions'].includes(hash)) showTab(hash);
</script>
</body>
</html>`
