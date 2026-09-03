// Package classify explains what an unresolved call edge points at (P8).
//
// P2 decided which candidates a resolver strategy may consider (same persisted
// language, both known). P3 decided what happens when several survive (refuse to
// guess). P4 explained bindings that happened. P7 preferred production symbols
// over test shadows. P8 answers the question left over: when nothing was bound,
// what *is* that destination -- a language builtin, a standard-library call, a
// call into an imported third-party package, or something we cannot name.
//
// The one rule this package exists to enforce:
//
//	Classification describes evidence CodeGraph actually indexed. It never
//	guesses what the developer probably meant.
//
// Consequences, all deliberate:
//
//   - Classification NEVER resolves anything. It is a pure function of already
//     persisted data and has no access to the resolver, so it cannot promote an
//     unresolved edge, break a P3 ambiguity tie, or outrank a P7 decision. An
//     edge that classifies as `builtin` is still an unresolved edge.
//   - `unknown` is a first-class, expected answer -- not a failure. Most
//     languages CodeGraph parses do not persist enough import evidence to
//     separate a local module from a third-party package, and for those the
//     honest answer is `unknown`. Reducing the `unknown` count by adding a
//     heuristic would make the field a liability rather than a signal.
//   - Evidence must LINK. Knowing a file imports `fmt` says nothing about a call
//     to `helper()` in that file. Every stdlib/external rule below requires the
//     unresolved `dst_name` to be tied to a specific import by a language rule,
//     not merely to co-occur with it in the same file.
//   - Support is deliberately uneven per language. See conservativeGaps in
//     languages.go for what each language cannot prove and why.
package classify

import "strings"

// Classification values. These surface in export payloads, so they are a stable
// contract: rename with a deprecation, not in place.
const (
	// Project: the edge is resolved -- `dst_symbol_id` names an indexed symbol
	// in this repository. The only classification that implies a binding, and
	// the only one an unresolved edge can never carry.
	Project = "project"

	// Builtin: the destination is a predeclared identifier of the source
	// language, callable with no import and no project definition (Go `append`,
	// Python `sorted`). Requires that the repository defines no same-language
	// symbol of that name -- see Evidence.ProjectDefinesTarget.
	Builtin = "builtin"

	// Stdlib: the destination was linked by a language rule to an import of
	// that language's standard library (Go `fmt`, Python `os`, `java.*`,
	// Rust `std::`, Node `node:`).
	Stdlib = "stdlib"

	// External: the destination was linked by a language rule to an import that
	// unambiguously names a non-project dependency package. Only claimed where
	// the language's own module-resolution rules make "not from this project"
	// a property of the import form itself.
	External = "external"

	// Unknown: everything not proven above. Unresolved bare project-looking
	// names, unsupported languages, imports whose project-vs-external origin
	// cannot be established, two imports that disagree about one name, and any
	// case where the indexed evidence runs out.
	//
	// P3-ambiguous names reach `unknown` through the builtin rule's abstention
	// (Evidence.ProjectDefinesTarget). The stdlib and external rules have no
	// equivalent check; see shadowingGap in languages.go for the residual case
	// and why closing it would cost more signal than it buys.
	Unknown = "unknown"
)

// Evidence is everything the classifier is allowed to look at. It is
// deliberately small and value-only: no store handle, no filesystem, no
// network. A caller that cannot fill a field leaves it zero, and the classifier
// degrades to `unknown` rather than inventing the missing evidence.
type Evidence struct {
	// Resolved reports whether the edge has a non-NULL `dst_symbol_id`.
	Resolved bool

	// Language is the persisted `files.language` of the edge's own source file
	// -- never inferred from a path extension. Empty means unknown language,
	// which fails closed exactly as the P2 gate does.
	Language string

	// DstName is the persisted `edges.dst_name`: the call target as the parser
	// recorded it. Its shape is language- and adapter-specific (see
	// languages.go); the rules below encode those shapes rather than assuming a
	// universal one.
	DstName string

	// CallSite is the persisted `edges.evidence`: the call-site text the parser
	// recorded alongside the edge. It exists here for one job -- detecting that
	// a bare DstName was actually a member call whose receiver the adapter threw
	// away (see nameUsedAsMember). Empty means no such evidence was recorded,
	// which cannot disprove anything.
	CallSite string

	// Imports holds the source file's import paths, ALREADY NORMALIZED by
	// NormalizeImports. Callers normalize once per file rather than once per
	// edge, so a file's edges share one slice; passing raw parser strings here
	// would silently defeat the language rules that expect clean paths.
	// Imports of other files are not evidence about this call and must not be
	// passed.
	Imports []string

	// ProjectDefinesTarget reports whether the repository indexes at least one
	// symbol in the SAME language whose bare name equals the builtin candidate
	// in DstName (see BuiltinCandidate). When true, the builtin rule abstains:
	// the project may shadow the builtin, and the edge may be unresolved only
	// because P3 refused to pick between two project definitions. Calling that
	// `builtin` would relabel an ambiguity as a certainty.
	//
	// It is language-scoped on purpose. A Swift `sorted` is not evidence about
	// a Python call to `sorted`; treating it as such is precisely the
	// cross-language name coincidence P2 exists to reject.
	ProjectDefinesTarget bool
}

