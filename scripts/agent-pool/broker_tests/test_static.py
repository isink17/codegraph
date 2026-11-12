"""Display (C9), operation table (C1), provenance (C7) and the AST boundaries
(A1 §A2.3 item 3, LB3, LB5, CommitTxn constructor, no verb string operations)."""

import ast
import hashlib
import random
import subprocess
import sys
import unittest
from pathlib import Path

import harness

from broker import authz, canonical, lifecycle, opclass, protocol, provenance, sanitize
from broker._testsupport import capability
from broker.refusal import Code, Refused

BROKER = Path(harness.HERE.parent / "broker")


class Sanitize(unittest.TestCase):
    def test_esc_is_injective_and_reversible(self):
        rng = random.Random(5)
        seen = {}
        for _ in range(3000):
            b = bytes(rng.randrange(256) for _ in range(rng.randrange(0, 12)))
            e = sanitize.esc(b)
            self.assertEqual(sanitize.unesc(e), b)
            self.assertTrue(all(0x20 <= ord(c) <= 0x7E for c in e))
            self.assertEqual(seen.setdefault(e, b), b)

    def test_dangerous_bytes_are_escaped(self):
        for raw in (b"\x1b]52;c;aGk=\x07", "‮".encode(), b"a\r\nb", b"\x7f", b'"', b"\\"):
            e = sanitize.esc(raw)
            self.assertNotIn("\x1b", e)
            self.assertNotIn("\n", e)
            self.assertTrue(e.replace("\\\\", "").replace('\\"', "").count('"') == 0)

    def test_render_is_a_function_of_the_bytes(self):
        doc = {"v": 1, "doc": "request", "verb": "request", "slot": "w", "reason": 'say "hi" \\ ' + "x" * 300,
               "evidence": {"v": 1, "components": [{"name": "a", "sha256": "f" * 64}]},
               "repair_plan": [{"log": "ledger", "action": "trim", "class": "TAIL_PREFIX", "offset": 10, "len": 4,
                                "sha256": "e" * 64, "body_sha256": "d" * 64, "evidence_pending": 0}]}
        data = canonical.canon(doc)
        text, digest = sanitize.render(data)
        self.assertEqual(sanitize.render(bytes(data)), (text, digest))
        self.assertEqual(digest, hashlib.sha256(data).hexdigest())
        lines = text.split("\n")
        self.assertTrue(lines[1].startswith("REPAIR ") and lines[2] == sanitize.UNPROVEN)
        self.assertTrue(lines[-2].startswith("CONFIRM " + digest))
        reason = [ln for ln in lines if ln.startswith("REASON = ")][0]
        self.assertIn("[truncated: ", reason)
        self.assertIn("EVIDENCE.COMPONENTS.0.SHA256", text)
        order = [ln.split(" = ")[0] for ln in lines if " = " in ln]
        self.assertLess(order.index("VERB"), order.index("EVIDENCE.COMPONENTS.0.SHA256"))
        self.assertEqual(order[-2], "REASON")

    def test_truncation_never_splits_an_escape(self):
        for n in range(110, 130):
            value = "a" * n + '"' * 10
            shown = sanitize._quoted(value)
            body = shown[1:shown.index('..."')]
            self.assertEqual(sanitize.unesc(body), value.encode()[:len(sanitize.unesc(body))])

    def test_nonce_line_holds_only_constants_and_the_digest(self):
        self.assertEqual(sanitize.nonce_prompt("a" * 64),
                         "CONFIRM " + "a" * 64 + ": type the nonce shown for this document")


