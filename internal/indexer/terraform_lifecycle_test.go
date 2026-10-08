//go:build cgo

package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	heuristicparser "github.com/isink17/codegraph/internal/parser/heuristic"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// A small, original two-module Terraform configuration with a tfvars file, a
// cached copy of a module under .terraform/ and a Packer file beside it.
func terraformSampleTree() tree {
	return tree{
		"infra/main.tf": `provider "aws" {
  region = var.region
}

module "network" {
  source = "./modules/network"
  cidr   = var.vpc_cidr
}

resource "aws_instance" "app" {
  count     = var.app_count
  ami       = data.aws_ami.base.id
  subnet_id = module.network.private_subnet_ids[0]
  tags      = local.common_tags
}

resource "aws_eip" "app" {
  instance = aws_instance.app[count.index].id
}
`,
		"infra/data.tf": `data "aws_ami" "base" {
  most_recent = true
  owners      = ["self"]
}
`,
		"infra/variables.tf": `variable "region" {
  type = string
}

variable "vpc_cidr" {
  default = "10.0.0.0/16"
}

variable "app_count" {
  default = 2
}

variable "environment" {
  default = "dev"
}
`,
		"infra/locals.tf": `locals {
  common_tags = { env = var.environment, missing = var.undeclared }
}
`,
		"infra/outputs.tf": `output "app_ids" {
  value = aws_instance.app[*].id
}

output "eip" {
  value = aws_eip.app.public_ip
}
`,
		"infra/terraform.tfvars": "region = \"eu-central-1\"\nunknown_setting = 1\n",
		"infra/modules/network/main.tf": `variable "cidr" {}

variable "environment" {}

resource "aws_vpc" "this" {
  cidr_block = var.cidr
  tags       = { env = var.environment }
}

resource "aws_subnet" "private" {
  vpc_id = aws_vpc.this.id
}

output "private_subnet_ids" {
  value = aws_subnet.private[*].id
}
`,
		// Downloaded module copies live in a hidden directory the scan skips.
		"infra/.terraform/modules/network/main.tf": "variable \"region\" {}\n",
		"packer/build.pkr.hcl":                     "variable \"region\" {\n  default = var.region\n}\n",
	}
}

// hclRefs renders every Terraform reference edge of srcPath to dst.
func (r *lifecycleRepo) hclRefs(t *testing.T, srcPath, dst string) string {
	t.Helper()
	prefix := "edge " + srcPath + ":"
	needle := `-references-> "` + dst + `"`
	var out []string
	for _, line := range r.projection(t) {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, needle) {
			out = append(out, line[strings.Index(line, needle)+len(needle):])
		}
	}
	if len(out) == 0 {
		return "<no edge>"
	}
	sort.Strings(out)
	return strings.Join(out, " | ")
}

const unresolvedRef = ` => :: [/]`

