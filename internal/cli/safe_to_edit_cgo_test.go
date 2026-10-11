//go:build cgo

package cli

import (
	"context"
	"strconv"
	"testing"

	"github.com/isink17/codegraph/internal/constraints"
	"github.com/isink17/codegraph/internal/editsafety"
)

// TestSafeToEditCLIContainerIncludesNestedMembers pins the scope of a symbol
// target that contains declarations: editing a class edits the methods and
// classes declared inside its source span, so a violation by one of them is
// the class's violation too. Declarations outside the span (a same-named
// method of another class, the same class text in another file) are not.
// Python classes need the cgo tree-sitter registry.
func TestSafeToEditCLIContainerIncludesNestedMembers(t *testing.T) {
	const cfg = `{"schema_version":1,
	"groups":{"domain":{"include":["internal/domain/**"]},"infra":{"include":["internal/infra/**"]}},
	"rules":[{"id":"domain-no-infra","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}]}`
	const svc = `from internal.infra.helpers import helper


class Service:
    def run(self):
        helper()


class Other:
    def run(self):
        return 1


class Outer:
    class Inner:
        def go(self):
            helper()


class Clean:
    def run(self):
        return 2
`
	root, _ := safeToEditRepo(t, map[string]string{
		constraints.ConfigFileName:       cfg,
		"internal/__init__.py":           "",
		"internal/infra/__init__.py":     "",
		"internal/infra/helpers.py":      "def helper():\n    return 0\n",
		"internal/domain/__init__.py":    "",
		"internal/domain/svc.py":         svc,
		"internal/domain/use.py":         "from internal.domain.svc import Service, Other, Outer, Clean\n\n\ndef use():\n    Service()\n    Other()\n    Outer()\n    Clean()\n",
		"internal/elsewhere/__init__.py": "",
		"internal/elsewhere/svc.py":      svc,
		"internal/elsewhere/use.py":      "from internal.elsewhere.svc import Service\n\n\ndef use():\n    Service()\n",
	})
	opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
	if err != nil {
		t.Fatal(err)
	}
	ft, err := opened.Store.EditSafetyFile(context.Background(), opened.Repo.ID, "internal/domain/svc.py")
	opened.Close()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, s := range ft.Symbols {
		ids[s.Kind+" "+s.Name+":"+strconv.Itoa(s.Range.StartLine)] = s.ID
	}
	idOf := func(key string) string {
		id, ok := ids[key]
		if !ok {
			t.Fatalf("no symbol %q in %v", key, ids)
		}
		return strconv.FormatInt(id, 10)
	}
	for _, tc := range []struct {
		key      string
		violates bool
	}{
		{"class Service:4", true},
		{"class Outer:14", true},
		{"class Inner:15", true},
		{"class Other:9", false},
		{"class Clean:20", false},
	} {
		out, res, code, _ := safeToEdit(t, root, "--symbol-id", idOf(tc.key))
		fired := ruleOutcome(res, editsafety.RuleConstraintViolation) == editsafety.OutcomeFired
		if fired != tc.violates {
			t.Fatalf("%s: constraint fired=%v want %v\n%s", tc.key, fired, tc.violates, out)
		}
		if tc.violates {
			if res.Status != editsafety.StatusAssessed || verdictOf(res) != editsafety.VerdictUnsafe || code != 1 {
				t.Fatalf("%s: %s %s exit %d\n%s", tc.key, res.Status, verdictOf(res), code, out)
			}
			for _, v := range res.Evidence.ConstraintViolations {
				if v.Source.Path != "internal/domain/svc.py" {
					t.Fatalf("%s: violation from another file %+v", tc.key, v)
				}
			}
		} else if verdictOf(res) == editsafety.VerdictUnsafe {
			t.Fatalf("%s: unrelated nested symbol made it UNSAFE\n%s", tc.key, out)
		}
	}
	// The same class text in an unconstrained directory takes no violation
	// from its twin.
	if _, res, _, _ := safeToEdit(t, root, "--file", "internal/elsewhere/svc.py"); ruleOutcome(res, editsafety.RuleConstraintViolation) == editsafety.OutcomeFired {
		t.Fatalf("elsewhere file fired: %+v", res.Evidence)
	}
	// Repeated runs give byte-identical output.
	a, _, _, _ := safeToEdit(t, root, "--symbol-id", idOf("class Outer:14"))
	b, _, _, _ := safeToEdit(t, root, "--symbol-id", idOf("class Outer:14"))
	if a != b {
		t.Fatalf("output is not deterministic")
	}
}
