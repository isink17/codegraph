"""R2 model repair tests, ported from the approved R2 model suite: B1 identity before walk I/O (direct and pre-check entrypoints), B2 content pass needs the
read's own location check, B3 the call re-checks object identity, and the reviewer's two fail-closed survivors.
Deterministic: no sleeps, no real filesystem effects, observation hooks only."""
import os, sys, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fsharness as M  # noqa: E402
from test_fscap_fsmodel import attempt, XOBJ, RAW  # noqa: E402
from test_fscap_repair import Repair, DEV1, DEV2, GATE  # noqa: E402

QBAD = {"key": "9" * 64, "dev": M.ROOT_DEV}
SPBAD = {"key": "8" * 64, "dev": M.ROOT_DEV}
NEEDS_A = ("Refused", "content read needs every location observation A")


def walk_first(store, fscap):
    fscap.observe("walk_q")
    return M.effect_thaw(store, fscap)


def quarantine_first(store, fscap):
    fscap.observe("quarantine")
    return M.effect_thaw(store, fscap)


def content_first(store, fscap):
    fscap.read_content("originals")
    return M.effect_thaw(store, fscap)


ENTRYPOINTS = {M.effect_move_back: "thaw.move.intent", M.effect_thaw: "slot", walk_first: "walk_q",
               quarantine_first: "quarantine", content_first: "slot"}


# ---------------------------------------------------------------- B1: proven C is not masked by walk I/O

