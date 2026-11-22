"""PR-4c obligations beyond the ported R2 model suites: the review's N5 pin
(thaw_target_occupied with later walk I/O), N2 (the content gate is bound to
the current intent's digest), the capability-gated stores, the store
capability's artifact and blob namespaces, the reader, the mint and its
binding, key-only `_traces`, and the constant protected-mode refusal; then
the post-review hardening: the seal on every exit of the effect (N1), the
cycle identity recomputed at every mint (N2), the deliberate refusal
boundaries of the unimplemented operations (N3) and the pair's store scope
(N4)."""

import json
import operator
import threading
import unittest
from unittest import mock

import fsharness as M
from test_fscap_fsmodel import RAW, XOBJ, attempt, named, spawn
from test_fscap_repair import GATE, Repair
from test_fscap_repair2 import ENTRYPOINTS

from broker import authz_txn, canonical, freeze, fscap, invocation, journal, o5contract as oc
from broker.authz_txn import StoreCap, TxnError
from broker.refusal import Refused as BrokerRefused
from broker.stores import CapStore, Crash, EvidenceStore, RecordStore

POISONED = ("Refused", "authority FSCAP_POISONED")


# ---------------------------------------------------------------- N5: a proven C is not downgraded by later walk I/O

class N5OccupiedThenWalkIO(Repair):
    def test_occupied_slot_and_walk_EIO_every_entrypoint(self):
        for effect, first in ENTRYPOINTS.items():
            with self.subTest(effect=effect.__name__):
                self.fresh()
                disk, inv = self.thaw(slot=XOBJ, objects="io")
                before = disk.fs()
                result = M.invoke(inv, effect)
                self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "thaw_target_occupied"))
                rec = self.outcome(disk, inv, poison="thaw_target_occupied", f="FREEZE_BLOCKED",
                                   tail="thaw.move.intent")
                self.assertEqual(self.obs(rec), [(first, "C")])
                self.assertEqual(([e for e in rec.log if e[0] in ("obs_io", "rename", "content", "put")],
                                  rec.rename_issued), ([], False))
                self.assertEqual(disk.fs(), before)
                self.assertNotIn("thaw.move.done", self.kinds(disk))
                self.assertEqual(M.inspect_durable(inv)["fwal"], "completion_durable")

    def test_walk_EIO_after_the_occupied_classification_changes_nothing(self):
        for name, later in (("objects EIO", dict(objects="io")), ("walk observation EIO", dict(io=["walk_q"]))):
            with self.subTest(name):
                self.fresh()
                disk, inv = self.thaw(slot=XOBJ)
                seen = {}

                def eff(store, fs, later=later):
                    seen["slot"] = attempt(fs.observe, "slot")
                    self.set(disk, **later)
                    seen["walk"] = attempt(fs.observe, "walk_q")
                    self.set(disk, slot=None)                       # A would now hold
                    seen["back"] = attempt(fs.move_back)
                    return "NONE", "", {"journal": "d" * 64}        # the effect claims a thaw
                result = M.invoke(inv, eff)
                self.assertEqual(seen["slot"], ("Refused", "FREEZE_BLOCKED(thaw_target_occupied)"))
                self.assertEqual((seen["walk"], seen["back"]), (POISONED, POISONED))
                self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "thaw_target_occupied"))
                rec = self.outcome(disk, inv, poison="thaw_target_occupied", f="FREEZE_BLOCKED",
                                   tail="thaw.move.intent")
                self.assertEqual((rec.io, rec.claimed, rec.observations), (None, False, 1))
                self.assertEqual((disk.fs()["dst"], disk.fs()["slot"]), (RAW, None))


# ---------------------------------------------------------------- N2: the content gate is bound to the current intent

class N2ContentGateDigest(Repair):
    def other_intent(self, disk, **header):
        tail = disk.journal()[-1]
        if not header:
            return M.h(disk.journal()[2])                           # this cycle's move.intent: another intent
        return M.h(dict(tail, header=dict(tail["header"], **header)))

    def test_a_verdict_for_another_intent_cycle_or_slot_authorizes_nothing(self):
        for name, header in (("another intent", {}), ("another cycle", {"cycle_id": "f" * 64}),
                             ("another slot", {"slot": "codegraph-worker-y"})):
            with self.subTest(name):
                self.fresh()
                disk, inv = self.thaw()
                foreign = self.other_intent(disk, **header)
                self.assertNotEqual(foreign, M.h(disk.journal()[-1]))
                seen = {}

                def eff(store, fs, foreign=foreign):
                    seen["pre"] = M.thaw_precheck(store, fs)
                    for w in ("originals", "attrs"):
                        inv.authority.content_ok[w] = foreign
                    seen["back"] = attempt(fs.move_back)
                    return "NONE", "", {"journal": "d" * 64}
                result = M.invoke(inv, eff)
                self.assertEqual((seen["pre"], seen["back"]), ("A", ("Unfinished", GATE[1])))
                self.assertEqual(result[0], "UNFINISHED")
                rec = self.outcome(disk, inv, f="THAW_INTENT", tail="thaw.move.intent")
                self.assertEqual((rec.outcome, rec.rename_issued), (GATE, False))
                self.assertEqual((disk.fs()["dst"], disk.fs()["slot"]), (RAW, None))

    def test_the_verdict_names_the_current_intent_digest(self):
        disk, inv = self.thaw()
        seen = {}

        def eff(store, fs):
            M.thaw_precheck(store, fs)
            seen["pass"] = dict(inv.authority.content_ok)
            return M.effect_move_back(store, fs)
        tail = M.h(disk.journal()[-1])
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "NONE", ""))
        self.assertEqual(seen["pass"], {"originals": tail, "attrs": tail})
        self.assertEqual(inv.authority.renames, 1)


