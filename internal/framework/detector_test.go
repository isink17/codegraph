package framework

import "testing"

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
	if len(got.Evidence) != 2 || got.Evidence[0] != "x/y" || got.Evidence[1] != `x\y` {
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
