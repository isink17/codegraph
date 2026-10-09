//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
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

const terraformRegionModule = "output \"o\" {\n  value = var.region\n}\n"

// A new .tf file the scan retires without parsing owned nothing, yet it may
// declare anything: its directory must become unbound on update as on a fresh
// index.
func TestTerraformNewOversizeFileUnbindsItsDirectory(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		".codegraph/config.json": `{"max_file_size_bytes": 4096}`,
		"b/main.tf":              terraformRegionModule,
		"b/vars.tf":              "variable \"region\" {}\n",
	})
	if got := r.hclRefs(t, "b/main.tf", "var.region"); got == unresolvedRef {
		t.Fatalf("fresh: var.region unresolved")
	}
	r.write(t, "b/big.tf", "variable \"region\" {}\n"+strings.Repeat("# padding\n", 1000))
	r.update(t)
	if got := r.hclRefs(t, "b/main.tf", "var.region"); got != unresolvedRef {
		t.Fatalf("oversize added: var.region = %q, want unresolved", got)
	}
	r.assertFreshParity(t, "oversize added")
	r.remove(t, "b/big.tf")
	r.update(t)
	r.assertFreshParity(t, "oversize removed")
}

// failingTerraform refuses to parse bad.tf, standing in for a parse failure
// under best_effort, which the tree-sitter grammar itself never reports.
type failingTerraform struct{ *tsparser.HCLAdapter }

func (a failingTerraform) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	if filepath.Base(path) == "bad.tf" {
		return graph.ParsedFile{}, errors.New("unparseable")
	}
	return a.HCLAdapter.Parse(ctx, path, content)
}

func newFailingTerraformRepo(t *testing.T, root string) *lifecycleRepo {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codegraph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: dbPath, store: s,
		idx: New(s, parser.NewRegistry(failingTerraform{tsparser.NewHCL()}), nil)}
	if _, err := r.idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	r.repoID = repo.ID
	return r
}

func TestTerraformNewUnparseableFileUnbindsItsDirectory(t *testing.T) {
	root := t.TempDir()
	r := &lifecycleRepo{root: root}
	r.write(t, ".codegraph/config.json", `{"parse_error_policy": "best_effort"}`)
	r.write(t, "b/main.tf", terraformRegionModule)
	r.write(t, "b/vars.tf", "variable \"region\" {}\n")
	r = newFailingTerraformRepo(t, root)
	if got := r.hclRefs(t, "b/main.tf", "var.region"); got == unresolvedRef {
		t.Fatalf("fresh: var.region unresolved")
	}
	r.write(t, "b/bad.tf", "variable \"region\" {}\n")
	r.update(t)
	if got := r.hclRefs(t, "b/main.tf", "var.region"); got != unresolvedRef {
		t.Fatalf("unparseable added: var.region = %q, want unresolved", got)
	}
	fresh := t.TempDir()
	for rel, content := range r.currentTree(t) {
		(&lifecycleRepo{root: fresh}).write(t, rel, content)
	}
	if diff := projectionDiff(newFailingTerraformRepo(t, fresh).projection(t), r.projection(t)); diff != "" {
		t.Fatalf("update diverges from a fresh index:\n%s", diff)
	}
}