func TestTerraformReferencesBindWithinTheirModuleDirectory(t *testing.T) {
	r := newLifecycleRepo(t, terraformSampleTree())
	bound := func(path, qname, kind string) string {
		return ` => ` + path + `:` + qname + `(` + kind + `) [terraform_module_scope/high]`
	}
	expect := func(step string, cases map[[2]string]string) {
		t.Helper()
		for k, want := range cases {
			if got := r.hclRefs(t, k[0], k[1]); got != want {
				t.Fatalf("%s: %s -> %s = %q, want %q", step, k[0], k[1], got, want)
			}
		}
		r.assertFreshParity(t, step)
	}
	region := bound("infra/variables.tf", "var.region", "terraform_variable")
	expect("fresh", map[[2]string]string{
		{"infra/main.tf", "var.region"}:          region,
		{"infra/main.tf", "var.vpc_cidr"}:        bound("infra/variables.tf", "var.vpc_cidr", "terraform_variable"),
		{"infra/main.tf", "data.aws_ami.base"}:   bound("infra/data.tf", "data.aws_ami.base", "terraform_data"),
		{"infra/main.tf", "module.network"}:      bound("infra/main.tf", "module.network", "terraform_module"),
		{"infra/main.tf", "local.common_tags"}:   bound("infra/locals.tf", "local.common_tags", "terraform_local"),
		{"infra/main.tf", "aws_instance.app"}:    unresolvedRef, // count.index selects an instance
		{"infra/outputs.tf", "aws_instance.app"}: unresolvedRef, // splat
		{"infra/outputs.tf", "aws_eip.app"}:      bound("infra/main.tf", "aws_eip.app", "terraform_resource"),
		// Same name, other module: each directory binds its own declaration.
		{"infra/locals.tf", "var.environment"}:               bound("infra/variables.tf", "var.environment", "terraform_variable"),
		{"infra/modules/network/main.tf", "var.environment"}: bound("infra/modules/network/main.tf", "var.environment", "terraform_variable"),
		{"infra/modules/network/main.tf", "aws_vpc.this"}:    bound("infra/modules/network/main.tf", "aws_vpc.this", "terraform_resource"),
		{"infra/locals.tf", "var.undeclared"}:                unresolvedRef,
		{"infra/terraform.tfvars", "var.region"}:             region,
		{"infra/terraform.tfvars", "var.unknown_setting"}:    unresolvedRef,
		{"packer/build.pkr.hcl", "var.region"}:               "<no edge>",
	})
	for _, line := range r.projection(t) {
		if strings.Contains(line, ".terraform/") {
			t.Fatalf("cached module copy was indexed: %s", line)
		}
	}

	if noop := r.update(t); noop.FilesChanged != 0 || noop.FilesIndexed != 0 {
		t.Fatalf("no-op update = %+v", noop)
	}

	// A second declaration anywhere in the directory withdraws the proof from
	// files the update did not touch.
	r.write(t, "infra/extra.tf", "variable \"region\" {}\n")
	r.update(t, "infra/extra.tf")
	expect("duplicate added", map[[2]string]string{
		{"infra/main.tf", "var.region"}:          unresolvedRef,
		{"infra/terraform.tfvars", "var.region"}: unresolvedRef,
		{"infra/main.tf", "var.vpc_cidr"}:        bound("infra/variables.tf", "var.vpc_cidr", "terraform_variable"),
	})
	r.remove(t, "infra/extra.tf")
	r.update(t, "infra/extra.tf")
	expect("duplicate removed", map[[2]string]string{{"infra/main.tf", "var.region"}: region})

	// A declaration in another module never answers.
	r.write(t, "infra/modules/network/region.tf", "variable \"region\" {}\n")
	r.update(t, "infra/modules/network/region.tf")
	expect("other module declares", map[[2]string]string{{"infra/main.tf", "var.region"}: region})

	// A file the grammar cannot parse leaves its whole directory unbound.
	r.write(t, "infra/broken.tf", "resource \"aws_s3_bucket\" \"b\" {\n  bucket = \n")
	r.update(t, "infra/broken.tf")
	expect("malformed file", map[[2]string]string{
		{"infra/main.tf", "var.region"}:                   unresolvedRef,
		{"infra/modules/network/main.tf", "aws_vpc.this"}: bound("infra/modules/network/main.tf", "aws_vpc.this", "terraform_resource"),
	})
	r.remove(t, "infra/broken.tf")
	r.update(t)
	expect("malformed file removed", map[[2]string]string{{"infra/main.tf", "var.region"}: region})

	// Renaming the declaration leaves the reference with nothing to bind.
	variables := r.currentTree(t)["infra/variables.tf"]
	r.write(t, "infra/variables.tf", strings.Replace(variables, `variable "region"`, `variable "location"`, 1))
	r.update(t, "infra/variables.tf")
	expect("declaration renamed", map[[2]string]string{{"infra/main.tf", "var.region"}: unresolvedRef})
	r.write(t, "infra/variables.tf", variables)
	r.update(t, "infra/variables.tf")
	expect("declaration restored", map[[2]string]string{{"infra/main.tf", "var.region"}: region})

	// Same resource name under another type is a different address.
	r.write(t, "infra/storage.tf", "resource \"aws_s3_bucket\" \"app\" {}\n")
	r.update(t, "infra/storage.tf")
	expect("same name, other type", map[[2]string]string{
		{"infra/outputs.tf", "aws_eip.app"}: bound("infra/main.tf", "aws_eip.app", "terraform_resource"),
	})
	r.write(t, "infra/storage.tf", "resource \"aws_eip\" \"app\" {}\n")
	r.update(t, "infra/storage.tf")
	expect("duplicate resource", map[[2]string]string{{"infra/outputs.tf", "aws_eip.app"}: unresolvedRef})
}

// The non-cgo fallback records no references, so replacing a tree-sitter HCL
// graph with it is refused like any other call-graph downgrade.
func TestTerraformFallbackReindexIsRefusedAsDowngrade(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "main.tf"), "variable \"region\" {}\noutput \"r\" { value = var.region }\n")
	s, err := store.Open(filepath.Join(t.TempDir(), "codegraph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := New(s, parser.NewRegistry(tsparser.NewHCL()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	fallback := New(s, parser.NewRegistry(heuristicparser.NewHCL()), nil)
	if _, err := fallback.Update(ctx, Options{RepoRoot: root}); !errors.Is(err, ErrParserDowngradeRefused) {
		t.Fatalf("fallback update err = %v, want ErrParserDowngradeRefused", err)
	}
}