class OperationTable(unittest.TestCase):
    def test_one_class_and_principal_per_pair(self):
        pairs = [(op.entry, op.verb) for op in opclass.TABLE]
        self.assertEqual(len(pairs), len(set(pairs)))
        for op in opclass.TABLE:
            self.assertIn(op.cls, opclass.CLASSES)
            if op.entry == opclass.POOLCTL:
                self.assertEqual(op.cls, opclass.READ_ONLY)

    def test_expected_classes(self):
        expect = {"stop": opclass.AVAILABILITY_ONLY, "mark-suspect": opclass.AVAILABILITY_ONLY,
                  "acquire": opclass.WORKTREE_MUTATING, "clear-suspect": opclass.WORKTREE_MUTATING,
                  "resume": opclass.OWNER_ADMIN, "custody-destroy": opclass.OWNER_ADMIN,
                  "quarantine": opclass.WORKTREE_MUTATING, "status": opclass.READ_ONLY}
        for verb, cls in expect.items():
            self.assertEqual(opclass.lookup(opclass.POOLCTL_OWNER, verb).cls, cls)

    def test_unknown_and_removed_refuse(self):
        for verb in sorted(opclass.REMOVED) + ["", "STATUS", "status ", "acquire\n"]:
            with self.assertRaises(Refused) as e:
                opclass.lookup(opclass.POOLCTL_OWNER, verb)
            self.assertIs(e.exception.code, Code.UNKNOWN_OPERATION)
        for verb in opclass.OWNER_VERBS:
            with self.assertRaises(Refused):
                opclass.lookup(opclass.POOLCTL, verb)

    def test_delegated_rows_always_refuse(self):
        self.assertTrue(all(v is False for v in opclass.DELEGATED_ROWS.values()))
        for op in opclass.TABLE:
            for owner in (True, False):
                with self.assertRaises(Refused) as e:
                    opclass.authorize(op.entry, op.verb, owner=owner, delegated=True)
                self.assertIs(e.exception.code, Code.DELEGATION_DISABLED)

    def test_owner_required(self):
        for verb in opclass.OWNER_VERBS:
            with self.assertRaises(Refused) as e:
                opclass.authorize(opclass.POOLCTL_OWNER, verb, owner=False)
            self.assertIs(e.exception.code, Code.OWNER_REQUIRED)
        opclass.authorize(opclass.POOLCTL, "status", owner=False)

    def test_every_owner_verb_has_a_row(self):
        row_verbs = {v for verbs in lifecycle.ROW_VERBS.values() for v in verbs} | {"stop", "resume"}
        self.assertEqual(row_verbs, set(opclass.OWNER_VERBS))


class Provenance(unittest.TestCase):
    def test_no_public_decisive_constructor(self):
        for prov in provenance.DECISIVE:
            with self.assertRaises(TypeError):
                provenance.supporting("a", prov, "0" * 64)
            with self.assertRaises(TypeError):
                provenance.Component("a", prov, "decisive", "0" * 64)

    def test_weakest_input_rule(self):
        root = capability.observed("a", "1" * 64)
        agent = provenance.supporting("b", provenance.Provenance.AGENT_REPORT, "2" * 64)
        d = provenance.derived("c", "3" * 64, [root, agent])
        self.assertEqual((d.provenance, d.role), (provenance.Provenance.AGENT_REPORT, "supporting"))
        d = provenance.derived("c", "3" * 64, [root])
        self.assertEqual(d.role, "decisive")

    def test_reverified_only_against_a_decisive_expected_digest(self):
        expected = capability.observed("manifest", "4" * 64)
        self.assertEqual(provenance.reverified("x", "4" * 64, True, expected).provenance,
                         provenance.Provenance.ROOT_REVERIFIED)
        self.assertEqual(provenance.reverified("x", "5" * 64, True, expected).role, "supporting")
        self.assertEqual(provenance.reverified("x", "4" * 64, False, expected).role, "supporting")
        weak = provenance.supporting("m", provenance.Provenance.BROKER_RECORD, "4" * 64)
        self.assertEqual(provenance.reverified("x", "4" * 64, True, weak).role, "supporting")

    def test_evidence_object(self):
        a = capability.observed("b_item", "1" * 64)
        b = provenance.supporting("a_item", provenance.Provenance.ISOLATED_VALIDATION, "2" * 64)
        ev = provenance.evidence([a, b])
        self.assertEqual([c["name"] for c in ev["components"]], ["a_item", "b_item"])
        with self.assertRaises(Refused):
            provenance.evidence([a, a])
        provenance.require_decisive([a], ["b_item"])
        with self.assertRaises(Refused):
            provenance.require_decisive([b], ["a_item"])

    def test_owner_decision_is_limited_to_its_fields(self):
        doc = {"reason": "r", "quiescence_method": "m", "evidence": {}, "head": "h", "task": "t"}
        self.assertEqual(set(provenance.owner_decision_fields(doc)), {"reason", "quiescence_method"})

    def test_trusted_observer_decides_nothing(self):
        with self.assertRaises(Refused):
            provenance.Component("a", provenance.Provenance.TRUSTED_OBSERVER, "decisive", "1" * 64)


