package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// Ruby implicit/self scope (P22.47).
//
// This pass OWNS every ordinary Ruby call edge (`edge_kind = 'calls'`).
// Ownership means two things, and the second holds even when the first proves
// nothing:
//
//  1. It binds a call whose receiver identity is already syntax-proven by the
//     P22.46 parser -- implicit `run()` and literal `self.run()` / `self::run()`
//     -- to the one eligible method of the caller's own semantic container.
//  2. Every other Ruby call is withheld from the generic strategies
//     (exact_name, exact_qualified, receiver_method, dot_tail*, dot_suffix):
//     in SQL by rubyScopeVetoSQL inside resolverBindableCandidateSQL, in the
//     Go-side binder by rubyScopeOwned dropping the target. Constant, value,
//     chained and safe-navigation receivers, class/module body calls and
//     top-level calls stay unresolved on purpose; repository-wide name matching
//     does not understand Ruby receivers and would invent an owner.
//
// The veto is a static predicate rather than a per-edge temp table because
// unsupported receiver forms dominate Ruby call volume and can never bind in
// this phase; materialising them on every incremental save would cost O(all
// unresolved Ruby calls) for nothing.
//
// The receiver's runtime identity comes from the SOURCE method, never from the
// spelling: an instance method's implicit/literal self is an instance of its
// container, a singleton method's is the class/module object. Source staticness
// therefore selects destination staticness exactly, and the two natures are
// never mixed. `def run` and `def self.run` on one container are distinct
// methods, not an ambiguity. Two eligible declarations of the same nature are
// an ambiguity: Ruby's runtime load order is not modelled, so the call fails
// closed rather than guessing a row.
//
// Ruby lexical constant scope (P22.48) is the second positive path. A call
// spelled `Service.run` or `Service::run`, with a receiver the parser proved is
// exactly one constant token, is bound in two strictly sequential stages that
// never backtrack into each other:
//
//  1. Constant identity. The caller's own file records a lexical parent for
//     every constant it declares (ruby_lexical_parent, P22.46), so the nesting
//     the source method's definition sits in is reconstructable from that file
//     alone -- and only from that file: one file may reopen `A.B` both as
//     `module A; class B` and as `class A::B`, which are different nestings of
//     the same semantic constant, and a fact from another file's reopening
//     would answer a question about a definition site it never saw. The chain
//     is walked innermost first and the FIRST level that owns a constant of
//     that name is the answer, whether or not it holds the method. Ruby decides
//     the constant before it dispatches the method, so a shadowing inner
//     constant with no `run` is a NoMethodError, never a fallback to an outer
//     `Service`. Nothing beyond the lexical chain is modelled: no cref-ancestor
//     lookup, no top-level/Object fallback, no multi-segment path.
//
//  2. Method identity. The receiver is the class/module object, so the target
//     is a singleton method of that exact owner, and an instance method of the
//     same name is not a candidate at all.
//
// Method visibility is not consulted for self receivers: the receiver is
// exactly self, which may call private and protected methods. A constant
// receiver may not -- `Service.run` on a private singleton method raises, even
// from inside `Service` -- so a constant target must be provably public. That
// is why the parser now states singleton visibility (treesitter:ruby:v4): the
// symbol carries the definition-site answer, `ruby_singleton_visibility` facts
// carry named overrides (which Ruby lets another file make, so they are read
// repo-wide per owner), and `ruby_singleton_visibility_unknown` marks an owner
// whose singleton surface a dynamic form or `module_function` made unprovable.
// Facts that disagree without a load order to break the tie fail closed.
//
// Constant-path semantics use a resolver repair because v5 parser facts already
// contain the receiver spelling. The repair clears Ruby call bindings and
// re-decides them without reparsing; parser profile remains treesitter:ruby:v5.

const rubyScopeResolution = "tmp_ruby_scope_resolution"
const rubyConstantPathRepairSettingKey = "resolver.ruby_constant_path_repaired.v1"

// rubyScopeStrategies is every strategy this pass writes; the incremental
// invalidation keys on it.
var rubyScopeStrategies = []string{
	ResolutionStrategyRubyImplicitSelf,
	ResolutionStrategyRubyExplicitSelf,
	ResolutionStrategyRubyLexicalConstant,
	ResolutionStrategyRubyConstantPath,
}

// rubyScopeVetoSQL keeps every repo-wide strategy off ordinary Ruby calls. It
// requires the surroundings every strategy UPDATE already has: the update
// target is `edges` and `files` is joined in as `f`. Cross-language reference
// edges (P22.37) are not ordinary calls and are untouched.
const rubyScopeVetoSQL = `NOT (f.language = 'ruby' AND edges.edge_kind = '` + EdgeKindCalls + `')`

