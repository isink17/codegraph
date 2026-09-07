package graph

type Repo struct {
	ID            int64  `json:"id"`
	RootPath      string `json:"root_path"`
	CanonicalPath string `json:"canonical_path"`
}

type Position struct {
	StartLine int `json:"start_line"`
	StartCol  int `json:"start_col"`
	EndLine   int `json:"end_line"`
	EndCol    int `json:"end_col"`
}

type Symbol struct {
	ID            int64  `json:"symbol_id"`
	FileID        int64  `json:"file_id"`
	Language      string `json:"language"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	ContainerName string `json:"container_name,omitempty"`
	Signature     string `json:"signature,omitempty"`
	Visibility    string `json:"visibility,omitempty"`
	// Static is nil when the parser cannot prove declaration staticness.
	Static     *bool    `json:"static,omitempty"`
	Range      Position `json:"range"`
	DocSummary string   `json:"doc_summary,omitempty"`
	StableKey  string   `json:"stable_key"`
	FilePath   string   `json:"file,omitempty"`
}

type Reference struct {
	ID              int64    `json:"reference_id"`
	FileID          int64    `json:"file_id"`
	SymbolID        *int64   `json:"symbol_id,omitempty"`
	Kind            string   `json:"kind"`
	Name            string   `json:"name"`
	QualifiedName   string   `json:"qualified_name,omitempty"`
	ContextSymbolID *int64   `json:"context_symbol_id,omitempty"`
	Range           Position `json:"range"`
}

type Edge struct {
	ID          int64  `json:"edge_id"`
	SrcSymbolID int64  `json:"src_symbol_id"`
	DstSymbolID *int64 `json:"dst_symbol_id,omitempty"`
	DstName     string `json:"dst_name"`
	Kind        string `json:"kind"`
	Evidence    string `json:"evidence,omitempty"`
	FileID      int64  `json:"file_id"`
	Line        int    `json:"line"`
}

type ParsedFile struct {
	Language   string
	Symbols    []Symbol
	References []Reference
	Edges      []Edge
	Imports    []string
	Scope      ScopeEvidence
	ReExports  []ReExport
	FileTokens map[string]float64
	TestLinks  []TestLink
}

// ScopeEvidence contains syntax-proven facts used by later language-specific
// resolvers. It deliberately contains no destination identity.
type ScopeEvidence struct {
	Package    string
	ModulePath string
	Imports    []ScopeImport
	Modules    []RustModule
	GoLocals   []GoLocalBinding
}

// GoLocalBinding is one syntax-proven Go lexical binding: a name that some
// block in the file binds itself, over the line range that block spans.
//
// It exists to answer two separate questions, and conflating them is the bug it
// was written for. The first is whether a selector qualifier is local at all --
// `Name` plus the range answers that, and a local qualifier can never be an
// imported package no matter what the file's import aliases say. The second is
// what that local's type is, which the type fields answer only when the syntax
// states it outright. A binding with an empty TypeName is still a complete
// answer to the first question, and no answer at all to the second: it vetoes,
// and it binds nothing.
//
// The range is the whole enclosing block, not the span from the declaration
// onwards. That over-suppresses a use textually above its own `:=` (which Go
// would reject anyway) and costs nothing a correct program relies on, and it is
// the one scope rule both Go adapters can compute identically.
type GoLocalBinding struct {
	Name           string
	ScopeStartLine int
	ScopeEndLine   int

	// TypeName is the bare type name with pointer stars, parentheses and type
	// arguments stripped: `*Store` and `Store[K,V]` both give `Store`. Empty
	// means the syntax did not prove a type.
	TypeName string

	// TypePackage is the qualifier as written in the type expression -- `pkg`
	// in `var x pkg.Type` -- and is empty for a type named without one, which
	// in Go means the file's own package.
	TypePackage string

	// TypeImportPath is TypePackage resolved through the file's import aliases,
	// empty when TypePackage is empty or matches no import.
	TypeImportPath string

	// Pointer records that the proven type was a pointer expression. It is the
	// only method-set input this evidence carries.
	Pointer bool
}

type ScopeImport struct {
	SourceSpecifier string
	ImportedName    string
	LocalName       string
	Kind            string
	Wildcard        bool
	Static          bool
	ReExport        bool
	NamespaceExport bool
	TypeOnly        bool
	OwnerModule     string
}

type RustModule struct {
	Name         string
	OwnerModule  string
	ExternalPath string
	Inline       bool
	Visibility   string
}

const (
	ScopeImportNamed      = "named"
	ScopeImportDefault    = "default"
	ScopeImportNamespace  = "namespace"
	ScopeImportSideEffect = "side_effect"
	ScopeImportUse        = "use"

	// ScopeImportLocalBinding is negative scope evidence rather than an import:
	// it records that a lexical scope binds a name itself (a parameter, an
	// assignment, a nested declaration), so a resolver can tell that an import
	// of that name does not reach a call site below it. OwnerModule carries the
	// scope, LocalName the bound name, and SourceSpecifier is empty.
	ScopeImportLocalBinding = "local_binding"

	// ScopeImportTypedBinding records a value binding whose declared type is
	// syntax-proven. SourceSpecifier carries the type spelling; OwnerModule
	// carries the exact lexical owner.
	ScopeImportTypedBinding = "typed_binding"

	// ScopeImportNestedDeclaration is a local binding a nested `def` or `class`
	// makes in the function that encloses it. It shadows an import of the same
	// name the way any other local does, but the name it binds is a symbol this
	// graph holds, so it is not a reason to refuse every other strategy.
	ScopeImportNestedDeclaration = "nested_decl"
)

type ReExport struct {
	Source       string
	Name         string
	ExportedName string
	Wildcard     bool
}

type TestLink struct {
	TestName        string
	TargetName      string
	Reason          string
	Score           float64
	TestSymbolKey   string
	TargetStableKey string
	// TestSymbolIndex is an ephemeral reference to the exact symbol in the
	// ParsedFile that emitted this link. It is consumed while persisting the
	// file and is never a durable or public identity.
	TestSymbolIndex *int `json:"-"`
}

type Stats struct {
	RepoRoot      string         `json:"repo_root"`
	RepoID        int64          `json:"repo_id"`
	Files         int64          `json:"files"`
	Symbols       int64          `json:"symbols"`
	References    int64          `json:"references"`
	Edges         int64          `json:"edges"`
	DirtyFiles    int64          `json:"dirty_files"`
	LastScanID    int64          `json:"last_scan_id"`
	LastIndexedAt string         `json:"last_indexed_at,omitempty"`
	Languages     map[string]int `json:"languages"`
}

type TaskContext struct {
	Task      string            `json:"task"`
	Files     []TaskContextFile `json:"files"`
	TestFiles []TaskContextFile `json:"test_files,omitempty"`

	// EstimatedTokens is the estimated size of this response, measured on the
	// serialized document itself. It is an estimate (bytes/4), not a model
	// provider's token count.
	EstimatedTokens int `json:"estimated_tokens"`
	// MaxTokens is the budget this page was selected to fit.
	MaxTokens int `json:"max_tokens"`
	// HasMore reports that ranked context was withheld to fit the budget.
	HasMore bool `json:"has_more"`
	// NextCursor is an opaque continuation token, present only when HasMore.
	NextCursor string `json:"next_cursor,omitempty"`
	// ReturnedSymbols and RemainingSymbols count symbol records on this page and
	// still withheld after it.
	ReturnedSymbols  int `json:"returned_symbols"`
	RemainingSymbols int `json:"remaining_symbols"`
}

type TaskContextFile struct {
	Path           string              `json:"path"`
	Language       string              `json:"language"`
	RelevanceScore float64             `json:"relevance_score"`
	Symbols        []TaskContextSymbol `json:"symbols"`
}

type TaskContextSymbol struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Signature     string `json:"signature,omitempty"`
	DocSummary    string `json:"doc_summary,omitempty"`
	Relevance     string `json:"relevance"` // "direct_match", "caller", "callee", "test"
	QualifiedName string `json:"qualified_name"`

	// SymbolID is the exact drill-down identity: pass it to
	// find_symbol/find_callers/find_callees to fetch this symbol at a higher
	// detail level. StableKey is semantic metadata and may collide; it is not a
	// general exact selector. context_for_task itself never carries source.
	SymbolID  int64  `json:"symbol_id,omitempty"`
	StableKey string `json:"stable_key,omitempty"`
}
