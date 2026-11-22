"""Regression tests for the FsCap authority (ported from the approved R2 model suite test_fsmodel.py; they run
against the production capabilities through fsharness).
Concurrency tests use deterministic barriers (Gate, await_ on lock waiters); GUARD is only a deadlock guard."""
import copy, os, pickle, sys, tempfile, threading, types, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fsharness as M  # noqa: E402
from fsharness import Refused, Unfinished  # noqa: E402

XOBJ = {"key": M.X, "dev": M.ROOT_DEV}
RAW = {"key": M.ROOT, "dev": M.ROOT_DEV}
SEALED = ("Refused", "authority FSCAP_SEALED")
POISONED = ("Refused", "authority FSCAP_POISONED")
CLAIMED = ("Refused", "move call already claimed in this invocation")


def attempt(fn, *a, **kw):
    """Assertions never run inside an effect: a failing one would raise there and be absorbed by the
    poison rule under test. Effects record outcomes; tests assert after `complete`."""
    try:
        return "ok", fn(*a, **kw)
    except (Refused, Unfinished) as e:
        return type(e).__name__, str(e)


def spawn(out, key, fn, *a, **kw):
    def run():
        try:
            out[key] = attempt(fn, *a, **kw)
        except BaseException as e:            # a model AssertionError in a thread is a result, not a hang
            out[key] = ("error", repr(e))
        finally:
            with M.EVENTS:
                out.setdefault("_done", set()).add(key)
                M.EVENTS.notify_all()
    t = threading.Thread(target=run, name=key, daemon=True)
    t.start()
    return t


def done(out, *keys):
    return all(k in out.get("_done", ()) for k in keys)


def named(name, fn):
    return lambda: fn() if threading.current_thread().name == name else None


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        M.HOOKS.clear()
        M.FAULTS.clear()

    def tearDown(self):
        M.reset()
        self.tmp.cleanup()

    def fresh(self):
        self.tmp.cleanup()
        self.tmp = tempfile.TemporaryDirectory()
        M.HOOKS.clear()
        M.FAULTS.clear()

    def world(self, **fs):
        return M.new_world(self.tmp.name, **fs)

    def set(self, disk, **kv):
        fs = disk.fs()
        fs.update(kv)
        disk.set_fs(fs)

    def kinds(self, disk):
        return [r["kind"] for r in disk.journal()]

    def unmoved(self, disk):
        fs = disk.fs()
        self.assertEqual((fs["slot"], fs["dst"]), (RAW, None))

    def no_thaw_path(self, disk):
        with self.assertRaisesRegex(Refused, "nothing pending"):
            M.start_invocation(disk, "resume-step")
        with self.assertRaisesRegex(Refused, "not selectable"):
            M.start_invocation(disk, "freeze-start", M.A_START)
        with self.assertRaisesRegex(Refused, "evidence_missing"):
            M.start_invocation(disk, "thaw", M.A_THAW)
        j = disk.journal()                              # merged grammar: no thaw.* without move.done
        r = {"schema": M.J.J_SCHEMA_V3, "header": {}, "seq": len(j), "prev": M.h(j[-1]), "kind": "thaw.intent",
             "early": M.EARLY, "attr": "owner", "lo": 0, "hi": 0}
        with self.assertRaisesRegex(Refused, "journal_mismatch"):
            M.walk(j + [r])


# ---------------------------------------------------------------- retained from v8 (RT12 and the v8 suite)

