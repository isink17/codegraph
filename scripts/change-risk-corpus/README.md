# Change-risk real-corpus verification

Reproduces the real-code checks of `codegraph diff` described in
`docs/change-risk-verification.md`. Requires Git, Go, Python 3 and network
access to GitHub. Corpus clones, scratch commits and outputs stay in the work
directory; nothing is written to this repository.

```sh
go build -o /abs/bin/codegraph-cgo ./cmd/codegraph
CGO_ENABLED=0 go build -o /abs/bin/codegraph-nocgo ./cmd/codegraph
python3 -I scripts/change-risk-corpus/run.py --work /abs/empty-dir \
  --cgo /abs/bin/codegraph-cgo --nocgo /abs/bin/codegraph-nocgo
python3 -I scripts/change-risk-corpus/summarize.py /abs/empty-dir
```

`manifest.tsv` pins every repository (URL and commit) and lists the cases:
self diffs, real commits against their first parent, and controlled edits.
A controlled edit is applied to its base in a scratch clone (`run.py` edit
functions, or a patch under `patches/`) and committed with a fixed identity
and date, so its SHA is the same on every run. Every git, `codegraph` and Go
command runs without inherited `GIT_*` variables, without the system git
config and with an empty global git config in the work directory, so
`core.autocrlf`, filters, hooks or a `GIT_DIR` from the caller cannot change
the clones or the controlled SHAs. `--case ID` limits a run.

Per case `run.py` runs each binary twice and records: exit codes, byte
equality of the two runs, CGO / `CGO_ENABLED=0` agreement, the score's
traceability to evidence rows, the source checkout's HEAD, status, index,
refs and file times before and after, and `go build`, `go vet` and `go test`
on both revisions in an exported copy (a `go.mod` is added to that copy only
for a repository without one). `results/results.json` holds the rows.

The oracle executes code from the corpus repositories: their `go build`,
`go vet` and `go test` run on this machine, and the Go command downloads the
modules they require.
