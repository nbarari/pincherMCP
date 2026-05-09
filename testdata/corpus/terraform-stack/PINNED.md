# Pinned corpus: `terraform-stack`

A small synthetic Terraform stack used by the corpus-snapshot tooling (#33)
to lock pincher's HCL extractor against regression. Hand-crafted so the
expected snapshot stays stable across pincher upgrades.

This corpus was added in #189 to close a gap exposed by #178 (var.NAME
references) and #188 (full local/module/data/resource references): both
shipped with all gates green even though they materially change graph
shape on real Terraform, because none of the four pre-existing corpora
contained any `.tf` or `.tfvars` files.

## What this corpus exercises

- All five HCL reference-edge shapes:
  - `var.NAME` (var-block ref) — variables.tf → main.tf, locals.tf, outputs.tf
  - `local.NAME` (locals ref) — locals.tf → main.tf (cross-local + cross-file)
  - `module.NAME.attr` (module output ref) — main.tf → modules/network, outputs.tf
  - `data.TYPE.NAME.attr` (data source ref) — main.tf → main.tf data block
  - `TYPE.NAME.attr` (bare resource ref) — modules/network/main.tf inter-resource
- `.tfvars` Setting symbols (terraform.tfvars top-level assignments)
- Multi-file resolution (resource and variable in different files)
- Nested HCL blocks (`lifecycle`, `filter`, `ingress`)
- A nested module (`modules/network/main.tf`) so `module.network.*`
  references have a real target to resolve against

## Layout

- `main.tf` — provider, 2 data sources, 3 resources, 1 module call
- `variables.tf` — 5 variables (one with no default so .tfvars is meaningful)
- `outputs.tf` — 3 outputs covering resource / module / data references
- `locals.tf` — 4 locals; `common_tags` references two earlier locals
  to exercise cross-local resolution
- `terraform.tfvars` — overrides for the no-default variable
- `modules/network/main.tf` — nested module: 2 vars, 3 resources, 3 outputs

## Maintenance

If the HCL extractor changes intentionally and the snapshot diff is
expected, run `make corpus-snapshot-update` and review the diff in the
same PR — the diff IS the rationale.

If the change is unintentional (silent count drop, a new
`extraction_failures_by_reason` entry), treat the snapshot diff as a
regression report and fix the extractor.
