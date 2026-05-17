package ast

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ansibleJinjaVarRefRE matches `{{ name` (with optional whitespace and
// optional leading `-` for trim markers) and captures the bare leading
// identifier. Captures only the LEFTMOST identifier in the expression —
// `{{ db_host.port | default('x') }}` yields `db_host`. The regex is
// deliberately permissive about what comes after the name (filter
// chains, attribute access, calls); resolution of the rest is the
// extractor's job for .j2 files (#1165 jinja path), not the YAML
// string-value scanner. Multiple `{{ ... }}` expressions in the same
// string each contribute one reference via FindAllStringSubmatch.
var ansibleJinjaVarRefRE = regexp.MustCompile(`\{\{-?\s*([A-Za-z_][A-Za-z0-9_]*)`)

// extractYAML parses a YAML or JSON document and emits a Setting symbol per key.
//
// Each key's QualifiedName is the dotted path from the root, e.g. "services.web.image".
// Sequence elements use numeric indices, e.g. "tasks.0.name". When a file contains
// multiple YAML documents, the doc index is included in the path: "doc1.services.web".
//
// The byte range of each Setting covers from the key (or sequence element) on the
// page through to the start of the next sibling-or-shallower entry, so retrieving
// the symbol returns the key plus its full nested value — the same shape as
// retrieving a function body in code.
//
// Confidence is 1.0 (real YAML parser, not regex).
func extractYAML(source []byte, relPath string) *FileResult {
	result := &FileResult{}

	base := filepath.Base(relPath)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	result.Module = base

	if len(source) == 0 {
		return result
	}

	lineOffsets := buildLineOffsets(source)
	sourceLen := len(source)

	// Decode all documents (handles YAML's `---` multi-document streams).
	var docs []*yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	for {
		var doc yaml.Node
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		d := doc
		docs = append(docs, &d)
	}
	if len(docs) == 0 {
		return result
	}
	multiDoc := len(docs) > 1

	// Collect entries via DFS — entries are produced in source order.
	type entry struct {
		path []string
		val  *yaml.Node
		line int
		col  int
	}
	var entries []entry

	var walk func(n *yaml.Node, path []string)
	walk = func(n *yaml.Node, path []string) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode:
			for _, child := range n.Content {
				walk(child, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				v := n.Content[i+1]
				if k.Kind != yaml.ScalarNode {
					continue
				}
				childPath := append(append([]string(nil), path...), yamlSanitizeKey(k.Value))
				entries = append(entries, entry{
					path: childPath,
					val:  v,
					line: k.Line,
					col:  k.Column,
				})
				walk(v, childPath)
			}
		case yaml.SequenceNode:
			for i, child := range n.Content {
				childPath := append(append([]string(nil), path...), fmt.Sprintf("%d", i))
				entries = append(entries, entry{
					path: childPath,
					val:  child,
					line: child.Line,
					col:  child.Column,
				})
				walk(child, childPath)
			}
		}
	}

	for i, doc := range docs {
		var prefix []string
		if multiDoc {
			prefix = []string{fmt.Sprintf("doc%d", i)}
			line := doc.Line
			if line < 1 {
				line = 1
			}
			col := doc.Column
			if col < 1 {
				col = 1
			}
			entries = append(entries, entry{
				path: prefix,
				val:  doc,
				line: line,
				col:  col,
			})
		}
		walk(doc, prefix)
	}

	// Convert entries to symbols.
	//
	// End-byte rule depends on the entry's value kind:
	//
	//   - Scalar / Alias: end at the end of the value's own line(s). For plain
	//     scalars that's the start of the next line; for block scalars (`|` /
	//     `>`) it's the first line whose non-blank column is shallower than
	//     the key's column. Without this carve-out, a scalar that's the last
	//     key in its parent mapping would extend through every aunt/uncle
	//     because the depth-aware rule below picks the parent's next sibling
	//     as the boundary.
	//
	//   - Mapping / Sequence / Document: end at the start of the next entry
	//     at same-or-shallower depth — the original "depth-aware" rule.
	for i, e := range entries {
		startByte := lineColToOffset(lineOffsets, e.line, e.col, sourceLen)

		var endByte int
		if e.val != nil && (e.val.Kind == yaml.ScalarNode || e.val.Kind == yaml.AliasNode) {
			endByte = scalarEndByte(source, lineOffsets, e.val, e.line, e.col, sourceLen)
		} else {
			endByte = sourceLen
			for j := i + 1; j < len(entries); j++ {
				if len(entries[j].path) <= len(e.path) {
					endByte = lineColToOffset(lineOffsets, entries[j].line, 1, sourceLen)
					break
				}
			}
		}

		if endByte <= startByte {
			if e.line < len(lineOffsets) {
				endByte = lineOffsets[e.line]
			} else {
				endByte = sourceLen
			}
		}
		endLine := offsetToLine(lineOffsets, endByte-1)
		if endLine < e.line {
			endLine = e.line
		}

		name := e.path[len(e.path)-1]
		qn := strings.Join(e.path, ".")

		sig := yamlSignature(e.val)

		result.Symbols = append(result.Symbols, ExtractedSymbol{
			Name:          name,
			QualifiedName: qn,
			Kind:          "Setting",
			StartByte:     startByte,
			EndByte:       endByte,
			StartLine:     e.line,
			EndLine:       endLine,
			Signature:     sig,
			IsExported:    true,
		})
	}

	// Ansible-aware edge extraction (#71 phase 1, RENDERS slice). Only
	// fires when the file path matches Ansible task / handler / playbook
	// conventions — keeps non-Ansible YAML untouched (Helm values, k8s
	// manifests, generic config, etc.). Emits RENDERS edges for
	// `template: src: foo.j2` patterns.
	if isAnsibleTaskFile(relPath) {
		result.Edges = append(result.Edges, ansibleRenderEdges(docs, result.Module)...)
	}

	// Ansible structural edges (#1160, completing #71 Phase 1).
	// Playbooks: emit INCLUDES edges for `roles:` / `import_role:` /
	// `include_role:` keys. YAML inventories: emit LOADS edges for hosts
	// to their `host_vars/<H>.yml` files. The two extractors gate on
	// distinct file-path conventions so a playbook isn't mistaken for an
	// inventory and vice versa.
	if isAnsiblePlaybookFile(relPath) {
		result.Edges = append(result.Edges, ansibleIncludesEdges(docs, result.Module)...)
	}
	if isAnsibleYAMLInventoryFile(relPath) {
		result.Edges = append(result.Edges, ansibleLoadsEdges(docs, result.Module)...)
	}

	// Ansible USES_VAR dataflow edges (#1165, completing #71 Phase 2).
	// Walk every string scalar in task / playbook files and emit a
	// USES_VAR edge per `{{ var_name ... }}` substitution. Confidence
	// 0.85: this is a regex over string values, not a Jinja parse —
	// false positives in the form of `{{ literal_looking_string }}`
	// inside intentional escape sequences are accepted in exchange for
	// not pulling gonja into the YAML hot path. The resolver drops any
	// reference whose name doesn't bind to a Setting symbol in a
	// canonical Ansible var-declaration path, so noise stays bounded.
	if isAnsibleTaskFile(relPath) {
		result.Edges = append(result.Edges, ansibleUsesVarEdges(docs, result.Module)...)
	}

	return result
}