// Target classifies one edge. It is the single production entry point: export,
// audit, and any future consumer must call this rather than re-deriving rules,
// so no two surfaces can disagree about what an edge points at.
func Target(ev Evidence) string {
	// A bound destination is a project symbol by definition, whatever its name
	// looks like. Checked first so no name rule can contradict the graph.
	if ev.Resolved {
		return Project
	}
	dstName := strings.TrimSpace(ev.DstName)
	if dstName == "" || ev.Language == "" {
		return Unknown
	}

	// Builtins first: a predeclared identifier needs no import, so import
	// evidence cannot confirm or deny it.
	if _, ok := BuiltinCandidate(ev.Language, dstName); ok {
		switch {
		case ev.ProjectDefinesTarget:
			// The project defines this name in this language. Whatever left the
			// edge unresolved (P3 ambiguity, a kind restriction), it is not
			// "obviously the language builtin".
			return Unknown
		case nameUsedAsMember(ev.CallSite, dstName):
			// The call site shows a receiver the adapter dropped from dst_name:
			// `qs.filter(...)` is a method on `qs`, not Python's `filter`.
			return Unknown
		default:
			return Builtin
		}
	}

	rule, ok := importRules[ev.Language]
	if !ok {
		// No import rule for this language: nothing here can be proven.
		return Unknown
	}
	return rule(dstName, ev.Imports)
}

// nameUsedAsMember reports whether callSite shows name being accessed through a
// receiver, which means a bare `name` in dst_name is a truncation rather than an
// unqualified call.
//
// This guard exists because import capture is not the only lossy part of the
// parser boundary: the non-cgo Python adapter matches calls with a regex that
// keeps only the identifier before `(`
// (internal/parser/python/adapter.go callRE), so `Model.objects.filter(x)`
// persists as dst_name `filter` -- a name that is also a Python builtin. Without
// this check the most common ORM call in a Django repository would be reported as
// a language builtin, and the same repository would classify differently under
// the cgo build, whose adapter keeps the receiver.
//
// `edges.evidence` is the recorded call-site text, so the receiver the adapter
// discarded is usually still visible there. Absent evidence proves nothing and
// does not block the builtin claim; the check only ever demotes to `unknown`.
func nameUsedAsMember(callSite, name string) bool {
	if callSite == "" || name == "" {
		return false
	}
	for _, accessor := range []string{".", "->", "::"} {
		if strings.Contains(callSite, accessor+name) {
			return true
		}
	}
	return false
}

// BuiltinCandidate reports whether dstName could be a predeclared identifier of
// language, returning the bare name that must be checked against project
// symbols. It is exported so a caller can batch-load the
// Evidence.ProjectDefinesTarget lookup for exactly those names and no others --
// keeping the language sets in this package rather than in a caller's query.
//
// Go and Python builtins must be unqualified. TypeScript additionally accepts
// a qualified name when its first component is a curated ECMAScript intrinsic
// root (`JSON.stringify`), never based on the member tail.
func BuiltinCandidate(language, dstName string) (string, bool) {
	name := strings.TrimSpace(dstName)
	if name == "" {
		return "", false
	}
	builtins, ok := builtinsByLanguage[language]
	if !ok {
		return "", false
	}
	if language == "typescript" {
		root, _, qualified := strings.Cut(name, ".")
		if qualified {
			if builtins[root] {
				return root, true
			}
			return "", false
		}
	}
	if strings.ContainsAny(name, ".:/->") {
		return "", false
	}
	if !builtins[name] {
		return "", false
	}
	return name, true
}
