package heuristic

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/parser/terraform"
	"github.com/isink17/codegraph/internal/texttoken"
)

// HCLAdapter is the non-cgo HCL fallback: Terraform top-level declarations of
// .tf files under the names the tree-sitter adapter uses, and nothing else --
// no locals, no tfvars assignments, no module sources, no references.
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

func (a *HCLAdapter) Profile() parser.Profile {
	return parser.NewProfile("hcl", "heuristic:hcl:v1", false)
}

var hclBlockHeadRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)((?:[ \t]+"[A-Za-z0-9_-]*")*)[ \t]*\{`)

func (a *HCLAdapter) Parse(_ context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf := graph.ParsedFile{Language: "hcl", FileTokens: texttoken.Weights(content)}
	if !terraform.IsTerraformPath(path) || terraform.IsTFVarsPath(path) {
		return pf, nil
	}
	depth := 0
	state := stripState{}
	for i, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSuffix(line, "\r")
		top := depth == 0 && state == (stripState{})
		normalized, next := stripForHeuristic(line, state, true, true)
		state = next
		depth += braceDelta(normalized)
		if !top {
			continue
		}
		m := hclBlockHeadRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var labels []string
		for _, label := range strings.Fields(m[2]) {
			labels = append(labels, strings.Trim(label, `"`))
		}
		d, ok := terraform.BlockDeclaration(m[1], labels)
		if !ok {
			continue
		}
		// ponytail: the symbol spans its header line only; brace matching
		// is the tree-sitter adapter's job.
		pf.Symbols = append(pf.Symbols, graph.Symbol{
			Language: "hcl", Kind: d.Kind, Name: d.Name, QualifiedName: d.QualifiedName, ContainerName: d.Container,
			Range:     graph.Position{StartLine: i + 1, StartCol: 1, EndLine: i + 1, EndCol: len(line) + 1},
			StableKey: d.StableKey(),
		})
	}
	return pf, nil
}