// isAnsibleTaskFile returns true when `relPath` looks like an Ansible
// task or handler file. The detection is intentionally conservative:
// only files inside `tasks/` or `handlers/` directories qualify, plus
// top-level playbook conventions (`site.yml`, `deploy.yml`, `playbook*`).
//
// Why path-based: YAML doesn't carry schema; a generic Helm or k8s
// manifest could also have `template:` keys with completely different
// semantics. Restricting to Ansible-canonical paths avoids false
// positives without requiring deeper YAML schema introspection.
func isAnsibleTaskFile(relPath string) bool {
	clean := strings.ReplaceAll(relPath, "\\", "/")
	if strings.Contains(clean, "/tasks/") || strings.Contains(clean, "/handlers/") {
		return true
	}
	base := filepath.Base(clean)
	switch base {
	case "site.yml", "site.yaml",
		"deploy.yml", "deploy.yaml",
		"playbook.yml", "playbook.yaml":
		return true
	}
	if strings.HasPrefix(base, "playbook") &&
		(strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")) {
		return true
	}
	return false
}

// ansibleRenderEdges scans a parsed YAML document for the Ansible
// `template:` action pattern and emits a RENDERS edge for each one.
//
// The patterns it recognises:
//
//	# Full keyed form
//	- name: render config
//	  template:
//	    src: foo.j2
//	    dest: /etc/foo.conf
//
//	# Short flow form
//	- template: { src: bar.j2 }
//
// Edges:
//
//	{ FromQN: <module>, ToName: <src-value>, Kind: "RENDERS", Confidence: 1.0 }
//
// The edge target is the literal `src` value as it appears in the YAML —
// the indexer's edge-resolution pass will look it up against the global
// symbol table. Confidence 1.0: this is YAML-parser-backed, not regex.
func ansibleRenderEdges(docs []*yaml.Node, module string) []ExtractedEdge {
	var edges []ExtractedEdge

	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.MappingNode:
			// Each mapping pair is (key, value). When key.Value == "template"
			// and value is a mapping with `src:`, emit an edge.
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				v := n.Content[i+1]
				if k != nil && k.Kind == yaml.ScalarNode && k.Value == "template" {
					if src := ansibleFindSrc(v); src != "" {
						edges = append(edges, ExtractedEdge{
							FromQN:     module,
							ToName:     src,
							Kind:       "RENDERS",
							Confidence: 1.0,
						})
					}
				}
				walk(v)
			}
		}
	}
	for _, d := range docs {
		walk(d)
	}
	return edges
}

