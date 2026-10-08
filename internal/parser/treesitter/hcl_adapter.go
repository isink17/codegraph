//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	hclgrammar "github.com/smacker/go-tree-sitter/hcl"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/terraform"
)

// HCLAdapter indexes HCL. .tf and .tfvars files get Terraform/OpenTofu
// declarations and references; any other .hcl file (Packer, Nomad,
// Terragrunt, ...) gets only its top-level blocks and attributes, with no
// Terraform meaning.
type HCLAdapter struct{}

func NewHCL() *HCLAdapter                  { return &HCLAdapter{} }
func (a *HCLAdapter) Language() string     { return "hcl" }
func (a *HCLAdapter) Extensions() []string { return []string{".hcl", ".tf", ".tfvars"} }
func (a *HCLAdapter) Supports(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".hcl", ".tf", ".tfvars":
		return true
	}
	return false
}

func (a *HCLAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, hclgrammar.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	pf := graph.ParsedFile{Language: "hcl", FileTokens: computeFileTokens(content)}
	body := firstChild(root, "body")
	if !terraform.IsTerraformPath(path) {
		hclGenericSymbols(body, content, &pf)
		return pf, nil
	}
	// A parse error may hide a declaration, so the file proves nothing: it
	// records no references, and its marker keeps its directory unbound.
	broken := root.HasError()
	if broken {
		pf.Symbols = append(pf.Symbols, graph.Symbol{
			Language: "hcl", Kind: terraform.KindSyntaxError, Name: filepath.Base(path),
			QualifiedName: terraform.KindSyntaxError, Range: nodeRange(hclFirstError(root)),
			StableKey: "tf:" + terraform.KindSyntaxError,
		})
	}
	refs := &hclRefs{content: content, pf: &pf}
	add := func(d terraform.Declaration, n *sitter.Node, signature string) {
		pf.Symbols = append(pf.Symbols, graph.Symbol{
			Language: "hcl", Kind: d.Kind, Name: d.Name, QualifiedName: d.QualifiedName,
			ContainerName: d.Container, Signature: signature, Range: nodeRange(n), StableKey: d.StableKey(),
		})
	}
	for _, n := range hclChildren(body) {
		if terraform.IsTFVarsPath(path) {
			if n.Type() != "attribute" {
				continue
			}
			name := firstChild(n, "identifier")
			if d, ok := terraform.VariableValueDeclaration(nodeText(name, content)); ok {
				add(d, n, "")
				if !broken {
					// An assignment sets the variable of that name.
					refs.emit(name, "var."+d.Name, false)
				}
			}
			continue
		}
		if n.Type() != "block" {
			continue
		}
		blockType, labels, ok := hclBlockHeader(n, content)
		if !ok {
			continue
		}
		blockBody := firstChild(n, "body")
		if blockType == "locals" && len(labels) == 0 {
			for _, attr := range hclChildren(blockBody) {
				if attr.Type() != "attribute" {
					continue
				}
				if d, ok := terraform.LocalDeclaration(nodeText(firstChild(attr, "identifier"), content)); ok {
					add(d, attr, "")
					if !broken {
						refs.walk(attr, nil)
					}
				}
			}
			continue
		}
		d, ok := terraform.BlockDeclaration(blockType, labels)
		if !ok {
			continue
		}
		signature := ""
		if d.Kind == terraform.KindModule {
			if source, ok := hclLiteralAttribute(blockBody, "source", content); ok && source != "" {
				pf.Imports = append(pf.Imports, source)
				signature = terraform.ModuleSourceSignature(source)
			}
		}
		add(d, n, signature)
		if !broken {
			refs.walk(blockBody, nil)
		}
	}
	return pf, nil
}

