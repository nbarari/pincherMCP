package ast

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// hclExtractor parses Terraform .tf and .tfvars files via hashicorp/hcl/v2's
// hclsyntax parser (the same parser Terraform itself uses) and emits one
// symbol per top-level block, nested block, locals/tfvars assignment.
//
// Qualified names use Terraform's own reference convention with a leading
// namespace, so the dotted path is unambiguous and matches what users would
// type in a TF expression:
//
//	resource "aws_instance" "web" {}   →  resource.aws_instance.web   Resource
//	data "aws_ami" "u" {}              →  data.aws_ami.u              DataSource
//	module "vpc" {}                    →  module.vpc                  Module
//	variable "region" {}               →  var.region                  Variable
//	output "ip" {}                     →  output.ip                   Output
//	provider "aws" {}                  →  provider.aws                Provider
//	locals { x = 1 }                   →  local.x                     Local
//	terraform { backend "s3" {} }      →  terraform                   Block
//	                                      terraform.backend.s3        Block
//
// Nested blocks of any depth (lifecycle, provisioner, connection, dynamic,
// content, backend, required_providers, etc.) are emitted with kind "Block"
// and a qualified name extending the parent block's path.
//
// .tfvars files are bag-of-attributes; each top-level assignment becomes a
// "Setting" symbol with the attribute name as the qualified name.
//
// Confidence is 1.0 (real HCL parser, not regex).
type hclExtractor struct{}

func newHCLExtractor() *hclExtractor { return &hclExtractor{} }

func (h *hclExtractor) Languages() []string { return []string{"HCL"} }
func (h *hclExtractor) Extensions() map[string]string {
	return map[string]string{
		".tf":     "HCL",
		".tfvars": "HCL",
		".hcl":    "HCL",
	}
}
func (h *hclExtractor) Confidence() float64 { return 1.0 }

func (h *hclExtractor) Extract(source []byte, _, relPath string, _ ExtractOptions) (result *FileResult) {
	result = &FileResult{Module: hclModuleName(relPath)}
	if len(source) == 0 {
		return result
	}

	// Defensive recover: hclsyntax should not panic on any input, but a
	// malformed file shouldn't be allowed to take down the indexer goroutine
	// — the partial result we've collected so far is more useful than a crash.
	defer func() {
		if r := recover(); r != nil {
			if result == nil {
				result = &FileResult{Module: hclModuleName(relPath)}
			}
		}
	}()

	// hclsyntax.ParseConfig returns a partial AST even when diagnostics are
	// non-empty, so we ignore diags and extract whatever symbols we got.
	file, _ := hclsyntax.ParseConfig(source, relPath, hcl.Pos{Line: 1, Column: 1})
	if file == nil {
		return result
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return result
	}

	if strings.HasSuffix(strings.ToLower(relPath), ".tfvars") {
		result.Symbols = append(result.Symbols, hclTFVarsSymbols(body, source)...)
		return result
	}

	for _, blk := range hclSortedBlocks(body) {
		result.Symbols = append(result.Symbols, hclTopLevelBlockSymbols(blk, source)...)
	}

	// Reference edges (#86 — minimum viable: var.NAME only).
	//
	// Walk every block's attribute expressions, detect `var.NAME`
	// traversals, and emit one REFERENCES edge per (containing-block,
	// referenced-variable) pair. The indexer's deferred resolution
	// pass matches `ToName="var.NAME"` against Variable symbols whose
	// qualified_name is `var.NAME` and writes a real edge if it
	// resolves.
	//
	// Deferred to follow-ups: local.NAME (Local), data.TYPE.NAME
	// (DataSource), module.NAME (Module), TYPE.NAME (Resource),
	// each.value, count.index. The minimum-viable shape lets agents
	// `trace --name=region --direction=outbound` and see which
	// resources use a given variable, which is the dominant use case.
	for _, blk := range hclSortedBlocks(body) {
		result.Edges = append(result.Edges, hclCollectVarReferences(blk, "")...)
	}
	return result
}

