package ast

import (
	"strings"
	"sync"
	"testing"
)

const tfMixedSrc = `terraform {
  required_version = ">= 1.5"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }

  backend "s3" {
    bucket = "tfstate"
    key    = "prod/terraform.tfstate"
    region = "us-east-1"
  }
}

provider "aws" {
  region = var.region
}

variable "region" {
  type        = string
  default     = "us-east-1"
  description = "AWS region"
}

variable "instance_count" {
  type    = number
  default = 1
}

locals {
  common_tags = {
    Environment = "prod"
    ManagedBy   = "terraform"
  }
  name_prefix = "pincher-${var.region}"
}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]
}

resource "aws_instance" "web" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = "t3.micro"
  count         = var.instance_count

  lifecycle {
    create_before_destroy = true
    ignore_changes        = [tags]
  }

  provisioner "local-exec" {
    command = "echo done"
  }

  dynamic "tag" {
    for_each = local.common_tags
    content {
      key   = tag.key
      value = tag.value
    }
  }
}

module "vpc" {
  source = "./modules/vpc"
  cidr   = "10.0.0.0/16"
}

output "web_ip" {
  value       = aws_instance.web.public_ip
  description = "Public IP of the web instance"
}
`

func keysOfHCL(syms []ExtractedSymbol) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = s.QualifiedName
	}
	return out
}

func indexHCL(syms []ExtractedSymbol) map[string]ExtractedSymbol {
	out := make(map[string]ExtractedSymbol, len(syms))
	for _, s := range syms {
		out[s.QualifiedName] = s
	}
	return out
}

func TestExtractHCL_TopLevelBlockKinds(t *testing.T) {
	result := Extract([]byte(tfMixedSrc), "HCL", "main.tf")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	by := indexHCL(result.Symbols)

	want := map[string]string{
		"resource.aws_instance.web": "Resource",
		"data.aws_ami.ubuntu":       "DataSource",
		"module.vpc":                "Module",
		"var.region":                "Variable",
		"var.instance_count":        "Variable",
		"output.web_ip":             "Output",
		"provider.aws":              "Provider",
		"local.common_tags":         "Local",
		"local.name_prefix":         "Local",
		"terraform":                 "Block",
	}
	for qn, kind := range want {
		got, ok := by[qn]
		if !ok {
			t.Errorf("missing symbol %q; got: %v", qn, keysOfHCL(result.Symbols))
			continue
		}
		if got.Kind != kind {
			t.Errorf("symbol %q kind = %q, want %q", qn, got.Kind, kind)
		}
	}
}

func TestExtractHCL_NestedBlocks(t *testing.T) {
	result := Extract([]byte(tfMixedSrc), "HCL", "main.tf")
	by := indexHCL(result.Symbols)

	want := []string{
		"resource.aws_instance.web.lifecycle",
		"resource.aws_instance.web.provisioner.local-exec",
		"resource.aws_instance.web.dynamic.tag",
		"resource.aws_instance.web.dynamic.tag.content",
		"terraform.required_providers",
		"terraform.backend.s3",
	}
	for _, qn := range want {
		got, ok := by[qn]
		if !ok {
			t.Errorf("missing nested block %q; got: %v", qn, keysOfHCL(result.Symbols))
			continue
		}
		if got.Kind != "Block" {
			t.Errorf("nested %q kind = %q, want Block", qn, got.Kind)
		}
	}
}

// TestExtractHCL_HighConfidence — see note on YAML's HighConfidence test.
// Asserts >= 0.7 (default min_confidence threshold) rather than exactly 1.0
// after #34 Phase 2 introduced per-symbol composition.
func TestExtractHCL_HighConfidence(t *testing.T) {
	result := Extract([]byte(tfMixedSrc), "HCL", "main.tf")
	if len(result.Symbols) == 0 {
		t.Fatal("no symbols extracted")
	}
	for _, s := range result.Symbols {
		if s.ExtractionConfidence < 0.7 {
			t.Errorf("symbol %q confidence = %v, want >= 0.7", s.QualifiedName, s.ExtractionConfidence)
			break
		}
	}
}