// hclGenericSymbols records the top-level blocks and attributes of a non
// Terraform file. Nothing is resolved: the block vocabulary belongs to
// whatever tool reads the file.
func hclGenericSymbols(body *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, n := range hclChildren(body) {
		var kind, name, qname, container string
		switch n.Type() {
		case "attribute":
			kind, name = terraform.KindHCLAttribute, nodeText(firstChild(n, "identifier"), content)
			qname = name
		case "block":
			blockType, labels, ok := hclBlockHeader(n, content)
			if !ok {
				continue
			}
			kind, container, name = terraform.KindHCLBlock, blockType, blockType
			if len(labels) > 0 {
				name = labels[len(labels)-1]
			}
			qname = strings.Join(append([]string{blockType}, labels...), ".")
		default:
			continue
		}
		if name == "" || n.HasError() {
			continue
		}
		pf.Symbols = append(pf.Symbols, graph.Symbol{
			Language: "hcl", Kind: kind, Name: name, QualifiedName: qname, ContainerName: container,
			Range: nodeRange(n), StableKey: "hcl:" + kind + ":" + qname,
		})
	}
}

func hclChildren(n *sitter.Node) []*sitter.Node {
	if n == nil {
		return nil
	}
	out := make([]*sitter.Node, 0, n.NamedChildCount())
	for i := 0; i < int(n.NamedChildCount()); i++ {
		out = append(out, n.NamedChild(i))
	}
	return out
}

// hclBlockHeader returns a block's type and its labels. ok is false when a
// label is not a literal string or identifier.
func hclBlockHeader(block *sitter.Node, content []byte) (string, []string, bool) {
	var blockType string
	var labels []string
	for i := 0; i < int(block.NamedChildCount()); i++ {
		c := block.NamedChild(i)
		switch c.Type() {
		case "identifier":
			if blockType == "" {
				blockType = nodeText(c, content)
			} else {
				labels = append(labels, nodeText(c, content))
			}
		case "string_lit":
			label, ok := hclLiteralString(c, content)
			if !ok {
				return "", nil, false
			}
			labels = append(labels, label)
		case "block_start":
			return blockType, labels, blockType != ""
		default:
			return "", nil, false
		}
	}
	return "", nil, false
}

// hclLiteralString is the text of a string literal without interpolation or
// directives.
func hclLiteralString(n *sitter.Node, content []byte) (string, bool) {
	var b strings.Builder
	for i := 0; i < int(n.NamedChildCount()); i++ {
		switch c := n.NamedChild(i); c.Type() {
		case "quoted_template_start", "quoted_template_end":
		case "template_literal":
			b.WriteString(nodeText(c, content))
		default:
			return "", false
		}
	}
	return b.String(), true
}

// hclLiteralAttribute is the literal string value of the attribute `name`
// directly in body.
func hclLiteralAttribute(body *sitter.Node, name string, content []byte) (string, bool) {
	for _, attr := range hclChildren(body) {
		if attr.Type() != "attribute" || nodeText(firstChild(attr, "identifier"), content) != name {
			continue
		}
		expr := firstChild(attr, "expression")
		if expr == nil || expr.NamedChildCount() != 1 || expr.NamedChild(0).Type() != "literal_value" {
			return "", false
		}
		lit := expr.NamedChild(0)
		if lit.NamedChildCount() != 1 || lit.NamedChild(0).Type() != "string_lit" {
			return "", false
		}
		return hclLiteralString(lit.NamedChild(0), content)
	}
	return "", false
}

func hclFirstError(n *sitter.Node) *sitter.Node {
	if n.IsError() || n.IsMissing() {
		return n
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		if c := n.Child(i); c.HasError() || c.IsMissing() {
			return hclFirstError(c)
		}
	}
	return n
}

// hclBuiltinRoots name objects that are not declarations of the module:
// count.index, each.key, self.x, path.module, terraform.workspace.
var hclBuiltinRoots = map[string]bool{"count": true, "each": true, "self": true, "path": true, "terraform": true}

type hclRefs struct {
	content []byte
	pf      *graph.ParsedFile
}