# ---------------------------------------------------------------- N3: the activation residual, reproduced and pinned

class N3ActivationResidual(Repair):
    def test_resume_after_durable_thaw_move_done_does_not_observe_again(self):
        """Not a safety property: a residual pinned for the real-backend activation gate. After a durable
        `thaw.move.done`, a foreign move of the tree back into Q, and a resume, the resume completes F NONE
        from the journal alone (D2.2.1 R21-11) while the tree sits in Q. Before activation the deployed
        privilege and holder model must exclude that external move, or the owner must approve another check."""
        disk, inv = self.thaw()

        def done_then_stopped(store, fs):
            M.thaw_precheck(store, fs)
            fs.move_back()
            store.put("thaw.move.done", intent=M.h(disk.journal()[-1]))
            raise RuntimeError("stopped before end{thawed}")
        self.assertEqual(M.invoke(inv, done_then_stopped)[0], "UNFINISHED")
        self.assertEqual((self.kinds(disk)[-1], disk.fs()["slot"], disk.fs()["dst"]), ("thaw.move.done", RAW, None))
        self.set(disk, slot=None, dst=RAW)                              # 2. an external actor moves it back into Q
        inv2 = M.start_invocation(disk, "resume-step")
        self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))   # 3. logical F NONE
        self.assertEqual((inv2.authority.observations, inv2.authority.renames), (0, 0))
        self.assertEqual((M.fstate(disk)[0], disk.fs()["slot"], disk.fs()["dst"]), ("NONE", None, RAW))


# ---------------------------------------------------------------- stores and the store capability

