"""v10 model repair tests, ported from the approved R2 model suite (RF-1 identity precedence, RF-2 content gate, RF-3 observation history, RF-4 reviewer
mutants). Deterministic: no sleeps, no real filesystem effects, observation hooks only."""
import os, sys, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fsharness as M  # noqa: E402
from test_fscap_fsmodel import attempt, XOBJ, RAW, SEALED, POISONED  # noqa: E402
from test_fscap_v10 import V10  # noqa: E402

DEV2 = {"key": M.SP, "dev": 2}                 # slot parent on another device than quarantine/<op_id>
DEV1 = {"key": M.SP, "dev": M.ROOT_DEV}
GATE = ("unavailable", "content checks not passed for this intent")


class Repair(V10):
    def completions(self, disk):
        """F WAL completions after the last intent (0 = the intent is pending)."""
        n = 0
        for e in disk.fwal():
            n = 0 if e["kind"] == "intent" else n + 1
        return n

    def outcome(self, disk, inv, *, poison=None, veto=None, io=None, renames=0, f=None, tail=None):
        rec = inv.authority.record
        self.assertEqual((rec.poison, rec.veto, rec.io, inv.authority.renames), (poison, veto, io, renames))
        self.assertIs(inv.authority.state, M.FSCAP_SEALED)
        self.assertEqual(M.fstate(disk)[0], f)
        self.assertEqual(self.completions(disk), 0 if f in ("THAW_INTENT", "MOVE_INTENT") else 1)
        if tail:
            self.assertEqual(self.kinds(disk)[-1], tail)
        return rec

    def obs(self, rec):
        return [e[1:] for e in rec.log if e[0] == "obs"]


# ---------------------------------------------------------------- RF-1: identity precedes device and content

class RF1Identity(Repair):
    def test_P1_device_mismatch_and_substituted_object_is_C(self):
        disk, inv = self.thaw(slot_parent=DEV2, objects="substituted")
        result = M.invoke(inv, M.effect_thaw)
        self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity"))
        rec = self.outcome(disk, inv, poison="identity", f="FREEZE_BLOCKED", tail="thaw.move.intent")
        self.assertEqual(self.obs(rec), [("slot", "mismatch"), ("quarantine", "mismatch"), ("walk_q", "C")])
        self.assertEqual((rec.claimed, [e for e in rec.log if e[0] == "content"]), (False, []))
        self.assertEqual(M.inspect_durable(inv)["fwal"], "completion_durable")

    def test_P1_direct_move_back_classifies_identity_before_the_device(self):
        disk, inv = self.thaw(slot_parent=DEV2, objects="substituted")
        result = M.invoke(inv, M.effect_move_back)
        self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity"))
        rec = self.outcome(disk, inv, poison="identity", f="FREEZE_BLOCKED", tail="thaw.move.intent")
        self.assertEqual(self.obs(rec), [("thaw.move.intent", "C")])
        self.assertNotEqual(rec.outcome, ("unavailable", "clean device mismatch"))

    def test_P2_identity_beats_content_veto_under_device_mismatch(self):
        for what, fact in (("attrs", "journal_mismatch"), ("originals", "identity")):
            with self.subTest(content=what):
                self.fresh()
                disk, inv = self.thaw(slot_parent=DEV2, objects="substituted", content={what: fact})
                result = M.invoke(inv, M.effect_thaw)
                self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity"))
                rec = self.outcome(disk, inv, poison="identity", f="FREEZE_BLOCKED")
                self.assertEqual([e for e in rec.log if e[0] in ("content", "veto")], [])

    def test_P2_content_read_first_cannot_mask_identity(self):
        disk, inv = self.thaw(slot_parent=DEV2, objects="substituted", content={"attrs": "journal_mismatch"})
        seen = {}

        def eff(store, fscap):
            seen["read"] = attempt(fscap.read_content, "attrs")
            return "FREEZE_BLOCKED", "journal_mismatch", {}
        result = M.invoke(inv, eff)
        self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity"))
        self.assertEqual(seen["read"], ("Refused", "FREEZE_BLOCKED(identity)"))
        self.outcome(disk, inv, poison="identity", f="FREEZE_BLOCKED")

    def test_P3_device_mismatch_with_identities_verified_is_unfinished(self):
        for effect in (M.effect_thaw, M.effect_move_back):
            with self.subTest(effect=effect.__name__):
                self.fresh()
                disk, inv = self.thaw(slot_parent=DEV2)
                result = M.invoke(inv, effect)
                self.assertEqual(result[0], "UNFINISHED")
                rec = self.outcome(disk, inv, f="THAW_INTENT", tail="thaw.move.intent")
                self.assertEqual(rec.outcome, ("unavailable", "clean device mismatch"))
                self.assertEqual((disk.fs()["dst"], disk.fs()["slot"]), (RAW, None))
                self.assertEqual(M.inspect_durable(inv)["fwal"], "intent_pending")
                self.assertNotIn("C", [c for _, c in self.obs(rec)])

    def test_P4_device_mismatch_with_identity_evidence_unavailable_fails_closed(self):
        for effect, key in ((M.effect_thaw, "walk_q"), (M.effect_move_back, "call_pre")):
            with self.subTest(effect=effect.__name__):
                self.fresh()
                disk, inv = self.thaw(slot_parent=DEV2, objects="io")
                result = M.invoke(inv, effect)
                self.assertEqual(result[0], "UNFINISHED")
                rec = self.outcome(disk, inv, io=key, f="THAW_INTENT", tail="thaw.move.intent")
                self.assertNotEqual(rec.outcome, ("unavailable", "clean device mismatch"))
                self.assertNotIn("C", [c for _, c in self.obs(rec)])
                self.set(disk, slot_parent=DEV1, objects="ok")           # evidence available, layout fixed
                inv2 = M.start_invocation(disk, "resume-step")
                self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))
                self.assertEqual(inv2.authority.renames, 1)

    def test_P5_valid_device_and_identities_definite_content_mismatch_vetoes(self):
        for what, fact in (("attrs", "journal_mismatch"), ("originals", "identity")):
            with self.subTest(content=what):
                self.fresh()
                disk, inv = self.thaw(content={what: fact})
                result = M.invoke(inv, M.effect_thaw)
                self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", fact))
                rec = self.outcome(disk, inv, veto=fact, f="FREEZE_BLOCKED", tail="thaw.move.intent")
                self.assertEqual(rec.claimed, False)

    def test_P6_all_valid_positive_paths(self):
        disk, inv = self.thaw()
        result = M.invoke(inv, M.effect_thaw)
        self.assertEqual(result, ("COMMITTED", "NONE", ""))
        self.outcome(disk, inv, renames=1, f="NONE", tail="end")
        self.assertEqual(disk.fs()["slot"], RAW)
        self.fresh()
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        result = M.invoke(inv, M.effect_freeze_start)
        self.assertEqual(result, ("COMMITTED", "MOVED", ""))
        self.outcome(disk, inv, renames=1, f="MOVED", tail="move.done")


