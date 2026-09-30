package framework

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func detection(t *testing.T, name string, files []string, imports map[string][]string) Detection {
	t.Helper()
	for _, got := range Detect(files, imports) {
		if got.Name == name {
			return got
		}
	}
	t.Fatalf("Detect() missing %q", name)
	return Detection{}
}

func TestDetectLogicalPaths(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  bool
	}{
		{"rails root", []string{"config/routes.rb"}, true},
		{"rails nested", []string{"deeply/nested/config/routes.rb"}, true},
		{"rails literal backslash", []string{`config\routes.rb`}, false},
		{"rails mixed literal backslash", []string{`app\config/routes.rb`}, false},
		{"laravel root", []string{"artisan"}, true},
		{"laravel nested", []string{"deeply/nested/artisan"}, true},
		{"laravel literal backslash", []string{`deeply\nested\artisan`}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(tt.files, nil)
			if (len(got) != 0) != tt.want {
				t.Fatalf("Detect(%q) = %v, want detected=%v", tt.files, got, tt.want)
			}
		})
	}
}

func TestDetectPreservesImportEvidenceAndIdentity(t *testing.T) {
	files := []string{"x/y", `x\y`}
	imports := map[string][]string{
		"x/y": {"express"},
		`x\y`: {"express"},
	}
	got := detection(t, "express", files, imports)
	seen := map[string]bool{}
	for _, evidence := range got.Evidence {
		seen[evidence] = true
	}
	if len(got.Evidence) != 2 || !seen["x/y"] || !seen[`x\y`] {
		t.Fatalf("Evidence = %q, want exact distinct paths", got.Evidence)
	}
}

func TestDetectImportBehaviorUnchanged(t *testing.T) {
	got := detection(t, "django", []string{"src/app.py"}, map[string][]string{
		"src/app.py": {"django"},
	})
	if got.Language != "python" || len(got.Evidence) != 1 || got.Evidence[0] != "src/app.py" {
		t.Fatalf("Detection = %+v", got)
	}
}

// Evidence is gathered by ranging the imports map, which Go reorders on every
// range, and from a files slice whose order is the caller's. The answer must be
// byte-identical across repeated calls and permuted input.
func TestDetectIsOrderIndependent(t *testing.T) {
	imports := map[string][]string{}
	var files []string
	for i := range 40 {
		f := fmt.Sprintf("src/m%02d.js", i)
		imports[f] = []string{"react", "react-dom", "express"}
		files = append(files, f)
	}
	for i := range 5 {
		files = append(files, fmt.Sprintf("app%d/config/routes.rb", i))
	}
	encode := func(files []string) string {
		blob, err := json.Marshal(Detect(files, imports))
		if err != nil {
			t.Fatal(err)
		}
		return string(blob)
	}
	want := encode(files)
	reversed := slices.Clone(files)
	slices.Reverse(reversed)
	for i := range 20 {
		in := files
		if i%2 == 1 {
			in = reversed
		}
		if got := encode(in); got != want {
			t.Fatalf("call %d differs:\nfirst: %s\nnow  : %s", i+2, want, got)
		}
	}
	react := detection(t, "react", files, imports)
	if len(react.Evidence) != 40 || !slices.IsSorted(react.Evidence) {
		t.Fatalf("react evidence = %q, want 40 sorted distinct files", react.Evidence)
	}
}