func (r *hclRefs) emit(at *sitter.Node, address string, dynamic bool) {
	rng := nodeRange(at)
	evidence := graph.HCLTerraformReferenceEvidence
	if dynamic {
		evidence = graph.HCLTerraformDynamicEvidence
	}
	r.pf.References = append(r.pf.References, graph.Reference{Kind: "reference", Name: address, QualifiedName: address, Range: rng})
	r.pf.Edges = append(r.pf.Edges, graph.Edge{DstName: address, Kind: "references", Evidence: evidence, Line: rng.StartLine, Col: rng.StartCol})
}

// walk records every traversal under n whose root names a module object:
// var.X, local.X, module.M (module.M.out), data.T.N and T.N. shadow holds the
// names a for expression or dynamic block binds around n; a traversal rooted
// at one of them is an iterator, not a reference.
func (r *hclRefs) walk(n *sitter.Node, shadow map[string]bool) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "for_tuple_expr", "for_object_expr":
		intro := firstChild(n, "for_intro")
		inner := hclShadow(shadow)
		for _, c := range hclChildren(intro) {
			if c.Type() == "identifier" {
				inner[nodeText(c, r.content)] = true
			} else {
				// The collection is evaluated outside the loop's own names.
				r.walk(c, shadow)
			}
		}
		for _, c := range hclChildren(n) {
			if c.Type() != "for_intro" {
				r.walk(c, inner)
			}
		}
		return
	case "block":
		if blockType, labels, ok := hclBlockHeader(n, r.content); ok && blockType == "dynamic" && len(labels) == 1 {
			inner := hclShadow(shadow)
			inner[labels[0]] = true
			if body := firstChild(n, "body"); body != nil {
				for _, attr := range hclChildren(body) {
					if attr.Type() == "attribute" && nodeText(firstChild(attr, "identifier"), r.content) == "iterator" {
						inner[strings.TrimSpace(nodeText(firstChild(attr, "expression"), r.content))] = true
					}
				}
			}
			r.walk(firstChild(n, "body"), inner)
			return
		}
	case "expression":
		if n.NamedChildCount() > 0 && n.NamedChild(0).Type() == "variable_expr" {
			r.traversal(n, shadow)
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		r.walk(n.NamedChild(i), shadow)
	}
}

// traversal reads the address an expression's leading traversal names. An
// index or splat before the address is complete names nothing; a splat or a
// non-literal index after it makes the reference dynamic.
func (r *hclRefs) traversal(expr *sitter.Node, shadow map[string]bool) {
	head := expr.NamedChild(0)
	root := nodeText(firstChild(head, "identifier"), r.content)
	if root == "" || shadow[root] || hclBuiltinRoots[root] {
		return
	}
	need := 1
	if root == "data" {
		need = 2
	}
	parts := []string{root}
	dynamic := false
postfix:
	for i := 1; i < int(expr.NamedChildCount()); i++ {
		c := expr.NamedChild(i)
		complete := len(parts) > need
		switch c.Type() {
		case "get_attr":
			if !complete {
				parts = append(parts, nodeText(firstChild(c, "identifier"), r.content))
			}
			continue
		case "index":
			if !complete {
				return
			}
			if !hclLiteralIndex(c) {
				dynamic = true
			}
			continue
		case "splat":
			if !complete {
				return
			}
			dynamic = true
			continue
		}
		break postfix
	}
	if len(parts) <= need {
		return
	}
	// Any root but var, local, module and data is a resource type: T.N.
	r.emit(head, strings.Join(parts, "."), dynamic)
}

func hclLiteralIndex(index *sitter.Node) bool {
	c := index.NamedChild(0)
	if c == nil {
		return false
	}
	if c.Type() == "legacy_index" {
		return true
	}
	expr := firstChild(c, "expression")
	return expr != nil && expr.NamedChildCount() == 1 && expr.NamedChild(0).Type() == "literal_value"
}

func hclShadow(outer map[string]bool) map[string]bool {
	inner := make(map[string]bool, len(outer)+1)
	for k := range outer {
		inner[k] = true
	}
	return inner
}