class Retained(Base):
    def test_RT12_case_c_then_original_reappears_second_move_refused(self):
        disk = self.world(slot=XOBJ)
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = {}

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            seen["first"] = attempt(fscap.move)
            self.set(disk, slot=RAW)                    # R reappears before the effect returns
            obs = inv.authority.observations
            seen["second"] = attempt(fscap.move)
            seen["obs"] = (obs, inv.authority.observations)
            return "MOVED", "", {"early": M.EARLY}
        self.assertEqual(inv.complete(eff), ("FREEZE_BLOCKED", "identity_swap"))
        self.assertEqual(seen["first"], ("Refused", "FREEZE_BLOCKED(identity_swap)"))
        self.assertEqual(seen["second"], POISONED)
        self.assertEqual(seen["obs"][0], seen["obs"][1])  # no fresh observation
        self.assertEqual(inv.authority.renames, 0)
        self.unmoved(disk)
        self.assertEqual(self.kinds(disk)[-1], "move.intent")
        self.assertEqual(M.fstate(disk)[0], "FREEZE_BLOCKED")

    def test_move_back_case_C_then_A_refused(self):
        disk = self.world()
        M.seed_thaw(disk)
        self.set(disk, slot=XOBJ)                       # occupant at the slot name: C, thaw_target_occupied
        inv = M.start_invocation(disk, "thaw", M.A_THAW)
        seen = {}

        def eff(store, fscap):
            seen["first"] = attempt(fscap.move_back)
            self.set(disk, slot=None)                   # occupant removed: A would now hold
            seen["second"] = attempt(fscap.move_back)
            return "NONE", "", {"journal": "d" * 64}
        self.assertEqual(inv.complete(eff), ("FREEZE_BLOCKED", "thaw_target_occupied"))
        self.assertEqual(seen["first"], ("Refused", "FREEZE_BLOCKED(thaw_target_occupied)"))
        self.assertEqual(seen["second"], POISONED)
        self.assertEqual(inv.authority.renames, 0)
        self.assertEqual(disk.fs()["dst"]["key"], M.ROOT)
        self.assertEqual(self.kinds(disk)[-1], "thaw.move.intent")

    def test_aliases_share_state(self):
        disk = self.world(slot=XOBJ)
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = []

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            other = fscap.view()
            seen.append(other is not fscap)
            seen.append(attempt(other.move))
            self.set(disk, slot=RAW)
            alias = fscap
            seen.extend(attempt(f.move) for f in (fscap, alias, other.view()))
            seen.extend(attempt(f.observe, "slot") for f in (fscap, other.view()))
            return "MOVED", "", {"early": M.EARLY}
        self.assertEqual(inv.complete(eff), ("FREEZE_BLOCKED", "identity_swap"))
        self.assertEqual(seen, [True, ("Refused", "FREEZE_BLOCKED(identity_swap)")] + [POISONED] * 5)
        self.assertEqual(inv.authority.renames, 0)

    def test_not_copyable_or_serializable(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = []

        def eff(store, fscap):
            for f in (copy.copy, copy.deepcopy, pickle.dumps):
                try:
                    f(fscap)
                    seen.append("copied")
                except TypeError:
                    seen.append("refused")
            M.journal_through_move_intent(store)
            return M.finish_move(store, fscap.move())
        self.assertEqual(inv.complete(eff), ("MOVED", ""))
        self.assertEqual(seen, ["refused"] * 3)

    def test_retry_after_caught_exception_and_lying_effect(self):
        disk = self.world(slot=XOBJ)
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = []

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            for _ in range(3):
                try:
                    fscap.move()
                except Refused as e:
                    seen.append(str(e))
                    self.set(disk, slot=RAW)
            seen.append(attempt(store.put, "move.abort", intent=M.h(disk.journal()[-1]), reason="rename_failed"))
            return "MOVED", "", {"early": M.EARLY}
        self.assertEqual(inv.complete(eff), ("FREEZE_BLOCKED", "identity_swap"))
        self.assertEqual(seen, ["FREEZE_BLOCKED(identity_swap)"] + ["authority FSCAP_POISONED"] * 2 + [POISONED])
        self.assertEqual(self.kinds(disk)[-1], "move.intent")
        self.assertEqual(inv.authority.renames, 0)

    def test_effect_raises_after_poison(self):
        disk = self.world(slot=XOBJ)
        inv = M.start_invocation(disk, "freeze-start", M.A_START)

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            try:
                fscap.move()
            finally:
                raise RuntimeError("effect bug")
        self.assertEqual(inv.complete(eff), ("FREEZE_BLOCKED", "identity_swap"))

    def test_second_mint_refused_state_kept(self):
        disk = self.world(slot=XOBJ)
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = []

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            seen.append(attempt(fscap.move))
            a = inv.authority
            seen.append(attempt(inv.mint)[0])
            seen.append((inv.authority is a, a.state))
            self.set(disk, slot=RAW)
            seen.append(attempt(fscap.move))
            return "MOVED", "", {"early": M.EARLY}
        self.assertEqual(inv.complete(eff), ("FREEZE_BLOCKED", "identity_swap"))
        self.assertEqual(seen, [("Refused", "FREEZE_BLOCKED(identity_swap)"), "Refused", (True, M.FSCAP_POISONED),
                                POISONED])
        self.assertEqual(inv.authority.renames, 0)

    def test_rename_error_gives_abort_and_no_second_rename(self):
        for err, reason in (("EEXIST", "target_exists"), ("EIO", "rename_failed"), ("silent", "rename_failed")):
            with self.subTest(err=err):
                self.fresh()
                disk = self.world(rename_error=err)
                inv = M.start_invocation(disk, "freeze-start", M.A_START)
                seen = []

                def eff(store, fscap):
                    M.journal_through_move_intent(store)
                    out = fscap.move()
                    self.set(disk, rename_error=None)
                    seen.append(attempt(fscap.move))
                    return M.finish_move(store, out)
                self.assertEqual(inv.complete(eff), ("NONE", reason))
                self.assertEqual(seen, [CLAIMED])
                self.assertEqual(inv.authority.renames, 1)
                self.assertEqual(self.kinds(disk)[-2:], ["move.abort", "end"])

    def test_case_c_after_issued_rename_poisons_and_refuses_abort(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)

        def interfere():
            if inv.authority.observations == 2:       # the observation after the rename
                self.set(disk, extra=["intruder"])
        M.HOOKS["observe"] = interfere
        seen = []

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            seen.append(attempt(fscap.move))
            seen.append(attempt(store.put, "move.abort", intent=M.h(disk.journal()[-1]), reason="rename_failed"))
            return "NONE", "rename_failed", {}
        self.assertEqual(inv.complete(eff), ("FREEZE_BLOCKED", "identity_swap"))
        self.assertEqual(seen, [("Refused", "FREEZE_BLOCKED(identity_swap)"), POISONED])
        self.assertEqual(inv.authority.renames, 1)
        self.assertEqual(self.kinds(disk)[-1], "move.intent")

    def test_case_c_precedes_device_mismatch(self):
        disk = self.world(qdir={"key": M.QD, "dev": 2}, slot=XOBJ)
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        self.assertEqual(inv.complete(M.effect_freeze_start), ("FREEZE_BLOCKED", "identity_swap"))
        self.assertEqual(inv.authority.record.poison, "identity_swap")

    def test_device_substitution_and_mount_boundary_are_case_c(self):
        for fs in ({"slot": {"key": M.ROOT, "dev": 3}},
                   {"slot_parent": {"key": M.SP, "dev": 4}, "qdir": {"key": M.QD, "dev": 4}}):
            with self.subTest(fs=fs):
                self.fresh()
                disk = self.world(**fs)
                inv = M.start_invocation(disk, "freeze-start", M.A_START)
                self.assertEqual(inv.complete(M.effect_freeze_start), ("FREEZE_BLOCKED", "identity_swap"))
                self.assertEqual(inv.authority.renames, 0)


# ---------------------------------------------------------------- RR8-1: sealing (RT13, RT17)

class Sealing(Base):
    def inflight(self, pause_at, substitute=False):
        """The effect starts a move in thread T, which pauses at `pause_at`, and returns FREEZE_BLOCKED
        without joining T (the reviewer's RR8-1 probe). The call site then seals."""
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        gate, out, sealed, first = M.Gate(), {}, {}, []

        def pause():
            if not first:
                first.append(1)
                gate.arrive()
        M.HOOKS[pause_at] = named("T", pause)
        M.HOOKS["sealed"] = lambda: (sealed.setdefault("renames", inv.authority.renames), M.signal())

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            spawn(out, "T", fscap.move)
            M.await_(lambda: gate.arrived, f"T at {pause_at}")
            if substitute:
                self.set(disk, slot=XOBJ)
            a = inv.authority

            def releaser():                         # release T only once the seal waits or is done
                M.await_(lambda: a.lock.waiting >= 1 or "renames" in sealed, "seal waiting or sealed")
                gate.release()
            threading.Thread(target=releaser, daemon=True).start()
            return "FREEZE_BLOCKED", "identity", {}
        result = M.invoke(inv, eff)
        M.await_(lambda: done(out, "T"), "T finished")
        return disk, inv, result, out, sealed

    def test_RT13_move_in_flight_before_its_lock_refused_after_seal(self):
        disk, inv, result, out, sealed = self.inflight("pre_acquire")
        self.assertEqual(out["T"], SEALED)
        self.assertEqual(result[0], "UNFINISHED")     # R6: no record supports FREEZE_BLOCKED at move.intent
        self.assertEqual((inv.authority.renames, inv.authority.observations, sealed["renames"]), (0, 0, 0))
        self.unmoved(disk)
        self.assertEqual(M.fstate(disk)[0], "MOVE_INTENT")

    def test_RT13_move_in_flight_holding_lock_no_rename_after_seal(self):
        disk, inv, result, out, sealed = self.inflight("claim")
        self.assertEqual(out["T"], ("ok", "completed"))
        self.assertEqual(sealed["renames"], 1)
        self.assertEqual(inv.authority.renames, sealed["renames"])     # nothing renamed after the seal
        late = {}
        spawn(late, "L", inv.authority and M.FsCap(inv.authority, inv).move)
        M.await_(lambda: done(late, "L"), "late call")
        self.assertEqual(late["L"], SEALED)
        self.assertEqual(inv.authority.renames, 1)

    def test_RT13_in_flight_case_c_completion_comes_from_classification(self):
        disk, inv, result, out, sealed = self.inflight("claim", substitute=True)
        self.assertEqual(out["T"], ("Refused", "FREEZE_BLOCKED(identity_swap)"))
        self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity_swap"))
        self.assertEqual(inv.authority.record.poison, "identity_swap")
        self.assertEqual((inv.authority.renames, sealed["renames"]), (0, 0))

    def test_RT17_in_flight_move_succeeds_before_seal_no_contradictory_completion(self):
        disk, inv, result, out, sealed = self.inflight("claim")
        rec = inv.authority.record
        self.assertEqual((rec.rename_issued, rec.outcome, rec.renames, rec.poison), (True, ("completed",), 1, None))
        self.assertEqual(result[0], "UNFINISHED")
        self.assertIn("no result determined by the sealed record", result[1])
        self.assertEqual(M.fstate(disk)[0], "MOVE_INTENT")              # no FREEZE_BLOCKED completion
        self.assertEqual((disk.fs()["slot"], disk.fs()["dst"]), (None, RAW))
        self.assertEqual(self.kinds(disk)[-1], "move.intent")
        inv2 = M.start_invocation(disk, "resume-step")                  # separately authorized resume: (b)
        self.assertEqual(M.invoke(inv2, M.effect_resume_move), ("COMMITTED", "MOVED", ""))
        self.assertEqual(inv2.authority.renames, 0)

    def test_seal_idempotent_terminal_and_record_immutable(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        leak = {}

        def eff(store, fscap):
            leak.update(store=store, fscap=fscap)
            return M.effect_freeze_start(store, fscap)
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "MOVED", ""))
        a = inv.authority
        rec = a.record
        self.assertIs(a.seal(), rec)
        with self.assertRaises(AttributeError):
            rec.poison = "x"
        self.assertEqual(a.state, M.FSCAP_SEALED)
        self.assertEqual(attempt(leak["fscap"].view().observe, "slot"), SEALED)
        self.assertEqual(attempt(leak["store"].put, "end", outcome="thawed", reason=""), SEALED)
        self.assertEqual(attempt(inv.mint), ("Refused", "second mint in this invocation"))
        with self.assertRaises(AssertionError):
            a._to(M.FSCAP_OPEN)