// ansibleFindSrc extracts the `src` value from an Ansible task's
// `template:` argument node. Handles both full mapping form
// (`{ src: foo.j2, dest: ... }`) and flow form (`src: foo.j2` inline).
// Returns "" if not present or not a scalar string.
func ansibleFindSrc(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == "src" &&
			v != nil && v.Kind == yaml.ScalarNode {
			return v.Value
		}
	}
	return ""
}

// isAnsiblePlaybookFile returns true when relPath looks like an Ansible
// playbook — i.e. a top-level orchestration YAML that may declare
// `roles:` / `import_role:` / `include_role:` against sibling
// `roles/<name>/` directories.
//
// Detection covers the canonical filenames (`site.yml`, `deploy.yml`,
// `playbook.yml`, anything starting with `playbook`) plus YAML files at
// the project root that aren't inside `tasks/` / `handlers/` /
// `vars/` / `defaults/` / `group_vars/` / `host_vars/` / `roles/`
// (those are role-internal files, not playbooks). Files in
// `playbooks/` are also accepted (common Ansible layout convention).
func isAnsiblePlaybookFile(relPath string) bool {
	clean := strings.ReplaceAll(relPath, "\\", "/")
	base := filepath.Base(clean)
	if !strings.HasSuffix(base, ".yml") && !strings.HasSuffix(base, ".yaml") {
		return false
	}
	switch base {
	case "site.yml", "site.yaml",
		"deploy.yml", "deploy.yaml",
		"playbook.yml", "playbook.yaml":
		return true
	}
	if strings.HasPrefix(base, "playbook") {
		return true
	}
	if strings.Contains(clean, "/playbooks/") || strings.HasPrefix(clean, "playbooks/") {
		return true
	}
	return false
}

// isAnsibleYAMLInventoryFile returns true when relPath looks like an
// Ansible YAML-format inventory. The conservative detection covers
// files literally named `inventory.yml` / `inventory.yaml` /
// `hosts.yml` / `hosts.yaml`, plus anything under an `inventory/` or
// `inventories/` directory ending in `.yml` / `.yaml`. INI-format
// inventories aren't covered here — they don't parse as YAML and
// would need a separate extractor (out of scope for #1160 Phase 1
// per its "Out of scope" section on Salt + INI inventories).
func isAnsibleYAMLInventoryFile(relPath string) bool {
	clean := strings.ReplaceAll(relPath, "\\", "/")
	base := filepath.Base(clean)
	if !strings.HasSuffix(base, ".yml") && !strings.HasSuffix(base, ".yaml") {
		return false
	}
	switch base {
	case "inventory.yml", "inventory.yaml",
		"hosts.yml", "hosts.yaml":
		return true
	}
	if strings.Contains(clean, "/inventory/") || strings.HasPrefix(clean, "inventory/") {
		return true
	}
	if strings.Contains(clean, "/inventories/") || strings.HasPrefix(clean, "inventories/") {
		return true
	}
	return false
}

