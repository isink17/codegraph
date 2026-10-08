//go:build cgo

package treesitter

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/terraform"
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
// references, and carries the marker that keeps its directory unbound.
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
	if !slices.Contains(hclSymbols(pf), terraform.KindSyntaxError+" "+terraform.KindSyntaxError) {
		t.Fatalf("symbols = %v, want a syntax error marker", hclSymbols(pf))
	}
	if !slices.Contains(hclSymbols(pf), "terraform_variable var.region") {
		t.Fatalf("symbols = %v, want var.region kept", hclSymbols(pf))
	}
}
