package framework

import (
	"path/filepath"
	"strings"
)

// Detection represents a detected framework in the project.
type Detection struct {
	Name       string   `json:"name"`       // e.g., "express", "django", "gin", "react"
	Language   string   `json:"language"`   // e.g., "javascript", "python", "go"
	Confidence float64  `json:"confidence"` // 0.0-1.0
	Evidence   []string `json:"evidence"`   // what files/patterns triggered detection
}

type frameworkSpec struct {
	name      string
	language  string
	importSig string // substring to look for in import paths
	fileSig   string // substring to look for in file paths (optional)
}

var knownFrameworks = []frameworkSpec{
	// Go frameworks
	{name: "gin", language: "go", importSig: "github.com/gin-gonic/gin"},
	{name: "echo", language: "go", importSig: "github.com/labstack/echo"},
	{name: "fiber", language: "go", importSig: "github.com/gofiber/fiber"},
	{name: "chi", language: "go", importSig: "github.com/go-chi/chi"},

	// Python frameworks
	{name: "django", language: "python", importSig: "django"},
	{name: "flask", language: "python", importSig: "flask"},
	{name: "fastapi", language: "python", importSig: "fastapi"},

	// JavaScript/TypeScript frameworks
	{name: "express", language: "javascript", importSig: "express"},
	{name: "react", language: "javascript", importSig: "react"},
	{name: "next", language: "javascript", importSig: "next"},
	{name: "vue", language: "javascript", importSig: "vue"},
	{name: "angular", language: "javascript", importSig: "@angular"},
	{name: "svelte", language: "javascript", importSig: "svelte"},

	// Java frameworks
	{name: "spring", language: "java", importSig: "org.springframework"},

	// Ruby frameworks
	{name: "rails", language: "ruby", importSig: "rails"},

	// PHP frameworks
	{name: "laravel", language: "php", importSig: "Illuminate"},
	{name: "symfony", language: "php", importSig: "Symfony"},

	// Rust frameworks
	{name: "actix", language: "rust", importSig: "actix"},
	{name: "axum", language: "rust", importSig: "axum"},
}

// Detect analyzes project files to detect frameworks.
func Detect(files []string, imports map[string][]string) []Detection {
	// Count total files with imports to calculate confidence.
	totalFiles := len(imports)
	if totalFiles == 0 {
		totalFiles = 1
	}

	// Build a set of file basenames for file-based detection.
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[filepath.ToSlash(f)] = true
	}

	type match struct {
		files []string
	}
	matches := make(map[string]*match)

	// Check imports for each framework signature.
	for filePath, fileImports := range imports {
		for _, imp := range fileImports {
			for i := range knownFrameworks {
				spec := &knownFrameworks[i]
				if strings.Contains(imp, spec.importSig) {
					m, ok := matches[spec.name]
					if !ok {
						m = &match{}
						matches[spec.name] = m
					}
					m.files = append(m.files, filePath)
				}
			}
		}
	}

	// File-based detection for Rails.
	for _, f := range files {
		normalized := filepath.ToSlash(f)
		if strings.HasSuffix(normalized, "config/routes.rb") {
			m, ok := matches["rails"]
			if !ok {
				m = &match{}
				matches["rails"] = m
			}
			m.files = append(m.files, f)
		}
	}

	// File-based detection for Laravel (artisan file).
	for _, f := range files {
		base := filepath.Base(f)
		if base == "artisan" {
			m, ok := matches["laravel"]
			if !ok {
				m = &match{}
				matches["laravel"] = m
			}
			m.files = append(m.files, f)
		}
	}

	// Build detections from matches.
	var detections []Detection
	for _, spec := range knownFrameworks {
		m, ok := matches[spec.name]
		if !ok {
			continue
		}
		// Deduplicate evidence files.
		seen := make(map[string]bool)
		var evidence []string
		for _, f := range m.files {
			if !seen[f] {
				seen[f] = true
				evidence = append(evidence, f)
			}
		}

		confidence := float64(len(evidence)) / float64(totalFiles)
		if confidence > 1.0 {
			confidence = 1.0
		}
		// Minimum confidence floor if we have any evidence.
		if confidence < 0.1 {
			confidence = 0.1
		}

		detections = append(detections, Detection{
			Name:       spec.name,
			Language:   spec.language,
			Confidence: confidence,
			Evidence:   evidence,
		})
	}

	return detections
}
