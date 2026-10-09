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

func TestIsTerraformPathMatchesTerraformLoader(t *testing.T) {
	for path, want := range map[string]bool{
		"main.tf":           true,
		"infra/override.tf": true,
		"terraform.tfvars":  true,
		"main.TF":           false,
		"vars.Tf":           false,
		"override.TF":       false,
		"main.tf.json":      false,
		"main.tofu":         false,
	} {
		if got := IsTerraformPath(path); got != want {
			t.Errorf("IsTerraformPath(%q) = %v, want %v", path, got, want)
		}
	}
}
