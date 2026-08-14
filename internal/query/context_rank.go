package query

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

	"github.com/isink17/codegraph/internal/graph"
)

// Relevance classes, in the order an agent should read them. These strings are
// the public `relevance` values of TaskContextSymbol and predate P14.
const (
	relevanceDirectMatch = "direct_match"
	relevanceCaller      = "caller"
	relevanceCallee      = "callee"
	relevanceTest        = "test"
)

// The scoring model. It is small, centralized, and deliberately not learned.
//
// A candidate's score is
//
//	classBase(relevance) + classSignalWeight(relevance)*taskSignal + fileSupportBonus
//
// where taskSignal is the search relevance behind the candidate, normalized to
// [0,1], and fileSupportBonus is at most maxFileSupport*fileSupportUnit = 0.05.
// The resulting bands, bonus included, are
//
//	direct_match 0.60 .. 1.05
//	caller       0.35 .. 0.70
//	callee       0.30 .. 0.65
//	test         0.10 .. 0.30
//
// which encodes three properties the ranking tests pin:
//
//   - A task match with real search relevance outranks every caller, callee and
//     test, however large the fan-in of a hub in the same file: fan-in is not a
//     scoring input at all, and co-location is worth 0.01 a candidate up to five.
//   - The direct and caller bands overlap deliberately at their edges. A caller
//     or callee of the strongest seed reaches 0.70, above a hit the search itself
//     rated near zero (0.60), so strong graph evidence beats search noise --
//     while any hit the search rated even moderately stays ahead of it.
//   - Implementation context outranks the tests that cover it, which stay
//     available at the tail of the same stream.
const (
	classBaseDirectMatch = 0.60
	classBaseCaller      = 0.35
	classBaseCallee      = 0.30
	classBaseTest        = 0.10

	// signalWeightDirect scales the search engine's own relevance for a hit the
	// task matched directly; the graph weights scale the relevance a
	// caller/callee/test inherits from the seed it was expanded from, so graph
	// evidence supplements task relevance instead of replacing it.
	signalWeightDirect = 0.40
	signalWeightGraph  = 0.30
	signalWeightTest   = 0.15

	// fileSupportUnit rewards a file that several candidates point at. It is
	// small on purpose: co-location is weak evidence.
	fileSupportUnit = 0.01
	maxFileSupport  = 5
)

func classBase(relevance string) float64 {
	switch relevance {
	case relevanceDirectMatch:
		return classBaseDirectMatch
	case relevanceCaller:
		return classBaseCaller
	case relevanceCallee:
		return classBaseCallee
	default:
		return classBaseTest
	}
}

func signalWeight(relevance string) float64 {
	switch relevance {
	case relevanceDirectMatch:
		return signalWeightDirect
	case relevanceCaller, relevanceCallee:
		return signalWeightGraph
	default:
		return signalWeightTest
	}
}

// classPriority orders the classes when scores tie exactly.
func classPriority(relevance string) int {
	switch relevance {
	case relevanceDirectMatch:
		return 0
	case relevanceCaller:
		return 1
	case relevanceCallee:
		return 2
	default:
		return 3
	}
}

// contextCandidate is one symbol offered to the budget, with the evidence that
// earned it a place.
type contextCandidate struct {
	sym       graph.Symbol
	relevance string
	// taskSignal is the search relevance behind this candidate, normalized to
	// [0,1] against the strongest hit of the same request.
	taskSignal float64
	// fileSupport counts candidates that share this candidate's file.
	fileSupport int
	isTest      bool
	score       float64
}

// identity is the candidate's stable semantic identity, used for dedup and as
// the final ordering tie-break.
//
// The file path is part of it even when stable_key is present: a stable key is
// scoped to a package or to a file's base name, not to a path, so two files can
// legitimately produce the same key -- `store_cgo.go` and `store_nocgo.go` both
// declare `func:store::isSQLiteBusy` in this repository, and a TypeScript repo
// with an `index.ts` per directory does it wholesale. Keying dedup on the bare
// stable key would silently drop one of the two from the answer and from the
// counters.
func (c contextCandidate) identity() string {
	if c.sym.StableKey != "" {
		return c.sym.FilePath + "\x00" + c.sym.StableKey
	}
	return c.sym.FilePath + "\x00" + c.sym.QualifiedName + "\x00" + c.sym.Name
}

