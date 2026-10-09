package terraform

import "testing"

func TestIsOverridePath(t *testing.T) {
	for path, want := range map[string]bool{
		"infra/override.tf":      true,
		"infra/dns_override.tf":  true,
		"main.tf":                false,
		"overrides.tf":           false,
		"my_override.tfvars":     false,
		"override.tf.json":       false,
		"infra/override.tf/x.tf": false,
	} {
		if got := IsOverridePath(path); got != want {
			t.Errorf("IsOverridePath(%q) = %v, want %v", path, got, want)
		}
	}
}