// ansibleIncludesEdges scans a parsed playbook for the three forms of
// role inclusion and emits INCLUDES edges to the role's
// `tasks/main.yml` Module.
//
// The patterns recognised:
//
//	# play-level roles list
//	- hosts: all
//	  roles:
//	    - common
//	    - { role: nginx, vars: {...} }
//
//	# task-level import / include
//	- import_role: { name: postgres }
//	- include_role: { name: backup }
//
// Edge target: the literal `roles/<name>/tasks/main.yml` relative path.
// The indexer's edge-resolution pass will bind it against the global
// symbol table (modules are keyed by file_path on the YAML extractor
// side, so the lookup hits whichever role's tasks/main.yml has been
// indexed). When the role lives at a non-standard path (custom
// `roles_path`), the edge will dangle on the resolution pass — the
// downstream synthetic-external path (#1340) catches those so the
// edge persists rather than silently dropping.
func ansibleIncludesEdges(docs []*yaml.Node, module string) []ExtractedEdge {
	var edges []ExtractedEdge

	emit := func(roleName string) {
		if roleName == "" {
			return
		}
		edges = append(edges, ExtractedEdge{
			FromQN:     module,
			ToName:     "roles/" + roleName + "/tasks/main.yml",
			Kind:       "INCLUDES",
			Confidence: 1.0,
		})
	}

	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				v := n.Content[i+1]
				if k == nil || k.Kind != yaml.ScalarNode {
					continue
				}
				switch k.Value {
				case "roles":
					// `roles:` value is a sequence; each item is either a
					// bare string or a mapping with a `role:` field.
					if v != nil && v.Kind == yaml.SequenceNode {
						for _, item := range v.Content {
							switch item.Kind {
							case yaml.ScalarNode:
								emit(item.Value)
							case yaml.MappingNode:
								emit(ansibleFindMappingScalar(item, "role"))
							}
						}
					}
				case "import_role", "include_role":
					// Both take a mapping with a `name:` field. The role
					// argument is sometimes also passed as a bare scalar
					// in older playbooks — handle both shapes.
					if v != nil {
						switch v.Kind {
						case yaml.ScalarNode:
							emit(v.Value)
						case yaml.MappingNode:
							emit(ansibleFindMappingScalar(v, "name"))
						}
					}
				}
				walk(v)
			}
		}
	}
	for _, d := range docs {
		walk(d)
	}
	return edges
}

// ansibleLoadsEdges scans a parsed YAML inventory for host entries and
// emits LOADS edges to their `host_vars/<H>.yml` files. The YAML
// inventory shape is a nested map:
//
//	all:
//	  hosts:
//	    web-01:
//	      ansible_host: 10.0.1.1
//	  children:
//	    db_servers:
//	      hosts:
//	        db-01: {}
//
// The walker emits one LOADS edge per host name encountered under any
// `hosts:` mapping, regardless of nesting depth. The edge target is
// the canonical `host_vars/<H>.yml` path; if the project uses the
// directory-form (`host_vars/<H>/main.yml`) the indexer's resolution
// pass + synthetic-external fallback (#1340) keeps the edge bound.
//
// `group_vars/<G>.yml` for groups is not emitted in this Phase 1
// slice — group resolution requires walking the `children:` tree
// structurally, and the value-add over host-only edges is marginal
// for the audit-shaped queries the edges feed. Filed as a follow-up
// rather than expanding the Phase 1 scope.
func ansibleLoadsEdges(docs []*yaml.Node, module string) []ExtractedEdge {
	var edges []ExtractedEdge
	seen := make(map[string]bool) // dedup repeat host references

	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				v := n.Content[i+1]
				if k != nil && k.Kind == yaml.ScalarNode && k.Value == "hosts" &&
					v != nil && v.Kind == yaml.MappingNode {
					for j := 0; j+1 < len(v.Content); j += 2 {
						hk := v.Content[j]
						if hk != nil && hk.Kind == yaml.ScalarNode && hk.Value != "" && !seen[hk.Value] {
							seen[hk.Value] = true
							edges = append(edges, ExtractedEdge{
								FromQN:     module,
								ToName:     "host_vars/" + hk.Value + ".yml",
								Kind:       "LOADS",
								Confidence: 1.0,
							})
						}
					}
				}
				walk(v)
			}
		}
	}
	for _, d := range docs {
		walk(d)
	}
	return edges
}

// ansibleUsesVarEdges walks every scalar string value in an Ansible
// task/playbook document and emits one USES_VAR edge per `{{ var_name }}`
// substitution. The walker descends through every YAML kind (Mapping,
// Sequence, Document) so substitutions in deeply-nested constructs like
// `when:`, `loop:`, `vars: { foo: "{{ bar }}" }`, and `with_items:` all
// get captured.
//
// Dedup is per-(module, var_name): a task file that references `{{ db_host }}`
// in five places emits one edge, not five. The graph cares "does file X
// reference var Y?", not "how many syntactic occurrences"; the latter
// adds N row noise without changing answers to any audit-shaped query.
//
// Confidence 0.85: regex over string scalars, not a parsed Jinja AST —
// see ansibleJinjaVarRefRE godoc for the tradeoff. Skips
// jinja-reserved tokens (loop / true / none / ...) via
// isUsefulJinjaVarName so common loop bodies don't emit dangling
// `loop`-bound edges.
func ansibleUsesVarEdges(docs []*yaml.Node, module string) []ExtractedEdge {
	var edges []ExtractedEdge
	seen := make(map[string]bool)

	emit := func(name string) {
		if !isUsefulJinjaVarName(name) {
			return
		}
		if seen[name] {
			return
		}
		seen[name] = true
		edges = append(edges, ExtractedEdge{
			FromQN:     module,
			ToName:     name,
			Kind:       "USES_VAR",
			Confidence: 0.85,
		})
	}

	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.MappingNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.ScalarNode:
			if n.Value == "" || !strings.Contains(n.Value, "{{") {
				return
			}
			for _, m := range ansibleJinjaVarRefRE.FindAllStringSubmatch(n.Value, -1) {
				if len(m) >= 2 {
					emit(m[1])
				}
			}
		}
	}
	for _, d := range docs {
		walk(d)
	}
	return edges
}

