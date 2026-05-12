package server

import "encoding/json"

// outputSchemaJSON returns the JSON Schema for each tool's success
// response body (#581). Lifted to its own file so the per-tool
// payload — 24 schemas, ~5-15 fields each — doesn't bloat
// registerTools or server.go's main flow. The wireToolOutputSchemas
// helper threads them onto s.outputSchemas after registerTools so
// the OpenAPI spec carries a real contract per endpoint instead of
// the bare {type: object} placeholder.
//
// Each schema describes the success-path response only. The shared
// _meta envelope is referenced via $ref to the Meta component
// declared in openAPIComponentSchemas. Errors fall through to the
// `default` response with $ref to Error.
//
// When adding a new tool, its OutputSchema MUST be declared here OR
// the TestOpenAPI_EveryToolHasNonPlaceholderOutputSchema gate fails
// CI (sibling of the request-side TestOpenAPI_PerToolSchemaIsRealNotPlaceholder
// from #560).
func outputSchemaJSON(name string) json.RawMessage {
	if s, ok := outputSchemas[name]; ok {
		return json.RawMessage(s)
	}
	return nil
}

// metaRef is the $ref to the shared _meta envelope component.
// Inlined as a string to keep the per-tool schema declarations
// compact.
const metaRef = `{"$ref":"#/components/schemas/Meta"}`

