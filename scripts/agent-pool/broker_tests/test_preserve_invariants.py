"""PR-3b provider invariants that the merged gate cannot reach on its own.
Each test records one provider call from a passing flow and replays it with a
single input changed: a document, record or observation the resolver
returns, and the matching selection digest. The provider is pure, so a
replay sees exactly what the original call saw except that one value. The
unchanged replay must still pass, so every refusal is caused by the change."""

import copy
import unittest

import harness  # noqa: F401 - sys.path for broker
import preserve_world as pw
from broker import canonical, o5contract as oc, preserve, protocol
from broker.refusal import Code
from broker.stores import Crash
from preserve_world import SLOT

OTHER64 = "0" * 63 + "f"


class Res:
    """The world's resolver with some reads replaced."""

    def __init__(self, res, **fns):
        self._res, self._fns = res, fns

    def __getattr__(self, name):
        return self._fns[name] if name in self._fns else getattr(self._res, name)


def swap(sel, kind, digest):
    out = copy.deepcopy(sel)
    for part in ("evidence", "provider_inputs"):
        out[part] = [[k, digest if k == kind else d] for k, d in out.get(part, [])]
    return out


class Replay(unittest.TestCase):
    def setUp(self):
        self.w = pw.World()
        self.addCleanup(self.w.close)

    def record(self, name):
        calls, real = [], getattr(self.w.prov, name)

        def spy(*a):
            out = real(*a)
            calls.append((a, out))
            return out
        setattr(self.w.prov, name, spy)
        return calls

    def prov(self, **fns):
        return preserve.Preservation(Res(self.w.res, **fns))

    def decision(self, calls, cell):
        (args, out), = [(a, o) for a, o in calls if o.cell == cell and o.result == "PROCEED"][-1:]
        return args

    def decide(self, args, sel=None, **fns):
        ctx, s, view = args
        return self.prov(**fns).decide(ctx, s if sel is None else sel, view)

    def refuses(self, args, code, detail, sel=None, **fns):
        self.assertEqual(self.decide(args).result, "PROCEED")
        d = self.decide(args, sel, **fns)
        self.assertEqual((d.result, d.code, d.detail), ("REFUSE", code, detail))

    def doc_wrapper(self, change):
        real = self.w.res.burned_doc

        def burned_doc(d):
            doc = real(d)
            return change(d, doc) if doc is not None else None
        return burned_doc

    def ev(self, kind, data, at=None, scope="slot"):
        return pw.ev(self.w.env, kind, SLOT, data, self.w.w.clock() if at is None else at, scope=scope)