class B1IdentityBeforeWalkIO(Repair):
    def proven_C(self, **fs):
        for effect, first in ENTRYPOINTS.items():
            with self.subTest(effect=effect.__name__):
                self.fresh()
                disk, inv = self.thaw(objects="io", **fs)
                before = disk.fs()
                result = M.invoke(inv, effect)
                self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity"))
                rec = self.outcome(disk, inv, poison="identity", f="FREEZE_BLOCKED", tail="thaw.move.intent")
                self.assertEqual(self.obs(rec), [(first, "C")])
                self.assertEqual(([e for e in rec.log if e[0] in ("obs_io", "rename", "content")], rec.rename_issued),
                                 ([], False))
                self.assertEqual(disk.fs(), before)
                self.assertEqual(M.inspect_durable(inv)["fwal"], "completion_durable")

    def test_wrong_parent_identity_proven_before_walk_EIO(self):
        for name, fs in (("quarantine/<op_id>", dict(qdir=QBAD)), ("slot parent", dict(slot_parent=SPBAD))):
            with self.subTest(parent=name):
                self.proven_C(**fs)

    def test_substituted_key_at_the_Q_name_proven_before_walk_EIO(self):
        self.proven_C(dst=XOBJ)

    def test_unexpected_Q_entry_proven_before_walk_EIO(self):
        for name, fs in (("same device", {}), ("device mismatch", dict(slot_parent=DEV2))):
            with self.subTest(devices=name):
                self.proven_C(extra=["intruder"], **fs)

    def test_identity_observation_itself_EIO_fabricates_no_C(self):
        for effect, io, key, outcome in ((M.effect_move_back, ["call_pre"], "call_pre", ("unavailable", "observation I/O")),
                                         (M.effect_thaw, ["slot"], "slot", None),
                                         (walk_first, ["walk_q"], "walk_q", None),
                                         (quarantine_first, ["quarantine"], "quarantine", None)):
            with self.subTest(effect=effect.__name__):
                self.fresh()
                disk, inv = self.thaw(qdir=QBAD, io=io, objects="io")
                result = M.invoke(inv, effect)
                self.assertEqual(result[0], "UNFINISHED")
                rec = self.outcome(disk, inv, io=key, f="THAW_INTENT", tail="thaw.move.intent")
                self.assertEqual((self.obs(rec), rec.outcome, rec.rename_issued), ([], outcome, False))
                self.set(disk, io=[])                                   # evidence available: the C is now proven
                inv2 = M.start_invocation(disk, "resume-step")
                self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", "identity"))
                self.assertEqual(inv2.authority.renames, 0)

    def test_correct_Q_transient_EIO_is_unfinished_then_resume_completes(self):
        for name, fs, effect, key, outcome in (
                ("walk, direct", dict(objects="io"), M.effect_move_back, "call_pre", ("unavailable", "observation I/O")),
                ("walk, pre-check", dict(objects="io"), M.effect_thaw, "walk_q", None),
                ("content read", dict(content={"originals": "io"}), M.effect_thaw, "content originals", None)):
            with self.subTest(name):
                self.fresh()
                disk, inv = self.thaw(**fs)
                result = M.invoke(inv, effect)
                self.assertEqual(result[0], "UNFINISHED")
                rec = self.outcome(disk, inv, io=key, f="THAW_INTENT", tail="thaw.move.intent")
                self.assertEqual((rec.outcome, "C" in [c for _, c in self.obs(rec)]), (outcome, False))
                self.assertEqual((disk.fs()["dst"], disk.fs()["slot"]), (RAW, None))
                self.set(disk, objects="ok", content={})
                inv2 = M.start_invocation(disk, "resume-step")
                self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))
                self.assertEqual(inv2.authority.renames, 1)

    def test_correct_Q_authenticated_negative_verdict(self):
        for effect in (M.effect_move_back, M.effect_thaw):              # rows 1-7 below a verified Q: C
            with self.subTest(rows="1-7", effect=effect.__name__):
                self.fresh()
                disk, inv = self.thaw(objects="substituted")
                self.assertEqual(M.invoke(inv, effect), ("COMMITTED", "FREEZE_BLOCKED", "identity"))
                self.outcome(disk, inv, poison="identity", f="FREEZE_BLOCKED", tail="thaw.move.intent")
        with self.subTest(content="attrs veto, pre-check"):
            self.fresh()
            disk, inv = self.thaw(content={"attrs": "journal_mismatch"})
            self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", "journal_mismatch"))
            rec = self.outcome(disk, inv, veto="journal_mismatch", f="FREEZE_BLOCKED", tail="thaw.move.intent")
            self.assertEqual(rec.claimed, False)
        with self.subTest(content="attrs mismatch, direct call: no content classification, no rename"):
            self.fresh()
            disk, inv = self.thaw(content={"attrs": "journal_mismatch"})
            self.assertEqual(M.invoke(inv, M.effect_move_back)[0], "UNFINISHED")
            rec = self.outcome(disk, inv, f="THAW_INTENT", tail="thaw.move.intent")
            self.assertEqual(rec.outcome, GATE)

    def test_valid_Q_and_content_succeed(self):
        disk, inv = self.thaw()
        self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "NONE", ""))
        rec = self.outcome(disk, inv, renames=1, f="NONE", tail="end")
        self.assertEqual(self.obs(rec), [("slot", "A"), ("quarantine", "A"), ("walk_q", "A"),
                                         ("thaw.move.intent", "A"), ("thaw.move.intent", "B")])

    def test_walk_io_does_not_apply_to_move(self):
        disk = self.world(objects="io")
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        self.assertEqual(M.invoke(inv, M.effect_freeze_start), ("COMMITTED", "MOVED", ""))
        self.assertEqual((inv.authority.record.io, inv.authority.renames), (None, 1))

    def test_walk_io_at_call_post_decided_by_the_observed_case(self):
        for name, fs, result, io, outcome, f in (
                ("rename back landed: B", {}, ("COMMITTED", "NONE", ""), None, ("completed",), "NONE"),
                ("rename back failed: A", dict(rename_error="EIO"), None, "call_post",
                 ("uncertain", "observation I/O"), "THAW_INTENT")):
            with self.subTest(name):
                self.fresh()
                disk, inv = self.thaw(**fs)
                M.HOOKS["before_rename"] = lambda: self.set(disk, objects="io")
                got = M.invoke(inv, M.effect_thaw)
                M.HOOKS.clear()
                if result:
                    self.assertEqual(got, result)
                else:
                    self.assertEqual(got[0], "UNFINISHED")
                rec = self.outcome(disk, inv, io=io, renames=1, f=f)
                self.assertEqual((rec.outcome, rec.rename_issued), (outcome, True))


