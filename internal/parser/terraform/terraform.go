// Package terraform names the Terraform/OpenTofu declarations an HCL adapter
// records, so the tree-sitter adapter and the non-cgo fallback spell every
// symbol identically. It holds naming rules only; it parses nothing.
package terraform

import (
	"path/filepath"
	"strings"
)

// Symbol kinds. Terraform kinds are recorded only for .tf and .tfvars files;
// a generic .hcl file (Packer, Nomad, Terragrunt, ...) carries no Terraform
// meaning and gets the generic kinds.
const (
	KindSettings      = "terraform_settings"
	KindProvider      = "terraform_provider"
	KindResource      = "terraform_resource"
	KindData          = "terraform_data"
	KindVariable      = "terraform_variable"
	KindLocal         = "terraform_local"
	KindOutput        = "terraform_output"
	KindModule        = "terraform_module"
	KindVariableValue = "terraform_variable_value"

	KindHCLBlock     = "hcl_block"
	KindHCLAttribute = "hcl_attribute"
)

// IsTerraformPath reports whether a file has Terraform semantics. .tf.json and
// .tofu files are not indexed.
func IsTerraformPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".tf", ".tfvars":
		return true
	}
	return false
}

// IsTFVarsPath reports whether a file is a variable definitions file.
func IsTFVarsPath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".tfvars")
}

// Declaration is one Terraform block's symbol identity.
type Declaration struct {
	Kind, Name, QualifiedName, Container string
}

// StableKey is the symbol's stable key. Directories are not part of it: the
// parser does not see repository-relative paths.
func (d Declaration) StableKey() string { return "tf:" + d.Kind + ":" + d.QualifiedName }

// BlockDeclaration names a top-level Terraform block from its type and
// literal labels. ok is false for blocks that declare nothing addressable
// here (locals, moved, import, removed, check, unknown types) or whose label
// count is wrong.
func BlockDeclaration(blockType string, labels []string) (Declaration, bool) {
	for _, label := range labels {
		if !isName(label) {
			return Declaration{}, false
		}
	}
	switch {
	case blockType == "terraform" && len(labels) == 0:
		return Declaration{KindSettings, "terraform", "terraform", ""}, true
	case blockType == "provider" && len(labels) == 1:
		return Declaration{KindProvider, labels[0], "provider." + labels[0], "provider"}, true
	case blockType == "resource" && len(labels) == 2:
		return Declaration{KindResource, labels[1], labels[0] + "." + labels[1], labels[0]}, true
	case blockType == "data" && len(labels) == 2:
		return Declaration{KindData, labels[1], "data." + labels[0] + "." + labels[1], "data." + labels[0]}, true
	case blockType == "variable" && len(labels) == 1:
		return Declaration{KindVariable, labels[0], "var." + labels[0], "var"}, true
	case blockType == "output" && len(labels) == 1:
		return Declaration{KindOutput, labels[0], "output." + labels[0], "output"}, true
	case blockType == "module" && len(labels) == 1:
		return Declaration{KindModule, labels[0], "module." + labels[0], "module"}, true
	}
	return Declaration{}, false
}

// LocalDeclaration names one entry of a locals block.
func LocalDeclaration(name string) (Declaration, bool) {
	if !isName(name) {
		return Declaration{}, false
	}
	return Declaration{KindLocal, name, "local." + name, "local"}, true
}

// VariableValueDeclaration names one assignment of a .tfvars file. Its
// qualified name is the file-local assignment, not the variable it sets.
func VariableValueDeclaration(name string) (Declaration, bool) {
	if !isName(name) {
		return Declaration{}, false
	}
	return Declaration{KindVariableValue, name, "tfvars." + name, "tfvars"}, true
}

// ModuleSourceSignature describes a module's literal source. A local path
// (./ or ../) is path evidence only; anything else is a registry, VCS or
// archive address no index entry resolves.
func ModuleSourceSignature(source string) string {
	if strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") {
		return "source " + source + " (local path)"
	}
	return "source " + source + " (remote)"
}

// isName is Terraform's identifier syntax for block labels and names.
func isName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
		case i > 0 && (r == '-' || r >= '0' && r <= '9'):
		default:
			return false
		}
	}
	return true
}