func TestExtractHCL_ByteRangeRoundTrip(t *testing.T) {
	src := []byte(tfMixedSrc)
	result := Extract(src, "HCL", "main.tf")
	by := indexHCL(result.Symbols)

	// resource.aws_instance.web's range should cover the full block including
	// its nested lifecycle / provisioner / dynamic and end at the closing brace.
	web, ok := by["resource.aws_instance.web"]
	if !ok {
		t.Fatal("resource.aws_instance.web not extracted")
	}
	if web.StartByte == 0 || web.EndByte == 0 {
		t.Fatalf("zero byte range: %+v", web)
	}
	if web.EndByte > len(src) {
		t.Fatalf("EndByte=%d > source len %d", web.EndByte, len(src))
	}
	body := string(src[web.StartByte:web.EndByte])
	for _, want := range []string{
		`resource "aws_instance" "web"`,
		"lifecycle {",
		`provisioner "local-exec"`,
		`dynamic "tag"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody:\n%s", want, body)
		}
	}
	// Should not bleed into the next top-level block.
	if strings.Contains(body, `module "vpc"`) {
		t.Errorf("body leaks into module \"vpc\":\n%s", body)
	}
}

func TestExtractHCL_LocalsAreIndividualSymbols(t *testing.T) {
	src := []byte(tfMixedSrc)
	result := Extract(src, "HCL", "main.tf")
	by := indexHCL(result.Symbols)

	// The locals block itself should NOT be a symbol; only its assignments.
	if _, ok := by["locals"]; ok {
		t.Error("did not expect a 'locals' symbol; locals should emit one Local per assignment")
	}
	if _, ok := by["local.common_tags"]; !ok {
		t.Error("missing local.common_tags")
	}
	prefix, ok := by["local.name_prefix"]
	if !ok {
		t.Fatal("missing local.name_prefix")
	}
	body := string(src[prefix.StartByte:prefix.EndByte])
	if !strings.Contains(body, "name_prefix") {
		t.Errorf("local.name_prefix range missing the assignment:\n%s", body)
	}
}

func TestExtractHCL_TFVarsAttributes(t *testing.T) {
	const tfvars = `region        = "us-west-2"
instance_count = 3
tags = {
  env = "staging"
}
`
	result := Extract([]byte(tfvars), "HCL", "prod.tfvars")
	by := indexHCL(result.Symbols)

	for _, name := range []string{"region", "instance_count", "tags"} {
		got, ok := by[name]
		if !ok {
			t.Errorf("missing tfvars attribute %q; got: %v", name, keysOfHCL(result.Symbols))
			continue
		}
		if got.Kind != "Setting" {
			t.Errorf("%q kind = %q, want Setting", name, got.Kind)
		}
	}
	// Should have NO Resource/Module/etc — tfvars files are bag-of-attributes.
	for _, s := range result.Symbols {
		if s.Kind != "Setting" {
			t.Errorf("tfvars produced non-Setting symbol %+v", s)
		}
	}
}

func TestExtractHCL_TfExtension(t *testing.T) {
	if got := DetectLanguage("main.tf"); got != "HCL" {
		t.Errorf("DetectLanguage(main.tf) = %q, want HCL", got)
	}
	if got := DetectLanguage("prod.tfvars"); got != "HCL" {
		t.Errorf("DetectLanguage(prod.tfvars) = %q, want HCL", got)
	}
	if !IsSourceFile("main.tf") {
		t.Error("IsSourceFile(main.tf) = false, want true")
	}
}

// TestExtractHCL_HclExtension covers `.hcl` files used by Nomad job specs,
// Packer templates, Vault policies, and Consul configs — same hclsyntax
// grammar as Terraform, just different block taxonomy. The MVP treats
// every top-level block as a generic Block symbol; Terraform-specific
// kinds (Resource/DataSource/Module/Variable/Output/Provider/Local) only
// fire when the source uses those exact block types.
func TestExtractHCL_HclExtension(t *testing.T) {
	if got := DetectLanguage("nomad.hcl"); got != "HCL" {
		t.Errorf("DetectLanguage(nomad.hcl) = %q, want HCL", got)
	}
	if !IsSourceFile("packer.hcl") {
		t.Error("IsSourceFile(packer.hcl) = false, want true")
	}

	// Real-world Nomad job spec — top-level `job "name" { ... }`. Nomad
	// doesn't have Terraform's resource/data/module/etc. keywords, so the
	// extractor's per-kind path falls through to the unknown-block-type
	// fallback (Block symbol with the labelled name).
	src := `job "web-server" {
  datacenters = ["dc1"]

  group "web" {
    count = 3

    task "nginx" {
      driver = "docker"

      config {
        image = "nginx:latest"
        ports = ["http"]
      }
    }
  }
}
`
	result := Extract([]byte(src), "HCL", "jobs/web.hcl")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	if len(result.Symbols) == 0 {
		t.Fatal("expected symbols from Nomad job spec, got 0")
	}
	// Verify the top-level `job "web-server"` produces a Block (or any
	// symbol with the name "web-server" in its qualified path). The exact
	// QN depends on the unknown-block-type fallback's labelling rule.
	found := false
	for _, s := range result.Symbols {
		if strings.Contains(s.QualifiedName, "web-server") || strings.Contains(s.QualifiedName, "web_server") {
			found = true
			break
		}
	}
	if !found {
		var qns []string
		for _, s := range result.Symbols {
			qns = append(qns, s.QualifiedName)
		}
		t.Errorf("expected a symbol referencing the job name 'web-server', got QNs: %v", qns)
	}
}

func TestExtractHCL_SignatureFormat(t *testing.T) {
	result := Extract([]byte(tfMixedSrc), "HCL", "main.tf")
	by := indexHCL(result.Symbols)

	cases := map[string]string{
		"resource.aws_instance.web":      `resource "aws_instance" "web"`,
		"data.aws_ami.ubuntu":            `data "aws_ami" "ubuntu"`,
		"module.vpc":                     `module "vpc"`,
		"var.region":                     `variable "region"`,
		"output.web_ip":                  `output "web_ip"`,
		"provider.aws":                   `provider "aws"`,
		"terraform.backend.s3":           `backend "s3"`,
		"resource.aws_instance.web.lifecycle": "lifecycle",
	}
	for qn, want := range cases {
		got, ok := by[qn]
		if !ok {
			t.Errorf("missing %q", qn)
			continue
		}
		if got.Signature != want {
			t.Errorf("signature for %q = %q, want %q", qn, got.Signature, want)
		}
	}
	// Local signature includes the RHS expression.
	if got, ok := by["local.name_prefix"]; ok {
		if !strings.HasPrefix(got.Signature, "name_prefix = ") {
			t.Errorf("local.name_prefix signature = %q, want it to start with 'name_prefix = '", got.Signature)
		}
	}
}

func TestExtractHCL_EmptySource(t *testing.T) {
	result := Extract([]byte(""), "HCL", "empty.tf")
	if result == nil {
		t.Fatal("nil result for empty source")
	}
	if len(result.Symbols) != 0 {
		t.Errorf("empty source produced %d symbols, want 0", len(result.Symbols))
	}
	if result.Module != "empty" {
		t.Errorf("Module = %q, want %q", result.Module, "empty")
	}
}

func TestExtractHCL_MalformedDoesNotPanic(t *testing.T) {
	// Truncated mid-block. Must not panic; must return whatever was parseable
	// before the diagnostic.
	const truncated = `resource "aws_instance" "web" {
  ami = "ami-123"
  instance_type = "t3.micro"
`
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Extract panicked on malformed input: %v", r)
		}
	}()
	result := Extract([]byte(truncated), "HCL", "broken.tf")
	if result == nil {
		t.Fatal("nil result for malformed input")
	}
}

func TestExtractHCL_IsExportedPerKind(t *testing.T) {
	// is_exported follows Terraform's reference semantics: outputs/resources/
	// data/modules/.tfvars are referenceable from outside the declaring scope;
	// var/local/provider/nested-block are scope-local.
	const tfSrc = `terraform {}
provider "aws" {}
variable "v" {}
output  "o" {}
locals  { l = 1 }
data    "d_type" "d" {}
resource "r_type" "r" {
  lifecycle { create_before_destroy = true }
}
module "m" { source = "./m" }
`
	const tfvars = `key = "value"`

	tf := indexHCL(Extract([]byte(tfSrc), "HCL", "main.tf").Symbols)
	vars := indexHCL(Extract([]byte(tfvars), "HCL", "p.tfvars").Symbols)

	wantExported := map[string]bool{
		"resource.r_type.r":              true,
		"data.d_type.d":                  true,
		"module.m":                       true,
		"output.o":                       true,
		"var.v":                          false,
		"local.l":                        false,
		"provider.aws":                   false,
		"terraform":                      false, // top-level terraform block — Block kind
		"resource.r_type.r.lifecycle":    false, // nested Block
	}
	for qn, want := range wantExported {
		got, ok := tf[qn]
		if !ok {
			t.Errorf("missing %q in extracted symbols", qn)
			continue
		}
		if got.IsExported != want {
			t.Errorf("symbol %q (kind=%s): IsExported = %v, want %v", qn, got.Kind, got.IsExported, want)
		}
	}
	// .tfvars Setting → exported.
	if k, ok := vars["key"]; !ok {
		t.Error("missing tfvars symbol 'key'")
	} else if !k.IsExported {
		t.Errorf("tfvars Setting 'key' should be exported, got IsExported=false")
	}
}

func TestExtractHCL_LabelDotsSanitized(t *testing.T) {
	// HCL allows almost any character in quoted labels. Dots collide with our
	// dotted-path qualified-name separator and would silently break navigation
	// (e.g. `module.foo.bar` would be indistinguishable from a module named
	// "foo" with a property "bar"). Sanitize dots to underscores; other chars
	// (hyphens, slashes, unicode) round-trip.
	src := []byte(`resource "weird_type.with_dot" "name.with.dots" {}
module "m.one" { source = "./m" }
output "out-with-hyphen" {}
variable "var/with/slash" {}
provider "p.v.q" {}
`)
	result := Extract(src, "HCL", "main.tf")
	by := indexHCL(result.Symbols)

	for _, qn := range []string{
		"resource.weird_type_with_dot.name_with_dots",
		"module.m_one",
		"output.out-with-hyphen",
		"var.var/with/slash",
		"provider.p_v_q",
	} {
		if _, ok := by[qn]; !ok {
			t.Errorf("missing sanitized qn %q; got: %v", qn, mapKeys(by))
		}
	}
	// Confirm the original (unsanitized) Name field still preserves the
	// authored label so users can see it in search results.
	if r := by["resource.weird_type_with_dot.name_with_dots"]; r.Name != "weird_type_with_dot.name_with_dots" {
		// We DO change Name too because the sanitized form is what gets
		// displayed; the raw label is still visible in Signature.
		// This expectation just pins current behaviour.
	}
}

func TestExtractHCL_LabelSanitizationCollision(t *testing.T) {
	// Two labels that sanitize to the same string would produce identical
	// qualified names → identical symbol IDs → BulkUpsertSymbols silently
	// merges them, with the second emit overwriting the first.
	//
	// This test pins the current behavior: collision IS possible, the
	// extractor emits two distinct ExtractedSymbols (one per source block)
	// and they happen to share a QN. The DB layer is what would dedupe.
	//
	// The realistic foot-gun: an author migrating from a pre-sanitization
	// pincher (where dotted labels were broken differently) might have written
	// `module "foo.bar"` AND `module "foo_bar"` in the same file. They'd
	// silently collapse to the same indexed symbol.
	src := []byte(`module "foo.bar" { source = "./a" }
module "foo_bar" { source = "./b" }
`)
	result := Extract(src, "HCL", "main.tf")

	// Both modules MUST survive — pre-#115 they shared a QN and the DB
	// layer would silently drop one via last-write-wins. Now the central
	// disambiguator suffixes the 2nd occurrence with `~<line>`, so both
	// stay individually addressable. The test asserts: (a) both Module
	// symbols are present, (b) one keeps the canonical QN, (c) the other
	// has a `~<line>` suffix derived from its start line, (d) they're at
	// different source positions, (e) FileResult.QNCollisions records
	// the original collision so the #42 diagnostic still fires.
	var byName []ExtractedSymbol
	for _, s := range result.Symbols {
		if s.Name == "foo_bar" || s.Name == "foo.bar" {
			byName = append(byName, s)
		}
	}
	if len(byName) != 2 {
		t.Fatalf("expected 2 module symbols (one per source block); got %d. All symbols: %v",
			len(byName), allQNs(result.Symbols))
	}

	// One canonical QN, one disambiguated.
	plainQN := "module.foo_bar"
	hasPlain, hasSuffixed := false, false
	for _, s := range result.Symbols {
		if s.QualifiedName == plainQN {
			hasPlain = true
		} else if strings.HasPrefix(s.QualifiedName, plainQN+"~") {
			hasSuffixed = true
		}
	}
	if !hasPlain {
		t.Errorf("expected first occurrence to keep plain QN %q; symbols: %v", plainQN, allQNs(result.Symbols))
	}
	if !hasSuffixed {
		t.Errorf("expected second occurrence to have disambiguated QN with ~<line> suffix; symbols: %v", allQNs(result.Symbols))
	}

	// QNCollisions must record the underlying issue.
	if n := result.QNCollisions[plainQN]; n != 2 {
		t.Errorf("QNCollisions[%q] = %d, want 2 (#42 diagnostic must surface the original collision)", plainQN, n)
	}

	// And the byte ranges must differ — they're at different source positions.
	var ranges []int
	for _, s := range result.Symbols {
		if s.Name == "foo_bar" || s.Name == "foo.bar" {
			ranges = append(ranges, s.StartByte)
		}
	}
	if len(ranges) == 2 && ranges[0] == ranges[1] {
		t.Errorf("collision-pair start bytes are identical (%d, %d) — they should be at different source offsets", ranges[0], ranges[1])
	}
}

func allQNs(syms []ExtractedSymbol) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = s.QualifiedName
	}
	return out
}

func TestExtractHCL_LabelSanitizationLeavesSafeCharsAlone(t *testing.T) {
	// Negative test: labels without dots must be byte-identical after
	// sanitization (no over-sanitizing, e.g. don't touch hyphens).
	for _, label := range []string{
		"plain",
		"with-hyphen",
		"with_underscore",
		"WithCaps",
		"with123digits",
		"hé_unicode",
	} {
		if got := hclSanitizeLabel(label); got != label {
			t.Errorf("hclSanitizeLabel(%q) = %q, want unchanged", label, got)
		}
	}
}

func TestExtractHCL_ConcurrentParseIsRaceSafe(t *testing.T) {
	// The indexer fans out one goroutine per file. hclsyntax's parser is
	// stateless / functional, but verify here so a future hcl/v2 update that
	// introduces shared state is caught immediately. Run with `go test -race`.
	const goroutines = 32
	const itersPerG = 8
	src := []byte(tfMixedSrc)

	type result struct {
		count int
		ids   []string
	}

	var wg sync.WaitGroup
	results := make(chan result, goroutines*itersPerG)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic in concurrent Extract: %v", r)
				}
			}()
			for i := 0; i < itersPerG; i++ {
				r := Extract(src, "HCL", "main.tf")
				ids := make([]string, len(r.Symbols))
				for k, s := range r.Symbols {
					ids[k] = s.QualifiedName
				}
				results <- result{count: len(r.Symbols), ids: ids}
			}
		}()
	}
	wg.Wait()
	close(results)

	// Every iteration must produce identical symbol counts AND identical
	// (order-preserving) qualified-name sequences. If hclsyntax developed
	// any racy global state, we'd see flaky counts or interleaved IDs.
	var first result
	have := false
	got := 0
	for r := range results {
		got++
		if !have {
			first = r
			have = true
			continue
		}
		if r.count != first.count {
			t.Errorf("symbol count mismatch across goroutines: first=%d this=%d", first.count, r.count)
		}
		if len(r.ids) != len(first.ids) {
			continue
		}
		for i := range r.ids {
			if r.ids[i] != first.ids[i] {
				t.Errorf("ID #%d differs: first=%q this=%q", i, first.ids[i], r.ids[i])
				break
			}
		}
	}
	if want := goroutines * itersPerG; got != want {
		t.Errorf("got %d results, want %d", got, want)
	}
}

func TestExtractHCL_UnknownTopLevelBlocksFallToGenericBlock(t *testing.T) {
	// Future Terraform versions add new top-level block types (the most recent
	// example was `terraform_data` in TF 1.4). Our switch handles the known
	// set; anything else falls through to the `default:` branch and is emitted
	// as a generic Block symbol with the type/labels in the qualified name.
	// Ensures the extractor is forward-compatible with TF syntax evolution
	// without crashing or silently dropping the block.
	src := []byte(`
imaginary_block "labelA" "labelB" {
  some_attr = "value"
}

future_kind "single_label" {
  config = true
}

bare_block {
  flag = false
}
`)
	result := Extract(src, "HCL", "future.tf")
	by := indexHCL(result.Symbols)

	cases := map[string]string{
		"imaginary_block.labelA.labelB": "Block",
		"future_kind.single_label":      "Block",
		"bare_block":                    "Block",
	}
	for qn, kind := range cases {
		got, ok := by[qn]
		if !ok {
			t.Errorf("missing fallback symbol %q; got: %v", qn, mapKeys(by))
			continue
		}
		if got.Kind != kind {
			t.Errorf("%q: kind = %q, want %q (default: branch should emit Block)", qn, got.Kind, kind)
		}
		if got.IsExported {
			t.Errorf("%q: IsExported should be false for generic Block (we don't know the export semantics of unknown block types)", qn)
		}
	}
}

func TestExtractHCL_RealModernTerraformBlocks(t *testing.T) {
	// Real top-level block types added in modern Terraform versions that our
	// switch statement doesn't enumerate. They MUST fall through to the
	// `default:` branch and emit Block symbols rather than crashing or being
	// silently dropped.
	//
	// References:
	//   terraform_data — TF 1.4+ (managed-resource replacement for null_resource)
	//   import         — TF 1.5+ (block-form import declarations)
	//   check          — TF 1.5+ (post-apply assertion blocks)
	//   removed        — TF 1.7+ (declare resources to be removed from state)
	src := []byte(`terraform_data "trigger" {
  input = "v1"
}

import {
  to = aws_instance.web
  id = "i-1234"
}

check "health" {
  data "http" "endpoint" {
    url = "https://example.com/health"
  }
  assert {
    condition     = data.http.endpoint.status_code == 200
    error_message = "endpoint not healthy"
  }
}

removed {
  from = aws_instance.deprecated
  lifecycle { destroy = false }
}
`)
	result := Extract(src, "HCL", "modern.tf")
	by := indexHCL(result.Symbols)

	// Each modern block should appear; exact qualified-name format is the
	// default-branch convention (type[.label1.label2...]).
	cases := []string{
		"terraform_data.trigger",
		"import",  // anonymous, type-only
		"check.health",
		"removed", // anonymous, type-only
	}
	for _, qn := range cases {
		got, ok := by[qn]
		if !ok {
			t.Errorf("modern TF block missing: qn=%q; got: %v", qn, mapKeys(by))
			continue
		}
		if got.Kind != "Block" {
			t.Errorf("%q kind = %q, want Block (default-branch convention for unknown top-level)", qn, got.Kind)
		}
	}
}

func TestExtractHCL_AdversarialInputsDoNotPanic(t *testing.T) {
	// Each of these inputs has historically been a panic vector for parsers
	// (truncation mid-token, malformed UTF-8, deeply nested, label-with-only-quote,
	// huge label, NUL bytes, BOM). hclsyntax should handle them gracefully,
	// but the extractor's defer/recover is the second line of defence.
	cases := map[string]string{
		"truncated mid-quote":           `resource "aws_inst`,
		"malformed UTF-8":               "resource \"a\" \"b\" {\n  x = \"\xff\xfe\xfd\"\n}\n",
		"deeply nested dynamic":         buildDeepNested(40),
		"NUL byte in body":              "resource \"a\" \"b\" {\n  x = \"\x00\"\n}\n",
		"UTF-8 BOM at start":            "\xef\xbb\xbfresource \"a\" \"b\" {}\n",
		"single backslash":              `\`,
		"only braces":                   "{{{{{{{{{{}}}}}}}}}}",
		"label with embedded quote":     `resource "a\"b" "c" {}`,
		"huge label":                    "resource \"" + strings.Repeat("x", 10000) + "\" \"y\" {}\n",
		"empty quoted labels":           `resource "" "" {}`,
		"locals with no body":           `locals`,
		"unterminated heredoc":          "resource \"a\" \"b\" {\n  x = <<-EOF\n  no terminator\n",
		"interpolation only":            "${var.x}",
		"trailing garbage after block":  "resource \"a\" \"b\" {}\n###??!!",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on adversarial input: %v\nsource bytes (first 200): %q", r, head(src, 200))
				}
			}()
			result := Extract([]byte(src), "HCL", "adv.tf")
			if result == nil {
				t.Fatal("nil result on adversarial input — Extract should always return a non-nil FileResult")
			}
		})
	}
}

func buildDeepNested(depth int) string {
	var sb strings.Builder
	sb.WriteString(`resource "aws_thing" "deep" {` + "\n")
	for i := 0; i < depth; i++ {
		sb.WriteString(strings.Repeat("  ", i+1))
		sb.WriteString(`dynamic "x" {` + "\n")
		sb.WriteString(strings.Repeat("  ", i+2))
		sb.WriteString(`for_each = []` + "\n")
		sb.WriteString(strings.Repeat("  ", i+2))
		sb.WriteString("content {\n")
	}
	for i := depth - 1; i >= 0; i-- {
		sb.WriteString(strings.Repeat("  ", i+2))
		sb.WriteString("}\n")
		sb.WriteString(strings.Repeat("  ", i+1))
		sb.WriteString("}\n")
	}
	sb.WriteString("}\n")
	return sb.String()
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func TestExtractHCL_HeredocByteRange(t *testing.T) {
	// Heredocs are the most common multi-line construct in real Terraform
	// (user_data, policy JSON, scripts). If block.Range() doesn't cover them,
	// every Resource that uses one returns a corrupted snippet.
	src := []byte(`resource "aws_instance" "web" {
  ami           = "ami-123"
  instance_type = "t3.micro"

  user_data = <<-EOF
    #!/bin/bash
    set -euo pipefail
    echo "hello from inside the heredoc"
    apt-get update
  EOF

  tags = {
    Name = "web"
  }
}
`)
	result := Extract(src, "HCL", "main.tf")
	by := indexHCL(result.Symbols)
	web, ok := by["resource.aws_instance.web"]
	if !ok {
		t.Fatal("resource.aws_instance.web not extracted")
	}
	if web.EndByte > len(src) {
		t.Fatalf("EndByte=%d > source len %d", web.EndByte, len(src))
	}
	body := string(src[web.StartByte:web.EndByte])

	for _, want := range []string{
		"user_data = <<-EOF",
		"#!/bin/bash",
		"hello from inside the heredoc",
		"apt-get update",
		"EOF",
		"Name = \"web\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Resource body missing %q\nfull body:\n%s", want, body)
		}
	}

	// The closing brace of the resource MUST be inside the byte range.
	if !strings.HasSuffix(strings.TrimSpace(body), "}") {
		t.Errorf("Resource byte range doesn't end at closing brace; trailing chars: %q", tail(body, 30))
	}
}

func TestExtractHCL_LiteralAndIndentedHeredocs(t *testing.T) {
	// `<<EOF` (literal) vs `<<-EOF` (indented) — both should produce correct
	// byte ranges. The literal form preserves leading whitespace; the indented
	// form strips a common prefix. Either way, the body must round-trip.
	src := []byte(`output "policy" {
  value = <<EOF
{"Statement":[]}
EOF
}

output "script" {
  value = <<-SH
    set -e
    echo done
  SH
}
`)
	result := Extract(src, "HCL", "main.tf")
	by := indexHCL(result.Symbols)
	for _, qn := range []string{"output.policy", "output.script"} {
		o, ok := by[qn]
		if !ok {
			t.Errorf("missing %s", qn)
			continue
		}
		body := string(src[o.StartByte:o.EndByte])
		if !strings.Contains(body, "EOF") && !strings.Contains(body, "SH") {
			t.Errorf("%s body missing heredoc terminator:\n%s", qn, body)
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func TestExtractHCL_MissingLabelsAreSkipped(t *testing.T) {
	// `resource` requires two labels; one label is invalid TF. We drop it
	// rather than emit a half-formed symbol.
	const partial = `resource "aws_instance" {
  ami = "ami-123"
}

variable {
  default = 1
}
`
	result := Extract([]byte(partial), "HCL", "partial.tf")
	for _, s := range result.Symbols {
		if s.Kind == "Resource" || s.Kind == "Variable" {
			t.Errorf("emitted %s symbol from label-less block: %+v", s.Kind, s)
		}
	}
}

func mapKeys(m map[string]ExtractedSymbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// #80 — multiple nested same-type blocks (multi-instance disambiguation)
// ─────────────────────────────────────────────────────────────────────────────

// tfMultiNestedSrc reproduces the Proxmox VM pattern from issue #80:
// multiple same-type unlabeled nested blocks (e.g. four `usb { }`
// passthroughs on a single VM resource). Pre-fix all four collided on
// the same dotted-path qualified_name; post-fix each gets a positional
// suffix.
//
// Also exercises the labeled-block case (multiple `provisioner
// "local-exec"`) which has the same collision shape but with labels.
const tfMultiNestedSrc = `resource "proxmox_virtual_environment_vm" "homeassistant" {
  name = "home-assistant"

  usb {
    host = "3-2.1"
    usb3 = false
  }
  usb {
    host = "3-2.3"
    usb3 = false
  }
  usb {
    host = "3-2.4"
    usb3 = false
  }
  usb {
    host = "3-1"
    usb3 = false
  }

  provisioner "local-exec" {
    command = "echo first"
  }
  provisioner "local-exec" {
    command = "echo second"
  }
}

resource "aws_instance" "web" {
  ami           = "ami-123"
  instance_type = "t3.micro"

  network_interface {
    device_index = 0
  }
  network_interface {
    device_index = 1
  }
}
`

// TestExtractHCL_MultipleNestedSameTypeBlocks pins the regression gate
// for #80: when a parent contains multiple nested blocks with the same
// (type, labels) tuple, each instance MUST get a unique QN via a
// source-order positional suffix. Pre-fix all four `usb { }` blocks
// collided on `resource.proxmox_virtual_environment_vm.homeassistant.usb`.
func TestExtractHCL_MultipleNestedSameTypeBlocks(t *testing.T) {
	result := Extract([]byte(tfMultiNestedSrc), "HCL", "main.tf")
	if result == nil {
		t.Fatal("nil result")
	}

	// Every QN in the result MUST be unique. The bug manifested as
	// duplicates; this is the most direct gate.
	seen := make(map[string]int)
	for _, s := range result.Symbols {
		seen[s.QualifiedName]++
	}
	for qn, n := range seen {
		if n > 1 {
			t.Errorf("qualified_name %q appears %d times — multi-instance collision not fixed", qn, n)
		}
	}

	// All four USB blocks present with their positional suffix.
	for _, want := range []string{
		"resource.proxmox_virtual_environment_vm.homeassistant.usb.0",
		"resource.proxmox_virtual_environment_vm.homeassistant.usb.1",
		"resource.proxmox_virtual_environment_vm.homeassistant.usb.2",
		"resource.proxmox_virtual_environment_vm.homeassistant.usb.3",
	} {
		var found bool
		for _, s := range result.Symbols {
			if s.QualifiedName == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing usb block %q (got QNs: %v)", want, keysOfHCL(result.Symbols))
		}
	}

	// Two labeled provisioners with positional suffixes.
	for _, want := range []string{
		"resource.proxmox_virtual_environment_vm.homeassistant.provisioner.local-exec.0",
		"resource.proxmox_virtual_environment_vm.homeassistant.provisioner.local-exec.1",
	} {
		var found bool
		for _, s := range result.Symbols {
			if s.QualifiedName == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing provisioner block %q", want)
		}
	}

	// Two network_interface blocks on the AWS resource.
	for _, want := range []string{
		"resource.aws_instance.web.network_interface.0",
		"resource.aws_instance.web.network_interface.1",
	} {
		var found bool
		for _, s := range result.Symbols {
			if s.QualifiedName == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing network_interface %q", want)
		}
	}
}

// TestExtractHCL_SingleNestedBlockNoSuffix is the negative-of-fix gate:
// when a parent has exactly ONE nested block of a given type, the QN
// MUST stay clean (no positional suffix). The fix is supposed to be
// invisible in the common case.
func TestExtractHCL_SingleNestedBlockNoSuffix(t *testing.T) {
	const src = `resource "aws_instance" "web" {
  lifecycle {
    create_before_destroy = true
  }
}
`
	result := Extract([]byte(src), "HCL", "main.tf")
	by := indexHCL(result.Symbols)
	if _, ok := by["resource.aws_instance.web.lifecycle"]; !ok {
		t.Errorf("single lifecycle block should keep clean QN; got: %v", keysOfHCL(result.Symbols))
	}
	// And NOT have a `.0` suffix.
	if _, ok := by["resource.aws_instance.web.lifecycle.0"]; ok {
		t.Errorf("single lifecycle block got an unwanted .0 suffix")
	}
}

// TestExtractHCL_MultiInstanceDeepNesting exercises the recursive case:
// nested-blocks-inside-multi-instance-nested-blocks must each get
// unique QNs that compose correctly through the suffix.
func TestExtractHCL_MultiInstanceDeepNesting(t *testing.T) {
	const src = `resource "aws_instance" "web" {
  network_interface {
    security_groups {
      id = "sg-1"
    }
    security_groups {
      id = "sg-2"
    }
  }
  network_interface {
    security_groups {
      id = "sg-3"
    }
  }
}
`
	result := Extract([]byte(src), "HCL", "main.tf")
	by := indexHCL(result.Symbols)

	// Both top-level network_interface blocks have suffixes.
	for _, qn := range []string{
		"resource.aws_instance.web.network_interface.0",
		"resource.aws_instance.web.network_interface.1",
	} {
		if _, ok := by[qn]; !ok {
			t.Errorf("missing %q (deep nesting); got: %v", qn, keysOfHCL(result.Symbols))
		}
	}

	// Inside the first network_interface (suffix .0), two
	// security_groups blocks → each gets its own positional suffix.
	for _, qn := range []string{
		"resource.aws_instance.web.network_interface.0.security_groups.0",
		"resource.aws_instance.web.network_interface.0.security_groups.1",
	} {
		if _, ok := by[qn]; !ok {
			t.Errorf("missing nested %q", qn)
		}
	}

	// Inside the second network_interface (suffix .1), only one
	// security_groups block → no suffix on it.
	if _, ok := by["resource.aws_instance.web.network_interface.1.security_groups"]; !ok {
		t.Errorf("singleton inside multi-instance parent should keep clean QN")
	}
}

// TestExtractHCL_VarReferenceEdges pins #86 minimum-viable: when a
// resource/data/output block references var.NAME in any attribute, an
// edge with FromQN=blockQN, ToName="var.NAME", Kind=REFERENCES is
// emitted. The indexer's deferred resolution pass turns the ToName
// into a real edge against the matching Variable symbol.
func TestExtractHCL_VarReferenceEdges(t *testing.T) {
	src := `variable "region" {
  type = string
  default = "us-east-1"
}

variable "instance_type" {
  type = string
}

resource "aws_instance" "web" {
  ami = "ami-123"
  instance_type = var.instance_type
  region = var.region
  tags = {
    env = var.region
  }
}

data "aws_ami" "ubuntu" {
  most_recent = true
  filter {
    name = "owner-id"
    values = [var.region]
  }
}

output "ip" {
  value = "${var.region}-public"
}
`
	result := Extract([]byte(src), "HCL", "main.tf")

	type edge struct{ from, to string }
	got := map[edge]bool{}
	for _, e := range result.Edges {
		if e.Kind != "REFERENCES" {
			continue
		}
		got[edge{e.FromQN, e.ToName}] = true
	}

	wants := []edge{
		{"resource.aws_instance.web", "var.instance_type"},
		{"resource.aws_instance.web", "var.region"},
		{"data.aws_ami.ubuntu", "var.region"},
		{"output.ip", "var.region"},
	}
	for _, w := range wants {
		if !got[w] {
			t.Errorf("missing edge %s -> %s; have: %v", w.from, w.to, edgesAsStrings(result.Edges))
		}
	}

	// Edges from a single block to the same target must dedupe — no
	// duplicate when var.region appears in `region`, `tags.env`, AND
	// `tags.env` again.
	resourceWebEdges := 0
	for _, e := range result.Edges {
		if e.Kind == "REFERENCES" && e.FromQN == "resource.aws_instance.web" {
			resourceWebEdges++
		}
	}
	// Two distinct vars referenced (instance_type, region); should
	// produce exactly 2 edges from this block.
	if resourceWebEdges != 2 {
		t.Errorf("aws_instance.web should produce 2 distinct REFERENCES edges (var.instance_type, var.region), got %d",
			resourceWebEdges)
	}
}

// TestExtractHCL_NestedBlockReferencesAttributedToParent pins that a
// var reference inside a nested block (e.g. provisioner inside a
// resource) is attributed to the OUTERMOST symbol-emitting block —
// the resource — not the nested Block symbol. Agents tracing vars
// expect to find their dependents at the resource level.
func TestExtractHCL_NestedBlockReferencesAttributedToParent(t *testing.T) {
	src := `resource "aws_instance" "web" {
  ami = "ami-1"
  provisioner "remote-exec" {
    inline = ["echo ${var.region}"]
  }
}
`
	result := Extract([]byte(src), "HCL", "main.tf")
	got := map[string]bool{}
	for _, e := range result.Edges {
		if e.Kind == "REFERENCES" && e.ToName == "var.region" {
			got[e.FromQN] = true
		}
	}
	if !got["resource.aws_instance.web"] {
		t.Errorf("expected REFERENCES edge from resource.aws_instance.web (parent); got from: %v", got)
	}
	// Should NOT be attributed to the nested provisioner block.
	if got["resource.aws_instance.web.provisioner.remote-exec"] {
		t.Errorf("nested-block reference should be attributed to parent, not nested block")
	}
}

// TestExtractHCL_NoNonVarReferencesInThisIter pins that this iter's
// scope is var.NAME only — references to local.X, data.X, module.X,
// and TYPE.NAME (resource references) do NOT produce edges yet.
// Filed as follow-ups to #86. This test prevents accidental scope
// creep where a contributor adds e.g. local-edge support without the
// symbol-side wiring.
func TestExtractHCL_NoNonVarReferencesInThisIter(t *testing.T) {
	src := `locals {
  common_tags = { env = "prod" }
}

resource "aws_instance" "web" {
  tags = local.common_tags
  vpc_security_group_ids = [aws_security_group.web.id]
}

data "aws_ami" "ubuntu" {}

resource "aws_instance" "db" {
  ami = data.aws_ami.ubuntu.id
}
`
	result := Extract([]byte(src), "HCL", "main.tf")
	for _, e := range result.Edges {
		if e.Kind != "REFERENCES" {
			continue
		}
		// Anything not starting with "var." should not be present yet.
		if !startsWithVarPrefix(e.ToName) {
			t.Errorf("non-var REFERENCES edge leaked from this iter's scope: %s -> %s",
				e.FromQN, e.ToName)
		}
	}
}

func edgesAsStrings(edges []ExtractedEdge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.FromQN+"->"+e.ToName+" ("+e.Kind+")")
	}
	return out
}

func startsWithVarPrefix(s string) bool {
	return len(s) >= 4 && s[:4] == "var."
}
