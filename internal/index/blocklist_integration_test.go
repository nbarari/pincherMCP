package index

import (
	"context"
	"testing"

	"github.com/pincherMCP/pincher/internal/db"
)

// blocklist_integration_test pins the cross-feature contract between the
// JSON/YAML extractor (PR #23) and the file-level blocklist (PR #24): when
// indexed together, a noise-shaped JSON/YAML file (package-lock.json,
// yarn.lock, *.min.js, *.map) MUST NOT produce Setting symbols, while a
// real config file in the same project MUST produce Settings.
//
// Without this gate, regressions could land in two ways no individual PR
// would catch:
//  1. The blocklist runs but the extractor's dispatch order means lockfile
//     content reaches yaml.v3 anyway (hundreds of spurious Settings).
//  2. The blocklist over-reaches and suppresses real config (regression on
//     legitimate `tsconfig.json` / `mcp.json` indexing).

const realYAMLConfig = `services:
  web:
    image: nginx:1.25
    ports:
      - "80:80"
  db:
    image: postgres:16
`

// Truncated package-lock.json shape — enough structure that yaml.v3 (which
// also parses JSON) would produce Settings for every key absent the blocklist.
const lockfileShape = `{
  "name": "demo",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "demo",
      "dependencies": {
        "react": "18.0.0",
        "lodash": "4.17.21",
        "axios": "1.6.0",
        "express": "4.18.0",
        "moment": "2.29.4"
      }
    }
  }
}
`

func TestIndex_LockfileBlockedFromYAMLExtraction(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	// Real config — must produce Settings.
	writeFile(t, dir, "compose.yaml", realYAMLConfig)
	// Lockfile shape — must produce zero Settings.
	writeFile(t, dir, "package-lock.json", lockfileShape)

	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if res.Blocked == 0 {
		t.Errorf("expected at least one blocked file (package-lock.json), got Blocked=0")
	}

	pid := db.ProjectIDFromPath(dir)

	// Positive: real YAML produces Settings under the expected dotted path.
	servicesWeb, err := store.GetSymbolsByQN(pid, "services.web")
	if err != nil {
		t.Fatalf("GetSymbolsByQN(services.web): %v", err)
	}
	if len(servicesWeb) == 0 {
		t.Error("expected Setting `services.web` from real compose.yaml; got 0")
	}

	// Negative: every Setting symbol's source file MUST NOT be the lockfile.
	// This is the cross-PR contract: even if the lockfile parses cleanly as
	// JSON-via-yaml, it never reaches the extractor.
	rows, err := store.DB().Query(
		`SELECT file_path, qualified_name FROM symbols
		 WHERE project_id = ? AND kind = 'Setting'`, pid)
	if err != nil {
		t.Fatalf("query Settings: %v", err)
	}
	defer rows.Close()

	var lockfileSettings, realSettings int
	for rows.Next() {
		var fp, qn string
		if err := rows.Scan(&fp, &qn); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if fp == "package-lock.json" {
			lockfileSettings++
			t.Errorf("unexpected Setting from lockfile: %s :: %s", fp, qn)
		} else if fp == "compose.yaml" {
			realSettings++
		}
	}
	if lockfileSettings != 0 {
		t.Errorf("expected 0 Settings from package-lock.json, got %d", lockfileSettings)
	}
	if realSettings == 0 {
		t.Error("expected non-zero Settings from compose.yaml; blocklist may be over-broad")
	}
}

func TestIndex_RealJSONConfigStillExtracts(t *testing.T) {
	// Negative-of-negative: the blocklist must not suppress legitimate JSON
	// config. tsconfig.json and mcp.json are common files agents need to
	// query — if they ever land in the blocklist by accident, this test
	// fails immediately.
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "tsconfig.json", `{
  "compilerOptions": {
    "target": "es2022",
    "strict": true
  }
}
`)
	writeFile(t, dir, "mcp.json", `{
  "mcpServers": {
    "pincher": { "command": "/usr/local/bin/pincher" }
  }
}
`)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	pid := db.ProjectIDFromPath(dir)
	for _, qn := range []string{"compilerOptions.target", "mcpServers.pincher.command"} {
		syms, err := store.GetSymbolsByQN(pid, qn)
		if err != nil {
			t.Fatalf("GetSymbolsByQN(%s): %v", qn, err)
		}
		if len(syms) == 0 {
			t.Errorf("expected Setting %q to exist (real JSON config must not be blocked)", qn)
		}
	}
}