class Trust(unittest.TestCase):
    def test_trust_is_derived(self):
        cap = capability.owner_capability()
        syn = authz.SyntheticOwner()
        self.assertEqual(authz.trust_of(authz.Ctx(cap, authz.PROTECTED)), authz.TRUST_OWNER)
        self.assertEqual(authz.trust_of(authz.Ctx(syn, authz.SYNTHETIC)), authz.TRUST_SYNTHETIC)
        for ctx in (authz.Ctx(cap, authz.SYNTHETIC), authz.Ctx(syn, authz.PROTECTED), authz.Ctx(None, "synthetic")):
            with self.assertRaises(Refused) as e:
                authz.trust_of(ctx)
            self.assertIs(e.exception.code, Code.TRUST_INSUFFICIENT)

    def test_capability_has_no_public_constructor(self):
        with self.assertRaises(TypeError):
            authz.OwnerCapability()
        with self.assertRaises(TypeError):
            authz._new_capability(object(), {})

    def test_commit_txn_has_no_public_constructor(self):
        from broker.authz_txn import CommitTxn
        with self.assertRaises(TypeError):
            CommitTxn()


BANNED_IMPORTS = {"threading", "asyncio", "concurrent", "multiprocessing", "atexit", "signal", "weakref", "sched",
                  "socket", "subprocess", "urllib", "http", "io", "os", "shutil", "tempfile", "pathlib", "ctypes"}
BANNED_CALLS = {"open", "exec", "eval", "compile", "__import__", "input"}
STR_METHODS = {"startswith", "endswith", "split", "rsplit", "partition", "rpartition", "replace", "lower", "upper",
               "strip", "find", "index", "format", "removeprefix", "removesuffix"}


# The read-only Git observer; every other module is protocol core.
OBSERVER = {"fdwalk.py", "gitraw.py", "indexverify.py"}
OBSERVER_IMPORTS = {"collections", "contextlib", "dataclasses", "enum", "errno", "hashlib", "os", "re", "stat", "zlib"}
OBSERVER_SIBLINGS = {"fdwalk", "gitraw", "indexverify", "sanitize"}
OS_FORBIDDEN = {"write", "writev", "pwrite", "remove", "unlink", "rename", "renames", "replace", "mkdir", "makedirs",
                "rmdir", "removedirs", "chmod", "fchmod", "lchmod", "chown", "fchown", "lchown", "chflags", "lchflags",
                "utime", "symlink", "link", "truncate", "ftruncate", "mkfifo", "mknod", "system", "popen", "fork",
                "forkpty", "kill", "killpg", "putenv", "unsetenv", "environ", "environb", "getenv", "getenvb",
                "posix_spawn", "posix_spawnp", "chdir", "fchdir", "chroot", "setxattr", "removexattr", "walk",
                "scandir", "fwalk", "O_WRONLY", "O_RDWR", "O_CREAT", "O_TRUNC", "O_APPEND", "O_EXCL"}


def modules(include_testsupport=False, observer=False):
    for p in sorted(BROKER.rglob("*.py")):
        if "_testsupport" in p.parts and not include_testsupport:
            continue
        if observer is not None and (p.name in OBSERVER) != observer:
            continue
        yield p, ast.parse(p.read_text(), str(p))


