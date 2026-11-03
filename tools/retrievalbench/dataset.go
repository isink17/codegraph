package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// DatasetSchema is the frozen dataset format this tool accepts.
const DatasetSchema = "codegraph.retrieval_bench_dataset/v1"

// Truth values. A query labelled TruthComplete asserts that its relevant set
// was enumerated exhaustively over the corpus, which is what makes an
// unlabelled returned file a false positive and precision a defined quantity.
// TruthPartial withholds precision and nDCG rather than computing them against
// a denominator the labels do not support.
const (
	TruthComplete = "complete"
	TruthPartial  = "partial"
)

// Label is one relevance judgement about a file, with the source evidence that
// justifies it.
type Label struct {
	File     string   `json:"file"`
	Symbols  []string `json:"symbols,omitempty"`
	Grade    int      `json:"grade,omitempty"`
	Evidence string   `json:"evidence"`
}

// Corpus pins the exact repository revision a query's labels were read from.
type Corpus struct {
	Name       string `json:"name"`
	Upstream   string `json:"upstream"`
	Tag        string `json:"tag"`
	Commit     string `json:"commit"`
	Tree       string `json:"tree"`
	Files      int64  `json:"files"`
	Symbols    int64  `json:"symbols"`
	References int64  `json:"references"`
	Edges      int64  `json:"edges"`
}

// Query is one frozen retrieval query with its labels.
type Query struct {
	ID         string  `json:"id"`
	Corpus     string  `json:"corpus"`
	Category   string  `json:"category"`
	Origin     string  `json:"origin"`
	HeldOut    bool    `json:"held_out"`
	Truth      string  `json:"truth"`
	QueryText  string  `json:"query"`
	Relevant   []Label `json:"relevant"`
	Irrelevant []Label `json:"irrelevant"`
}

// Dataset is the frozen query set.
type Dataset struct {
	Schema  string   `json:"schema"`
	ID      string   `json:"id"`
	Frozen  string   `json:"frozen"`
	Notes   string   `json:"notes"`
	Corpora []Corpus `json:"corpora"`
	Queries []Query  `json:"queries"`

	// SHA256 is the digest of the file the dataset was read from. It is the
	// dataset's identity in a report: two runs that disagree are comparable
	// only when this matches.
	SHA256 string `json:"-"`
}

// LoadDataset reads and validates a frozen dataset.
func LoadDataset(path string) (*Dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ds Dataset
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ds); err != nil {
		return nil, fmt.Errorf("decode dataset %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	ds.SHA256 = hex.EncodeToString(sum[:])
	if err := ds.Validate(); err != nil {
		return nil, err
	}
	return &ds, nil
}

// Validate refuses a dataset that cannot produce honest metrics: an unknown
// schema, a duplicate or empty query id, a query naming a corpus that is not
// pinned, an unknown truth value, a label with no evidence, a grade outside
// 1..3, or a file labelled both relevant and irrelevant for the same query.
func (d *Dataset) Validate() error {
	if d.Schema != DatasetSchema {
		return fmt.Errorf("dataset schema %q, want %q", d.Schema, DatasetSchema)
	}
	if len(d.Queries) == 0 {
		return fmt.Errorf("dataset has no queries")
	}
	corpora := map[string]bool{}
	for _, c := range d.Corpora {
		if c.Name == "" {
			return fmt.Errorf("corpus with empty name")
		}
		if corpora[c.Name] {
			return fmt.Errorf("duplicate corpus %q", c.Name)
		}
		if c.Commit == "" || c.Tree == "" {
			return fmt.Errorf("corpus %q: commit and tree are required identity", c.Name)
		}
		corpora[c.Name] = true
	}
	ids := map[string]bool{}
	for _, q := range d.Queries {
		switch {
		case q.ID == "":
			return fmt.Errorf("query with empty id")
		case ids[q.ID]:
			return fmt.Errorf("duplicate query id %q", q.ID)
		case !corpora[q.Corpus]:
			return fmt.Errorf("query %q names unpinned corpus %q", q.ID, q.Corpus)
		case strings.TrimSpace(q.QueryText) == "":
			return fmt.Errorf("query %q has empty query text", q.ID)
		case q.Category == "":
			return fmt.Errorf("query %q has no category", q.ID)
		case q.Truth != TruthComplete && q.Truth != TruthPartial:
			return fmt.Errorf("query %q: truth %q must be %q or %q", q.ID, q.Truth, TruthComplete, TruthPartial)
		case len(q.Relevant) == 0:
			return fmt.Errorf("query %q has no relevant labels", q.ID)
		}
		ids[q.ID] = true
		seen := map[string]string{}
		for _, l := range q.Relevant {
			if l.File == "" || strings.TrimSpace(l.Evidence) == "" {
				return fmt.Errorf("query %q: relevant label needs a file and evidence", q.ID)
			}
			if l.Grade < 1 || l.Grade > 3 {
				return fmt.Errorf("query %q: relevant %s grade %d outside 1..3", q.ID, l.File, l.Grade)
			}
			if prev, dup := seen[l.File]; dup {
				return fmt.Errorf("query %q: file %s labelled twice (%s)", q.ID, l.File, prev)
			}
			seen[l.File] = "relevant"
		}
		for _, l := range q.Irrelevant {
			if l.File == "" || strings.TrimSpace(l.Evidence) == "" {
				return fmt.Errorf("query %q: irrelevant label needs a file and evidence", q.ID)
			}
			if prev, dup := seen[l.File]; dup {
				return fmt.Errorf("query %q: file %s labelled twice (%s)", q.ID, l.File, prev)
			}
			seen[l.File] = "irrelevant"
		}
	}
	return nil
}

// CorpusByName returns the pinned corpus.
func (d *Dataset) CorpusByName(name string) (Corpus, bool) {
	for _, c := range d.Corpora {
		if c.Name == name {
			return c, true
		}
	}
	return Corpus{}, false
}

// CorpusNames lists the pinned corpora in a stable order.
func (d *Dataset) CorpusNames() []string {
	out := make([]string, 0, len(d.Corpora))
	for _, c := range d.Corpora {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}