var outputSchemas = map[string]string{
	// 1. index — write-side; returns counts.
	"index": `{
		"type":"object",
		"required":["project","files","symbols","edges"],
		"properties":{
			"project":{"type":"string"},
			"path":{"type":"string"},
			"files":{"type":"integer"},
			"symbols":{"type":"integer"},
			"edges":{"type":"integer"},
			"deleted":{"type":"integer"},
			"skipped":{"type":"integer"},
			"blocked":{"type":"integer"},
			"duration_ms":{"type":"integer"},
			"_meta":` + metaRef + `
		}
	}`,

	// 2. symbol — read by ID.
	"symbol": `{
		"type":"object",
		"required":["id","name","kind","language","file_path"],
		"properties":{
			"id":{"type":"string"},
			"name":{"type":"string"},
			"qualified_name":{"type":"string"},
			"kind":{"type":"string"},
			"language":{"type":"string"},
			"file_path":{"type":"string"},
			"start_line":{"type":"integer"},
			"end_line":{"type":"integer"},
			"start_byte":{"type":"integer"},
			"end_byte":{"type":"integer"},
			"signature":{"type":"string"},
			"docstring":{"type":"string"},
			"return_type":{"type":"string"},
			"is_exported":{"type":"boolean"},
			"is_test":{"type":"boolean"},
			"complexity":{"type":"integer"},
			"extraction_confidence":{"type":"number"},
			"source":{"type":"string"},
			"_meta":` + metaRef + `
		}
	}`,

	// 3. symbols — batch read.
	"symbols": `{
		"type":"object",
		"required":["symbols","_meta"],
		"properties":{
			"symbols":{"type":"array","items":{"type":"object"}},
			"missing":{"type":"array","items":{"type":"string"}},
			"_meta":` + metaRef + `
		}
	}`,

	// 4. context — symbol + imports.
	"context": `{
		"type":"object",
		"required":["symbol","_meta"],
		"properties":{
			"symbol":{"type":"object"},
			"imports":{"type":"array","items":{"type":"object"}},
			"_meta":` + metaRef + `
		}
	}`,

	// 5. search — BM25-ranked.
	"search": `{
		"type":"object",
		"required":["query","count","results","_meta"],
		"properties":{
			"query":{"type":"string"},
			"count":{"type":"integer"},
			"results":{"type":"array","items":{
				"type":"object",
				"properties":{
					"id":{"type":"string"},
					"name":{"type":"string"},
					"qualified_name":{"type":"string"},
					"kind":{"type":"string"},
					"language":{"type":"string"},
					"file_path":{"type":"string"},
					"start_line":{"type":"integer"},
					"signature":{"type":"string"},
					"snippet":{"type":"string"},
					"score":{"type":"number"},
					"extraction_confidence":{"type":"number"}
				}
			}},
			"_meta":` + metaRef + `
		}
	}`,

	// 6. query — pinchQL.
	"query": `{
		"type":"object",
		"required":["columns","rows","total","_meta"],
		"properties":{
			"columns":{"type":"array","items":{"type":"string"}},
			"rows":{"type":"array","items":{"type":"object"}},
			"total":{"type":"integer"},
			"warnings":{"type":"array","items":{"type":"string"}},
			"_meta":` + metaRef + `
		}
	}`,

	// 7. trace — graph BFS.
	"trace": `{
		"type":"object",
		"required":["root","direction","hops","total","_meta"],
		"properties":{
			"root":{"type":"string"},
			"direction":{"type":"string","enum":["inbound","outbound","both"]},
			"hops":{"type":"array","items":{"type":"object"}},
			"total":{"type":"integer"},
			"risk_summary":{"type":"object","properties":{
				"CRITICAL":{"type":"integer"},
				"HIGH":{"type":"integer"},
				"MEDIUM":{"type":"integer"},
				"LOW":{"type":"integer"}
			}},
			"_meta":` + metaRef + `
		}
	}`,

	// 8. changes — git-diff blast radius.
	"changes": `{
		"type":"object",
		"required":["changed_files","changed_symbols","impacted","summary","tests_to_run","_meta"],
		"properties":{
			"changed_files":{"type":"array","items":{"type":"string"}},
			"changed_symbols":{"type":"array","items":{"type":"object"}},
			"impacted":{"type":"array","items":{"type":"object"}},
			"summary":{"type":"object","properties":{
				"changed_files":{"type":"integer"},
				"changed_symbols":{"type":"integer"},
				"total_impacted":{"type":"integer"},
				"critical":{"type":"integer"},
				"high":{"type":"integer"},
				"medium":{"type":"integer"},
				"low":{"type":"integer"},
				"tests_to_run":{"type":"integer"}
			}},
			"tests_to_run":{"type":"array","items":{"type":"object"}},
			"_meta":` + metaRef + `
		}
	}`,

	// 9. dead_code — unreachable internal symbols.
	"dead_code": `{
		"type":"object",
		"required":["dead_symbols","filters","total","_meta"],
		"properties":{
			"dead_symbols":{"type":"array","items":{
				"type":"object",
				"properties":{
					"id":{"type":"string"},
					"name":{"type":"string"},
					"kind":{"type":"string"},
					"language":{"type":"string"},
					"file_path":{"type":"string"},
					"start_line":{"type":"integer"},
					"complexity":{"type":"integer"}
				}
			}},
			"filters":{"type":"object"},
			"total":{"type":"integer"},
			"_meta":` + metaRef + `
		}
	}`,

	// 10. architecture — orientation.
	"architecture": `{
		"type":"object",
		"required":["project","languages","node_kinds","edge_kinds","entry_points","hotspots","_meta"],
		"properties":{
			"project":{"type":"object"},
			"languages":{"type":"object"},
			"node_kinds":{"type":"object"},
			"edge_kinds":{"type":"object"},
			"entry_points":{"type":"array","items":{"type":"object"}},
			"hotspots":{"type":"array","items":{"type":"object"}},
			"_meta":` + metaRef + `
		}
	}`,

	// 11. schema — schema diagram.
	"schema": `{
		"type":"object",
		"required":["node_kinds","edge_kinds","_meta"],
		"properties":{
			"node_kinds":{"type":"object"},
			"edge_kinds":{"type":"object"},
			"total_nodes":{"type":"integer"},
			"total_edges":{"type":"integer"},
			"_meta":` + metaRef + `
		}
	}`,

	// 12. list — projects.
	"list": `{
		"type":"object",
		"required":["projects","count","_meta"],
		"properties":{
			"projects":{"type":"array","items":{"type":"object"}},
			"count":{"type":"integer"},
			"filtered_out":{"type":"integer"},
			"filtered_breakdown":{"type":"object","properties":{
				"dead_path":{"type":"integer"},
				"inactive":{"type":"integer"},
				"low_edges":{"type":"integer"}
			}},
			"page":{"type":"object"},
			"pruned":{"type":"array","items":{"type":"string"}},
			"_meta":` + metaRef + `
		}
	}`,

	// 13. adr — persistent decisions store.
	"adr": `{
		"type":"object",
		"properties":{
			"key":{"type":"string"},
			"value":{"type":"string"},
			"entries":{"type":"object","additionalProperties":{"type":"string"}},
			"deleted":{"type":"boolean"},
			"_meta":` + metaRef + `
		}
	}`,

	// 14. health — extraction quality + drift.
	"health": `{
		"type":"object",
		"required":["schema_version","db_path","_meta"],
		"properties":{
			"schema_version":{"type":"integer"},
			"db_path":{"type":"string"},
			"project":{"type":"object"},
			"extraction_coverage":{"type":"array","items":{"type":"object"}},
			"binary_stale":{"type":"boolean"},
			"binary_stale_message":{"type":"string"},
			"index_drift":{"type":"boolean"},
			"index_drift_message":{"type":"string"},
			"_meta":` + metaRef + `
		}
	}`,

	// 15. stats — savings counter.
	"stats": `{
		"type":"object",
		"required":["_meta"],
		"properties":{
			"session":{"type":"object"},
			"all_time":{"type":"object"},
			"project":{"type":"object"},
			"_meta":` + metaRef + `
		}
	}`,

	// 16. fetch — external URL → Document.
	"fetch": `{
		"type":"object",
		"required":["id","url","stored","_meta"],
		"properties":{
			"id":{"type":"string"},
			"url":{"type":"string"},
			"title":{"type":"string"},
			"text":{"type":"string"},
			"raw_bytes":{"type":"integer"},
			"stored":{"type":"boolean"},
			"_meta":` + metaRef + `
		}
	}`,

	// 17. neighborhood — same-file symbols.
	"neighborhood": `{
		"type":"object",
		"required":["seed_id","file_path","language","neighbors","count","_meta"],
		"properties":{
			"seed_id":{"type":"string"},
			"file_path":{"type":"string"},
			"language":{"type":"string"},
			"count":{"type":"integer"},
			"neighbors":{"type":"array","items":{"type":"object"}},
			"page":{"type":"object"},
			"_meta":` + metaRef + `
		}
	}`,

	// 18. guide — task → recommended tools.
	"guide": `{
		"type":"object",
		"required":["task","shape","recommended_next_tools","_meta"],
		"properties":{
			"task":{"type":"string"},
			"hint":{"type":"string"},
			"shape":{"type":"string"},
			"recommended_next_tools":{"type":"array","items":{
				"type":"object",
				"properties":{
					"tool":{"type":"string"},
					"args":{"type":"string"},
					"why":{"type":"string"}
				}
			}},
			"_meta":` + metaRef + `
		}
	}`,

	// 19. init — inject pincher policy into editor rules.
	"init": `{
		"type":"object",
		"required":["target","action","_meta"],
		"properties":{
			"target":{"type":"string"},
			"action":{"type":"string"},
			"path":{"type":"string"},
			"backup":{"type":"string"},
			"applied":{"type":"boolean"},
			"_meta":` + metaRef + `
		}
	}`,

	// 20. doctor — diagnostic report (#558 phase 2).
	"doctor": `{
		"type":"object",
		"required":["binary_version","schema_version","db_size_bytes","wal_size_bytes","projects","extraction_failures","slow_queries","_meta"],
		"properties":{
			"binary_version":{"type":"string"},
			"generated_at":{"type":"string"},
			"schema_version":{"type":"integer"},
			"db_size_bytes":{"type":"integer"},
			"wal_size_bytes":{"type":"integer"},
			"lookback_hours":{"type":"integer"},
			"projects":{"type":"array","items":{"type":"object"}},
			"extraction_failures":{"type":"array","items":{"type":"object"}},
			"slow_queries":{"type":"array","items":{"type":"object"}},
			"_meta":` + metaRef + `
		}
	}`,

	// 21. rebuild_fts — admin (#558 phase 2).
	"rebuild_fts": `{
		"type":"object",
		"required":["dry_run","_meta"],
		"properties":{
			"dry_run":{"type":"boolean"},
			"would_reindex_symbols":{"type":"integer"},
			"rebuilt_rows":{"type":"integer"},
			"duration_ms":{"type":"integer"},
			"hint":{"type":"string"},
			"_meta":` + metaRef + `
		}
	}`,

	// 22. self_test — smoke test (#558 phase 2).
	"self_test": `{
		"type":"object",
		"required":["ok","steps","_meta"],
		"properties":{
			"ok":{"type":"boolean"},
			"steps":{"type":"array","items":{
				"type":"object",
				"properties":{
					"label":{"type":"string"},
					"ok":{"type":"boolean"},
					"duration_ms":{"type":"integer"},
					"error":{"type":"string"}
				}
			}},
			"_meta":` + metaRef + `
		}
	}`,
}