class AstBoundaries(unittest.TestCase):
    def test_no_io_threads_or_deferred_writes(self):
        for path, tree in modules():
            for node in ast.walk(tree):
                if isinstance(node, (ast.Import, ast.ImportFrom)):
                    names = [a.name for a in node.names] if isinstance(node, ast.Import) else [node.module or ""]
                    for n in names:
                        self.assertNotIn(n.split(".")[0], BANNED_IMPORTS, f"{path.name}: import {n}")
                if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
                    self.assertNotIn(node.func.id, BANNED_CALLS, f"{path.name}: {node.func.id}()")
                if isinstance(node, ast.FunctionDef):
                    self.assertNotEqual(node.name, "__del__", path.name)

    def test_private_constructors_are_referenced_only_where_allowed(self):
        allowed = {"_issue": {"authz_txn.py", "authz.py"}, "_new_capability": {"authz.py", "capability.py"},
                   "_SEAL": {"authz.py", "provenance.py", "capability.py"}, "_produce": {"provenance.py", "capability.py"}}
        for path, tree in modules(include_testsupport=True, observer=None):
            for node in ast.walk(tree):
                name = node.id if isinstance(node, ast.Name) else node.attr if isinstance(node, ast.Attribute) else (
                    node.name if isinstance(node, (ast.FunctionDef, ast.alias)) else None)
                if isinstance(node, ast.alias):
                    name = node.name
                if name in allowed:
                    self.assertIn(path.name, allowed[name], f"{path.name} references {name}")

    def test_no_production_module_imports_testsupport(self):
        for path, tree in modules(observer=None):
            for node in ast.walk(tree):
                if isinstance(node, ast.ImportFrom):
                    self.assertNotIn("_testsupport", (node.module or ""), path.name)
                    self.assertNotIn("_testsupport", [a.name for a in node.names], path.name)
                if isinstance(node, ast.Import):
                    self.assertFalse(any("_testsupport" in a.name for a in node.names), path.name)

    def test_no_string_operations_on_verb_names(self):
        for path, tree in modules(observer=None):
            for node in ast.walk(tree):
                if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) \
                        and node.func.attr in STR_METHODS:
                    target = node.func.value
                    name = target.id if isinstance(target, ast.Name) else (
                        target.attr if isinstance(target, ast.Attribute) else None)
                    self.assertNotEqual(name, "verb", f"{path.name}: verb.{node.func.attr}()")

    def test_module_level_has_no_side_effects(self):
        allowed = (ast.Import, ast.ImportFrom, ast.FunctionDef, ast.ClassDef, ast.Assign, ast.AnnAssign, ast.Expr)
        for path, tree in modules(include_testsupport=True, observer=None):
            for node in tree.body:
                self.assertIsInstance(node, allowed, f"{path.name}: {type(node).__name__}")
                if isinstance(node, ast.Expr):
                    self.assertIsInstance(node.value, ast.Constant, f"{path.name}: module-level call")

    def test_import_has_no_side_effects(self):
        code = ("import sys, os; sys.path.insert(0, %r); before=set(os.listdir(%r));"
                "import broker.invocation, broker._testsupport.capability;"
                "assert set(os.listdir(%r)) == before; print('ok')") % (
            str(BROKER.parent), str(BROKER), str(BROKER))
        out = subprocess.run([sys.executable, "-I", "-B", "-c", code], capture_output=True, text=True, timeout=60)
        self.assertEqual(out.stdout.strip(), "ok", out.stderr)

    def test_broker_is_not_imported_by_production_code(self):
        for p in sorted(BROKER.parent.glob("*.py")):
            if p.name.startswith("test_"):
                continue
            tree = ast.parse(p.read_text())
            for node in ast.walk(tree):
                if isinstance(node, (ast.Import, ast.ImportFrom)):
                    names = [a.name for a in node.names] + [getattr(node, "module", "") or ""]
                    self.assertFalse(any(n.split(".")[0] == "broker" for n in names), p.name)

    def test_core_does_not_import_the_observer(self):
        for path, tree in modules(include_testsupport=True):
            for node in ast.walk(tree):
                if isinstance(node, ast.ImportFrom):
                    names = {node.module or ""} | {a.name for a in node.names}
                    self.assertFalse(names & {"fdwalk", "gitraw", "indexverify"}, path.name)

    def test_observer_is_read_only_and_runs_nothing(self):
        found = set()
        for path, tree in modules(observer=True):
            found.add(path.name)
            for node in ast.walk(tree):
                if isinstance(node, ast.Import):
                    for a in node.names:
                        self.assertIn(a.name, OBSERVER_IMPORTS, f"{path.name}: import {a.name}")
                if isinstance(node, ast.ImportFrom):
                    if node.level:
                        names = {node.module} if node.module else {a.name for a in node.names}
                        self.assertLessEqual(names, OBSERVER_SIBLINGS, path.name)
                    else:
                        self.assertIn(node.module, OBSERVER_IMPORTS, path.name)
                if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
                    self.assertNotIn(node.func.id, BANNED_CALLS, f"{path.name}: {node.func.id}()")
                if isinstance(node, ast.Attribute) and isinstance(node.value, ast.Name) and node.value.id == "os":
                    self.assertFalse(node.attr in OS_FORBIDDEN or node.attr.startswith(("exec", "spawn")),
                                     f"{path.name}: os.{node.attr}")
                name = node.id if isinstance(node, ast.Name) else node.attr if isinstance(node, ast.Attribute) else None
                self.assertNotIn(name, {"_produce", "_SEAL", "_new_capability", "_issue", "provenance", "authz",
                                        "custody", "invocation", "Provenance"}, path.name)
        self.assertEqual(found, OBSERVER)

    def test_request_parser_never_trusts_owner_path_fields(self):
        for k in ("trust", "actor", "nonce", "session", "repair_plan", "issued_at", "not_after", "class"):
            self.assertIn(k, protocol.NOT_ALLOWED)


if __name__ == "__main__":
    unittest.main()