// rubyScopeOwned is the Go-side twin of rubyScopeVetoSQL.
func rubyScopeOwned(t edgeTarget) bool {
	return t.srcLanguage == "ruby" && t.edgeKind == EdgeKindCalls
}

// Parser evidence, not punctuation, decides the receiver category (P22.46).
const (
	rubyImplicitReceiver = "ruby:implicit_receiver"
	rubySelfReceiver     = "ruby:self_receiver"
	rubyConstantReceiver = "ruby:constant_receiver"
)

// rubyVisibilityPublic is the only singleton visibility a constant receiver may
// reach. An empty visibility is not treated as public: it means the parser said
// nothing, and a repository still on treesitter:ruby:v3 says nothing about
// every method.
const rubyVisibilityPublic = "public"

// rubyScopeMethod returns the called method name and the strategy to record for
// a self-identified receiver, or ok=false when the spelling is not exactly a
// bare name or a literal `self.`/`self::` prefix on one. Anything else -- a
// chain, an operator, a receiver the evidence did not promise -- fails closed.
func rubyScopeMethod(evidence, dstName string) (method, strategy string, ok bool) {
	switch evidence {
	case rubyImplicitReceiver:
		method, strategy = dstName, ResolutionStrategyRubyImplicitSelf
	case rubySelfReceiver:
		rest, found := strings.CutPrefix(dstName, "self.")
		if !found {
			if rest, found = strings.CutPrefix(dstName, "self::"); !found {
				return "", "", false
			}
		}
		method, strategy = rest, ResolutionStrategyRubyExplicitSelf
	default:
		return "", "", false
	}
	if method == "" || strings.ContainsAny(method, ".:&(") {
		return "", "", false
	}
	return method, strategy, true
}

// rubyConstantCall parses only explicit constant receivers with one final
// method operator. Punctuation is accepted only after parser evidence owns it.
func rubyConstantCall(dstName string) (absolute bool, segments []string, method, operator string, ok bool) {
	receiver, tail, operator := "", "", ""
	if i := strings.IndexByte(dstName, '.'); i >= 0 {
		receiver, tail, operator = dstName[:i], dstName[i+1:], "."
	} else if i := strings.LastIndex(dstName, "::"); i >= 0 {
		receiver, tail, operator = dstName[:i], dstName[i+2:], "::"
	} else {
		return false, nil, "", "", false
	}
	if tail == "" || strings.ContainsAny(tail, ".:&()") {
		return false, nil, "", "", false
	}
	absolute = strings.HasPrefix(receiver, "::")
	if absolute {
		receiver = receiver[2:]
	}
	if receiver == "" {
		return false, nil, "", "", false
	}
	for _, segment := range strings.Split(receiver, "::") {
		if !rubySimpleConstant(segment) {
			return false, nil, "", "", false
		}
		segments = append(segments, segment)
	}
	return absolute, segments, tail, operator, true
}

// rubySimpleConstant reports whether text is exactly one Ruby constant token.
func rubySimpleConstant(text string) bool {
	if text == "" || text[0] < 'A' || text[0] > 'Z' {
		return false
	}
	for i := range len(text) {
		switch c := text[i]; {
		case c == '_', c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		default:
			return false
		}
	}
	return true
}

// rubyLexicalChain reconstructs the constant nesting a method definition sits
// in, innermost first, from the ruby_lexical_parent facts of the definition's
// own file. It stops at the root boundary, which the parser stores truthfully
// as an empty parent, and it fails closed on a missing fact, a cycle, or an
// owner whose file records more than one distinct parent -- a file that reopens
// the same semantic constant both nested and qualified gives its methods two
// different nestings, and picking a row would pick one at random.
//
// A top-level method has no container and so no chain: `Service.run` there has
// no lexical scope to scan and stays unresolved.
func rubyLexicalChain(parents map[string]map[string]struct{}, container string) ([]string, bool) {
	var chain []string
	seen := make(map[string]struct{}, 4)
	for owner := container; owner != ""; {
		if _, cycle := seen[owner]; cycle {
			return nil, false
		}
		seen[owner] = struct{}{}
		chain = append(chain, owner)
		set, found := parents[owner]
		if !found || len(set) != 1 {
			return nil, false
		}
		for parent := range set {
			owner = parent
		}
	}
	return chain, len(chain) > 0
}