# ---------------------------------------------------------------- RF-2: content prerequisites before move_back

class RF2ContentGate(Repair):
    def test_move_back_without_both_content_checks_is_rejected(self):
        for reads, content in (((), {}), ((), {"attrs": "journal_mismatch"}), (("originals",), {}),
                               (("attrs",), {"originals": "identity"})):
            with self.subTest(reads=reads, content=content):
                self.fresh()
                disk, inv = self.thaw(content=content)
                seen = {}

                def eff(store, fscap):
                    for w in M.LOCATION:
                        fscap.observe(w)
                    seen["reads"] = [fscap.read_content(w) for w in reads]
                    seen["back"] = attempt(fscap.move_back)
                    return "NONE", "", {"journal": "d" * 64}
                result = M.invoke(inv, eff)
                self.assertEqual(result[0], "UNFINISHED")
                self.assertEqual(seen["back"], ("Unfinished", GATE[1]))
                rec = self.outcome(disk, inv, f="THAW_INTENT", tail="thaw.move.intent")
                self.assertEqual(rec.outcome, GATE)
                self.assertEqual((disk.fs()["dst"], disk.fs()["slot"]), (RAW, None))

    def test_content_read_after_the_call_is_refused_and_vetoes_nothing(self):
        disk, inv = self.thaw(content={"attrs": "journal_mismatch"})
        seen = {}

        def eff(store, fscap):
            seen["loc"] = [fscap.observe(w)[0] for w in M.LOCATION]
            seen["back"] = attempt(fscap.move_back)
            seen["content"] = attempt(fscap.read_content, "attrs")
            return "NONE", "", {"journal": "d" * 64}
        result = M.invoke(inv, eff)
        self.assertEqual(result[0], "UNFINISHED")
        self.assertEqual((seen["back"][0], seen["content"]),
                         ("Unfinished", ("Refused", "content read after the move call")))
        self.outcome(disk, inv, f="THAW_INTENT", tail="thaw.move.intent")
        inv2 = M.start_invocation(disk, "resume-step")                  # correctly ordered: the veto decides
        self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", "journal_mismatch"))
        self.assertEqual(inv2.authority.renames, 0)

    def test_correct_precheck_permits_case_A(self):
        disk, inv = self.thaw()
        result = M.invoke(inv, M.effect_thaw)
        self.assertEqual(result, ("COMMITTED", "NONE", ""))
        log = [e[:2] for e in inv.authority.record.log if e[0] in ("content", "claim", "rename")]
        self.assertEqual(log, [("content", "originals"), ("content", "attrs"), ("claim", "thaw.move.intent"),
                               ("rename",)])

    def test_verified_case_B_completes_without_content_or_rename(self):
        disk, inv = self.thaw(slot=RAW, dst=None, content={"attrs": "journal_mismatch"})
        result = M.invoke(inv, M.effect_thaw)
        self.assertEqual(result, ("COMMITTED", "NONE", ""))
        rec = self.outcome(disk, inv, f="NONE", tail="end")
        self.assertEqual((rec.outcome, [e for e in rec.log if e[0] == "content"]), (("completed",), []))

    def test_verdict_is_not_reused_by_a_later_authority(self):
        disk, inv = self.thaw()

        def checked_then_stopped(store, fscap):
            M.thaw_precheck(store, fscap)
            raise RuntimeError("stopped after the pre-check")
        self.assertEqual(M.invoke(inv, checked_then_stopped)[0], "UNFINISHED")
        inv2 = M.start_invocation(disk, "resume-step")
        result = M.invoke(inv2, M.effect_move_back)
        self.assertEqual(result[0], "UNFINISHED")
        self.assertEqual((inv2.authority.record.outcome, inv2.authority.renames), (GATE, 0))
        inv3 = M.start_invocation(disk, "resume-step")
        self.assertEqual(M.invoke(inv3, M.effect_thaw), ("COMMITTED", "NONE", ""))

    def test_verdict_is_bound_to_the_intent_it_passed_at(self):
        """White-box: the model's journal cannot change while its tail stays an authenticated thaw.move.intent,
        so a verdict from another intent is simulated in the authority itself."""
        disk, inv = self.thaw()
        seen = {}

        def eff(store, fscap):
            M.thaw_precheck(store, fscap)
            inv.authority.content_ok["attrs"] = "0" * 64
            seen["back"] = attempt(fscap.move_back)
            return "NONE", "", {"journal": "d" * 64}
        self.assertEqual(M.invoke(inv, eff)[0], "UNFINISHED")
        self.assertEqual((seen["back"][0], inv.authority.renames), ("Unfinished", 0))

    def test_content_read_after_seal_or_poison_authorizes_nothing(self):
        disk, inv = self.thaw(extra=["intruder"], content={"attrs": "journal_mismatch"})
        leak, seen = {}, {}

        def eff(store, fscap):
            leak["f"] = fscap
            seen["obs"] = attempt(fscap.observe, "slot")
            seen["read"] = attempt(fscap.read_content, "attrs")
            return "NONE", "", {}
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "FREEZE_BLOCKED", "identity"))
        self.assertEqual(seen["read"], POISONED)
        self.assertEqual((attempt(leak["f"].read_content, "attrs"), attempt(leak["f"].move_back)), (SEALED, SEALED))
        self.assertEqual((inv.authority.record.veto, inv.authority.content_ok, inv.authority.renames), (None, {}, 0))

    def test_content_io_is_not_a_definite_mismatch(self):
        disk, inv = self.thaw(content={"originals": "io", "attrs": "journal_mismatch"})
        result = M.invoke(inv, M.effect_thaw)
        self.assertEqual(result[0], "UNFINISHED")
        self.outcome(disk, inv, io="content originals", f="THAW_INTENT", tail="thaw.move.intent")
        self.set(disk, content={})
        inv2 = M.start_invocation(disk, "resume-step")
        self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))


