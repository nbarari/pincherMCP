package ast

import (
	"strings"
	"testing"
)

const yamlSrc = `services:
  web:
    image: nginx:1.25
    ports:
      - "80:80"
      - "443:443"
    environment:
      LOG_LEVEL: info
  api:
    image: myapp:latest
    replicas: 3
version: "3.8"
`

func TestExtractYAML_TopLevelKeys(t *testing.T) {
	result := Extract([]byte(yamlSrc), "YAML", "docker-compose.yml")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	for _, want := range []string{"services", "version", "services.web", "services.web.image", "services.api.replicas"} {
		if _, ok := byQN[want]; !ok {
			t.Errorf("expected symbol qn=%q to be extracted", want)
		}
	}
}

func TestExtractYAML_ScalarSignature(t *testing.T) {
	result := Extract([]byte(yamlSrc), "YAML", "docker-compose.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	if got := byQN["services.web.image"].Signature; got != "nginx:1.25" {
		t.Errorf("services.web.image signature = %q, want %q", got, "nginx:1.25")
	}
	if got := byQN["version"].Signature; got != "3.8" {
		t.Errorf("version signature = %q, want %q", got, "3.8")
	}
}

func TestExtractYAML_AllSettingKind(t *testing.T) {
	result := Extract([]byte(yamlSrc), "YAML", "docker-compose.yml")
	for _, s := range result.Symbols {
		if s.Kind != "Setting" {
			t.Errorf("symbol %q has kind %q, want Setting", s.QualifiedName, s.Kind)
		}
	}
}

func TestExtractYAML_ConfidenceOne(t *testing.T) {
	result := Extract([]byte(yamlSrc), "YAML", "docker-compose.yml")
	for _, s := range result.Symbols {
		if s.ExtractionConfidence != 1.0 {
			t.Errorf("symbol %q confidence = %v, want 1.0", s.QualifiedName, s.ExtractionConfidence)
			break
		}
	}
}

func TestExtractYAML_ByteOffsets(t *testing.T) {
	result := Extract([]byte(yamlSrc), "YAML", "docker-compose.yml")
	src := []byte(yamlSrc)
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
		if s.StartByte >= s.EndByte {
			t.Errorf("symbol %q has start_byte(%d) >= end_byte(%d)", s.QualifiedName, s.StartByte, s.EndByte)
		}
		if s.EndByte > len(src) {
			t.Errorf("symbol %q end_byte(%d) > len(source)(%d)", s.QualifiedName, s.EndByte, len(src))
		}
	}
	// Retrieving services.web.image by byte-offset must contain "nginx:1.25"
	img := byQN["services.web.image"]
	slice := string(src[img.StartByte:img.EndByte])
	if !strings.Contains(slice, "nginx:1.25") {
		t.Errorf("services.web.image byte slice = %q, want it to contain 'nginx:1.25'", slice)
	}
	// Retrieving services.web by byte-offset must cover its full subtree
	web := byQN["services.web"]
	slice = string(src[web.StartByte:web.EndByte])
	if !strings.Contains(slice, "nginx:1.25") || !strings.Contains(slice, "LOG_LEVEL") {
		t.Errorf("services.web byte slice missing children: %q", slice)
	}
	if strings.Contains(slice, "myapp:latest") {
		t.Errorf("services.web byte slice should NOT contain api keys, got: %q", slice)
	}
}

func TestExtractYAML_Sequences(t *testing.T) {
	result := Extract([]byte(yamlSrc), "YAML", "docker-compose.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	if _, ok := byQN["services.web.ports.0"]; !ok {
		t.Error("expected sequence element services.web.ports.0")
	}
	if _, ok := byQN["services.web.ports.1"]; !ok {
		t.Error("expected sequence element services.web.ports.1")
	}
	if got := byQN["services.web.ports.0"].Signature; got != "80:80" {
		t.Errorf("ports.0 signature = %q, want '80:80'", got)
	}
}

func TestExtractYAML_MultiDoc(t *testing.T) {
	src := []byte(`apiVersion: v1
kind: Service
metadata:
  name: web
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: web-config
`)
	result := Extract(src, "YAML", "k8s.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	if _, ok := byQN["doc0.kind"]; !ok {
		t.Error("expected doc0.kind for first document")
	}
	if _, ok := byQN["doc1.kind"]; !ok {
		t.Error("expected doc1.kind for second document")
	}
	if got := byQN["doc0.kind"].Signature; got != "Service" {
		t.Errorf("doc0.kind signature = %q, want Service", got)
	}
	if got := byQN["doc1.kind"].Signature; got != "ConfigMap" {
		t.Errorf("doc1.kind signature = %q, want ConfigMap", got)
	}
}