// rubyLoadLexicalParents reads the lexical-parent facts of the given files
// only, keyed by file and then by owner. A repository-wide read would be a scan
// of every Ruby constant in the graph to answer a question about a handful of
// calling files.
func rubyLoadLexicalParents(ctx context.Context, q rubyScopeQuery, repoID int64, files []int64) (map[int64]map[string]map[string]struct{}, error) {
	parents := make(map[int64]map[string]map[string]struct{}, len(files))
	err := sqliteBatchedIDQuery(ctx, q, files, `SELECT p.file_id,p.owner_module,p.source_specifier
FROM scope_import_evidence p JOIN files f ON f.id=p.file_id
WHERE p.repo_id=? AND p.language='ruby' AND f.is_deleted=0
  AND p.import_kind='`+graph.ScopeImportRubyLexicalParent+`' AND p.owner_module<>'' AND p.file_id IN (`,
		[]any{repoID}, func(scan func(...any) error) error {
			var file int64
			var owner, parent string
			if err := scan(&file, &owner, &parent); err != nil {
				return err
			}
			byOwner := parents[file]
			if byOwner == nil {
				byOwner = map[string]map[string]struct{}{}
				parents[file] = byOwner
			}
			if byOwner[owner] == nil {
				byOwner[owner] = map[string]struct{}{}
			}
			byOwner[owner][parent] = struct{}{}
			return nil
		})
	return parents, err
}

// rubyConstantRows is what one candidate qualified name resolves to. Reopened
// declarations of the same kind are one semantic constant, so the kinds are a
// set rather than a count: two `class App.Service` rows are not an ambiguity,
// while a `class` row and a `module` row for one name is a contradiction Ruby
// would raise on and this pass refuses to break.
//
// `reassigned` is the P22.48-F1 hazard: syntax proved the constant's identity
// moved (`Service = Other`, `const_set`, `remove_const`, `autoload`), so the
// declarations are no longer evidence of what the name denotes. It is tracked
// per production/test origin like the declarations themselves -- spec code that
// monkey-patches a constant must not erase the production answer.
type rubyConstantRows struct {
	anyKinds, productionKinds map[string]struct{}
	reassignedAny             bool
	reassignedProduction      bool
}

type rubyConstantVisibility struct {
	overrides, productionOverrides map[string]map[string]struct{}
	hazards, productionHazards     map[string]struct{}
}

func rubyConstantVisible(v rubyConstantVisibility, owner, name string, callerIsTest, explicit bool) bool {
	if !explicit {
		return true
	}
	if owner == "" {
		owner = "Object"
	}
	hazards, overrides := v.hazards, v.overrides
	if !callerIsTest {
		hazards, overrides = v.productionHazards, v.productionOverrides
	}
	if _, ok := hazards[owner]; ok {
		return false
	}
	values := overrides[owner+"."+name]
	_, public := values[rubyVisibilityPublic]
	return len(values) == 0 || (len(values) == 1 && public)
}

func rubyLoadConstantVisibility(ctx context.Context, q rubyScopeQuery, repoID int64, qnames []string, testFiles map[int64]struct{}) (rubyConstantVisibility, error) {
	v := rubyConstantVisibility{
		overrides: map[string]map[string]struct{}{}, productionOverrides: map[string]map[string]struct{}{},
		hazards: map[string]struct{}{}, productionHazards: map[string]struct{}{},
	}
	owners := make(map[string]struct{}, len(qnames)*2)
	names := make(map[string]struct{}, len(qnames))
	for _, qname := range qnames {
		if dot := strings.LastIndexByte(qname, '.'); dot >= 0 {
			owners[qname[:dot]] = struct{}{}
			names[qname[dot+1:]] = struct{}{}
		} else {
			owners["Object"] = struct{}{}
			names[qname] = struct{}{}
		}
	}
	if len(owners) == 0 || len(names) == 0 {
		return v, nil
	}
	err := sqliteBatchedQuery(ctx, q, `SELECT h.import_kind,h.owner_module,h.local_name,h.source_specifier,h.file_id
FROM scope_import_evidence h JOIN files f ON f.id=h.file_id
WHERE h.repo_id=? AND h.language='ruby' AND f.is_deleted=0 AND h.is_static=1
  AND h.import_kind IN ('`+graph.ScopeImportRubyConstantVisibility+`','`+graph.ScopeImportRubyConstantVisibilityUnknown+`')
  AND h.owner_module IN (`, "%s)", []any{repoID}, stringSliceToAny(sortedKeys(owners)), true,
		func(rows *sql.Rows) error {
			var kind, owner, name, specifier string
			var fileID int64
			if err := rows.Scan(&kind, &owner, &name, &specifier, &fileID); err != nil {
				return err
			}
			if kind != graph.ScopeImportRubyConstantVisibilityUnknown {
				if _, ok := names[name]; !ok {
					return nil
				}
			}
			if kind == graph.ScopeImportRubyConstantVisibilityUnknown {
				v.hazards[owner] = struct{}{}
				if _, test := testFiles[fileID]; !test {
					v.productionHazards[owner] = struct{}{}
				}
				return nil
			}
			if _, ok := v.overrides[owner+"."+name]; !ok {
				v.overrides[owner+"."+name] = map[string]struct{}{}
			}
			v.overrides[owner+"."+name][specifier] = struct{}{}
			if _, test := testFiles[fileID]; !test {
				if v.productionOverrides[owner+"."+name] == nil {
					v.productionOverrides[owner+"."+name] = map[string]struct{}{}
				}
				v.productionOverrides[owner+"."+name][specifier] = struct{}{}
			}
			return nil
		})
	return v, err
}