class Stores(Repair):
    def run_effect(self, eff, verb="freeze-start", **fs):
        disk = self.world(**fs)
        inv = M.start_invocation(disk, verb, M.A_START)
        return disk, inv, M.invoke(inv, eff)

    def test_new_stores_gate_the_effect_namespaces(self):
        st = freeze.new_stores(None)
        self.assertEqual({n for n, s in st.items() if type(s) is CapStore}, {"journals", "artifacts", "blobs"})
        self.assertEqual({n for n, s in st.items() if type(s) is RecordStore},
                         {"cutovers", "cutover_heads", "enumerations", "barriers"})

    def test_cap_stores_refuse_the_commit_txn_and_writes_outside_the_hold(self):
        txn = authz_txn._issue("0" * 64, "0" * 64)
        self.addCleanup(txn.close)
        st = CapStore("journals")
        with self.assertRaisesRegex(TxnError, "accepts only the store capability"):
            st.put(txn, "journal/x/00000000", b"{}", True)
        leak = {}
        self.run_effect(lambda s, f: (leak.update(cap=s), M.effect_freeze_start(s, f))[1])
        with self.assertRaisesRegex(TxnError, "accepts only the store capability"):
            st.put(leak["cap"], "journal/x/00000000", b"{}", True)     # not inside its own hold
        self.assertEqual(st.items, {})

    def test_journal_header_and_sequence_come_from_the_capability(self):
        seen = {}

        def eff(s, f):
            seen["hdr"] = attempt(s.put, "start", header={"slot": "x"})
            seen["early"] = [attempt(s.put, "start"), attempt(s.put, "ep.done", binding=M.EARLY, early="0" * 64)]
            return "NONE", "store_error", {}
        disk, inv, _ = self.run_effect(eff)
        self.assertEqual(seen["hdr"], ("Refused", "journal_mismatch"))
        self.assertEqual(seen["early"][1], ("Refused", "journal_mismatch"))
        self.assertEqual([(r["seq"], r["header"]) for r in disk.journal()], [(0, M.HEADER)])

    def test_v2_journal_in_synthetic_mode(self):
        seen = {}

        def eff(s, f):
            seen["puts"] = [attempt(s.put, "start", schema=journal.J_SCHEMA),
                            attempt(s.put, "move.done", linked=[]),
                            attempt(s.put, "end", early=M.EARLY)]
            return "NONE", "store_error", {}
        disk, inv, _ = self.run_effect(eff)
        self.assertEqual([p[0] for p in seen["puts"]], ["ok", "Refused", "ok"])
        self.assertEqual([r["schema"] for r in disk.journal()], [journal.J_SCHEMA] * 2)

    def test_artifacts_and_blobs_follow_the_start_record_and_are_content_addressed(self):
        seen = {}
        big = {"header": M.HEADER, "set": []}
        while len(canonical.canon(big)) <= journal.MAX_OBJECT:
            big["set"].append(canonical.sha256(str(len(big["set"])).encode()))
        n = len(big["set"]) - 1

        def eff(s, f):
            seen["early"] = [attempt(s.put_blob, b"x"), attempt(s.put_artifact, {"header": M.HEADER})]
            s.put("start")
            seen["blob"] = [attempt(s.put_blob, b"x"), attempt(s.put_blob, b"x"), attempt(s.put_blob, "x")]
            seen["art"] = [attempt(s.put_artifact, {"header": M.HEADER, "v": 1}),
                           attempt(s.put_artifact, {"header": dict(M.HEADER, cycle_id="f" * 64), "v": 1}),
                           attempt(s.put_artifact, {"v": 1}),
                           attempt(s.put_artifact, {"header": M.HEADER, "set": big["set"][:n]}),
                           attempt(s.put_artifact, big)]
            disk.world.o5.stores["blobs"].items[f"blob/{canonical.sha256(b'y')}"] = b"tampered"
            seen["conflict"] = attempt(s.put_blob, b"y")
            return "NONE", "store_error", {}
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        M.invoke(inv, eff)
        st = disk.world.o5.stores
        self.assertEqual(seen["early"], [("Refused", "journal start not durable")] * 2)
        self.assertEqual(seen["blob"][:2], [("ok", canonical.sha256(b"x"))] * 2)
        self.assertEqual(seen["blob"][2], ("Refused", "blob bytes"))
        self.assertEqual(set(st["blobs"].items), {f"blob/{canonical.sha256(b'x')}", f"blob/{canonical.sha256(b'y')}"})
        self.assertEqual([a[0] for a in seen["art"]], ["ok", "Refused", "Refused", "ok", "Refused"])
        self.assertEqual(seen["art"][4], ("Refused", "artifact too large"))
        self.assertTrue(700 <= n <= 740, n)                            # the ~722-blob fail-closed bound
        self.assertEqual(seen["conflict"], ("Refused", f"blobs: blob/{canonical.sha256(b'y')} exists"))
        self.assertTrue(all(k.startswith(f"artifact/{M.CID}/") for k in st["artifacts"].items))

    def test_a_put_that_raised_after_the_write_is_relied_on_only_once_made_durable(self):
        st = {}

        def eff(s, f):
            M.journal_through_move_intent(s)
            f.move()
            M.FAULTS.add("journal:put:after")
            attempt_err = None
            try:
                s.put("move.done", intent=M.h(disk.journal()[-1]), linked=[])
            except OSError as e:
                attempt_err = e.strerror
            M.FAULTS.clear()
            st["pending"] = set(disk.world.o5.stores["journals"].pending)
            return attempt_err
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        self.assertEqual(M.invoke(inv, eff)[0], "UNFINISHED")
        key = f"journal/{M.CID}/{3:08d}"
        self.assertEqual(st["pending"], {key})
        self.assertFalse(disk.world.o5.stores["journals"].durable(key))
        inv2 = M.start_invocation(disk, "resume-step")
        reread = lambda s, f: (f.observe("slot"), M.effect_resume_move(s, f))[1]  # noqa: E731
        self.assertEqual(M.invoke(inv2, reread), ("COMMITTED", "MOVED", ""))
        self.assertTrue(disk.world.o5.stores["journals"].durable(key))   # the reader asked the store to sync it

    def test_store_listing_is_complete_or_refuses(self):
        st = CapStore("artifacts", M.Faults())
        st.items["artifact/a/b"] = b"x"
        self.assertEqual(st.keys(), ["artifact/a/b"])
        st.faults = mock.Mock(hit=mock.Mock(side_effect=OSError(5, "listing failed")))
        with self.assertRaises(BrokerRefused) as e:
            st.keys()
        self.assertEqual(e.exception.detail, "artifact_inventory")


# ---------------------------------------------------------------- the reader