// ansibleFindMappingScalar returns the scalar value at key `key` in
// mapping node n, or "" if absent or not a scalar.
func ansibleFindMappingScalar(n *yaml.Node, key string) string {
	if n == nil || n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == key &&
			v != nil && v.Kind == yaml.ScalarNode {
			return v.Value
		}
	}
	return ""
}

// yamlSanitizeKey replaces characters that would collide with the dotted-path
// qualified name format.
func yamlSanitizeKey(k string) string {
	k = strings.ReplaceAll(k, ".", "_")
	k = strings.ReplaceAll(k, "/", "_")
	k = strings.ReplaceAll(k, " ", "_")
	return k
}

// yamlSignature returns a short, FTS-friendly description of a YAML value node.
func yamlSignature(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind {
	case yaml.ScalarNode:
		v := n.Value
		if len(v) > 200 {
			v = v[:200]
		}
		return v
	case yaml.MappingNode:
		return fmt.Sprintf("<mapping with %d keys>", len(n.Content)/2)
	case yaml.SequenceNode:
		return fmt.Sprintf("<sequence with %d items>", len(n.Content))
	case yaml.AliasNode:
		if n.Alias != nil {
			return "*" + n.Alias.Anchor
		}
		return "<alias>"
	case yaml.DocumentNode:
		return "<document>"
	}
	return ""
}

// scalarEndByte returns the end-byte offset for an entry whose value is a
// scalar or alias. For a single-line scalar the end is the start of the next
// line. For a block scalar (literal `|` or folded `>`) it walks forward from
// the key line until it finds a non-blank line whose first-non-blank column
// is ≤ the key's column — that line marks the start of the next sibling-or-
// shallower entry, so the block scalar ends at its byte offset. Reaching
// end-of-file ends the block scalar at sourceLen.
//
// yaml.v3's Node API doesn't expose end positions, so block-scalar end has
// to be derived from indentation rather than the parser's structural model.
func scalarEndByte(source []byte, lineOffsets []int, val *yaml.Node, keyLine, keyCol, sourceLen int) int {
	isBlock := val != nil && val.Kind == yaml.ScalarNode &&
		(val.Style == yaml.LiteralStyle || val.Style == yaml.FoldedStyle)

	if !isBlock {
		// Plain scalar / quoted scalar / alias — end at start of next line.
		if keyLine < len(lineOffsets) {
			return lineOffsets[keyLine]
		}
		return sourceLen
	}

	// Block scalar — walk forward looking for an outdent.
	if keyCol < 1 {
		keyCol = 1
	}
	for line := keyLine + 1; line-1 < len(lineOffsets); line++ {
		lineStart := lineOffsets[line-1]
		lineEnd := sourceLen
		if line < len(lineOffsets) {
			lineEnd = lineOffsets[line]
		}
		// First non-blank column on this line (1-based). Pure blank lines
		// don't count — they belong to the block scalar.
		col := 0
		for i := lineStart; i < lineEnd; i++ {
			ch := source[i]
			if ch == '\n' || ch == '\r' {
				break
			}
			if ch != ' ' && ch != '\t' {
				col = (i - lineStart) + 1
				break
			}
		}
		if col == 0 {
			continue // blank line — still inside the block scalar
		}
		if col <= keyCol {
			return lineStart
		}
	}
	return sourceLen
}

// lineColToOffset converts a 1-based (line, col) to a byte offset, clamped to source bounds.
func lineColToOffset(lineOffsets []int, line, col, sourceLen int) int {
	if line < 1 || line-1 >= len(lineOffsets) {
		return sourceLen
	}
	off := lineOffsets[line-1] + (col - 1)
	if off > sourceLen {
		return sourceLen
	}
	if off < 0 {
		return 0
	}
	return off
}