# ---------------------------------------------------------------- RF-3: observation history

class RF3History(Repair):
    def test_A_then_device_only_then_B_without_a_rename_contradicts(self):
        disk, inv = self.thaw()

        def eff(store, fscap):
            fscap.observe("slot")
            self.set(disk, slot_parent=DEV2)
            fscap.observe("quarantine")
            self.set(disk, slot_parent=DEV1, slot=RAW, dst=None)           # foreign move, no rename of ours
            return M.effect_move_back(store, fscap)
        result = M.invoke(inv, eff)
        self.assertEqual(result[0], "UNFINISHED")
        rec = inv.authority.record
        self.assertEqual(self.obs(rec), [("slot", "A"), ("quarantine", "mismatch"), ("thaw.move.intent", "B")])
        self.assertEqual((rec.outcome, inv.authority.renames, M.fstate(disk)[0]), (("completed",), 0, "THAW_INTENT"))

    def test_device_only_then_B_without_earlier_A_is_not_a_contradiction(self):
        disk, inv = self.thaw(slot_parent=DEV2)

        def eff(store, fscap):
            fscap.observe("slot")
            self.set(disk, slot_parent=DEV1, slot=RAW, dst=None)
            return M.effect_move_back(store, fscap)
        result = M.invoke(inv, eff)
        self.assertEqual(result, ("COMMITTED", "NONE", ""))
        rec = self.outcome(disk, inv, f="NONE", tail="end")
        self.assertEqual(self.obs(rec), [("slot", "mismatch"), ("thaw.move.intent", "B")])

    def test_A_then_own_authorized_rename_then_B_completes(self):
        disk, inv = self.thaw()
        result = M.invoke(inv, M.effect_thaw)
        self.assertEqual(result, ("COMMITTED", "NONE", ""))
        rec = self.outcome(disk, inv, renames=1, f="NONE", tail="end")
        self.assertEqual(self.obs(rec)[-2:], [("thaw.move.intent", "A"), ("thaw.move.intent", "B")])

    def test_fresh_invocation_rebuilds_from_durable_evidence_only(self):
        disk, inv = self.thaw()

        def observed_A_then_crashed(store, fscap):
            fscap.observe("slot")
            self.set(disk, slot=RAW, dst=None)                          # the earlier process's rename landed
            raise RuntimeError("stopped")
        self.assertEqual(M.invoke(inv, observed_A_then_crashed)[0], "UNFINISHED")
        inv2 = M.start_invocation(disk, "resume-step")
        self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))
        self.assertEqual(self.obs(inv2.authority.record)[0], ("slot", "B"))
        self.assertEqual(inv2.authority.renames, 0)


