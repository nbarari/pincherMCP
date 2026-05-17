package ast

import (
	"testing"
)

// #1342 v0.71: Terraform `module "x" { source = "..." }` declarations
// now emit IMPORTS edges keyed on the source attribute's literal
// value. Local-path sources resolve to in-tree Module symbols cleanly;
// registry / git sources drop at the cross-file resolver until non-Go
// IMPORTS resolution is in (#1340), but the extracted edge is present
// in either case.

func TestExtractHCL_ModuleSourceEmitsIMPORTS_1342(t *testing.T) {
	src := []byte(`
module "vpc" {
  source = "./modules/vpc"
  cidr   = "10.0.0.0/16"
}

module "consul" {
  source  = "hashicorp/consul/aws"
  version = "0.5.0"
}
`)
	result := Extract(src, "HCL", "infra/main.tf")
	if result == nil {
		t.Fatal("nil result")
	}

	want := map[string]string{
		"module.vpc":    "./modules/vpc",
		"module.consul": "hashicorp/consul/aws",
	}
	for fromQN, toName := range want {
		var found bool
		for _, e := range result.Edges {
			if e.Kind == "IMPORTS" && e.FromQN == fromQN && e.ToName == toName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected IMPORTS edge %s → %s; got edges: %+v", fromQN, toName, result.Edges)
		}
	}
}

// Negative control: a module block without a `source` attribute (the
// HCL parser tolerates this for partial/templated configs) must NOT
// emit an IMPORTS edge with empty ToName.
func TestExtractHCL_ModuleWithoutSource_NoEdge_1342(t *testing.T) {
	src := []byte(`
module "incomplete" {
  cidr = "10.0.0.0/16"
}
`)
	result := Extract(src, "HCL", "infra/bad.tf")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, e := range result.Edges {
		if e.Kind == "IMPORTS" && e.ToName == "" {
			t.Errorf("expected no IMPORTS edge for sourceless module; got %+v", e)
		}
	}
}

// Negative control: an interpolated source (`source = "${var.path}"`)
// can't be statically resolved — emit nothing rather than a meaningless
// "${var.path}" edge that no resolver can ever bind.
func TestExtractHCL_ModuleInterpolatedSource_NoEdge_1342(t *testing.T) {
	src := []byte(`
variable "modpath" {
  type = string
}

module "templated" {
  source = "${var.modpath}/vpc"
}
`)
	result := Extract(src, "HCL", "infra/templated.tf")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, e := range result.Edges {
		if e.Kind == "IMPORTS" && e.FromQN == "module.templated" {
			t.Errorf("expected no IMPORTS edge for interpolated source; got %+v", e)
		}
	}
}

// Cross-check: non-module blocks with a `source`-named attribute
// (e.g. a `provider "x" { source = "..." }` block in a
// required_providers context) MUST NOT emit a module-IMPORTS edge —
// the walk filters on `blk.Type == "module"`.
func TestExtractHCL_NonModuleBlockSource_NoModuleImport_1342(t *testing.T) {
	src := []byte(`
provider "aws" {
  region = "us-east-1"
}

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "5.0"
    }
  }
}
`)
	result := Extract(src, "HCL", "infra/providers.tf")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, e := range result.Edges {
		if e.Kind == "IMPORTS" && e.ToName == "hashicorp/aws" {
			t.Errorf("expected provider source NOT to emit a module IMPORTS edge; got %+v", e)
		}
	}
}