# ---------------------------------------------------------------- RR8-2: protected observations (RT14, RT15, RT16)

class Observations(Base):
    def test_RT14_slot_observation_case_c_then_R_reappears_move_refused(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)

        def crash(store, fscap):
            M.journal_through_move_intent(store)
            raise RuntimeError("crash before move")
        self.assertEqual(M.invoke(inv, crash)[0], "UNFINISHED")
        self.set(disk, slot=XOBJ)                                       # substitute at S
        inv2 = M.start_invocation(disk, "resume-step")
        seen = {}

        def eff(store, fscap):
            seen["read"] = attempt(fscap.observe, "slot")              # the resume-row observation
            seen["state"] = inv2.authority.state
            self.set(disk, slot=RAW)                                    # R reappears
            seen["obs"] = inv2.authority.observations
            seen["move"] = attempt(fscap.move)
            seen["obs2"] = inv2.authority.observations
            return "MOVED", "", {"early": M.EARLY}
        self.assertEqual(M.invoke(inv2, eff), ("COMMITTED", "FREEZE_BLOCKED", "identity_swap"))
        self.assertEqual(seen["read"], ("Refused", "FREEZE_BLOCKED(identity_swap)"))
        self.assertEqual(seen["state"], M.FSCAP_POISONED)
        self.assertEqual(seen["move"], POISONED)
        self.assertEqual(seen["obs"], seen["obs2"])                     # refused without observing again
        self.assertEqual(inv2.authority.renames, 0)
        self.unmoved(disk)

    def test_RT15_quarantine_read_walk_case_C_then_move_back_A_refused(self):
        disk = self.world()
        M.seed_thaw(disk)
        self.set(disk, extra=["intruder"])                              # Q holds an extra entry: C
        inv = M.start_invocation(disk, "thaw", M.A_THAW)
        seen = {}

        def eff(store, fscap):
            seen["walk"] = attempt(fscap.observe, "walk_q")
            self.set(disk, extra=[])                                    # A would now hold
            seen["obs"] = inv.authority.observations
            seen["back"] = attempt(fscap.move_back)
            seen["obs2"] = inv.authority.observations
            return "NONE", "", {"journal": "d" * 64}
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "FREEZE_BLOCKED", "identity"))
        self.assertEqual(seen["walk"], ("Refused", "FREEZE_BLOCKED(identity)"))
        self.assertEqual(seen["back"], POISONED)
        self.assertEqual(seen["obs"], seen["obs2"])
        self.assertEqual((inv.authority.renames, disk.fs()["dst"], disk.fs()["slot"]), (0, RAW, None))
        self.assertEqual(self.kinds(disk)[-1], "thaw.move.intent")

    def test_RT16_aliases_concurrent_observe_and_move_serialized(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        gate, out = M.Gate(), {}
        M.HOOKS["observe"] = named("T1", gate.arrive)

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            a = inv.authority
            spawn(out, "T1", fscap.view().observe, "slot")
            M.await_(lambda: gate.arrived, "T1 observing")
            spawn(out, "T2", fscap.view().move)
            M.await_(lambda: a.lock.waiting >= 1 or done(out, "T2"), "T2 blocked or done")
            gate.release()
            M.await_(lambda: done(out, "T1", "T2"), "T1, T2 done")
            spawn(out, "T3", fscap.move)
            M.await_(lambda: done(out, "T3"), "T3 done")
            return M.finish_move(store, out["T2"][1]) if out["T2"][0] == "ok" else ("NONE", "x", {})
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "MOVED", ""))
        self.assertEqual(out["T1"], ("ok", ("A", RAW)))
        self.assertEqual(out["T2"], ("ok", "completed"))
        self.assertEqual(out["T3"], CLAIMED)
        self.assertEqual([e for e in inv.authority.record.log if e[0] != "put"],
                         [("obs", "slot", "A"), ("claim", "move.intent"), ("obs", "move.intent", "A"), ("rename",),
                          ("obs", "move.intent", "B")])
        self.assertEqual(inv.authority.renames, 1)

    def test_unprotected_observation_is_not_classified(self):
        disk = self.world(slot=XOBJ)
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = {}

        def eff(store, fscap):                       # D2.2.1 step 2 at tail ep.done: no intent, no poison
            store.put("start")
            store.put("ep.done", binding=M.EARLY)
            seen["obs"] = attempt(fscap.observe, "slot")
            store.put("end", outcome="none", reason="identity")
            return "NONE", "identity", {}
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "NONE", "identity"))
        self.assertEqual(seen["obs"], ("ok", ("unprotected", XOBJ)))
        self.assertIsNone(inv.authority.record.poison)
        self.assertEqual(inv.authority.observations, 0)