// Address namespaces and indexed forms: data.T.N and T.N never answer for each
// other, a literal index binds the declaring block (never an instance), a
// computed one stays unresolved, a module reference binds the module block in
// the caller's directory and never a declaration inside the child, and
// sibling directories with identical names stay apart through rename and
// delete.
func TestTerraformAddressNamespacesAndIndexedForms(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"live/main.tf": `data "aws_ami" "base" {}

resource "aws_ami" "base" {}

module "net" {
  source   = "../modules/net"
  for_each = var.zones
}

resource "aws_instance" "app" {
  count     = var.enabled && local.size > 0 ? 1 : 0
  ami       = data.aws_ami.base.id
  copy      = aws_ami.base.id
  subnet    = module.net["eu"].subnet_id
  any       = module.net[each.key].subnet_id
  peer      = aws_instance.peer[0].id
  missing   = aws_instance.ghost.id
  only_data = data.aws_ami.other.id
}

resource "aws_instance" "peer" {
  count = 2
}
`,
		"live/vars.tf": `variable "zones" {}
variable "enabled" {}
locals {
  size = 3
}
`,
		"modules/net/main.tf": `variable "zones" {}
variable "enabled" {}
data "aws_ami" "other" {}
output "subnet_id" { value = var.enabled ? data.aws_ami.other.id : "" }
`,
	})
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
	module := bound("live/main.tf", "module.net", "terraform_module")
	enabled := bound("live/vars.tf", "var.enabled", "terraform_variable")
	expect("fresh", map[[2]string]string{
		{"live/main.tf", "data.aws_ami.base"}:  bound("live/main.tf", "data.aws_ami.base", "terraform_data"),
		{"live/main.tf", "aws_ami.base"}:       bound("live/main.tf", "aws_ami.base", "terraform_resource"),
		{"live/main.tf", "module.net"}:         unresolvedRef + " | " + module, // each.key, then the literal key
		{"live/main.tf", "aws_instance.peer"}:  bound("live/main.tf", "aws_instance.peer", "terraform_resource"),
		{"live/main.tf", "aws_instance.ghost"}: unresolvedRef,
		// Declared only inside the child module: not visible to the caller.
		{"live/main.tf", "data.aws_ami.other"}:        unresolvedRef,
		{"live/main.tf", "var.enabled"}:               enabled,
		{"live/main.tf", "local.size"}:                bound("live/vars.tf", "local.size", "terraform_local"),
		{"modules/net/main.tf", "var.enabled"}:        bound("modules/net/main.tf", "var.enabled", "terraform_variable"),
		{"modules/net/main.tf", "data.aws_ami.other"}: bound("modules/net/main.tf", "data.aws_ami.other", "terraform_data"),
	})

	// Deleting the managed twin leaves the data reference bound and the
	// managed one unresolved; the data declaration never stands in.
	main := r.currentTree(t)["live/main.tf"]
	r.write(t, "live/main.tf", strings.Replace(main, "resource \"aws_ami\" \"base\" {}\n", "", 1))
	r.update(t, "live/main.tf")
	expect("managed twin deleted", map[[2]string]string{
		{"live/main.tf", "data.aws_ami.base"}: bound("live/main.tf", "data.aws_ami.base", "terraform_data"),
		{"live/main.tf", "aws_ami.base"}:      unresolvedRef,
	})
	r.write(t, "live/main.tf", main)
	r.update(t, "live/main.tf")

	// Moving the caller's variables away leaves its references unresolved
	// even though the child module still declares the same names.
	vars := r.currentTree(t)["live/vars.tf"]
	r.remove(t, "live/vars.tf")
	r.write(t, "live/moved/vars.tf", vars)
	r.update(t, "live/vars.tf", "live/moved/vars.tf")
	expect("variables moved to another directory", map[[2]string]string{
		{"live/main.tf", "var.enabled"}:        unresolvedRef,
		{"live/main.tf", "local.size"}:         unresolvedRef,
		{"modules/net/main.tf", "var.enabled"}: bound("modules/net/main.tf", "var.enabled", "terraform_variable"),
	})
	r.remove(t, "live/moved/vars.tf")
	r.write(t, "live/vars.tf", vars)
	r.update(t, "live/vars.tf", "live/moved/vars.tf")
	expect("variables restored", map[[2]string]string{{"live/main.tf", "var.enabled"}: enabled})
}