// hclCollectVarReferences walks a block's body recursively, finding
// `var.NAME` traversals in every attribute expression, and emits one
// REFERENCES edge per (containing-block, var.NAME) pair the indexer
// can then resolve.
//
// parentQN tracks the dotted-path of the closest enclosing top-level
// block. Empty on the top-level call; populated when recursing into
// nested blocks. References inside a nested block (e.g. a
// `lifecycle` block within a resource) are attributed to the
// **outermost** symbol-emitting block rather than the nested one,
// because that's the entity an agent would `trace` against.
func hclCollectVarReferences(blk *hclsyntax.Block, parentQN string) []ExtractedEdge {
	if blk == nil || blk.Body == nil {
		return nil
	}

	// Determine this block's QN. For top-level recognised types we
	// re-derive the prefix; for nested blocks we inherit parentQN
	// (so references in a `provisioner` block under a resource are
	// attributed to the resource itself).
	var qn string
	if parentQN != "" {
		qn = parentQN
	} else {
		switch blk.Type {
		case "resource", "data":
			if len(blk.Labels) >= 2 {
				qn = blk.Type + "." + hclSanitizeLabel(blk.Labels[0]) + "." + hclSanitizeLabel(blk.Labels[1])
			}
		case "module", "variable", "output", "provider":
			if len(blk.Labels) >= 1 {
				ns := blk.Type
				if blk.Type == "variable" {
					ns = "var"
				}
				qn = ns + "." + hclSanitizeLabel(blk.Labels[0])
			}
		case "locals", "terraform":
			// `locals` references go to per-attribute Local symbols
			// (qn = local.NAME); skipped here because we don't have
			// the per-attribute name from the locals block alone.
			// `terraform` blocks usually don't reference vars.
			return nil
		}
	}
	if qn == "" {
		return nil
	}

	var out []ExtractedEdge
	seen := map[string]bool{}
	for _, attr := range blk.Body.Attributes {
		if attr.Expr == nil {
			continue
		}
		for _, trav := range attr.Expr.Variables() {
			if len(trav) < 2 {
				continue
			}
			root, ok := trav[0].(hcl.TraverseRoot)
			if !ok || root.Name != "var" {
				continue
			}
			next, ok := trav[1].(hcl.TraverseAttr)
			if !ok {
				continue
			}
			toName := "var." + next.Name
			edgeKey := qn + "->" + toName
			if seen[edgeKey] {
				continue
			}
			seen[edgeKey] = true
			out = append(out, ExtractedEdge{
				FromQN:     qn,
				ToName:     toName,
				Kind:       "REFERENCES",
				Confidence: 1.0,
			})
		}
	}
	// Recurse into nested blocks; references inside attribute the
	// edge to the outermost block so agents reasoning about a
	// resource see all its var dependencies in one place.
	for _, sub := range blk.Body.Blocks {
		out = append(out, hclCollectVarReferences(sub, qn)...)
	}
	return out
}

