# CG-33 Composer oracle reproduction

Reproduces the accepted CG-33/CG-49 semantic checks without storing corpus
source or CodeGraph databases in this repository. Requires Git, Go, Python 3,
`sqlite3`, `shasum`, and network access to GitHub.

Run from a checkout containing commit `e532b8c6215bd9b1cdba0af537333d47f9eff364`:

```sh
scripts/cg33-oracle/reproduce.sh
```

The script archives and builds that exact CodeGraph commit, fetches four
repositories at the pinned commits in `repositories.tsv`, checks each
`composer.json` hash, and writes all working copies and output under a fresh
temporary directory. Set `CG33_WORK=/absolute/path` to retain output at a
chosen location; that path must be empty. Reproduction needs several gigabytes
of temporary space.

The independent `oracle.py` reads PHP source and Composer manifests for target
truth; the SQLite database supplies only the edge population. Fresh and S1
checks cover all six strategies: `php_composer_psr4`, `php_alias_static`,
`php_self_static`, `php_this_instance`, `php_type_scope`, and
`php_typed_property`. Each corpus must match its pinned `AGREE` count in the
manifest with zero `CHECK` rows. The script applies the byte-preserved S1
mutation definition (`mutate.py`, seed `cg33-seed-1`, first `3*K` sorted PHP
paths: delete, line shift, duplicate in rotation), then checks incremental vs
fresh S1 semantic digests. It also runs the nine source-grounded Composer
fixture assertions in `fixture_oracle.py`.

Accepted reference totals: fresh `68,444/68,444`; S1 `66,266/66,266`.
S1 per-corpus totals are in `repositories.tsv`. The nine fixtures cover
shorter-prefix fallback, root order, physical-but-unindexed target, wrong
class at the selected path, empty prefix, production/dev collision, dev-only,
malformed prefix, and unmapped class. Production-only exclusion of
`autoload-dev` is an explicit CodeGraph policy.

Original CG-49 outputs, binaries, corpora, and databases are preserved locally
outside the repository at:
`~/Library/Application Support/CodeGraph/evidence/cg49-acceptance-20261002/`.
They are evidence snapshots, not reproduction inputs. No CG-49 acceptance test
was rerun while preparing this setup.
