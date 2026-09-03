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
