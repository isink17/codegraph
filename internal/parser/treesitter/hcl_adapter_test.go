//go:build cgo

package treesitter

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func parseHCL(t *testing.T, path, src string) graph.ParsedFile {
	t.Helper()
	pf, err := NewHCL().Parse(context.Background(), path, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return pf
}

func hclSymbols(pf graph.ParsedFile) []string {
	var out []string
	for _, s := range pf.Symbols {
		out = append(out, s.Kind+" "+s.QualifiedName)
	}
	return out
}

func hclEdges(pf graph.ParsedFile) []string {
	var out []string
	for _, e := range pf.Edges {
		mark := ""
		if e.Evidence == graph.HCLTerraformDynamicEvidence {
			mark = " dynamic"
		}
		out = append(out, e.DstName+mark)
	}
	sort.Strings(out)
	return out
}

func TestHCLTerraformDeclarations(t *testing.T) {
	pf := parseHCL(t, "/r/main.tf", `terraform {
  required_version = ">= 1.5"
}

provider "aws" {
  region = var.region
}

resource "aws_instance" "web" {
  ami = data.aws_ami.ubuntu.id
}

data "aws_ami" "ubuntu" {
  most_recent = true
}

variable "region" {
  type = string
}

locals {
  name = "web"
  tags = { Name = local.name }
}

output "id" {
  value = aws_instance.web.id
}

module "vpc" {
  source = "./modules/vpc"
}

module "registry" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "5.0.0"
}

resource "aws_instance" "${var.dynamic}" {}
moved {
  from = aws_instance.old
  to   = aws_instance.web
}
`)
	want := []string{
		"terraform_settings terraform",
		"terraform_provider provider.aws",
		"terraform_resource aws_instance.web",
		"terraform_data data.aws_ami.ubuntu",
		"terraform_variable var.region",
		"terraform_local local.name",
		"terraform_local local.tags",
		"terraform_output output.id",
		"terraform_module module.vpc",
		"terraform_module module.registry",
	}
	if got := hclSymbols(pf); !slices.Equal(got, want) {
		t.Fatalf("symbols = %v\nwant %v", got, want)
	}
	if !pf.Scope.TerraformComplete {
		t.Fatal("clean file does not claim completeness")
	}
	if got := strings.Join(pf.Imports, ","); got != "./modules/vpc,terraform-aws-modules/vpc/aws" {
		t.Fatalf("imports = %q", got)
	}
	for _, s := range pf.Symbols {
		switch s.QualifiedName {
		case "module.vpc":
			if s.Signature != "source ./modules/vpc (local path)" {
				t.Fatalf("module.vpc signature = %q", s.Signature)
			}
		case "module.registry":
			if s.Signature != "source terraform-aws-modules/vpc/aws (remote)" {
				t.Fatalf("module.registry signature = %q", s.Signature)
			}
		case "aws_instance.web":
			if s.Name != "web" || s.ContainerName != "aws_instance" || s.StableKey != "tf:terraform_resource:aws_instance.web" || s.Range.StartLine != 9 || s.Range.EndLine != 11 {
				t.Fatalf("resource symbol = %+v", s)
			}
		}
	}
	want = []string{"aws_instance.web", "data.aws_ami.ubuntu", "local.name", "var.region"}
	if got := hclEdges(pf); !slices.Equal(got, want) {
		t.Fatalf("edges = %v\nwant %v", got, want)
	}
	for _, e := range pf.Edges {
		if e.Kind != "references" {
			t.Fatalf("edge kind = %q, want references", e.Kind)
		}
		if e.DstName == "var.region" && (e.Line != 6 || e.Col != 12) {
			t.Fatalf("var.region at %d:%d, want 6:12", e.Line, e.Col)
		}
	}
	if len(pf.References) != len(pf.Edges) {
		t.Fatalf("references = %d, edges = %d", len(pf.References), len(pf.Edges))
	}
}

func TestHCLTerraformReferenceForms(t *testing.T) {
	pf := parseHCL(t, "/r/main.tf", `resource "aws_instance" "web" {
  count     = var.n
  subnet    = module.net.subnet_ids[0]
  first     = aws_instance.peer[0].id
  all       = aws_instance.peer[*].id
  legacy    = aws_instance.peer.*.id
  picked    = local.by_zone[var.zone]
  each_one  = aws_instance.peer[count.index].id
  name      = "${var.prefix}-${count.index}"
  tagged    = lookup(var.tags, "k", local.fallback)
  ids       = [for s in var.subnets : s.id]
  pairs     = { for var, v in local.m : var => v.id }
  where     = path.module
  ws        = terraform.workspace
  me        = self.id
  bare      = var
  bad       = data.aws_ami
  dynamic "ingress" {
    for_each = var.rules
    content {
      from = ingress.value.port
      cidr = local.cidr
    }
  }
  dynamic "egress" {
    for_each = var.egress
    iterator = rule
    content {
      to = rule.value
    }
  }
}
`)
	want := []string{
		"aws_instance.peer",
		"aws_instance.peer dynamic",
		"aws_instance.peer dynamic",
		"aws_instance.peer dynamic",
		"local.by_zone dynamic",
		"local.cidr",
		"local.fallback",
		"local.m",
		"module.net",
		"var.egress",
		"var.n",
		"var.prefix",
		"var.rules",
		"var.subnets",
		"var.tags",
		"var.zone",
	}
	if got := hclEdges(pf); !slices.Equal(got, want) {
		t.Fatalf("edges = %v\nwant %v", got, want)
	}
}

func TestHCLTFVarsAssignmentsReferenceVariables(t *testing.T) {
	pf := parseHCL(t, "/r/prod.tfvars", "region = \"eu-west-1\"\ninstance_count = 3\n")
	if got := hclSymbols(pf); !slices.Equal(got, []string{"terraform_variable_value tfvars.region", "terraform_variable_value tfvars.instance_count"}) {
		t.Fatalf("symbols = %v", got)
	}
	if got := hclEdges(pf); !slices.Equal(got, []string{"var.instance_count", "var.region"}) {
		t.Fatalf("edges = %v", got)
	}
}

// A non-Terraform .hcl file has no Terraform meaning: generic top-level
// blocks and attributes, no references, no imports.
func TestHCLGenericFileHasNoTerraformSemantics(t *testing.T) {
	pf := parseHCL(t, "/r/build.pkr.hcl", `variable "region" {
  default = var.other
}
source "amazon-ebs" "ubuntu" {
  region = var.region
}
module "x" { source = "./y" }
datacenters = ["dc1"]
`)
	want := []string{
		"hcl_block variable.region",
		"hcl_block source.amazon-ebs.ubuntu",
		"hcl_block module.x",
		"hcl_attribute datacenters",
	}
	if got := hclSymbols(pf); !slices.Equal(got, want) {
		t.Fatalf("symbols = %v\nwant %v", got, want)
	}
	if len(pf.Edges) != 0 || len(pf.References) != 0 || len(pf.Imports) != 0 {
		t.Fatalf("generic HCL produced edges=%v refs=%v imports=%v", pf.Edges, pf.References, pf.Imports)
	}
}

// A malformed Terraform file keeps the declarations it can name, records no
// references, and does not claim completeness, which keeps its directory
// unbound.
func TestHCLMalformedTerraformFile(t *testing.T) {
	pf := parseHCL(t, "/r/broken.tf", `variable "region" {}

resource "aws_instance" "web" {
  ami = var.region
  tags = {
}
`)
	if len(pf.Edges) != 0 || len(pf.References) != 0 {
		t.Fatalf("malformed file produced edges %v", hclEdges(pf))
	}
	if pf.Scope.TerraformComplete {
		t.Fatal("malformed file claims completeness")
	}
	if !slices.Contains(hclSymbols(pf), "terraform_variable var.region") {
		t.Fatalf("symbols = %v, want var.region kept", hclSymbols(pf))
	}
}

// Names a template `for` directive binds are loop variables in its body, not
// resources; meta-arguments naming providers or the block's own arguments,
// and ephemeral resources, are not references.
func TestHCLTerraformNonReferences(t *testing.T) {
	pf := parseHCL(t, "/r/main.tf", `resource "aws_instance" "web" {
  provider  = aws.west
  user_data = "%{ for server in aws_instance.peer }${server.ip}%{ endfor }"
  shadow    = "%{ for aws_s3_bucket in var.names }${aws_s3_bucket.web}%{ endfor }"
  script    = <<EOT
%{ for k, v in local.env ~}
${k}=${v.value} ${var.suffix}
%{ endfor ~}
EOT
  secret    = ephemeral.random_password.db.result
  lifecycle {
    ignore_changes       = [tags, ami]
    replace_triggered_by = [aws_s3_bucket.logs]
  }
  depends_on = [aws_s3_bucket.logs]
}

module "child" {
  source    = "./child"
  providers = { aws = aws.west }
}

locals {
  providers = var.provider_names
}
`)
	want := []string{
		"aws_instance.peer",
		"aws_s3_bucket.logs",
		"aws_s3_bucket.logs",
		"local.env",
		"var.names",
		"var.provider_names",
		"var.suffix",
	}
	if got := hclEdges(pf); !slices.Equal(got, want) {
		t.Fatalf("edges = %v\nwant %v", got, want)
	}
}

// Indexed and computed addresses name at most the declaring block: a literal
// index or key keeps the block's address, a computed index or splat marks the
// reference dynamic, and nothing ever names an instance. data.T.N and T.N are
// distinct namespaces.
func TestHCLTerraformIndexedAndComputedAddresses(t *testing.T) {
	pf := parseHCL(t, "/r/main.tf", `resource "aws_instance" "web" {
  by_key     = aws_instance.peer["blue"].id
  by_legacy  = aws_instance.peer.0.id
  mod_index  = module.net[0].subnet_id
  mod_key    = module.net["eu"].subnet_id
  mod_each   = module.net[each.key].subnet_id
  mod_splat  = module.net[*].subnet_id
  mod_local  = module.net[local.zone].subnet_id
  computed   = aws_instance.peer[var.i + 1].id
  templated  = aws_instance.peer["${var.k}"].id
  data_ami   = data.aws_ami.base.id
  data_idx   = data.aws_ami.base[0].id
  managed    = aws_ami.base.id
  short_data = data.aws_ami
  parens     = (var.obj).field
  cond       = var.on ? aws_s3_bucket.a.id : aws_s3_bucket.b.id
  tried      = try(aws_s3_bucket.c[0].arn, null)
  key_expr   = { (local.key) = 1 }
  nested     = local.cfg.inner[0].leaf
}
`)
	want := []string{
		"aws_ami.base",
		"aws_instance.peer",
		"aws_instance.peer",
		"aws_instance.peer dynamic",
		"aws_instance.peer dynamic",
		"aws_s3_bucket.a",
		"aws_s3_bucket.b",
		"aws_s3_bucket.c",
		"data.aws_ami.base",
		"data.aws_ami.base",
		"local.cfg",
		"local.key",
		"local.zone",
		"module.net",
		"module.net",
		"module.net dynamic",
		"module.net dynamic",
		"module.net dynamic",
		"var.i",
		"var.k",
		"var.obj",
		"var.on",
	}
	if got := hclEdges(pf); !slices.Equal(got, want) {
		t.Fatalf("edges = %v\nwant %v", got, want)
	}
}

// The grammar splits an operand's attributes and indices away from it inside
// an operation; every operand traversal still names its own address, and
// builtins and loop variables stay out.
func TestHCLTerraformReferencesInOperations(t *testing.T) {
	pf := parseHCL(t, "/r/main.tf", `resource "aws_instance" "web" {
  count   = var.create && local.flags[0].on ? 1 : 0
  negated = !var.disabled.value
  minus   = -var.offset
  chain   = var.a + var.b * local.c.d
  mixed   = var.n.m - module.net[0].count
  splat   = 1 + aws_instance.peer[*].cpu
  keyed   = var.base + aws_instance.peer[var.k].cpu
  paren   = (var.p + 1) * local.q
  builtin = count.index + each.value.n + path.module
  loop    = [for s in var.list : s.id if s.size > local.min]
  partial = 1 + data.aws_ami
  # A postfix after a parenthesized operand or a call belongs to that
  # operand, never to a traversal before it.
  paren_attr = 1 + (var.pa).b
  paren_last = var.pl * (local.pb + 1).z
  call_attr  = foo(var.q).r + local.s.t
  paren_idx  = (var.ia + var.ib)[0] + var.ic
  short_lhs  = data.t + aws_k.n
}
`)
	want := []string{
		"aws_instance.peer dynamic",
		"aws_instance.peer dynamic",
		"aws_k.n",
		"local.c",
		"local.flags",
		"local.min",
		"local.pb",
		"local.q",
		"local.s",
		"module.net",
		"var.a",
		"var.b",
		"var.base",
		"var.create",
		"var.disabled",
		"var.ia",
		"var.ib",
		"var.ic",
		"var.k",
		"var.list",
		"var.n",
		"var.offset",
		"var.p",
		"var.pa",
		"var.pl",
		"var.q",
	}
	if got := hclEdges(pf); !slices.Equal(got, want) {
		t.Fatalf("edges = %v\nwant %v", got, want)
	}
}