# ---------------------------------------------------------------- B2: a content pass needs the read's location check

class B2ContentPassNeedsLocation(Repair):
    def test_refused_content_read_leaves_no_pass_and_move_back_is_gated(self):
        for name, observe_first in (("locations observed B first", True), ("no prior observation", False)):
            with self.subTest(name):
                self.fresh()
                disk, inv = self.thaw(slot=RAW, dst=None, content={"attrs": "journal_mismatch"})
                seen = {}

                def eff(store, fscap, observe_first=observe_first):
                    if observe_first:
                        seen["loc"] = [fscap.observe(w)[0] for w in M.LOCATION]
                    seen["reads"] = [attempt(fscap.read_content, w) for w in ("originals", "attrs")]
                    seen["pass"] = dict(inv.authority.content_ok)
                    self.set(disk, slot=None, dst=RAW)                    # the tree is put back in Q: case A
                    seen["back"] = attempt(fscap.move_back)
                    return "NONE", "", {}
                result = M.invoke(inv, eff)
                self.assertEqual((seen["reads"], seen["pass"]), ([NEEDS_A, NEEDS_A], {}))
                self.assertEqual(seen["back"], ("Unfinished", GATE[1]))
                self.assertEqual(result[0], "UNFINISHED")
                rec = self.outcome(disk, inv, f="THAW_INTENT", tail="thaw.move.intent")
                self.assertEqual((rec.outcome, rec.rename_issued), (GATE, False))
                self.assertEqual(self.obs(rec), [("slot", "B"), ("quarantine", "B"), ("walk_q", "B"),
                                                 ("thaw.move.intent", "A")])
                self.assertEqual([e for e in rec.log if e[0] in ("content", "veto", "rename")], [])
                self.assertEqual((disk.fs()["dst"], disk.fs()["slot"]), (RAW, None))
                inv2 = M.start_invocation(disk, "resume-step")          # ordered pre-check: the mismatch vetoes
                self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", "journal_mismatch"))
                self.assertEqual(inv2.authority.renames, 0)

    def test_content_read_under_C_or_location_io_leaves_no_pass(self):
        for name, fs, read, result in (("C", dict(extra=["intruder"]), ("Refused", "FREEZE_BLOCKED(identity)"),
                                        ("COMMITTED", "FREEZE_BLOCKED", "identity")),
                                       ("location I/O", dict(io=["quarantine"]),
                                        ("Unfinished", "observation I/O (quarantine)"), None)):
            with self.subTest(name):
                self.fresh()
                disk, inv = self.thaw(**fs)
                seen = {}

                def eff(store, fscap):
                    seen["read"] = attempt(fscap.read_content, "originals")
                    seen["pass"] = dict(inv.authority.content_ok)
                    return "NONE", "", {}
                got = M.invoke(inv, eff)
                self.assertEqual((seen["read"], seen["pass"]), (read, {}))
                if result:
                    self.assertEqual(got, result)
                else:
                    self.assertEqual(got[0], "UNFINISHED")
                self.assertEqual((inv.authority.renames, inv.authority.state), (0, M.FSCAP_SEALED))


# ---------------------------------------------------------------- B3: the call's own object-identity check

class B3CallRechecksObjects(Repair):
    def test_valid_precheck_does_not_excuse_the_call_object_check(self):
        for name, when in (("substituted before the call", "effect"), ("substituted inside the call before its own observation", "claim")):
            with self.subTest(name):
                self.fresh()
                disk, inv = self.thaw()
                seen = {}
                if when == "claim":
                    M.HOOKS["claim"] = lambda: self.set(disk, objects="substituted")

                def eff(store, fscap, when=when):
                    seen["pre"] = M.thaw_precheck(store, fscap)
                    seen["pass"] = sorted(inv.authority.content_ok)
                    if when == "effect":
                        self.set(disk, objects="substituted")
                    seen["back"] = attempt(fscap.move_back)
                    return "NONE", "", {}
                result = M.invoke(inv, eff)
                M.HOOKS.clear()
                self.assertEqual((seen["pre"], seen["pass"]), ("A", ["attrs", "originals"]))
                self.assertEqual(seen["back"], ("Refused", "FREEZE_BLOCKED(identity)"))
                self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity"))
                rec = self.outcome(disk, inv, poison="identity", f="FREEZE_BLOCKED", tail="thaw.move.intent")
                self.assertEqual(self.obs(rec), [("slot", "A"), ("quarantine", "A"), ("walk_q", "A"),
                                                 ("thaw.move.intent", "C")])
                self.assertEqual((rec.claimed, rec.rename_issued), (True, False))
                self.assertEqual((disk.fs()["dst"], disk.fs()["slot"]), (RAW, None))