func (r rubyConstantRows) kinds(callerIsTest bool) map[string]struct{} {
	if callerIsTest {
		return r.anyKinds
	}
	return r.productionKinds
}

func (r rubyConstantRows) reassigned(callerIsTest bool) bool {
	if callerIsTest {
		return r.reassignedAny
	}
	return r.reassignedProduction
}

// rubyConstantState is stage 1's answer for one lexical level.
type rubyConstantState int

const (
	// rubyConstantAbsent: the level does not own the name -- continue outward.
	rubyConstantAbsent rubyConstantState = iota
	// rubyConstantCoherent: the level owns the name and it denotes exactly one
	// semantic class/module -- choose it and stop.
	rubyConstantCoherent
	// rubyConstantConflicting: the level owns the name but its declarations
	// disagree materially (class and module) -- stop, unresolved.
	rubyConstantConflicting
	// rubyConstantIdentityUnknown: the level owns the name but syntax proved
	// the identity was reassigned -- stop, unresolved. A reassigned constant
	// still OWNS the lexical name, so this must not continue outward: Ruby
	// would find this level's constant, whatever it now points at.
	rubyConstantIdentityUnknown
)

// rubyLoadConstants loads the class/module declarations of exactly the
// candidate names the lexical chains produced. Deleted files establish nothing.
func rubyLoadConstants(ctx context.Context, q rubyScopeQuery, repoID int64, qnames []string, testFiles map[int64]struct{}) (map[string]rubyConstantRows, error) {
	constants := make(map[string]rubyConstantRows, len(qnames))
	row := func(qname string) rubyConstantRows {
		c, found := constants[qname]
		if !found {
			c = rubyConstantRows{anyKinds: map[string]struct{}{}, productionKinds: map[string]struct{}{}}
		}
		return c
	}
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.qualified_name,s.kind,s.file_id
FROM symbols s JOIN files f ON f.id=s.file_id
WHERE s.repo_id=? AND s.language='ruby' AND s.kind IN ('class','type') AND f.is_deleted=0
  AND s.qualified_name IN (`, "%s)", []any{repoID}, stringSliceToAny(qnames), true,
		func(rows *sql.Rows) error {
			var qname, kind string
			var fileID int64
			if err := rows.Scan(&qname, &kind, &fileID); err != nil {
				return err
			}
			c := row(qname)
			c.anyKinds[kind] = struct{}{}
			if _, isTest := testFiles[fileID]; !isTest {
				c.productionKinds[kind] = struct{}{}
			}
			constants[qname] = c
			return nil
		}); err != nil {
		return nil, err
	}
	// The identity hazards for the same names, read repository-wide: Ruby
	// reopens modules, so the reassignment may sit in a file that declares
	// nothing else about the constant.
	//
	// The selection is by last segment, not by owner: `local_name` is indexed
	// and `owner_module` is not, and the candidate set holds one qname per
	// lexical level, so an `owner_module IN (...)` predicate would turn into
	// one table scan per batch. The exact owner is matched in Go instead.
	wanted := make(map[string]struct{}, len(qnames))
	leafSet := make(map[string]struct{}, len(qnames))
	for _, qname := range qnames {
		wanted[qname] = struct{}{}
		leafSet[qname[strings.LastIndexByte(qname, '.')+1:]] = struct{}{}
	}
	err := sqliteBatchedQuery(ctx, q, `SELECT h.owner_module,h.file_id
FROM scope_import_evidence h JOIN files f ON f.id=h.file_id
WHERE h.repo_id=? AND h.language='ruby' AND f.is_deleted=0
  AND h.import_kind='`+graph.ScopeImportRubyConstantIdentityUnknown+`'
  AND h.local_name IN (`, "%s)", []any{repoID}, stringSliceToAny(sortedKeys(leafSet)), true,
		func(rows *sql.Rows) error {
			var qname string
			var fileID int64
			if err := rows.Scan(&qname, &fileID); err != nil {
				return err
			}
			if _, ok := wanted[qname]; !ok {
				return nil
			}
			c := row(qname)
			c.reassignedAny = true
			if _, isTest := testFiles[fileID]; !isTest {
				c.reassignedProduction = true
			}
			constants[qname] = c
			return nil
		})
	return constants, err
}

// rubyOwnsConstant answers stage 1 for one lexical level. The four outcomes are
// distinct on purpose: only "absent" continues outward, because an outer level
// cannot be the answer to a name an inner level already claims -- incoherently,
// or as something other than its own declaration.
func rubyOwnsConstant(constants map[string]rubyConstantRows, qname string, callerIsTest bool) rubyConstantState {
	rows := constants[qname]
	if rows.reassigned(callerIsTest) {
		return rubyConstantIdentityUnknown
	}
	switch len(rows.kinds(callerIsTest)) {
	case 0:
		return rubyConstantAbsent
	case 1:
		return rubyConstantCoherent
	}
	return rubyConstantConflicting
}

// rubySingletonVisibility is every visibility fact recorded for the owners a
// stage-1 lookup chose: named overrides keyed by `owner.name`, and the owners
// whose singleton surface is unprovable.
type rubySingletonVisibility struct {
	overrides map[string]map[string]struct{}
	hazards   map[string]struct{}
}

// eligible reports whether an owner's singleton method of this name may be
// reached through an explicit constant receiver. The definition site must say
// public and every override must agree; overrides that disagree with each other
// have no load order to settle them, and an owner-level hazard withdraws the
// whole owner.
func (v rubySingletonVisibility) eligible(owner, method, declared string) bool {
	if declared != rubyVisibilityPublic {
		return false
	}
	if _, hazard := v.hazards[owner]; hazard {
		return false
	}
	specifiers := v.overrides[owner+"."+method]
	if len(specifiers) > 1 {
		return false
	}
	for specifier := range specifiers {
		if specifier != rubyVisibilityPublic {
			return false
		}
	}
	return true
}

// rubyLoadSingletonVisibility reads the visibility facts of exactly the chosen
// owners, repository-wide: Ruby reopens classes, so `private_class_method :run`
// may sit in a file the definition never mentions. Deleted files state nothing.
//
// scope_import_evidence is indexed on (repo_id, source_specifier),
// (repo_id, local_name) and (repo_id, file_id), none of which leads with
// owner_module, so this is one repo-range scan of that table per pass rather
// than an index seek. It is bounded by the repository's import-evidence rows,
// not by anything quadratic, and a (repo_id, owner_module) index is the upgrade
// path if it ever shows up in a profile.
func rubyLoadSingletonVisibility(ctx context.Context, q rubyScopeQuery, repoID int64, owners []string) (rubySingletonVisibility, error) {
	v := rubySingletonVisibility{overrides: map[string]map[string]struct{}{}, hazards: map[string]struct{}{}}
	err := sqliteBatchedQuery(ctx, q, `SELECT v.import_kind,v.owner_module,v.local_name,v.source_specifier
FROM scope_import_evidence v JOIN files f ON f.id=v.file_id
WHERE v.repo_id=? AND v.language='ruby' AND f.is_deleted=0 AND v.is_static=1
  AND v.import_kind IN ('`+graph.ScopeImportRubySingletonVisibility+`','`+graph.ScopeImportRubySingletonVisibilityUnknown+`')
  AND v.owner_module IN (`, "%s)", []any{repoID}, stringSliceToAny(owners), true,
		func(rows *sql.Rows) error {
			var kind, owner, name, specifier string
			if err := rows.Scan(&kind, &owner, &name, &specifier); err != nil {
				return err
			}
			if kind == graph.ScopeImportRubySingletonVisibilityUnknown {
				v.hazards[owner] = struct{}{}
				return nil
			}
			if name == "" || specifier == "" {
				// A named override with no name proves nothing about any one
				// method, so it withdraws the owner rather than nothing.
				v.hazards[owner] = struct{}{}
				return nil
			}
			key := owner + "." + name
			if v.overrides[key] == nil {
				v.overrides[key] = map[string]struct{}{}
			}
			v.overrides[key][specifier] = struct{}{}
			return nil
		})
	return v, err
}

// rubyScopeQuery is the read/write surface the pass needs; *sql.Tx and *sql.DB
// both satisfy it. Callers on a pooled *sql.DB must wrap the pass in a
// transaction (resolveRubyScopeStandalone) so the resolution temp table stays
// on one connection.
type rubyScopeQuery = javaQuery

type rubyScopeEdge struct {
	id        int64
	fileID    int64
	evidence  string
	dstName   string
	container string
	static    int64
}

type rubyScopeBinding struct {
	dst      int64
	strategy string
}

// resolveRubyScope runs the pass over every unresolved ordinary Ruby call edge
// of the repository, or only over `only` when it is non-empty, and reports how
// many edges it bound. Unsupported receiver forms are owned but never loaded:
// nothing here can bind them, and the veto that keeps the generic strategies
// off them is a static predicate.
func resolveRubyScope(ctx context.Context, q rubyScopeQuery, repoID int64, only map[int64]struct{}) (int, error) {
	// The source method is required, not optional: an edge with no trustworthy
	// source symbol cannot prove whose self this is, nor which lexical constant
	// scope its definition sits in. It is still owned, so it simply never
	// appears here and stays unresolved.
	var edges []rubyScopeEdge
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.evidence,e.dst_name,src.container_name,src.is_static
FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols src ON src.id=e.src_symbol_id
WHERE e.repo_id=? AND f.language='ruby' AND e.edge_kind='`+EdgeKindCalls+`' AND e.dst_symbol_id IS NULL
  AND e.evidence IN ('`+rubyImplicitReceiver+`','`+rubySelfReceiver+`','`+rubyConstantReceiver+`')
  AND src.repo_id=e.repo_id AND src.language='ruby' AND src.kind='function'
  AND src.is_static IS NOT NULL`,
		" AND e.id IN (%s)", []any{repoID}, int64SliceToAny(sortedIDs(only)), len(only) > 0,
		func(rows *sql.Rows) error {
			var e rubyScopeEdge
			if err := rows.Scan(&e.id, &e.fileID, &e.evidence, &e.dstName, &e.container, &e.static); err != nil {
				return err
			}
			edges = append(edges, e)
			return nil
		}); err != nil {
		return 0, err
	}
	if len(edges) == 0 {
		return 0, nil
	}

	// Test-file classification is P7's, computed once from `files` rather than
	// per candidate, and read the same way on both resolver paths so the two
	// cannot disagree. Ruby needs it more than its siblings do: a spec file may
	// reopen a production class and add methods to it, and a production caller
	// must never be bound into one.
	testFiles, err := testFileIDsForRepo(ctx, q, repoID)
	if err != nil {
		return 0, err
	}

	// One exact qualified name per wanted member, loaded in batches: never a
	// query per edge, and never a scan of every Ruby method named `run`.
	type want struct {
		method, strategy, qname string
		static                  int64
		public                  bool
	}
	wants := make(map[int64]want, len(edges))
	qnameSet := make(map[string]struct{}, len(edges))
	constantFiles := make(map[int64]struct{}, 4)
	for _, e := range edges {
		if e.evidence == rubyConstantReceiver {
			if _, _, _, _, ok := rubyConstantCall(e.dstName); ok {
				constantFiles[e.fileID] = struct{}{}
			}
			continue
		}
		method, strategy, ok := rubyScopeMethod(e.evidence, e.dstName)
		if !ok {
			continue
		}
		if e.container == "" {
			continue
		}
		qname := e.container + "." + method
		wants[e.id] = want{method: method, strategy: strategy, qname: qname, static: e.static}
		qnameSet[qname] = struct{}{}
	}

	// Stage 1 for the constant receivers: which semantic constant the receiver
	// names, decided from the caller's own lexical nesting and from constant
	// existence alone. Member availability is deliberately not an input -- see
	// the file header -- so this runs to completion before any method is
	// considered.
	owners := make(map[string]struct{}, len(constantFiles))
	if len(constantFiles) > 0 {
		parents, err := rubyLoadLexicalParents(ctx, q, repoID, sortedIDs(constantFiles))
		if err != nil {
			return 0, err
		}
		type lookup struct {
			method     string
			fileID     int64
			absolute   bool
			segments   []string
			candidates []string
		}
		lookups := make(map[int64]lookup, len(constantFiles))
		candidateSet := make(map[string]struct{}, len(constantFiles))
		for _, e := range edges {
			if e.evidence != rubyConstantReceiver {
				continue
			}
			absolute, segments, method, _, ok := rubyConstantCall(e.dstName)
			if !ok {
				continue
			}
			var candidates []string
			if absolute {
				candidates = []string{segments[0]}
			} else {
				chain, chainOK := rubyLexicalChain(parents[e.fileID], e.container)
				if !chainOK {
					continue
				}
				for _, level := range chain {
					candidates = append(candidates, level+"."+segments[0])
				}
			}
			for _, candidate := range candidates {
				candidateSet[candidate] = struct{}{}
				for _, segment := range segments[1:] {
					candidate += "." + segment
					candidateSet[candidate] = struct{}{}
				}
			}
			lookups[e.id] = lookup{method: method, fileID: e.fileID, absolute: absolute, segments: segments, candidates: candidates}
		}
		declared, err := rubyLoadConstants(ctx, q, repoID, sortedKeys(candidateSet), testFiles)
		if err != nil {
			return 0, err
		}
		visibility, err := rubyLoadConstantVisibility(ctx, q, repoID, sortedKeys(candidateSet), testFiles)
		if err != nil {
			return 0, err
		}
		for id, l := range lookups {
			_, callerIsTest := testFiles[l.fileID]
			for _, candidate := range l.candidates {
				state := rubyOwnsConstant(declared, candidate, callerIsTest)
				if state == rubyConstantAbsent {
					continue
				}
				visibilityOwner := candidate
				if l.absolute {
					visibilityOwner = "Object"
				}
				if state != rubyConstantCoherent || !rubyConstantVisible(visibility, visibilityOwner, l.segments[0], callerIsTest, l.absolute) {
					break
				}
				owner := candidate
				valid := true
				for _, segment := range l.segments[1:] {
					parent := owner
					owner += "." + segment
					if rubyOwnsConstant(declared, owner, callerIsTest) != rubyConstantCoherent || !rubyConstantVisible(visibility, parent, segment, callerIsTest, true) {
						valid = false
						break
					}
				}
				if valid {
					qname := owner + "." + l.method
					strategy := ResolutionStrategyRubyLexicalConstant
					if l.absolute || len(l.segments) > 1 {
						strategy = ResolutionStrategyRubyConstantPath
					}
					wants[id] = want{method: l.method, strategy: strategy, qname: qname, static: 1, public: true}
					qnameSet[qname] = struct{}{}
					owners[owner] = struct{}{}
				}
				break
			}
		}
	}
	if len(qnameSet) == 0 {
		return 0, nil
	}
	visibility, err := rubyLoadSingletonVisibility(ctx, q, repoID, sortedKeys(owners))
	if err != nil {
		return 0, err
	}
	qnames := stringSliceToAny(sortedKeys(qnameSet))

	// candidates is keyed by (qualified name, staticness): the two natures are
	// separate methods, so one never makes the other ambiguous. A key holding
	// more than one declaration of the kind a caller may bind is a redefinition
	// whose runtime winner Ruby decides by load order, which is not modelled --
	// count it and fail closed -- including when the duplicates disagree about
	// visibility, since which one Ruby keeps is exactly the load order that is
	// not modelled. Visibility is therefore a veto on the sole declaration a
	// constant receiver would otherwise reach, never a filter that thins the
	// ambiguity count down to one.
	type natureKey struct {
		qname  string
		static int64
	}
	type candidate struct {
		anyID, productionID       int64
		anyCount, productionCount int
		anyOK, productionOK       bool
	}
	candidates := make(map[natureKey]candidate, len(qnameSet))
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.qualified_name,s.container_name,s.is_static,s.file_id,s.visibility
FROM symbols s JOIN files f ON f.id=s.file_id
WHERE s.repo_id=? AND s.language='ruby' AND s.kind='function' AND s.is_static IS NOT NULL AND f.is_deleted=0
  AND s.qualified_name IN (`,
		"%s)", []any{repoID}, qnames, true,
		func(rows *sql.Rows) error {
			var id, static, fileID int64
			var qname, container, declared string
			if err := rows.Scan(&id, &qname, &container, &static, &fileID, &declared); err != nil {
				return err
			}
			// The container must match exactly; a qualified name alone could be
			// a same-named member of a differently nested container.
			method := qname[strings.LastIndexByte(qname, '.')+1:]
			if container+"."+method != qname {
				return nil
			}
			key := natureKey{qname: qname, static: static}
			c := candidates[key]
			eligible := visibility.eligible(container, method, declared)
			c.anyCount++
			c.anyID, c.anyOK = id, eligible
			if _, isTest := testFiles[fileID]; !isTest {
				c.productionCount++
				c.productionID, c.productionOK = id, eligible
			}
			candidates[key] = c
			return nil
		}); err != nil {
		return 0, err
	}

	res := make(map[int64]rubyScopeBinding, len(wants))
	for _, e := range edges {
		w, ok := wants[e.id]
		if !ok {
			continue
		}
		c, found := candidates[natureKey{qname: w.qname, static: w.static}]
		if !found {
			continue
		}
		// P7: a test caller may reach any sole declaration; a production caller
		// may reach only a sole production one.
		dst, count, eligible := c.productionID, c.productionCount, c.productionOK
		if _, callerIsTest := testFiles[e.fileID]; callerIsTest {
			dst, count, eligible = c.anyID, c.anyCount, c.anyOK
		}
		if count != 1 || (w.public && !eligible) {
			continue
		}
		res[e.id] = rubyScopeBinding{dst: dst, strategy: w.strategy}
	}
	if len(res) == 0 {
		return 0, nil
	}
	return rubyScopeApply(ctx, q, res)
}

// rubyScopeApply writes the decided bindings through a temp table so one
// UPDATE covers every edge, whatever the batch size.
func rubyScopeApply(ctx context.Context, q rubyScopeQuery, res map[int64]rubyScopeBinding) (int, error) {
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+rubyScopeResolution+`(edge_id INTEGER PRIMARY KEY,dst_symbol_id INTEGER NOT NULL,strategy TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM `+rubyScopeResolution); err != nil {
		return 0, err
	}
	ids := make([]int64, 0, len(res))
	for id := range res {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows := make([][]any, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, []any{id, res[id].dst, res[id].strategy})
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO `+rubyScopeResolution+`(edge_id,dst_symbol_id,strategy) VALUES `, "(?,?,?)", nil, rows); err != nil {
		return 0, err
	}
	// Both Ruby strategies are registered at the same tier, so the confidence
	// is a constant here; resolutionConfidenceFor keeps that honest.
	confidence := resolutionConfidenceFor(ResolutionStrategyRubyImplicitSelf)
	if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT dst_symbol_id FROM `+rubyScopeResolution+` r WHERE r.edge_id=edges.id),resolution_strategy=(SELECT strategy FROM `+rubyScopeResolution+` r WHERE r.edge_id=edges.id),resolution_confidence='`+confidence+`' WHERE id IN (SELECT edge_id FROM `+rubyScopeResolution+`)`); err != nil {
		return 0, err
	}
	_, err := q.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+rubyScopeResolution)
	return len(res), err
}

// rubyStaleConstantBindings reports the ruby_lexical_constant bindings an
// incremental save may have invalidated. Those bindings depend on facts that
// are not the destination's own name: which constant the caller's nesting owns,
// whether that constant's identity was reassigned, and whether the owner made
// the method private somewhere else entirely. Ruby reopens classes and
// modules, so `private_class_method :run` or `Service = Other` sits in a body
// that declares some ANCESTOR of the owner -- a file holding only
// `module App; Service = Other; end` declares nothing but `App`. Every dotted
// segment of the destination's and the caller's container therefore counts, not
// just the last one.
//
// Over-approximating costs one re-decision that reaches the same answer;
// under-approximating leaves a binding asserting a call that now raises.
func (s *Store) rubyStaleConstantBindings(ctx context.Context, repoID int64, wanted map[string]struct{}, rubyChanged bool) ([]int64, error) {
	if len(wanted) == 0 && !rubyChanged {
		return nil, nil
	}
	// edges.resolution_strategy is not indexed, so the scan below is repo-wide.
	// A repository with no Ruby in it must not pay for that on every save.
	var hasRuby bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM files WHERE repo_id = ? AND language = 'ruby' AND is_deleted = 0)`,
		repoID).Scan(&hasRuby); err != nil || !hasRuby {
		return nil, err
	}
	namesAnySegment := func(qname string) bool {
		for _, segment := range strings.Split(qname, ".") {
			if _, ok := wanted[segment]; ok {
				return true
			}
		}
		return false
	}
	var stale []int64
	err := sqliteScanRows(ctx, s.db, `
		SELECT e.id, d.name, d.container_name, COALESCE(src.container_name, '')
		FROM edges e JOIN symbols d ON d.id = e.dst_symbol_id
		LEFT JOIN symbols src ON src.id = e.src_symbol_id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NOT NULL
		  AND e.resolution_strategy IN `+sqlQuotedList(rubyScopeStrategies), []any{repoID},
		func(rows *sql.Rows) error {
			var id int64
			var name, container, srcContainer string
			if err := rows.Scan(&id, &name, &container, &srcContainer); err != nil {
				return err
			}
			_, byMethod := wanted[name]
			if rubyChanged || byMethod || namesAnySegment(container) || namesAnySegment(srcContainer) {
				stale = append(stale, id)
			}
			return nil
		})
	return stale, err
}

// resolveRubyScopeStandalone runs the pass in its own transaction, for callers
// holding a pooled *sql.DB: the resolution temp table must see the same
// connection as the statements that fill and read it.
func (s *Store) resolveRubyScopeStandalone(ctx context.Context, repoID int64, only map[int64]struct{}) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := resolveRubyScope(ctx, tx, repoID, only)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func (s *Store) rubyConstantPathRepairApplies(ctx context.Context, repoID int64) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM files WHERE repo_id=? AND language='ruby' AND is_deleted=0)`, repoID).Scan(&found)
	return found, err
}

func (s *Store) repairRubyConstantPathBindings(ctx context.Context, repoID int64) error {
	clear := func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+`
WHERE repo_id=? AND edge_kind='`+EdgeKindCalls+`'
  AND file_id IN (SELECT id FROM files WHERE repo_id=? AND language='ruby')`, repoID, repoID)
		return err
	}
	_, err := s.resolveEdgesWithPreStep(ctx, repoID, clear)
	return err
}
