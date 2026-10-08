package heuristic

import (
	"context"
	"slices"
	"testing"
)

// The non-cgo fallback names top-level Terraform declarations exactly as the
// tree-sitter adapter does and records nothing else: no references, no
// imports, nothing for nested blocks, heredocs, tfvars or generic HCL.
func TestHCLFallbackTerraformDeclarationsOnly(t *testing.T) {
	src := `resource "aws_instance" "web" {
  ami = var.ami
  lifecycle {
    create_before_destroy = true
  }
  user_data = <<EOF
resource "fake" "inside_heredoc" {
EOF
}
# variable "commented" {}
variable "ami" {}
module "net" { source = "./net" }
data "aws_ami" "base" {
  nested "x" "y" {}
}
locals {
  a = 1
}
`
	pf, err := NewHCL().Parse(context.Background(), "main.tf", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range pf.Symbols {
		got = append(got, s.Kind+" "+s.QualifiedName+" "+s.StableKey)
	}
	want := []string{
		"terraform_resource aws_instance.web tf:terraform_resource:aws_instance.web",
		"terraform_variable var.ami tf:terraform_variable:var.ami",
		"terraform_module module.net tf:terraform_module:module.net",
		"terraform_data data.aws_ami.base tf:terraform_data:data.aws_ami.base",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("symbols = %v\nwant %v", got, want)
	}
	if len(pf.Edges) != 0 || len(pf.References) != 0 || len(pf.Imports) != 0 {
		t.Fatalf("fallback recorded edges=%v refs=%v imports=%v", pf.Edges, pf.References, pf.Imports)
	}
	for _, path := range []string{"build.pkr.hcl", "prod.tfvars"} {
		pf, err := NewHCL().Parse(context.Background(), path, []byte(`variable "x" {}`+"\nx = 1\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(pf.Symbols) != 0 {
			t.Fatalf("%s: fallback recorded %v", path, pf.Symbols)
		}
	}
}