# ---------------------------------------------------------------- reviewer survivors OWN2-WALKS-NO-CALL-POST, -CONTENT-MIXED

class ReviewerFailClosedSurvivors(Repair):
    def test_failed_rename_back_then_substitution_is_C_at_call_post(self):
        """OWN2-WALKS-NO-CALL-POST: C, not the recovery-eligible UNFINISHED."""
        disk, inv = self.thaw(rename_error="EIO")
        M.HOOKS["before_rename"] = lambda: self.set(disk, objects="substituted")
        result = M.invoke(inv, M.effect_thaw)
        M.HOOKS.clear()
        self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity"))
        rec = self.outcome(disk, inv, poison="identity", renames=1, f="FREEZE_BLOCKED", tail="thaw.move.intent")
        self.assertEqual(self.obs(rec)[-2:], [("thaw.move.intent", "A"), ("thaw.move.intent", "C")])
        self.assertEqual((rec.rename_issued, disk.fs()["dst"], disk.fs()["slot"]), (True, RAW, None))
        self.assertEqual(M.inspect_durable(inv)["fwal"], "completion_durable")

    def test_failed_rename_back_without_substitution_stays_unfinished(self):
        disk, inv = self.thaw(rename_error="EIO")
        self.assertEqual(M.invoke(inv, M.effect_thaw)[0], "UNFINISHED")
        rec = self.outcome(disk, inv, renames=1, f="THAW_INTENT", tail="thaw.move.intent")
        self.assertEqual(rec.outcome, ("unavailable", "rename back not observed"))

    def test_content_read_refuses_a_mix_of_A_and_clean_mismatch(self):
        """OWN2-CONTENT-MIXED-A-MISMATCH: a refused read grants no pass, so no rename after the device returns."""
        for name, start, flip in (("A then mismatch", DEV1, DEV2), ("mismatch then A", DEV2, DEV1)):
            with self.subTest(name):
                self.fresh()
                disk, inv = self.thaw(slot_parent=start)
                seen = {}

                def eff(store, fscap, start=start, flip=flip):
                    seen["slot"] = fscap.observe("slot")[0]
                    self.set(disk, slot_parent=flip)
                    seen["q"] = [fscap.observe(w)[0] for w in ("quarantine", "walk_q")]
                    seen["reads"] = [attempt(fscap.read_content, w) for w in ("originals", "attrs")]
                    seen["pass"] = dict(inv.authority.content_ok)
                    self.set(disk, slot_parent=DEV1)
                    seen["back"] = attempt(fscap.move_back)
                    return "NONE", "", {}
                result = M.invoke(inv, eff)
                self.assertEqual(sorted({seen["slot"], *seen["q"]}), ["A", "mismatch"])
                self.assertEqual((seen["reads"], seen["pass"]), ([NEEDS_A, NEEDS_A], {}))
                self.assertEqual(seen["back"], ("Unfinished", GATE[1]))
                self.assertEqual(result[0], "UNFINISHED")
                rec = self.outcome(disk, inv, f="THAW_INTENT", tail="thaw.move.intent")
                self.assertEqual((rec.outcome, rec.rename_issued), (GATE, False))
                self.assertEqual((disk.fs()["dst"], disk.fs()["slot"]), (RAW, None))


if __name__ == "__main__":
    unittest.main()
