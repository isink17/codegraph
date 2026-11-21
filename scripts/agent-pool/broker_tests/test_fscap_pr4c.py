"""PR-4c obligations beyond the ported R2 model suites: the review's N5 pin
(thaw_target_occupied with later walk I/O), N2 (the content gate is bound to
the current intent's digest), the capability-gated stores, the store
capability's artifact and blob namespaces, the reader, the mint and its
binding, key-only `_traces`, and the constant protected-mode refusal."""

import json
import unittest
from unittest import mock

import fsharness as M
from test_fscap_fsmodel import RAW, XOBJ, attempt
from test_fscap_repair import GATE, Repair
from test_fscap_repair2 import ENTRYPOINTS

from broker import authz_txn, canonical, freeze, fscap, invocation, journal, o5contract as oc
from broker.authz_txn import StoreCap, TxnError
from broker.refusal import Refused as BrokerRefused
from broker.stores import CapStore, RecordStore

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


if __name__ == "__main__":
    unittest.main()