# ---------------------------------------------------------------- RF-4: reviewer's surviving mutants

class RF4ReviewerMutants(Repair):
    def test_veto_then_the_effect_raises_is_still_R1b(self):
        """OWN-R1B-AFTER-R2."""
        disk, inv = self.thaw(content={"originals": "identity"})

        def eff(store, fscap):
            M.thaw_precheck(store, fscap)
            raise RuntimeError("effect fails after the veto")
        result = M.invoke(inv, eff)
        self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity"))
        self.outcome(disk, inv, veto="identity", f="FREEZE_BLOCKED", tail="thaw.move.intent")

    def test_content_read_refused_unless_every_location_is_A_or_clean_mismatch(self):
        """OWN-CONTENT-ANYLOC."""
        for name, fs, flip in (("all B", {"slot": RAW, "dst": None}, False), ("A then B", {}, True)):
            with self.subTest(name):
                self.fresh()
                disk, inv = self.thaw(content={"attrs": "journal_mismatch"}, **fs)
                if flip:
                    M.HOOKS["observe"] = lambda: self.set(disk, slot=RAW, dst=None) \
                        if inv.authority.observations == 2 else None
                seen = {}

                def eff(store, fscap):
                    seen["loc"] = [fscap.observe(w)[0] for w in M.LOCATION]
                    seen["read"] = attempt(fscap.read_content, "attrs")
                    return "NONE", "", {}
                result = M.invoke(inv, eff)
                M.HOOKS.clear()
                self.assertEqual(seen["read"], ("Refused", "content read needs every location observation A"))
                self.assertEqual(result[0], "UNFINISHED")
                self.outcome(disk, inv, f="THAW_INTENT", tail="thaw.move.intent")

    def test_precheck_with_mixed_A_and_B_makes_no_call_and_no_record(self):
        """OWN-PRECHECK-ANYB."""
        disk, inv = self.thaw()
        M.HOOKS["observe"] = lambda: self.set(disk, slot=RAW, dst=None) if inv.authority.observations == 2 else None
        result = M.invoke(inv, M.effect_thaw)
        M.HOOKS.clear()
        self.assertEqual(result, ("UNFINISHED", "Unfinished: no result determined by the sealed record "
                                                "(effect: None, Unfinished(\"pre-check location ['A', 'B']\"))"))
        rec = self.outcome(disk, inv, f="THAW_INTENT", tail="thaw.move.intent")
        self.assertEqual((rec.claimed, [e for e in rec.log if e[0] == "put"]), (False, []))


if __name__ == "__main__":
    unittest.main()