// hclTopLevelBlockSymbols converts a top-level .tf block into one or more
// ExtractedSymbol values, including nested blocks where applicable.
func hclTopLevelBlockSymbols(blk *hclsyntax.Block, source []byte) []ExtractedSymbol {
	rng := blk.Range()
	switch blk.Type {
	case "resource":
		if len(blk.Labels) < 2 {
			return nil
		}
		typeLabel, name := hclSanitizeLabel(blk.Labels[0]), hclSanitizeLabel(blk.Labels[1])
		qn := "resource." + typeLabel + "." + name
		out := []ExtractedSymbol{hclSymbol(typeLabel+"."+name, qn, "Resource", rng, hclBlockSignature(blk))}
		out = append(out, hclNestedBlockSymbols(blk.Body, qn)...)
		return out

	case "data":
		if len(blk.Labels) < 2 {
			return nil
		}
		typeLabel, name := hclSanitizeLabel(blk.Labels[0]), hclSanitizeLabel(blk.Labels[1])
		qn := "data." + typeLabel + "." + name
		out := []ExtractedSymbol{hclSymbol(typeLabel+"."+name, qn, "DataSource", rng, hclBlockSignature(blk))}
		out = append(out, hclNestedBlockSymbols(blk.Body, qn)...)
		return out

	case "module":
		if len(blk.Labels) < 1 {
			return nil
		}
		name := hclSanitizeLabel(blk.Labels[0])
		qn := "module." + name
		out := []ExtractedSymbol{hclSymbol(name, qn, "Module", rng, hclBlockSignature(blk))}
		out = append(out, hclNestedBlockSymbols(blk.Body, qn)...)
		return out

	case "variable":
		if len(blk.Labels) < 1 {
			return nil
		}
		name := hclSanitizeLabel(blk.Labels[0])
		qn := "var." + name
		out := []ExtractedSymbol{hclSymbol(name, qn, "Variable", rng, hclBlockSignature(blk))}
		out = append(out, hclNestedBlockSymbols(blk.Body, qn)...)
		return out

	case "output":
		if len(blk.Labels) < 1 {
			return nil
		}
		name := hclSanitizeLabel(blk.Labels[0])
		qn := "output." + name
		out := []ExtractedSymbol{hclSymbol(name, qn, "Output", rng, hclBlockSignature(blk))}
		out = append(out, hclNestedBlockSymbols(blk.Body, qn)...)
		return out

	case "provider":
		if len(blk.Labels) < 1 {
			return nil
		}
		name := hclSanitizeLabel(blk.Labels[0])
		qn := "provider." + name
		out := []ExtractedSymbol{hclSymbol(name, qn, "Provider", rng, hclBlockSignature(blk))}
		out = append(out, hclNestedBlockSymbols(blk.Body, qn)...)
		return out

	case "locals":
		// Don't emit a symbol for the locals block itself; emit one Local per
		// assignment so each is independently searchable.
		return hclLocalsSymbols(blk.Body, source)

	case "terraform":
		qn := "terraform"
		out := []ExtractedSymbol{hclSymbol("terraform", qn, "Block", rng, "terraform")}
		out = append(out, hclNestedBlockSymbols(blk.Body, qn)...)
		return out

	default:
		// Unknown top-level block (shouldn't happen in valid Terraform but we
		// surface it rather than drop it silently).
		qn := blk.Type
		if len(blk.Labels) > 0 {
			qn = qn + "." + strings.Join(hclSanitizeLabels(blk.Labels), ".")
		}
		out := []ExtractedSymbol{hclSymbol(qn, qn, "Block", rng, hclBlockSignature(blk))}
		out = append(out, hclNestedBlockSymbols(blk.Body, qn)...)
		return out
	}
}