# ---------------------------------------------------------------- RR8-3: store capability (RT18)

class Store(Base):
    def setup_after_move(self, inv, disk, store, fscap):
        M.journal_through_move_intent(store)
        out = fscap.move()                          # one rename, (b) observed
        self.set(disk, extra=["intruder"])          # a protected observation now would classify C
        return out

    def test_RT18_put_checked_then_observation_waits_for_the_write(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        gate, out = M.Gate(), {}
        M.HOOKS["store_checked"] = named("P", gate.arrive)

        def eff(store, fscap):
            self.setup_after_move(inv, disk, store, fscap)
            a = inv.authority
            spawn(out, "P", store.put, "move.done", intent=M.h(disk.journal()[-1]), linked=[])
            M.await_(lambda: gate.arrived, "P checked")
            spawn(out, "O", fscap.view().observe, "quarantine")
            M.await_(lambda: a.lock.waiting >= 1 or done(out, "O"), "O blocked or done")
            gate.release()
            M.await_(lambda: done(out, "P", "O"), "P, O done")
            return "MOVED", "", {"early": M.EARLY, "journal": out["P"][1]}     # RR9-5: merged MOVED binds journal
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "MOVED", ""))
        self.assertEqual(out["P"][0], "ok")
        self.assertEqual(out["O"][1][0], "unprotected")                # the tail was move.done by then
        self.assertIsNone(inv.authority.record.poison)
        self.assertEqual(self.kinds(disk)[-1], "move.done")

    def test_RT18_observation_poisons_then_put_refused(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        gate, out = M.Gate(), {}
        M.HOOKS["observe"] = named("O", gate.arrive)

        def eff(store, fscap):
            self.setup_after_move(inv, disk, store, fscap)
            a = inv.authority
            spawn(out, "O", fscap.view().observe, "quarantine")
            M.await_(lambda: gate.arrived, "O observing")
            spawn(out, "P", store.put, "move.done", intent=M.h(disk.journal()[-1]), linked=[])
            M.await_(lambda: a.lock.waiting >= 1 or done(out, "P"), "P blocked or done")
            gate.release()
            M.await_(lambda: done(out, "P", "O"), "P, O done")
            return "MOVED", "", {"early": M.EARLY}
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "FREEZE_BLOCKED", "identity_swap"))
        self.assertEqual(out["O"], ("Refused", "FREEZE_BLOCKED(identity_swap)"))
        self.assertEqual(out["P"], POISONED)
        self.assertEqual(self.kinds(disk)[-1], "move.intent")          # nothing written after the poison