func TestExtractYAML_AnchorsAndAliases(t *testing.T) {
	src := []byte(`defaults: &defaults
  timeout: 30
  retries: 3

production:
  <<: *defaults
  host: prod.example.com
`)
	result := Extract(src, "YAML", "config.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	if _, ok := byQN["defaults.timeout"]; !ok {
		t.Error("expected defaults.timeout (anchored)")
	}
	if _, ok := byQN["production.host"]; !ok {
		t.Error("expected production.host")
	}
}

func TestExtractYAML_KeySanitization(t *testing.T) {
	src := []byte(`key.with.dots: value
key/with/slashes: value
`)
	result := Extract(src, "YAML", "config.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	// Dots and slashes are replaced with underscores so they don't collide
	// with the dotted-path qualified name.
	if _, ok := byQN["key_with_dots"]; !ok {
		t.Errorf("expected sanitized key 'key_with_dots', got: %v", keysOf(byQN))
	}
	if _, ok := byQN["key_with_slashes"]; !ok {
		t.Errorf("expected sanitized key 'key_with_slashes', got: %v", keysOf(byQN))
	}
}

func TestExtractYAML_LongScalarTruncated(t *testing.T) {
	long := strings.Repeat("x", 500)
	src := []byte("description: " + long + "\n")
	result := Extract(src, "YAML", "doc.yml")
	for _, s := range result.Symbols {
		if s.QualifiedName == "description" {
			if len(s.Signature) != 200 {
				t.Errorf("long scalar signature length = %d, want 200", len(s.Signature))
			}
		}
	}
}

func TestExtractYAML_Empty(t *testing.T) {
	result := Extract([]byte(""), "YAML", "empty.yml")
	if result == nil {
		t.Fatal("Extract returned nil for empty input")
	}
	if len(result.Symbols) != 0 {
		t.Errorf("expected 0 symbols for empty input, got %d", len(result.Symbols))
	}
}

func TestExtractYAML_Malformed(t *testing.T) {
	// Malformed YAML should not panic; we accept whatever the parser produces.
	src := []byte("key: value\n  bad indent: foo\n[unclosed")
	result := Extract(src, "YAML", "bad.yml")
	if result == nil {
		t.Fatal("Extract returned nil for malformed YAML")
	}
	// No assertion on symbol count — just no panic.
}

func TestExtractJSON(t *testing.T) {
	src := []byte(`{
  "name": "pincherMCP",
  "version": "0.2.1",
  "scripts": {
    "build": "go build",
    "test": "go test ./..."
  },
  "keywords": ["mcp", "go", "search"]
}`)
	result := Extract(src, "JSON", "package.json")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	if got := byQN["name"].Signature; got != "pincherMCP" {
		t.Errorf("name signature = %q, want pincherMCP", got)
	}
	if _, ok := byQN["scripts.build"]; !ok {
		t.Error("expected scripts.build")
	}
	if _, ok := byQN["keywords.0"]; !ok {
		t.Error("expected keywords.0 (sequence element)")
	}
	for _, s := range result.Symbols {
		if s.Kind != "Setting" {
			t.Errorf("JSON symbol %q kind = %q, want Setting", s.QualifiedName, s.Kind)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Scalar Setting byte-range correctness (LATENT_ISSUES #3)
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractYAML_ScalarLastInMappingDoesNotOverextend(t *testing.T) {
	// A scalar that's the last key in its parent mapping should end at the
	// end of its own line, not extend through every aunt/uncle. Pre-fix,
	// `services.web.image` would cover ports/environment/api too.
	src := []byte(`services:
  web:
    image: nginx:1.25
    ports:
      - "80:80"
    environment:
      LOG_LEVEL: info
  api:
    image: myapp:latest
`)
	result := Extract(src, "YAML", "docker-compose.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}

	webImage, ok := byQN["services.web.image"]
	if !ok {
		t.Fatalf("expected services.web.image, got: %v", keysOf(byQN))
	}
	body := string(src[webImage.StartByte:webImage.EndByte])

	if strings.Contains(body, "ports") {
		t.Errorf("services.web.image leaks into ports — over-extending. body:\n%s", body)
	}
	if strings.Contains(body, "environment") {
		t.Errorf("services.web.image leaks into environment. body:\n%s", body)
	}
	if strings.Contains(body, "myapp") {
		t.Errorf("services.web.image leaks into services.api. body:\n%s", body)
	}
	if !strings.Contains(body, "nginx:1.25") {
		t.Errorf("services.web.image missing the actual value. body:\n%s", body)
	}
}

func TestExtractYAML_TopLevelTrailingScalar(t *testing.T) {
	// A top-level scalar that comes after a complex mapping should not
	// extend backwards — its byte range should be just its own line.
	src := []byte(`services:
  web:
    image: nginx
version: "3.8"
`)
	result := Extract(src, "YAML", "x.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}

	version := byQN["version"]
	body := string(src[version.StartByte:version.EndByte])
	if strings.Contains(body, "services") || strings.Contains(body, "image") {
		t.Errorf("version Setting should be one line only. body:\n%s", body)
	}
	if !strings.Contains(body, "3.8") {
		t.Errorf("version body missing the value. body:\n%s", body)
	}
}

func TestExtractYAML_BlockScalarLiteralRespectsIndent(t *testing.T) {
	// Block scalars (`|`) span multiple lines until an outdent.
	src := []byte(`description: |
  line one of block
  line two of block
  line three of block
next_key: foo
`)
	result := Extract(src, "YAML", "x.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}

	desc := byQN["description"]
	body := string(src[desc.StartByte:desc.EndByte])

	for _, want := range []string{"line one of block", "line two of block", "line three of block"} {
		if !strings.Contains(body, want) {
			t.Errorf("description body missing %q\nbody:\n%s", want, body)
		}
	}
	if strings.Contains(body, "next_key") {
		t.Errorf("description block scalar leaks into next_key:\n%s", body)
	}
}

func TestExtractYAML_BlockScalarFoldedRespectsIndent(t *testing.T) {
	src := []byte(`summary: >
  this folded
  scalar wraps
  multiple lines
done: true
`)
	result := Extract(src, "YAML", "x.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	body := string(src[byQN["summary"].StartByte:byQN["summary"].EndByte])
	if !strings.Contains(body, "multiple lines") {
		t.Errorf("folded scalar should include all content lines:\n%s", body)
	}
	if strings.Contains(body, "done") {
		t.Errorf("folded scalar leaked into next sibling key:\n%s", body)
	}
}

func TestExtractYAML_QuotedScalarSingleLine(t *testing.T) {
	// Quoted scalars are still single-line.
	src := []byte(`label: "hello world"
other: 42
`)
	result := Extract(src, "YAML", "x.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	body := string(src[byQN["label"].StartByte:byQN["label"].EndByte])
	if strings.Contains(body, "other") {
		t.Errorf("quoted scalar over-extends:\n%s", body)
	}
}

func TestExtractYAML_SequenceElementScalar(t *testing.T) {
	// Sequence elements that are scalars should also have one-line ranges.
	src := []byte(`tags:
  - first
  - second
  - third
next: val
`)
	result := Extract(src, "YAML", "x.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	// tags.0, tags.1, tags.2 should each be a one-line scalar
	for _, qn := range []string{"tags.0", "tags.1", "tags.2"} {
		s, ok := byQN[qn]
		if !ok {
			t.Errorf("missing %q", qn)
			continue
		}
		body := string([]byte(src)[s.StartByte:s.EndByte])
		// The last sequence element ("third") would over-extend into next:
		// pre-fix; verify that it doesn't.
		if strings.Contains(body, "next: val") {
			t.Errorf("%s sequence element leaks into next:\n%s", qn, body)
		}
	}
}

func TestExtractYAML_MappingStillCoversWholeSubtree(t *testing.T) {
	// Sanity: the fix should NOT break the existing mapping behaviour.
	// Retrieving services.web should still return image + ports + environment.
	src := []byte(`services:
  web:
    image: nginx
    ports:
      - "80:80"
    environment:
      LOG_LEVEL: info
  api:
    image: other
`)
	result := Extract(src, "YAML", "compose.yml")
	byQN := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byQN[s.QualifiedName] = s
	}
	web := byQN["services.web"]
	body := string(src[web.StartByte:web.EndByte])
	for _, want := range []string{"image: nginx", "ports", "80:80", "environment", "LOG_LEVEL"} {
		if !strings.Contains(body, want) {
			t.Errorf("services.web mapping missing %q\nbody:\n%s", want, body)
		}
	}
	if strings.Contains(body, "api:") || strings.Contains(body, "image: other") {
		t.Errorf("services.web should not leak into services.api:\n%s", body)
	}
}

func TestExtractYAML_ModuleName(t *testing.T) {
	result := Extract([]byte("foo: bar\n"), "YAML", "infrastructure/ansible/site.yml")
	if result.Module != "site" {
		t.Errorf("Module = %q, want 'site'", result.Module)
	}
}

func keysOf(m map[string]ExtractedSymbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// #71 — Ansible-aware edges (RENDERS slice)
// ─────────────────────────────────────────────────────────────────────────────

// TestExtractYAML_AnsibleRendersEdge_FullForm pins the canonical
// Ansible task syntax: `- name: ...` with `template:` action whose
// args include `src: foo.j2`. This is the most common form in real
// Ansible role tasks.
func TestExtractYAML_AnsibleRendersEdge_FullForm(t *testing.T) {
	src := `---
- name: render dhcp config
  template:
    src: dhcp.j2
    dest: /etc/dnsmasq/dhcp.conf
- name: render firewall rules
  template:
    src: firewall.j2
    dest: /etc/firewall.conf
`
	r := Extract([]byte(src), "YAML", "roles/network/tasks/main.yml")
	if r == nil {
		t.Fatal("nil result")
	}

	got := edgesByName(r.Edges, "RENDERS")
	for _, want := range []string{"dhcp.j2", "firewall.j2"} {
		if !contains(got, want) {
			t.Errorf("missing RENDERS edge to %q; got: %v", want, got)
		}
	}
}

// TestExtractYAML_AnsibleRendersEdge_FlowForm pins the short form:
// `template: { src: foo.j2 }` inline. Ansible accepts both.
func TestExtractYAML_AnsibleRendersEdge_FlowForm(t *testing.T) {
	src := `---
- template: { src: bar.j2, dest: /etc/bar.conf }
`
	r := Extract([]byte(src), "YAML", "roles/x/tasks/main.yml")
	got := edgesByName(r.Edges, "RENDERS")
	if !contains(got, "bar.j2") {
		t.Errorf("missing RENDERS edge to bar.j2; got: %v", got)
	}
}

// TestExtractYAML_AnsibleRendersEdge_PathFiltering — the edge MUST NOT
// fire for files outside Ansible-canonical paths. A Helm values.yaml
// or k8s manifest with a `template:` key has different semantics and
// shouldn't produce a render edge.
func TestExtractYAML_AnsibleRendersEdge_PathFiltering(t *testing.T) {
	src := `---
spec:
  template:
    src: not-ansible.j2
`
	for _, path := range []string{
		"helm/values.yaml",
		"k8s/deployment.yaml",
		"compose.yml",
	} {
		t.Run(path, func(t *testing.T) {
			r := Extract([]byte(src), "YAML", path)
			renders := edgesByName(r.Edges, "RENDERS")
			if len(renders) != 0 {
				t.Errorf("non-Ansible path %q produced RENDERS edges %v", path, renders)
			}
		})
	}
}

// TestExtractYAML_AnsibleRendersEdge_PlaybookPath asserts top-level
// playbook conventions (site.yml, deploy.yml, playbook*.yml) trigger
// the Ansible scan. A playbook with a `template:` task should still
// produce the edge.
func TestExtractYAML_AnsibleRendersEdge_PlaybookPath(t *testing.T) {
	src := `---
- hosts: all
  tasks:
    - template:
        src: motd.j2
        dest: /etc/motd
`
	for _, path := range []string{
		"site.yml",
		"deploy.yaml",
		"playbook-prod.yml",
	} {
		t.Run(path, func(t *testing.T) {
			r := Extract([]byte(src), "YAML", path)
			got := edgesByName(r.Edges, "RENDERS")
			if !contains(got, "motd.j2") {
				t.Errorf("playbook path %q didn't trigger Ansible scan; got edges: %v", path, got)
			}
		})
	}
}

// TestExtractYAML_AnsibleRendersEdge_FromModule — the edge's FromQN
// MUST be the file's module (basename minus extension). The indexer's
// resolution pass keys on that to attach the edge to the right symbol.
func TestExtractYAML_AnsibleRendersEdge_FromModule(t *testing.T) {
	src := `---
- template:
    src: foo.j2
`
	r := Extract([]byte(src), "YAML", "roles/web/tasks/main.yml")
	if len(r.Edges) == 0 {
		t.Fatal("no edges")
	}
	got := r.Edges[0].FromQN
	if got != "main" {
		t.Errorf("FromQN = %q, want %q (module name)", got, "main")
	}
}

// TestExtractYAML_AnsibleRendersEdge_NoTemplateNoEdge confirms the
// presence of an Ansible-canonical path alone doesn't fire spurious
// edges — only actual `template:` tasks do.
func TestExtractYAML_AnsibleRendersEdge_NoTemplateNoEdge(t *testing.T) {
	src := `---
- name: install package
  apt:
    name: nginx
    state: present
`
	r := Extract([]byte(src), "YAML", "roles/x/tasks/main.yml")
	renders := edgesByName(r.Edges, "RENDERS")
	if len(renders) != 0 {
		t.Errorf("non-template task in Ansible path emitted RENDERS edges: %v", renders)
	}
}

// edgesByName extracts ToName values from edges of a given kind.
func edgesByName(edges []ExtractedEdge, kind string) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e.Kind == kind {
			out = append(out, e.ToName)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