// baseScore is the score without the file-support bonus. Dedup compares this,
// because two records for one identity always share a file and so always share
// the bonus.
func (c contextCandidate) baseScore() float64 {
	return classBase(c.relevance) + signalWeight(c.relevance)*clamp01(c.taskSignal)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// candidateSet accumulates candidates, deduplicating by identity and keeping the
// strongest evidence for each. The same symbol legitimately arrives as a direct
// match, as a caller of one seed, and as a callee of another; the response
// carries it once, labelled with its strongest relevance.
type candidateSet struct {
	byIdentity map[string]int
	items      []contextCandidate
}

func newCandidateSet() *candidateSet {
	return &candidateSet{byIdentity: map[string]int{}}
}

func (cs *candidateSet) add(c contextCandidate) {
	if c.sym.FilePath == "" || (c.sym.QualifiedName == "" && c.sym.Name == "") {
		return
	}
	key := c.identity()
	idx, seen := cs.byIdentity[key]
	if !seen {
		cs.byIdentity[key] = len(cs.items)
		cs.items = append(cs.items, c)
		return
	}
	// The winning record decides both the relevance label and the section: a
	// symbol that is a caller of a seed is caller context even if a test link
	// also pointed at it, exactly as before P14.
	if betterEvidence(c, cs.items[idx]) {
		cs.items[idx] = c
	}
}

// betterEvidence reports whether candidate replaces existing. It is a strict,
// deterministic comparison: equal evidence never swaps, so insertion order
// cannot leak into the result.
func betterEvidence(candidate, existing contextCandidate) bool {
	cb, eb := candidate.baseScore(), existing.baseScore()
	if cb != eb {
		return cb > eb
	}
	if classPriority(candidate.relevance) != classPriority(existing.relevance) {
		return classPriority(candidate.relevance) < classPriority(existing.relevance)
	}
	// Prefer the record that carries a resolved identity over one that does not.
	return candidate.sym.ID != 0 && existing.sym.ID == 0
}

// finalize computes file support, scores every candidate, and returns the one
// deterministically ordered candidate stream the budget pages over.
func (cs *candidateSet) finalize() []contextCandidate {
	support := map[string]int{}
	for _, c := range cs.items {
		support[c.sym.FilePath]++
	}
	out := make([]contextCandidate, len(cs.items))
	copy(out, cs.items)
	for i := range out {
		out[i].fileSupport = support[out[i].sym.FilePath]
		bonus := float64(min(out[i].fileSupport, maxFileSupport)) * fileSupportUnit
		out[i].score = out[i].baseScore() + bonus
	}
	sortCandidates(out)
	return out
}

// sortCandidates imposes the total order: score, then class, then file path,
// then stable identity. No database row id and no insertion order participates,
// so any permutation of the same candidates yields byte-identical output.
func sortCandidates(items []contextCandidate) {
	sort.Slice(items, func(i, j int) bool { return lessCandidate(items[i], items[j]) })
}

func lessCandidate(a, b contextCandidate) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	if classPriority(a.relevance) != classPriority(b.relevance) {
		return classPriority(a.relevance) < classPriority(b.relevance)
	}
	if a.sym.FilePath != b.sym.FilePath {
		return a.sym.FilePath < b.sym.FilePath
	}
	return a.identity() < b.identity()
}

// rankingFingerprint hashes the ordered candidate stream. A continuation whose
// fingerprint no longer matches is refused: the same repository at the same scan
// can still re-rank (an embedding model swap changes hybrid scores), and
// silently resuming at an offset into a different order would skip and repeat
// context rather than continue it.
func rankingFingerprint(items []contextCandidate) string {
	h := sha256.New()
	_, _ = h.Write([]byte("codegraph/context-rank/v1\n"))
	for _, c := range items {
		_, _ = h.Write([]byte(c.identity()))
		_, _ = h.Write([]byte{'|'})
		_, _ = h.Write([]byte(c.relevance))
		_, _ = h.Write([]byte{'|'})
		_, _ = h.Write([]byte(strconv.FormatFloat(c.score, 'f', 6, 64)))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// normalizeTaskSignal maps a producer's raw relevance scores onto [0,1].
// Token-overlap sums and RRF scores live on entirely different scales, so the
// strongest hit of this request is the only meaningful reference point. When a
// producer reports no usable score, position in its ordered output stands in.
func normalizeTaskSignal(hits []searchHit) []float64 {
	signals := make([]float64, len(hits))
	maxScore := 0.0
	for _, h := range hits {
		if h.Score > maxScore {
			maxScore = h.Score
		}
	}
	for i, h := range hits {
		switch {
		case maxScore > 0:
			signals[i] = clamp01(h.Score / maxScore)
		case len(hits) > 0:
			signals[i] = 1 - float64(h.Rank)/float64(len(hits))
		}
	}
	return signals
}
