package classify

import "sort"

// Capability describes what CodeGraph can prove from indexed evidence for a
// persisted language. False means unsupported/not provable here, not that the
// language itself lacks the category.
type Capability struct {
	Language string
	Builtin  bool
	Stdlib   bool
	External bool
	Project  bool
	Unknown  bool
}

// Capabilities returns the complete deterministic classification coverage
// table. Project and unknown are universal outcomes; the other columns reflect
// executable rules installed in this package.
func Capabilities() []Capability {
	out := make([]Capability, 0, len(canonicalLanguages))
	for _, language := range canonicalLanguages {
		_, builtin := builtinsByLanguage[language]
		_, importRule := importRules[language]
		stdlib := language == "cpp" || importRule
		external := language == "typescript"
		out = append(out, Capability{
			Language: language,
			Builtin:  builtin,
			Stdlib:   stdlib,
			External: external,
			Project:  true,
			Unknown:  true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Language < out[j].Language })
	return out
}
