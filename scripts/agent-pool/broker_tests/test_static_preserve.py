"""PR-3b static boundaries (contract R4 §3.1 / B103, R4.1 N-R41-7): the PR-3b
modules are exactly the set test_static's A23 import-isolation check covers,
they import nothing that writes, executes or reaches live pool state, they
hold no write authority, and they keep no second copy of the PR-3a kind
tables."""

import ast
import unittest
from pathlib import Path

import harness  # noqa: F401 - sys.path for broker
import test_static
from broker import freeze, o5contract as oc, preserve

BROKER = Path(test_static.BROKER)
PR3B = {"gitclass.py", "journal.py", "preserve.py"}
ALLOWED_BROKER = {"barrier", "canonical", "evidence", "freeze", "gitbytes", "gitclass", "journal", "libview",
                  "o5contract", "protocol", "refusal"}
ALLOWED_TOP = {"re", "hashlib", "unicodedata", "zlib", "recovery_state"}
FORBIDDEN_NAMES = {"CommitTxn", "_issue", "_new_ingest", "_ISEAL", "_CSEAL", "_new_capability", "_SEAL", "_produce",
                   "OwnerCapability", "IngestCapability", "EffectCap", "FsCap", "BodyCap", "RecordStore", "BlobStore",
                   "fsprim", "fsstore", "fscap"}
FORBIDDEN_CALLS = {"put", "ingest", "revalidate", "finish", "confirm", "release", "kill", "unlink", "rename",
                   "write", "chmod", "chown", "chflags", "system", "popen"}
# The exact (module, names) each pure module imports: a new import is a reviewed change to this table.
PINNED_IMPORTS = {
    "journal.py": {("re", None), (".", "canonical"), (".", "o5contract"), (".", "protocol"), (".canonical", "is_hex64"),
                   (".refusal", "Code"), (".refusal", "Refused")},
    "preserve.py": {("re", None), ("recovery_state", None), *((".", m) for m in (
        "barrier", "canonical", "evidence", "freeze", "gitclass", "journal", "libview", "o5contract", "protocol")),
        (".canonical", "is_hex64"), *((".journal", n) for n in ("Mismatch", "need", "path_hex", "valid_component")),
        (".refusal", "Code"), (".refusal", "Refused")},
    "gitclass.py": {("hashlib", None), ("re", None), ("unicodedata", None), (".", "gitbytes"),
                    *((".gitbytes", n) for n in ("HEX40", "HEX64", "HFS_IGNORED", "OBJECT_TYPES", "is_gitdir_name"))},
    "gitbytes.py": {("hashlib", None), ("re", None), ("zlib", None)},
}


def imports(name):
    out = set()
    for node in ast.walk(ast.parse((BROKER / name).read_text(), name)):
        if isinstance(node, ast.Import):
            out |= {(a.name, None) for a in node.names}
        elif isinstance(node, ast.ImportFrom):
            out |= {("." * node.level + (node.module or ""), a.name) for a in node.names}
    return out


def trees():
    for name in sorted(PR3B):
        yield name, ast.parse((BROKER / name).read_text(), name)


class Boundaries(unittest.TestCase):
    def test_pr3b_modules_are_the_a23_set(self):
        self.assertEqual({n[:-3] for n in PR3B}, test_static.O5_PRESERVATION)
        found = {p.name for p in BROKER.glob("*.py")
                 if p.name[:-3] in test_static.O5_PRESERVATION or p.name.startswith(("preserve", "gitclass", "journal"))}
        self.assertEqual(found, PR3B)

    def test_imports_are_pure(self):
        for name, tree in trees():
            for node in ast.walk(tree):
                if isinstance(node, ast.Import):
                    for a in node.names:
                        self.assertIn(a.name, ALLOWED_TOP, f"{name}: import {a.name}")
                if isinstance(node, ast.ImportFrom):
                    if node.level:
                        mods = {node.module} if node.module else {a.name for a in node.names}
                        self.assertLessEqual(mods, ALLOWED_BROKER, name)
                    else:
                        self.assertIn(node.module, ALLOWED_TOP, name)

    def test_no_write_authority(self):
        for name, tree in trees():
            for node in ast.walk(tree):
                ident = node.id if isinstance(node, ast.Name) else node.attr if isinstance(node, ast.Attribute) else None
                self.assertNotIn(ident, FORBIDDEN_NAMES, name)
                if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute):
                    self.assertNotIn(node.func.attr, FORBIDDEN_CALLS, f"{name}: .{node.func.attr}()")
                for t in getattr(node, "targets", []) + ([node.target] if isinstance(node, ast.AugAssign) else []):
                    while isinstance(t, ast.Subscript):
                        t = t.value
                    if isinstance(t, ast.Attribute):
                        self.assertNotIn(t.attr, {"items", "docs", "data", "entries", "records"}, name)

    def test_imports_are_pinned(self):
        """No pure module, and not the shared `gitbytes` primitives, gains an
        import without a reviewed change here."""
        for name, want in PINNED_IMPORTS.items():
            self.assertEqual(imports(name), want, name)

    def test_only_the_provider_constructs_sealed_results(self):
        for name, tree in trees():
            uses = {n.attr for n in ast.walk(tree) if isinstance(n, ast.Attribute) and n.attr == "_PSEAL"}
            self.assertEqual(bool(uses), name == "preserve.py", name)


class Tables(unittest.TestCase):
    def test_no_second_kind_table(self):
        """B103: PR-3b reads the kind vocabulary from o5contract and the cell
        table from freeze; it defines neither."""
        for name, tree in trees():
            for node in tree.body:
                if isinstance(node, ast.Assign):
                    for t in node.targets:
                        if isinstance(t, ast.Name):
                            self.assertFalse(t.id.endswith("_KINDS") or t.id in ("DECISION_CELLS", "CELL_TABLE"),
                                             f"{name}: {t.id}")

    def test_resume_cells_match_the_relation(self):
        for cell, (intent, origins) in preserve._RESUME_INTENT.items():
            self.assertIn(cell, oc.DECISION_CELLS)
            self.assertEqual(oc.DECISION_CELLS[cell][0], "resume-step")
            self.assertIn("f_intent", oc.DECISION_CELLS[cell][2])
            self.assertIn(intent, freeze.INTENT_VERB)
            self.assertEqual(origins, (cell[:-2], cell))

    def test_decision_cell_is_the_gate_table(self):
        fs = ["NONE", "MOVED", "REVOKED", "FREEZE_INCOMPLETE", "FREEZE_BLOCKED", "THAW_INTENT", "SNAPSHOT_INTENT",
              "SNAPSHOTTED", "SNAPSHOT_FAILED", "VERIFY_INTENT", "VERIFIED", "VERIFY_FAILED"]
        for verb in list(oc.O5_VERBS) + ["resume-step"]:
            for row in oc.O5_ROWS + ("P6",):
                for f in fs:
                    pre = type("P", (), {"verb": verb, "row": row, "f": f})
                    spec = freeze.cell_spec(verb, row, f, False)
                    self.assertEqual(preserve.decision_cell(pre), spec.decision if spec else None)

    def test_failure_rows_are_the_relation_failures(self):
        rel = {(v, val) for v, spec in freeze.RELATION.items() for val in spec[2]}
        success = {("freeze-start", "MOVED"), ("freeze-continue", "REVOKED"), ("thaw", "NONE"),
                   ("snapshot", "SNAPSHOTTED"), ("verify-preservation", "VERIFIED")}
        self.assertEqual(set(preserve._FAILURE_ROWS) | success, rel)


if __name__ == "__main__":
    unittest.main()