// hclNestedBlockSymbols walks a body's nested blocks and emits one Block
// symbol per nested block, recursively. parentQN is the dotted-path prefix
// each nested block extends.
//
// **Multi-instance disambiguation (#80)**: HCL syntax allows multiple
// nested blocks of the same type under one parent — `multiple usb { }`
// passthroughs on a Proxmox VM, multiple `provisioner "local-exec" { }`
// on a Terraform resource, multiple `network_interface { }` on an AWS
// instance, etc. Each instance is semantically a separate symbol with
// its own byte range, but they'd all collide on the same dotted-path
// QN if we naively concatenated type+labels.
//
// First pass counts blocks per (type, sanitized-labels) key. Second
// pass appends a source-order positional suffix (`.0`, `.1`, ...) ONLY
// to the QN of blocks whose key has count > 1. Single-instance blocks
// keep their readable QN — the common case stays clean.
//
// Why source-order positional suffix and not a label-derived one:
// (a) the issue's "labelled-attribute-derived suffix" option requires
// per-block-type schema knowledge (Terraform-version-specific) that
// we don't have, and (b) source order is stable for a given file, so
// the QN survives re-indexing as long as block ordering is preserved.
func hclNestedBlockSymbols(body *hclsyntax.Body, parentQN string) []ExtractedSymbol {
	if body == nil {
		return nil
	}

	// Pass 1: count blocks per (type, sanitized-labels) key.
	count := make(map[string]int)
	for _, blk := range body.Blocks {
		count[hclBlockKey(blk)]++
	}

	// Pass 2: emit, suffixing only when count > 1.
	idx := make(map[string]int)
	var out []ExtractedSymbol
	for _, blk := range hclSortedBlocks(body) {
		key := hclBlockKey(blk)
		qn := parentQN + "." + blk.Type
		name := blk.Type
		if len(blk.Labels) > 0 {
			qn = qn + "." + strings.Join(hclSanitizeLabels(blk.Labels), ".")
			name = blk.Type + " " + strings.Join(blk.Labels, " ")
		}
		if count[key] > 1 {
			suffix := strconv.Itoa(idx[key])
			qn = qn + "." + suffix
			name = name + "[" + suffix + "]"
		}
		idx[key]++
		out = append(out, hclSymbol(name, qn, "Block", blk.Range(), hclBlockSignature(blk)))
		out = append(out, hclNestedBlockSymbols(blk.Body, qn)...)
	}
	return out
}

// hclBlockKey produces the dedup key used by hclNestedBlockSymbols's
// counting pass. Two blocks share a key iff their (type, sanitized
// labels) tuples are equal — which is exactly when their naive QN
// (without a positional suffix) would collide.
func hclBlockKey(blk *hclsyntax.Block) string {
	if len(blk.Labels) == 0 {
		return blk.Type
	}
	return blk.Type + "." + strings.Join(hclSanitizeLabels(blk.Labels), ".")
}

// hclLocalsSymbols emits one Local symbol per assignment inside a locals { } block.
func hclLocalsSymbols(body *hclsyntax.Body, source []byte) []ExtractedSymbol {
	if body == nil {
		return nil
	}
	var out []ExtractedSymbol
	for _, attr := range hclSortedAttrs(body) {
		out = append(out, hclSymbol(
			attr.Name,
			"local."+attr.Name,
			"Local",
			attr.SrcRange,
			hclAttrSignature(attr, source),
		))
	}
	return out
}

// hclTFVarsSymbols emits one Setting symbol per top-level attribute in a .tfvars file.
func hclTFVarsSymbols(body *hclsyntax.Body, source []byte) []ExtractedSymbol {
	if body == nil {
		return nil
	}
	var out []ExtractedSymbol
	for _, attr := range hclSortedAttrs(body) {
		out = append(out, hclSymbol(
			attr.Name,
			attr.Name,
			"Setting",
			attr.SrcRange,
			hclAttrSignature(attr, source),
		))
	}
	return out
}

// hclSymbol is the small constructor every HCL emitter funnels through so the
// shape stays consistent across kinds. IsExported follows Terraform's notion
// of what a parent module / external caller can reference (see hclKindExported).
func hclSymbol(name, qn, kind string, rng hcl.Range, sig string) ExtractedSymbol {
	return ExtractedSymbol{
		Name:          name,
		QualifiedName: qn,
		Kind:          kind,
		StartByte:     rng.Start.Byte,
		EndByte:       rng.End.Byte,
		StartLine:     rng.Start.Line,
		EndLine:       rng.End.Line,
		Signature:     sig,
		IsExported:    hclKindExported[kind],
	}
}