class Decisions(Replay):
    def test_freeze_start_document_generation_is_bound(self):
        calls = self.record("decide")
        self.w.to_p13()
        args = self.decision(calls, "D-QUAR")
        auth = args[0].pre.freeze_start_auth
        self.refuses(args, Code.STATE_UNKNOWN, "barrier_binding", burned_doc=self.doc_wrapper(
            lambda d, doc: dict(doc, slot_generation=doc["slot_generation"] + 1) if d == auth else doc))

    def test_quiescence_needs_the_revoked_record_as_the_journal_head(self):
        calls = self.record("decide")
        self.w.to_p13()
        args = self.decision(calls, "D-QUIES")
        later = self.w.fx.journal(self.w.st(), "thaw.done", attr="move")
        self.refuses(args, Code.PROVENANCE_INSUFFICIENT, "journal_mismatch", sel=swap(args[1], "journal_head", later))

    def at_decision(self, cell, changes):
        """Replays the first passing `cell` decision while the world is still
        in the state it saw: the unchanged decision, then one per change
        (selection, resolver overrides) that `changes(args)` returns."""
        got, real = [], self.w.prov.decide

        def spy(*args):
            out = real(*args)
            if not got and out.cell == cell and out.result == "PROCEED":
                got.append([self.decide(args)] + [self.decide(args, sel, **fns) for sel, fns in changes(args)])
            return out
        self.w.prov.decide = spy
        return got

    def assert_refused(self, got, code, detail):
        (normal, *changed), = got
        self.assertEqual(normal.result, "PROCEED")
        self.assertTrue(changed)
        for d in changed:
            self.assertEqual((d.result, d.code, d.detail), ("REFUSE", code, detail))

    def test_ready_observation_must_not_predate_the_verify_seal(self):
        self.w.to_verified()
        s = self.w.st()
        sealed = protocol.parse_doc(self.w.res.artifact(s["cid"], s["verify"]))["sealed_at"]
        old = self.ev("final_observation", self.w.final_data(), at=sealed - 1)
        got = self.at_decision("D-READY", lambda a: [(swap(a[1], "final_observation", old), {})])
        self.w.ok("preservation", "ready")
        self.assert_refused(got, Code.PROVENANCE_INSUFFICIENT, "evidence_stale")

    def test_ready_tier_c_record_must_name_this_quarantine(self):
        text = []

        def changes(a):
            t = self.w.w.tierc.read(SLOT)
            text.append(t)
            out = []
            for change in (("recovery_op=op1\n", "recovery_op=op2\n"), ("generation=2\n", "generation=3\n"),
                           ("state=quarantined\n", "state=leased\n")):
                other = t.replace(*change)
                self.assertNotEqual(other, t)
                out.append((swap(a[1], "tierc", canonical.sha256(other.encode())), {"tierc": lambda slot, o=other: o}))
            return out
        got = self.at_decision("D-READY", changes)
        self.w.to_ready()
        self.assert_refused(got, Code.NOT_CLEAN, "lease")

    def test_library_record_generation_is_bound(self):
        def changes(a):
            view = self.w.w.library.read(SLOT, "op1")
            rec = dict(view.rec, slot_generation=view.rec["slot_generation"] + 1)
            return [(swap(a[1], "library_record", canonical.digest(rec)), {"library": lambda slot, op: view._replace(rec=rec)})]
        got = self.at_decision("D-READY", changes)
        self.w.to_ready()
        self.assert_refused(got, Code.VERB_NOT_ALLOWED_IN_STATE, "library_state")

    def test_ready_terminal_resume_requires_no_marker(self):
        real = self.w.res.library
        got = self.at_decision("D-READY-T", lambda a: [(None, {"library": lambda slot, op: real(slot, op)._replace(
            marker="marker")})])
        self.w.to_verified()
        self.w.ok("preservation")
        self.w.env.faults.point = "library:before_marker_rename"
        with self.assertRaises(Crash):
            self.w.run("ready")
        self.w.env.faults.point = None
        self.w.ok("resume-step")
        self.assert_refused(got, Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")


class Handoff(Replay):
    def establish_args(self):
        calls = self.record("handoff_establish")
        self.w.to_p13()
        self.w.handoff()
        (args, out) = calls[-1]
        self.assertTrue(out.passed)
        return args

    def refused(self, args, reason, **sel_change):
        pre, sel, view = args
        for kind, digest in sel_change.items():
            sel = swap(sel, kind, digest)
        v = self.prov().handoff_establish(pre, sel, view)
        self.assertEqual((v.passed, v.reasons), (False, [reason]))

    def test_a_live_agent_process_fails_establish(self):
        args = self.establish_args()
        d = self.ev("uid_process_scan", {"uids": [pw.o5.AGENT_UID], "processes": [[4242, pw.o5.AGENT_UID]]},
                    scope="pool")
        self.refused(args, "uid_process", uid_process_scan=d)

    def test_claimed_linked_set_must_equal_the_journal(self):
        args = self.establish_args()
        data = {"parent": [0, 0o700, 0], "ancestors_root_owned": True, "entries": [["61", 0, 0o400, 0, 1]],
                "linked": [0o700, ["61"]]}
        self.refused(args, "linked", claimed_set_observation=self.ev("claimed_set_observation", data))

    def test_h_val_handoff_document_of_another_cycle(self):
        calls = self.record("handoff_validate")
        self.w.to_p13()
        self.w.handoff()
        self.w.ok("snapshot")
        (pre, sel, view), out = calls[-1]
        self.assertTrue(out.passed)
        v = self.prov(burned_doc=self.doc_wrapper(
            lambda _, d: dict(d, cycle_id=OTHER64) if d.get("verb") == "containment-handoff" else d)
        ).handoff_validate(pre, sel, view)
        self.assertEqual((v.passed, v.reasons), (False, ["barrier_binding"]))

    def test_d4_requires_the_ready_document_to_bind_this_preservation(self):
        calls = self.record("discharge")
        self.w.to_ready()
        self.assertEqual(pw.rel(self.w.release()), "D4")
        (view, holder, pre, sel), out = [c for c in calls if c[1] is not None and c[1].kind == "D4"][-1]
        self.assertIsNotNone(self.prov().discharge(view, holder, pre, sel))
        for change in (lambda d: dict(d, preservation=OTHER64), lambda d: dict(d, verb="preservation")):
            ready = self.doc_wrapper(lambda _, d, c=change: c(d) if d.get("verb") == "ready" else d)
            self.assertIsNone(self.prov(burned_doc=ready).discharge(view, holder, pre, sel))


if __name__ == "__main__":
    unittest.main()