// An index written by the v1 profile, which dropped references inside
// operators, is reparsed on update and gains them.
func TestTerraformV1ProfileUpgradeRecordsOperatorReferences(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"m/main.tf": "variable \"enabled\" {}\nresource \"aws_eip\" \"x\" {\n  count = var.enabled && true ? 1 : 0\n}\n",
	})
	want := ` => m/main.tf:var.enabled(terraform_variable) [terraform_module_scope/high]`
	if got := r.hclRefs(t, "m/main.tf", "var.enabled"); got != want {
		t.Fatalf("fresh: var.enabled = %q, want %q", got, want)
	}
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Stand in for the v1 graph: the profile and the missing edge.
	if _, err := db.Exec(`DELETE FROM edges WHERE repo_id = ? AND dst_name = 'var.enabled'`, r.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE files SET parser_profile = 'treesitter:hcl:v1' WHERE repo_id = ? AND language = 'hcl'`, r.repoID); err != nil {
		t.Fatal(err)
	}
	if got := r.hclRefs(t, "m/main.tf", "var.enabled"); got != "<no edge>" {
		t.Fatalf("v1 stand-in: var.enabled = %q", got)
	}
	if summary := r.update(t); strings.Join(summary.ParserProfileLanguages, ",") != "hcl" {
		t.Fatalf("update = %+v, want an hcl profile reparse", summary)
	}
	if got := fileParserProfile(t, db, r.repoID, "m/main.tf"); got != "treesitter:hcl:v3" {
		t.Fatalf("updated profile = %q", got)
	}
	if got := r.hclRefs(t, "m/main.tf", "var.enabled"); got != want {
		t.Fatalf("upgraded: var.enabled = %q, want %q", got, want)
	}
	r.assertFreshParity(t, "v1 profile upgrade")
}

// Blocks in override.tf and *_override.tf merge into the one ordinary
// declaration of their address, so they never compete with it and are never
// a bind target themselves.
func TestTerraformOverrideFilesMergeIntoTheirOriginal(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"infra/main.tf": `variable "region" {}

locals {
  tags = {}
}

module "net" {
  source = "./net"
}

resource "aws_eip" "app" {}

data "aws_eip" "app" {}

resource "aws_instance" "web" {
  a = var.region
  b = local.tags
  c = module.net.id
  d = aws_eip.app.id
  e = data.aws_eip.app.id
  f = aws_eip.only_overridden.id
}
`,
		"infra/override.tf": `variable "region" {
  default = "eu"
}

locals {
  tags = { a = 1 }
}

module "net" {
  source = "./net2"
}

resource "aws_eip" "app" {
  vpc = true
}

resource "aws_eip" "only_overridden" {}
`,
		"infra/dns_override.tf": `variable "region" {
  default = "us"
}

data "aws_eip" "app" {
  id = "x"
}

output "o" {
  value = var.region
}
`,
		"infra/sub/main.tf":     "output \"o\" {\n  value = var.region\n}\n",
		"infra/sub/override.tf": "variable \"region\" {}\n",
	})
	bound := func(qname, kind string) string {
		return ` => infra/main.tf:` + qname + `(` + kind + `) [terraform_module_scope/high]`
	}
	region := bound("var.region", "terraform_variable")
	base := map[[2]string]string{
		{"infra/main.tf", "var.region"}:              region, // base + 2 overrides
		{"infra/main.tf", "local.tags"}:              bound("local.tags", "terraform_local"),
		{"infra/main.tf", "module.net"}:              bound("module.net", "terraform_module"),
		{"infra/main.tf", "aws_eip.app"}:             bound("aws_eip.app", "terraform_resource"),
		{"infra/main.tf", "data.aws_eip.app"}:        bound("data.aws_eip.app", "terraform_data"),
		{"infra/main.tf", "aws_eip.only_overridden"}: unresolvedRef, // no original: Terraform errors
		{"infra/dns_override.tf", "var.region"}:      region,        // a reference inside an override file
		{"infra/sub/main.tf", "var.region"}:          unresolvedRef, // only an override in its directory
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
	expect("fresh", base)
	if noop := r.update(t); noop.FilesChanged != 0 || noop.FilesIndexed != 0 {
		t.Fatalf("no-op update = %+v", noop)
	}
	expect("no-op", base)

	// Editing an override keeps the original as the target.
	r.write(t, "infra/override.tf", strings.Replace(r.currentTree(t)["infra/override.tf"], `"eu"`, `"ap"`, 1))
	r.update(t, "infra/override.tf")
	expect("override edited", base)

	// Editing the original keeps it the target.
	main := r.currentTree(t)["infra/main.tf"]
	r.write(t, "infra/main.tf", "# edited\n"+main)
	r.update(t, "infra/main.tf")
	expect("base edited", base)

	// A second ordinary declaration is still a Terraform error.
	r.write(t, "infra/extra.tf", "variable \"region\" {}\n")
	r.update(t, "infra/extra.tf")
	expect("duplicate ordinary", map[[2]string]string{
		{"infra/main.tf", "var.region"}:  unresolvedRef,
		{"infra/main.tf", "aws_eip.app"}: bound("aws_eip.app", "terraform_resource"),
	})
	r.remove(t, "infra/extra.tf")
	r.update(t, "infra/extra.tf")
	expect("duplicate removed", base)

	// Deleting the original leaves only overrides: nothing binds.
	r.write(t, "infra/main.tf", strings.Replace(main, `variable "region" {}`, ``, 1))
	r.update(t, "infra/main.tf")
	expect("original deleted", map[[2]string]string{
		{"infra/main.tf", "var.region"}:         unresolvedRef,
		{"infra/dns_override.tf", "var.region"}: unresolvedRef,
	})
	r.write(t, "infra/main.tf", main)
	r.update(t, "infra/main.tf")
	expect("original restored", base)
}

// Terraform's loader matches the .tf suffix case-sensitively, so a declaration
// in vars.TF is never loaded and must not become a reference target.
func TestTerraformUppercaseExtensionDeclaresNothing(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"infra/main.tf": "output \"o\" {\n  value = var.x\n}\n",
		"infra/vars.TF": "variable \"x\" {}\n",
	})
	if got := r.hclRefs(t, "infra/main.tf", "var.x"); got != unresolvedRef {
		t.Fatalf("fresh: var.x = %q, want unresolved", got)
	}
	r.assertFreshParity(t, "fresh")

	r.write(t, "infra/decl.tf", "variable \"x\" {}\n")
	r.update(t, "infra/decl.tf")
	if got, want := r.hclRefs(t, "infra/main.tf", "var.x"), ` => infra/decl.tf:var.x(terraform_variable) [terraform_module_scope/high]`; got != want {
		t.Fatalf("lowercase added: var.x = %q, want %q", got, want)
	}
	r.assertFreshParity(t, "lowercase added")
}