// hclKindExported encodes Terraform's reference semantics:
//   - Output: yes — outputs ARE the export mechanism (`module.X.outputs.Y`).
//   - Resource / DataSource: yes — addressable from any expression in the module.
//   - Module: yes — module instances are referenced as `module.NAME.X` from elsewhere.
//   - Setting (.tfvars): yes — these ARE the file's exports.
//   - Variable: no — inputs to the module, scope-local. SET by parent, not "exported".
//   - Local: no — private helpers, scope-local.
//   - Provider: no — infrastructure plumbing, not a data export.
//   - Block (nested lifecycle / provisioner / dynamic / backend / etc.): no —
//     internal to parent block, not separately addressable.
var hclKindExported = map[string]bool{
	"Resource":   true,
	"DataSource": true,
	"Module":     true,
	"Output":     true,
	"Setting":    true,
	"Variable":   false,
	"Local":      false,
	"Provider":   false,
	"Block":      false,
}

// hclSortedBlocks returns body.Blocks in stable source order.
func hclSortedBlocks(body *hclsyntax.Body) []*hclsyntax.Block {
	blocks := append([]*hclsyntax.Block(nil), body.Blocks...)
	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].Range().Start.Byte < blocks[j].Range().Start.Byte
	})
	return blocks
}

// hclSortedAttrs returns body.Attributes as a slice in stable source order
// (the underlying map iteration is non-deterministic).
func hclSortedAttrs(body *hclsyntax.Body) []*hclsyntax.Attribute {
	attrs := make([]*hclsyntax.Attribute, 0, len(body.Attributes))
	for _, a := range body.Attributes {
		attrs = append(attrs, a)
	}
	sort.Slice(attrs, func(i, j int) bool {
		return attrs[i].SrcRange.Start.Byte < attrs[j].SrcRange.Start.Byte
	})
	return attrs
}

// hclBlockSignature returns a short FTS-friendly description of an HCL block,
// reproducing the source-code header (e.g. `resource "aws_instance" "web"`).
func hclBlockSignature(blk *hclsyntax.Block) string {
	if len(blk.Labels) == 0 {
		return blk.Type
	}
	parts := make([]string, 0, 1+len(blk.Labels))
	parts = append(parts, blk.Type)
	for _, l := range blk.Labels {
		parts = append(parts, `"`+l+`"`)
	}
	return strings.Join(parts, " ")
}

// hclAttrSignature returns "name = <expr>" with the RHS truncated for FTS.
func hclAttrSignature(attr *hclsyntax.Attribute, source []byte) string {
	if attr.Expr == nil {
		return attr.Name
	}
	rng := attr.Expr.Range()
	if rng.Start.Byte < 0 || rng.End.Byte > len(source) || rng.End.Byte <= rng.Start.Byte {
		return attr.Name
	}
	rhs := strings.TrimSpace(string(source[rng.Start.Byte:rng.End.Byte]))
	const maxRHS = 200
	if len(rhs) > maxRHS {
		rhs = rhs[:maxRHS]
	}
	return attr.Name + " = " + rhs
}

// hclSanitizeLabel collapses dots in a label to underscores so the dotted-path
// qualified-name format isn't broken by labels that legally contain dots.
// Other characters (hyphens, underscores, slashes, unicode) round-trip
// unchanged. HCL labels are quoted strings so almost anything is technically
// legal — we only sanitize the one character that collides with the QN
// separator.
//
// The lossless authored label survives in Signature (e.g. `module "foo.bar"`),
// so search results still display the original. Sanitization here only
// affects the QN slot used as a unique-within-file identifier.
func hclSanitizeLabel(label string) string {
	if !strings.Contains(label, ".") {
		return label
	}
	return strings.ReplaceAll(label, ".", "_")
}

// hclSanitizeLabels applies hclSanitizeLabel to every element in a slice.
func hclSanitizeLabels(labels []string) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = hclSanitizeLabel(l)
	}
	return out
}

// hclModuleName derives a module name from the file path (basename minus extension).
func hclModuleName(relPath string) string {
	base := filepath.Base(relPath)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

func init() {
	Register(newHCLExtractor())
}