# ---------------------------------------------------------------- RR8-5: completion failure (RT19, RT20, D)

class Completion(Base):
    def poisoned_then(self, fault):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        leak, out, gate = {}, {}, M.Gate()
        M.HOOKS["pre_acquire"] = named("D", gate.arrive)

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            self.set(disk, slot=XOBJ)
            leak["seen"] = attempt(fscap.observe, "slot")              # protected: C, poisoned, no claim
            self.set(disk, slot=RAW)
            leak.update(store=store, fscap=fscap)
            spawn(out, "D", fscap.view().move)                          # detached, not yet at the lock
            M.await_(lambda: gate.arrived, "D started")
            M.FAULTS.add(fault)
            return "MOVED", "", {"early": M.EARLY}
        result = M.invoke(inv, eff)
        M.FAULTS.clear()
        gate.release()
        M.await_(lambda: done(out, "D"), "D done")
        later = [attempt(leak["fscap"].move), attempt(leak["fscap"].view().observe, "slot"),
                 attempt(leak["store"].put, "end", outcome="none", reason="identity"), attempt(inv.mint)]
        self.assertEqual(leak["seen"], ("Refused", "FREEZE_BLOCKED(identity_swap)"))
        self.assertEqual(out["D"], SEALED)
        self.assertEqual(later, [SEALED, SEALED, SEALED, ("Refused", "second mint in this invocation")])
        self.assertEqual((inv.authority.state, inv.authority.renames, inv.authority.observations),
                         (M.FSCAP_SEALED, 0, 1))
        self.unmoved(disk)
        return disk, inv, result

    def test_RT19_completion_not_written_or_append_fails_stays_sealed(self):
        for fault in ("completion_prepared", "fwal:append:before"):
            with self.subTest(fault=fault):
                self.fresh()
                disk, inv, result = self.poisoned_then(fault)
                self.assertEqual(result[0], "UNFINISHED")
                self.assertEqual(M.fstate(disk)[0], "MOVE_INTENT")     # intent pending, nothing durable
                self.assertEqual(len(disk.fwal()), 1)
                inv2 = M.start_invocation(disk, "resume-step")         # later authorized invocation re-reads
                self.assertEqual(M.invoke(inv2, M.effect_resume_move), ("COMMITTED", "MOVED", ""))
                self.assertEqual(inv2.authority.renames, 1)            # the confirmed crash-boundary limit

    def test_RT19_append_raises_after_write_reported_unfinished_reread_decides(self):
        disk, inv, result = self.poisoned_then("fwal:append:after")
        self.assertEqual(result[0], "UNFINISHED")                       # never COMMITTED
        self.assertEqual(M.fstate(disk)[0], "FREEZE_BLOCKED")          # what the re-read finds
        self.no_thaw_path(disk)
        self.unmoved(disk)

    def test_RT20_check_fresult_rejects(self):
        for poisoned in (False, True):
            with self.subTest(poisoned=poisoned):
                self.fresh()
                disk = self.world(slot=XOBJ if poisoned else RAW)
                inv = M.start_invocation(disk, "freeze-start", M.A_START)
                leak = {}

                def eff(store, fscap):
                    leak["fscap"] = fscap
                    M.FAULTS.add("check_fresult")
                    return M.effect_freeze_start(store, fscap)
                result = M.invoke(inv, eff)
                M.FAULTS.clear()
                self.assertEqual(result, ("UNFINISHED", "ValueError: fresult_rejected:injected"))
                self.assertEqual(M.fstate(disk)[0], "MOVE_INTENT")
                self.assertEqual(attempt(leak["fscap"].move), SEALED)
                self.assertEqual(inv.authority.renames, 0 if poisoned else 1)
                if not poisoned:
                    self.assertEqual(self.kinds(disk)[-1], "move.done")
                    inv2 = M.start_invocation(disk, "resume-step")     # D2.2.1 row move.done: MOVED, no rename
                    self.assertEqual(M.invoke(inv2, M.effect_resume_move), ("COMMITTED", "MOVED", ""))
                    self.assertEqual(inv2.authority.renames, 0)

    def test_D_durable_completion_identity_and_transition(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        self.assertEqual(M.invoke(inv, M.effect_freeze_start), ("COMMITTED", "MOVED", ""))
        last = disk.fwal()[-1]
        self.assertEqual(M.canonical.digest(last), M.canonical.digest(inv.prepared))
        self.assertEqual(last, inv.prepared)
        self.assertEqual(M.fstate(disk), ("MOVED", last))
        self.assertEqual(M.cycle_bindings(disk), {"early": M.EARLY, "journal": M.h(disk.journal()[-1])})  # RR9-5


# ---------------------------------------------------------------- RR8-6: UNFINISHED (RT21)

class Unfinish(Base):
    def test_RT21_clean_device_mismatch_unfinished(self):
        disk = self.world(qdir={"key": M.QD, "dev": 2})
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = []

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            seen.append(attempt(fscap.move))
            seen.append(attempt(fscap.move))
            return "FREEZE_BLOCKED", "identity", {}                     # the effect tries a binding-free result
        result = M.invoke(inv, eff)
        self.assertEqual(result[0], "UNFINISHED")
        self.assertEqual(seen, [("Unfinished", "clean device mismatch"), CLAIMED])
        self.assertEqual(inv.authority.record.outcome, ("unavailable", "clean device mismatch"))
        self.assertIsNone(inv.authority.record.poison)
        self.assertEqual((inv.authority.renames, len(disk.fwal())), (0, 1))
        self.assertEqual(M.fstate(disk)[0], "MOVE_INTENT")
        self.assertEqual(disk.fs(), M.new_world(self.tmp.name + "/w", qdir={"key": M.QD, "dev": 2}).fs())  # no copy

    def test_RT21_no_effect_result_authority_unfinished(self):
        disk = self.world(dst={"key": M.X, "dev": M.ROOT_DEV})
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = []

        def eff(store, fscap):
            store.put("start")
            store.put("ep.done", binding=M.EARLY)
            seen.append(attempt(fscap.move))                            # no move.intent: no authority
            return "FREEZE_BLOCKED", "identity_swap", {}
        result = M.invoke(inv, eff)
        self.assertEqual(result[0], "UNFINISHED")
        self.assertEqual(seen, [("Unfinished", "intent authority")])
        self.assertEqual((inv.authority.renames, inv.authority.observations, M.fstate(disk)[0]), (0, 0, "MOVE_INTENT"))


# ---------------------------------------------------------------- crash, thaw, freeze-start boundaries (RT22-RT24)

class Boundaries(Base):
    def run_child(self, crash_at, how, **fs):
        """Port delta: the model killed a child process (os._exit); here the same child runs in process and stops
        with stores.Crash at the same point, and only what the world's stores hold survives."""
        disk = self.world(**fs)
        return disk, types.SimpleNamespace(returncode=M.crash_child(disk, crash_at, how), stderr="")

    def test_RT22_crash_before_durable_completion_fresh_resume_revalidates(self):
        for how in ("move", "observe"):
            for point in ("poisoned", "effect_returned", "sealed", "completion_prepared"):
                for back in (True, False):
                    with self.subTest(how=how, point=point, original_returns=back):
                        self.fresh()
                        disk, p = self.run_child(point, how)
                        self.assertEqual(p.returncode, 70, p.stderr)
                        self.assertEqual((self.kinds(disk)[-1], M.fstate(disk)[0]), ("move.intent", "MOVE_INTENT"))
                        self.assertIsNone(disk.fs()["dst"])
                        if back:
                            self.set(disk, slot=RAW)
                        inv = M.start_invocation(disk, "resume-step")  # new authorization, new authority
                        self.assertNotEqual(inv.resume_auth, M.A_START)
                        result = M.invoke(inv, M.effect_resume_move)
                        if back:
                            self.assertEqual(result, ("COMMITTED", "MOVED", ""))
                            self.assertEqual(inv.authority.renames, 1)
                        else:
                            self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity_swap"))
                            self.assertEqual(inv.authority.renames, 0)

    def test_RT22_crash_after_durable_completion_nothing_pending(self):
        for how in ("move", "observe"):
            for point in ("completion_durable", "none"):
                with self.subTest(how=how, point=point):
                    self.fresh()
                    disk, p = self.run_child(point, how)
                    self.assertEqual(p.returncode, 70 if point != "none" else 0, p.stderr)
                    self.assertEqual(M.fstate(disk)[0], "FREEZE_BLOCKED")
                    self.set(disk, slot=RAW)
                    self.no_thaw_path(disk)
                    self.unmoved(disk)

    def test_RT23_later_owner_thaw_not_blocked_by_previous_poisoned_authority(self):
        disk = self.world()
        M.seed_thaw(disk)
        self.set(disk, slot=XOBJ)
        inv = M.start_invocation(disk, "thaw", M.A_THAW)
        leak = {}

        def eff1(store, fscap):
            leak["fscap"] = fscap
            return M.effect_move_back(store, fscap)
        self.assertEqual(M.invoke(inv, eff1), ("COMMITTED", "FREEZE_BLOCKED", "thaw_target_occupied"))
        with self.assertRaisesRegex(Refused, "nothing pending"):
            M.start_invocation(disk, "resume-step")
        self.set(disk, slot=None)
        inv2 = M.start_invocation(disk, "thaw", M.A_THAW2)               # new owner authorization (merged relation)
        seen = {}

        def eff2(store, fscap):
            seen["old"] = attempt(leak["fscap"].move_back)
            return M.effect_thaw(store, fscap)
        self.assertEqual(M.invoke(inv2, eff2), ("COMMITTED", "NONE", ""))
        self.assertEqual(seen["old"], SEALED)
        self.assertIsNot(inv2.authority, inv.authority)
        self.assertEqual((inv.authority.renames, inv2.authority.renames), (0, 1))
        self.assertEqual(disk.fs()["slot"], RAW)

    def test_RT24_freeze_start_freeze_blocked_has_no_thaw_path(self):
        disk = self.world(slot=XOBJ)
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        self.assertEqual(M.invoke(inv, M.effect_freeze_start), ("COMMITTED", "FREEZE_BLOCKED", "identity_swap"))
        self.set(disk, slot=RAW)
        self.no_thaw_path(disk)
        self.unmoved(disk)

    def test_RT24_case_c_after_issued_rename_still_no_thaw_path(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        M.HOOKS["observe"] = lambda: self.set(disk, extra=["intruder"]) if inv.authority.observations == 2 else None
        self.assertEqual(M.invoke(inv, M.effect_freeze_start), ("COMMITTED", "FREEZE_BLOCKED", "identity_swap"))
        M.HOOKS.clear()
        self.assertEqual(disk.fs()["dst"], RAW)                         # the tree stays (owner recovery, LIM-7)
        self.no_thaw_path(disk)


# ---------------------------------------------------------------- positive lifecycle

class Positive(Base):
    def test_valid_move_A(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        self.assertEqual(M.invoke(inv, M.effect_freeze_start), ("COMMITTED", "MOVED", ""))
        self.assertEqual(inv.authority.renames, 1)
        self.assertEqual(disk.fs()["dst"], RAW)
        self.assertEqual(self.kinds(disk)[-1], "move.done")

    def test_valid_already_moved_B(self):
        for ep_reverifies in (True, False):
            with self.subTest(ep_reverifies=ep_reverifies):
                self.fresh()
                disk = self.world()
                inv = M.start_invocation(disk, "freeze-start", M.A_START)

                def crash_after_rename(store, fscap):
                    M.journal_through_move_intent(store)
                    self.set(disk, dst=RAW, slot=None)                  # renamed, then the process stopped
                    raise RuntimeError("crash")
                self.assertEqual(M.invoke(inv, crash_after_rename)[0], "UNFINISHED")
                inv2 = M.start_invocation(disk, "resume-step")

                def resume(store, fscap):
                    out = fscap.move()
                    if ep_reverifies:
                        return M.finish_move(store, out)
                    return "FREEZE_BLOCKED", "identity", {}             # D2.2.1 resume row (b), EP not re-verified
                expect = ("MOVED", "") if ep_reverifies else ("FREEZE_BLOCKED", "identity")
                self.assertEqual(M.invoke(inv2, resume), ("COMMITTED",) + expect)
                self.assertEqual(inv2.authority.renames, 0)

    def test_valid_move_back_A(self):
        disk = self.world()
        M.seed_thaw(disk)
        inv = M.start_invocation(disk, "thaw", M.A_THAW)
        self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "NONE", ""))
        self.assertEqual(inv.authority.renames, 1)
        self.assertEqual(disk.fs()["slot"], RAW)
        self.assertEqual(self.kinds(disk)[-2:], ["thaw.move.done", "end"])

    def test_valid_already_returned_B(self):
        disk = self.world()
        M.seed_thaw(disk)
        self.set(disk, slot=RAW, dst=None)
        inv = M.start_invocation(disk, "thaw", M.A_THAW)
        self.assertEqual(M.invoke(inv, M.effect_move_back), ("COMMITTED", "NONE", ""))
        self.assertEqual(inv.authority.renames, 0)

    def test_observations_do_not_consume_the_move(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = []

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            seen.extend(fscap.observe(w)[0] for w in ("slot", "slot_root", "quarantine", "slot"))
            return M.finish_move(store, fscap.move())
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "MOVED", ""))
        self.assertEqual(seen, ["A"] * 4)
        self.assertEqual((inv.authority.renames, inv.authority.observations), (1, 6))

        self.fresh()
        disk = self.world()
        M.seed_thaw(disk)
        inv = M.start_invocation(disk, "thaw", M.A_THAW)

        def back(store, fscap):
            seen.append([fscap.observe(w)[0] for w in ("walk_q", "quarantine")])
            return M.effect_thaw(store, fscap)
        self.assertEqual(M.invoke(inv, back), ("COMMITTED", "NONE", ""))
        self.assertEqual(seen[-1], ["A", "A"])
        self.assertEqual(inv.authority.renames, 1)


if __name__ == "__main__":
    unittest.main()
