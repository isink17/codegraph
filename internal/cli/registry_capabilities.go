package cli

import "github.com/isink17/codegraph/internal/parser"

// Keep the capability of the existing non-CGO adapters independent from the
// parser implementation selected by this build.
func noCGOCapabilities() map[string]parser.NoCGOCapability {
	return map[string]parser.NoCGOCapability{
		"cpp": {Parser: true}, "csharp": {Parser: true}, "go": {Parser: true, CallGraph: true},
		"lua":  {Parser: true},
		"java": {Parser: true}, "kotlin": {Parser: true}, "php": {Parser: true},
		"python": {Parser: true, CallGraph: true}, "ruby": {Parser: true},
		"rust": {Parser: true}, "swift": {Parser: true}, "typescript": {Parser: true},
	}
}
