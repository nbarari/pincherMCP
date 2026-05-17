package ast

import (
	"testing"
)

// TestExtractYAML_AnsibleUsesVar_TaskStringScan pins the positive case:
// an Ansible task with `{{ var }}` substitutions inside string values
// produces USES_VAR edges with Confidence 0.85 (regex-tier).
func TestExtractYAML_AnsibleUsesVar_TaskStringScan(t *testing.T) {
	src := `---
- name: configure
  shell: psql -h {{ db_host }} -U {{ db_user }} -c "SELECT 1"
  when: "{{ enable_db }}"
  loop: "{{ targets }}"
`
	r := Extract([]byte(src), "YAML", "roles/app/tasks/main.yml")
	got := map[string]float64{}
	for _, e := range r.Edges {
		if e.Kind == "USES_VAR" {
			got[e.ToName] = e.Confidence
		}
	}
	for _, want := range []string{"db_host", "db_user", "enable_db", "targets"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing USES_VAR edge for %q; got %v", want, got)
		}
	}
	for name, c := range got {
		if c != 0.85 {
			t.Errorf("yaml USES_VAR edge %q has Confidence=%v, want 0.85", name, c)
		}
	}
}

// TestExtractYAML_AnsibleUsesVar_NonAnsibleFileNoEdges pins the path
// gate: a generic YAML file (Helm values, k8s manifest) with `{{ var }}`
// expressions emits ZERO USES_VAR edges. Without the gate, every k8s
// values.yaml in a repo would produce noise edges that the resolver
// then has to drop.
func TestExtractYAML_AnsibleUsesVar_NonAnsibleFileNoEdges(t *testing.T) {
	src := `---
db:
  host: "{{ .Values.db.host }}"
  port: "{{ .Values.db.port }}"
`
	r := Extract([]byte(src), "YAML", "charts/myapp/values.yaml")
	for _, e := range r.Edges {
		if e.Kind == "USES_VAR" {
			t.Errorf("non-Ansible YAML produced USES_VAR edge: %+v", e)
		}
	}
}

// TestExtractYAML_AnsibleUsesVar_DedupePerName pins the dedup behavior:
// referencing the same var five times in one file produces ONE edge,
// not five. The graph cares about "does file X reference var Y?", not
// raw occurrence count — the latter inflates row volume without changing
// any audit answer.
func TestExtractYAML_AnsibleUsesVar_DedupePerName(t *testing.T) {
	src := `---
- name: first
  shell: "{{ db_host }}"
- name: second
  shell: "{{ db_host }}/secondary"
- name: third
  shell: "echo {{ db_host }} {{ db_host }}"
`
	r := Extract([]byte(src), "YAML", "playbook.yml")
	count := 0
	for _, e := range r.Edges {
		if e.Kind == "USES_VAR" && e.ToName == "db_host" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 deduped USES_VAR edge for db_host, got %d", count)
	}
}

// TestExtractYAML_AnsibleUsesVar_SkipsReserved pins that Jinja-reserved
// names (`loop`, `true`) inside YAML string values do NOT emit edges —
// they're the same skip-set the Jinja extractor honors.
func TestExtractYAML_AnsibleUsesVar_SkipsReserved(t *testing.T) {
	src := `---
- name: loop body
  debug: msg="iter {{ loop }} of {{ true }}"
`
	r := Extract([]byte(src), "YAML", "tasks/foo.yml")
	for _, e := range r.Edges {
		if e.Kind != "USES_VAR" {
			continue
		}
		switch e.ToName {
		case "loop", "true", "false", "none":
			t.Errorf("reserved name %q should not emit USES_VAR edge", e.ToName)
		}
	}
}