class ReaderTests(Repair):
    def test_exact_keys_of_its_own_cycle_and_an_empty_blob_allow_set(self):
        seen = {}
        disk = self.world()
        other = f"journal/{'f' * 64}/00000000"
        disk.world.o5.stores["journals"].items[other] = b"{}"
        disk.world.o5.stores["blobs"].items[f"blob/{canonical.sha256(b'x')}"] = b"x"

        def eff(s, f):
            r = f._c.reader
            seen["reads"] = [attempt(r.blob, canonical.sha256(b"x")), attempt(r.artifact, "../x"),
                             attempt(r.journal, 0)]
            seen["r"] = r
            return "NONE", "store_error", {}
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        M.invoke(inv, eff)
        self.assertEqual(seen["reads"], [("Refused", "blob not in the reader's allow-set"),
                                         ("Refused", "artifact key"), ("ok", None)])
        self.assertEqual(attempt(seen["r"].journal, 0), ("Refused", "reader closed (FSCAP_SEALED)"))  # closed with the pair
        self.assertNotIn("write", dir(seen["r"]))


# ---------------------------------------------------------------- the mint, its binding and the call site

class Mint(Repair):
    def test_no_public_constructors_and_no_copies(self):
        for cls in (StoreCap, fscap.FsCap, fscap.Reader, fscap._Authority):
            with self.subTest(cls=cls.__name__), self.assertRaises(TypeError):
                cls()

    def test_mint_refuses_protected_mode_a_closed_or_missing_txn_and_a_second_mint(self):
        binding = {"verb": "thaw", "cycle_id": M.CID}
        txn = authz_txn._issue("0" * 64, "0" * 64)
        with self.assertRaises(BrokerRefused) as e:
            authz_txn._issue_effect(txn, binding, M.HEADER, oc.PROTECTED, {}, M.Faults())
        self.assertEqual(e.exception.detail, "barrier_unavailable(protected_mode_unsupported)")
        self.assertFalse(txn._effect)
        authz_txn._issue_effect(txn, binding, M.HEADER, oc.SYNTHETIC, {}, M.Faults())
        with self.assertRaisesRegex(fscap.Refused, "second mint in this invocation"):
            authz_txn._issue_effect(txn, binding, M.HEADER, oc.SYNTHETIC, {}, M.Faults())
        txn2 = authz_txn._issue("0" * 64, "0" * 64)
        txn2.close()
        for t in (txn2, None):
            with self.subTest(txn=t), self.assertRaises(TxnError):
                authz_txn._issue_effect(t, binding, M.HEADER, oc.SYNTHETIC, {}, M.Faults())

    def test_protected_mode_at_the_call_site_is_unfinished_before_the_effect(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        calls = []
        with mock.patch.object(invocation.o5contract, "mode", lambda w: oc.PROTECTED):
            result = M.invoke(inv, lambda s, f: calls.append(1))
        self.assertEqual(result, ("UNFINISHED", "Refused: QUIESCENCE_NOT_ESTABLISHED: "
                                                "barrier_unavailable(protected_mode_unsupported)"))
        self.assertEqual((calls, M.fstate(disk)[0], disk.journal()), ([], "MOVE_INTENT", []))

    def test_the_effect_receives_exactly_seven_arguments(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = []
        real = invocation._f_complete

        def spy(world, *a):
            inner = world.freeze
            world.freeze = lambda *args: (seen.append(args), inner(*args))[1]
            return real(world, *a)
        with mock.patch.object(invocation, "_f_complete", spy):
            self.assertEqual(M.invoke(inv, M.effect_freeze_start), ("COMMITTED", "MOVED", ""))
        (args,) = seen
        self.assertEqual(len(args), 7)
        self.assertEqual(args[:5], ("freeze-start", M.SLOT, M.OP, M.CTX["freeze-start"], M.CID))
        self.assertEqual((type(args[5]), type(args[6])), (StoreCap, fscap.FsCap))

    def test_resume_binding_conflicts_refuse_before_the_effect(self):
        for name in ("target", "transition", "not this invocation's burn"):
            with self.subTest(name):
                self.fresh()
                disk = self.world()
                inv = M.start_invocation(disk, "freeze-start", M.A_START)
                M.invoke(inv, lambda s, f: (M.journal_through_move_intent(s), 1 / 0))
                f, last = M.fstate(disk)
                doc = {"verb": "resume-step", "slot": M.SLOT, "op_id": M.OP, "transition": "freeze-start",
                       "target_digest": canonical.digest(last), "n": 99}
                if name == "target":
                    doc["target_digest"] = "f" * 64
                if name == "transition":
                    doc["transition"] = "thaw"
                rdoc = disk.burn(doc, "resume")
                if name == "not this invocation's burn":
                    disk.burn({"verb": "stop", "slot": M.SLOT, "n": 100})
                calls = []
                result = M.invoke(M.Invocation(disk, "freeze-start", last["auth_digest"], rdoc),
                                  lambda s, fs: calls.append(1))
                self.assertEqual(result, ("UNFINISHED", "Refused: STATE_UNKNOWN: effect_binding"))
                self.assertEqual((calls, M.fstate(disk)[0]), ([], "MOVE_INTENT"))

    def test_without_a_backend_every_filesystem_operation_refuses(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = {}

        def eff(s, f):
            M.journal_through_move_intent(s)
            seen["ops"] = [attempt(f.observe, "slot"), attempt(f.move)]
            return "MOVED", "", {"early": M.EARLY}
        with mock.patch.object(M, "Backend", lambda d: None):
            result = M.invoke(inv, eff)
        self.assertEqual(seen["ops"], [("Refused", "no filesystem backend"),
                                       ("Unfinished", "no filesystem backend")])
        self.assertEqual((result[0], inv.authority.renames), ("UNFINISHED", 0))

    def test_an_installed_broker_has_no_backend(self):
        self.assertIsNone(fscap._BACKEND)


# ---------------------------------------------------------------- reconciliation and traces

class ResultAndTraces(unittest.TestCase):
    def test_an_unreadable_record_blocks_a_freeze_blocked_completion(self):
        st = CapStore("journals")
        st.items["journal/c/00000000"] = canonical.canon({"kind": "start"})
        st.items["journal/c/00000001"] = b"not canonical"
        self.assertEqual(fscap.journal_kinds(st, "c"), ["start", None])
        rec = fscap.Record("identity", False, False, None, 0, 0, (), None, None, None)
        self.assertIsNone(fscap.reconcile("thaw", rec, ["start", None], None, None))
        self.assertEqual(fscap.reconcile("thaw", rec, ["start"], None, None), ("FREEZE_BLOCKED", "identity", {}))

    def test_traces_come_from_keys_never_from_artifact_or_blob_bytes(self):
        cid, auth = "c" * 64, "a" * 64
        stores = freeze.new_stores(None)
        world = type("W", (), {"o5": oc.Wiring(stores)})()
        stores["artifacts"].items[f"artifact/{'f' * 64}/x"] = json.dumps({"cid": cid, "auth": auth}).encode()
        stores["blobs"].items["blob/x"] = (cid + auth).encode()
        self.assertFalse(freeze._traces(world, cid, auth))
        stores["artifacts"].items[f"artifact/{cid}/x"] = b"{}"
        self.assertTrue(freeze._traces(world, cid, auth))



# ---------------------------------------------------------------- N1: the seal on every exit of the effect

SEALED = ("Refused", "authority FSCAP_SEALED")
WAIT = 10


class N1SealOnEveryExit(Repair):
    def raising(self, exc):
        """A freeze-start effect that writes through move.intent, keeps both capabilities and raises `exc`."""
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        kept = {}

        def eff(s, f):
            M.journal_through_move_intent(s)
            kept.update(s=s, f=f)
            raise exc
        with self.assertRaises(type(exc)) as cm:
            M.invoke(inv, eff)
        self.assertIs(cm.exception, exc)                                # propagated unchanged, never replaced
        return disk, inv, kept

    def after_exit(self, disk, inv, kept, renames=0):
        """Sealed, no post-seal effect through any alias, and no F completion was attempted or written."""
        a, s, f = inv.authority, kept["s"], kept["f"]
        self.assertIs(a.state, M.FSCAP_SEALED)
        self.assertIsNotNone(a.record)
        before, journal = disk.fs(), disk.journal()
        tried = [attempt(f.move), attempt(f.view().move), attempt(M.FsCap(a, inv).move_back),
                 attempt(f.observe, "slot"), attempt(f.read_content, "attrs"),
                 attempt(s.put, "move.done", intent=M.h(journal[-1]), linked=[]),
                 attempt(s.put_blob, b"late"), attempt(f._c.reader.journal, 0)]
        self.assertEqual(tried, [SEALED] * 7 + [("Refused", "reader closed (FSCAP_SEALED)")])
        self.assertEqual((a.renames, disk.fs(), disk.journal()), (renames, before, journal))
        self.assertEqual((M.fstate(disk)[0], self.completions(disk), inv.prepared), ("MOVE_INTENT", 0, None))
        self.assertEqual(M.inspect_durable(inv)["fwal"], "intent_pending")

    def test_system_exit_keyboard_interrupt_and_crash_seal_and_propagate(self):
        for exc in (SystemExit(3), KeyboardInterrupt(), Crash("effect stopped")):
            with self.subTest(exc=type(exc).__name__):
                self.fresh()
                disk, inv, kept = self.raising(exc)
                self.after_exit(disk, inv, kept)
                self.unmoved(disk)

    def test_an_ordinary_exception_still_seals_and_ends_unfinished(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        kept = {}

        def eff(s, f):
            M.journal_through_move_intent(s)
            kept.update(s=s, f=f)
            raise RuntimeError("effect failed")
        self.assertEqual(M.invoke(inv, eff)[0], "UNFINISHED")
        self.after_exit(disk, inv, kept)

    def test_a_worker_in_flight_linearizes_before_the_seal_and_nothing_runs_after_it(self):
        for exc in (SystemExit(3), KeyboardInterrupt(), Crash("effect stopped")):
            with self.subTest(exc=type(exc).__name__):
                self.fresh()
                disk = self.world()
                inv = M.start_invocation(disk, "freeze-start", M.A_START)
                out, kept = {}, {}
                paused, release = threading.Event(), threading.Event()
                M.HOOKS["before_rename"] = named("worker", lambda: (paused.set(), release.wait(WAIT)))
                M.HOOKS["seal_enter"] = release.set                     # the seal starts only after the effect left

                def eff(s, f, exc=exc):
                    M.journal_through_move_intent(s)
                    kept.update(s=s, f=f, t=spawn(out, "worker", f.move))
                    paused.wait(WAIT)                                   # the worker holds the lock mid-call
                    raise exc
                with self.assertRaises(type(exc)):
                    M.invoke(inv, eff)
                kept["t"].join(WAIT)
                self.assertEqual(out["worker"], ("ok", "completed"))   # the in-flight call finished first
                rec = inv.authority.record
                self.assertEqual((rec.claimed, rec.rename_issued, rec.renames, rec.outcome),
                                 (True, True, 1, ("completed",)))      # and the sealed record holds it
                late = {}
                spawn(late, "late", kept["f"].view().move).join(WAIT)
                self.assertEqual(late["late"], SEALED)
                self.after_exit(disk, inv, kept, renames=1)
                self.assertEqual((disk.fs()["slot"], disk.fs()["dst"]), (None, RAW))
                self.assertNotIn("move.done", self.kinds(disk))

    def test_a_failed_completion_append_follows_the_seal(self):
        for how in ("fwal append fails", "crash before the append"):
            with self.subTest(how):
                self.fresh()
                disk = self.world()
                inv = M.start_invocation(disk, "freeze-start", M.A_START)
                kept = {}

                def eff(s, f):
                    kept.update(s=s, f=f)
                    return M.effect_freeze_start(s, f)
                if how == "fwal append fails":
                    M.FAULTS.add("fwal:append:before")
                    result = M.invoke(inv, eff)
                    self.assertEqual(result, ("UNFINISHED", "OSError: [Errno 5] fwal append failed before the write"))
                else:
                    M.CRASH["at"] = "completion_prepared"
                    with self.assertRaises(Crash):
                        M.invoke(inv, eff)
                    M.CRASH["at"] = None
                M.FAULTS.clear()
                self.assertEqual(inv.prepared["value"], "MOVED")       # prepared, never appended
                self.assertEqual(attempt(kept["f"].move_back), SEALED)
                self.assertEqual(attempt(kept["s"].put, "end", outcome="none", reason=""), SEALED)
                self.assertEqual((M.fstate(disk)[0], self.completions(disk), inv.authority.renames),
                                 ("MOVE_INTENT", 0, 1))
                self.assertEqual(M.inspect_durable(inv)["fwal"], "intent_pending")


# ---------------------------------------------------------------- N2: the cycle identity is recomputed at every mint

A_START2, A_CONT2, A_OTHER = "c5" * 32, "c6" * 32, "c7" * 32
BINDING_REFUSED = ("UNFINISHED", "Refused: STATE_UNKNOWN: effect_binding")


def moved(disk, token=M.A_START):
    """Test setup: F WAL MOVE_INTENT and MOVED for the freeze-start document of `token`; returns its digest."""
    a = disk.burn(M._doc("freeze-start", "NONE", token))
    disk.fwal_append(M._intent("MOVE_INTENT", a))
    disk.fwal_append({"op_id": M.OP, "kind": "complete", "value": "MOVED", "reason": "",
                      "bindings": {"early": M.EARLY}, "auth_digest": a, "utc": 1})
    return a


class N2FreshCycleIdentity(Repair):
    def mints(self, inv, effect):
        """Runs `inv`; returns (result, the bindings and headers the mint received, effect calls)."""
        minted, calls, real = [], [], authz_txn._issue_effect

        def spy(txn, binding, header, *a):
            minted.append((binding, header))
            return real(txn, binding, header, *a)

        def eff(s, f):
            calls.append(1)
            return effect(s, f)
        with mock.patch.object(authz_txn, "_issue_effect", spy):
            return M.invoke(inv, eff), minted, calls

    def refused_before_the_mint(self, disk, inv, f):
        result, minted, calls = self.mints(inv, lambda s, fs: ("NONE", "store_error", {}))
        self.assertEqual((result, minted, calls, inv.authority), (BINDING_REFUSED, [], [], None))
        self.assertFalse(inv.txn._effect)
        self.assertEqual((M.fstate(disk)[0], self.completions(disk)), (f, 0))

    def test_a_wrong_cycle_id_refuses_before_the_mint(self):
        for name, cid in (("wrong", "f" * 64), ("another slot", oc.cycle_id("codegraph-worker-y", M.GEN, M.OP,
                                                                              M.START_AUTH)),
                          ("another operation", oc.cycle_id(M.SLOT, M.GEN, "op2", M.START_AUTH)),
                          ("another generation", oc.cycle_id(M.SLOT, M.GEN + 1, M.OP, M.START_AUTH))):
            for ctx in ("the gate's context", "a context that agrees"):
                with self.subTest(name, ctx=ctx):
                    self.fresh()
                    disk, inv = self.thaw()
                    inv.cid = cid
                    if ctx == "a context that agrees":
                        inv.ctx = M._ctx(M._pre("thaw", cid, M.START_AUTH))
                    self.refused_before_the_mint(disk, inv, "THAW_INTENT")

    def test_a_moved_library_generation_refuses_before_the_mint(self):
        disk, inv = self.thaw()
        with mock.patch.object(invocation.o5freeze, "gen_of", lambda facts: M.GEN + 1):
            self.refused_before_the_mint(disk, inv, "THAW_INTENT")

    def test_a_freeze_start_with_another_cycle_id_refuses_before_the_mint(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        inv.cid = oc.cycle_id(M.SLOT, M.GEN, "op2", M.START_AUTH)
        self.refused_before_the_mint(disk, inv, "MOVE_INTENT")

    def test_a_stale_cycle_view_refuses_and_the_current_cycle_mints(self):
        disk, inv = self.thaw()
        self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "NONE", ""))   # cycle 1 is thawed
        a2 = moved(disk, A_START2)                                      # cycle 2 of the same operation
        cid2 = oc.cycle_id(M.SLOT, M.GEN, M.OP, a2)
        stale = M.start_invocation(disk, "freeze-continue", A_CONT2)    # cid and context still name cycle 1
        self.refused_before_the_mint(disk, stale, "REVOKE_INTENT")
        current = M.Invocation(disk, "freeze-continue", stale.auth)
        current.cid, current.ctx = cid2, M._ctx(M._pre("freeze-continue", cid2, a2))
        result, minted, calls = self.mints(current, lambda s, f: ("FREEZE_INCOMPLETE", "store_error",
                                                                  {"journal": "d" * 64}))
        self.assertEqual((result, calls), (("COMMITTED", "FREEZE_INCOMPLETE", "store_error"), [1]))
        (binding, header), = minted
        self.assertEqual((binding["cycle_id"], binding["auth_digest"], header["cycle_id"]), (cid2, stale.auth, cid2))
        mixed = M.start_invocation(disk, "thaw", M.A_THAW2)             # cycle 2's cid with cycle 1's context
        mixed.cid = cid2
        self.refused_before_the_mint(disk, mixed, "THAW_INTENT")

    def test_a_changed_original_authorization_refuses_before_the_mint(self):
        for name in ("another burned freeze-start", "the stored document changed"):
            with self.subTest(name):
                self.fresh()
                disk = self.world()
                M.seed_thaw(disk)
                other = disk.burn(M._doc("freeze-start", "NONE", A_OTHER))
                inv = M.start_invocation(disk, "thaw", M.A_THAW)
                if name == "another burned freeze-start":               # a fully consistent forged context
                    inv.cid = oc.cycle_id(M.SLOT, M.GEN, M.OP, other)
                    inv.ctx = M._ctx(M._pre("thaw", inv.cid, other))
                else:                                                   # re-hash fails: no original document
                    disk.world.authz.docs[M.START_AUTH] += b" "
                self.refused_before_the_mint(disk, inv, "THAW_INTENT")

    def test_every_verb_mints_with_the_recomputed_identity(self):
        cases = {}
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        cases["freeze-start"] = (disk, inv, M.effect_freeze_start, ("COMMITTED", "MOVED", ""), M.START_AUTH)
        disk = self.world()
        a = moved(disk)
        inv = M.start_invocation(disk, "freeze-continue", M.A_CONT)
        incomplete = lambda s, f: ("FREEZE_INCOMPLETE", "store_error", {"journal": "d" * 64})  # noqa: E731
        cases["freeze-continue"] = (disk, inv, incomplete, ("COMMITTED", "FREEZE_INCOMPLETE", "store_error"), inv.auth)
        self.assertEqual(a, M.START_AUTH)
        disk, inv = self.thaw()
        cases["thaw"] = (disk, inv, M.effect_thaw, ("COMMITTED", "NONE", ""), inv.auth)
        disk = self.world()
        first = M.start_invocation(disk, "freeze-start", M.A_START)
        M.invoke(first, lambda s, f: (M.journal_through_move_intent(s), 1 / 0))
        inv = M.start_invocation(disk, "resume-step")
        cases["resume-step"] = (disk, inv, M.effect_resume_move, ("COMMITTED", "MOVED", ""), M.START_AUTH)
        for verb, (disk, inv, effect, want, auth) in cases.items():
            with self.subTest(verb):
                result, minted, calls = self.mints(inv, effect)
                self.assertEqual((result, calls), (want, [1]))
                (binding, header), = minted
                self.assertEqual(binding, {"slot": M.SLOT, "op_id": M.OP, "verb": inv.verb, "cycle_id": M.CID,
                                           "auth_digest": auth})
                self.assertEqual(header, M.HEADER)


# ---------------------------------------------------------------- N3: the deliberate refusal boundaries

class N3RefusalBoundaries(Repair):
    def test_the_pair_exposes_no_unimplemented_privileged_operation(self):
        """prepare_quarantine, attribute changes, staging and flushes (PR-4e/PR-4f) do not exist on the pair."""
        public = {cls.__name__: sorted(n for n in dir(cls) if not n.startswith("_"))
                  for cls in (fscap.FsCap, StoreCap, fscap.Reader)}
        self.assertEqual(public, {"FsCap": ["move", "move_back", "observe", "read_content", "view"],
                                  "StoreCap": ["put", "put_artifact", "put_blob"],
                                  "Reader": ["artifact", "blob", "journal"]})

    def test_attribute_change_completions_refuse_without_an_effect(self):
        for verb, kind in (("freeze-continue", "revoke.done"), ("thaw", "thaw.done"), ("thaw", "revoke.done")):
            with self.subTest(verb=verb, kind=kind):
                self.fresh()
                if verb == "thaw":
                    disk, inv = self.thaw()
                else:
                    disk = self.world(slot=None, dst=RAW)
                    moved(disk)
                    disk.journal_seed("start")
                    disk.journal_seed("ep.done", binding=M.EARLY)
                    mi = disk.journal_seed("move.intent", src_parent=M.SP, src_name=M.SN, dst_parent=M.QD,
                                           dst_name=M.DN, root=M.ROOT)
                    disk.journal_seed("move.done", intent=mi, linked=[])
                    inv = M.start_invocation(disk, "freeze-continue", M.A_CONT)
                before, journal = disk.fs(), disk.journal()
                seen = {}

                def eff(s, f, kind=kind):
                    seen["put"] = attempt(s.put, kind, attr="flags", lo=0, hi=0)
                    return "FREEZE_BLOCKED", "journal_mismatch", {}
                M.invoke(inv, eff)
                self.assertEqual(seen["put"],
                                 ("Refused", "completion not observed by the paired filesystem capability"))
                self.assertEqual((disk.fs(), disk.journal(), inv.authority.renames), (before, journal, 0))

    def test_completed_ep_gate_is_absent_and_the_allow_set_stays_empty(self):
        """Not a safety property: pins the missing Completed-EP re-validation (PR-4e) for the activation gate.
        The move completes although the journal's EP binding names no stored blob; the reader's allow-set
        stays empty. An installed broker has no backend, so this path performs no real effect."""
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        self.assertEqual(M.invoke(inv, M.effect_freeze_start), ("COMMITTED", "MOVED", ""))
        self.assertEqual((disk.world.o5.stores["blobs"].items, inv.authority.renames), ({}, 1))
        self.assertEqual(inv.fscap._c.reader._allow, frozenset())
        self.assertIsNone(fscap._BACKEND)


# ---------------------------------------------------------------- N4: the pair reaches only the capability-gated stores

class N4StoreScope(Repair):
    def test_only_the_capability_gated_stores_are_reachable_and_read_only(self):
        disk = self.world()
        st = disk.world.o5.stores
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = {}

        def tries(fn):
            try:
                fn()
                return "ok"
            except (TypeError, KeyError, TxnError) as e:
                return type(e).__name__

        def eff(s, f):
            r = f._c.reader
            seen["maps"] = (s._stores, r._stores)
            seen["puts"] = [attempt(s.put, "start")[0], attempt(s.put_blob, b"x")[0],
                            attempt(s.put_artifact, {"header": M.HEADER})[0], attempt(r.journal, 0)[0]]
            seen["forbidden"] = [tries(lambda: operator.setitem(s._stores, "barriers", st["barriers"])),
                                 tries(lambda: operator.setitem(r._stores, "journals", st["barriers"])),
                                 tries(lambda: s._stores["barriers"]), tries(lambda: r._stores["evidence"]),
                                 tries(lambda: st["barriers"].put(s, "barrier/x", b"{}"))]
            return "NONE", "store_error", {}
        M.invoke(inv, eff)
        for m in seen["maps"]:
            self.assertEqual(sorted(m), ["artifacts", "blobs", "journals"])
            self.assertTrue(all(m[n] is st[n] for n in m))
            self.assertFalse(any(isinstance(v, (RecordStore, EvidenceStore)) for v in m.values()))
        self.assertEqual(seen["puts"], ["ok"] * 4)
        self.assertEqual(seen["forbidden"], ["TypeError", "TypeError", "KeyError", "KeyError", "TxnError"])
        self.assertEqual((st["barriers"].items, sorted(st)), ({}, sorted(freeze.STORES + freeze.CAP_STORES +
                                                                         ("evidence",))))


if __name__ == "__main__":
    unittest.main()
