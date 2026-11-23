#!/usr/bin/env python3
"""Tests for plane_sync.py against a fake Plane transport, a fake gh and a
fake key. Never touches the Keychain, Plane or GitHub.

    python3 scripts/agent-coordinator/test_plane_sync.py
"""

import errno
import fcntl
import json
import os
import re
import shutil
import stat
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock
from urllib.parse import parse_qs, urlsplit

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.dont_write_bytecode = True
import plane_sync as ps  # noqa: E402

KEY = "plane_api_TEST-ONLY-SECRET-3f9a"
ITEM = "f6ef654c-9cfe-46ab-a95c-083043930d86"
C554 = "1a963fff-210c-4b7c-82e1-12b05b1de6b8"
C553 = "2b963fff-210c-4b7c-82e1-12b05b1de6b8"
C55 = "3c963fff-210c-4b7c-82e1-12b05b1de6b8"
NEW = "4d963fff-210c-4b7c-82e1-12b05b1de6b8"


def fragments(text, key=KEY):
    """Every 8-character piece of the key found in text."""
    return sorted({key[i:i + 8] for i in range(len(key) - 7) if key[i:i + 8] in text})


def comment(cid, html, stripped=None, updated="2026-11-21T15:02:07.328673Z", **kw):
    return {"id": cid, "work_item_id": ITEM, "comment_html": html,
            # Django strip_tags, as Plane derives it: no separator between blocks.
            "comment_stripped": stripped if stripped is not None else re.sub(r"<[^>]+>", "", html),
            "parent_id": None, "hidden_at": None, "updated_at": updated, **kw}


def marked(pr, text="old"):
    return f"<p><code>codegraph-coordinator-sync/v1 isink17/codegraph#{pr}</code></p><p>{text}</p>"


class FakePlane:
    def __init__(self, comments, page=100):
        self.comments = {c["id"]: c for c in comments}
        self.page = page
        self.calls = []
        self.script = []  # queued (status, headers, body) or exceptions, consumed first
        self.post_lost = False
        self.tick = 0

    def __call__(self, method, url, headers, data):
        assert headers["X-API-Key"] == KEY
        u = urlsplit(url)
        assert url.startswith(ps.API + "/"), url
        path = u.path[len(urlsplit(ps.API).path) + 1:]
        self.calls.append((method, path))
        if self.script:
            r = self.script.pop(0)
            if isinstance(r, BaseException):
                raise r
            if r is not None:
                return r
        parts = path.strip("/").split("/")
        if parts[:2] != ["projects", ps.PROJECT]:
            return 404, {}, b'{"code": "not_found"}'
        if len(parts) == 4:
            return self.ok({"id": ITEM, "sequence_id": 150, "project_id": ps.PROJECT, "identifier": parts[3]}
                           if parts[3] == "CG-150" else None)
        if len(parts) == 5 and method == "GET":
            q = parse_qs(u.query)
            start = int(q.get("cursor", ["0"])[0])
            rows = list(self.comments.values())[start:start + self.page]
            more = start + self.page < len(self.comments)
            return self.ok({"data": rows, "has_more": more, "next_cursor": str(start + self.page) if more else None,
                            "pagination": {"style": "cursor"}})
        if len(parts) == 5 and method == "POST":
            body = json.loads(data)
            c = comment(NEW, body["comment_html"], updated=self.now())
            self.comments[NEW] = c
            if self.post_lost:
                raise TimeoutError("timed out")
            return self.ok(c)
        cid = parts[5]
        if cid not in self.comments:
            return 404, {}, b'{"code": "not_found"}'
        if method == "PATCH":
            c = dict(self.comments[cid], comment_html=json.loads(data)["comment_html"], updated_at=self.now())
            c["comment_stripped"] = re.sub(r"<[^>]+>", "", c["comment_html"])
            self.comments[cid] = c
        return self.ok(self.comments[cid])

    def now(self):
        self.tick += 1
        return f"2026-11-22T10:00:{self.tick:02d}.000000Z"

    def ok(self, value):
        if value is None:
            return 404, {}, b'{"code": "not_found"}'
        return 200, {}, json.dumps(value).encode()

    def writes(self):
        return [c for c in self.calls if c[0] != "GET"]


def gh_for(ticket="CG-150"):
    return lambda pr: {"number": pr, "title": f"{ticket} O5 PR-4c", "body": "", "headRefName": "feat/x"}


class PlaneSync(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="plane-sync-test-"))
        self.addCleanup(shutil.rmtree, self.tmp)
        self.state = self.tmp / "state"
        self.old_state = ps.STATE_DIR
        ps.STATE_DIR = self.state
        self.addCleanup(setattr, ps, "STATE_DIR", self.old_state)
        self.body = self.tmp / "body.html"
        self.body.write_text('<p>PR: <a href="https://github.com/isink17/codegraph/pull/554">#554</a> '
                             'merged as abc123 &amp; read back.</p>\n')
        self.out, self.err, self.slept = [], [], []
        self.plane = FakePlane([comment(C553, marked(553)), comment(C554, marked(554)),
                                comment(C55, marked(55))])

    def run_sync(self, *argv, plane=None, gh=None):
        rc = ps.main(list(argv), transport=plane or self.plane, key=lambda: KEY, gh=gh or gh_for(),
                     sleep=self.slept.append, out=self.out.append, err=self.err.append)
        return rc

    def ledger(self):
        p = self.state / "ledger.jsonl"
        return [json.loads(x) for x in p.read_text().splitlines()] if p.exists() else []

    def tearDown(self):
        everything = "\n".join(self.out + self.err)
        p = self.state / "ledger.jsonl"
        if p.exists():
            everything += p.read_text()
        self.assertEqual(fragments(everything), [])

    # ---------------------------------------------------------------- read-only paths

    def test_status_reports_unique_comment_and_writes_nothing(self):
        self.assertEqual(self.run_sync("status", "CG-150", "554"), 0)
        self.assertIn(C554, self.out[-1])
        self.assertEqual(self.plane.writes(), [])
        self.assertFalse(self.state.exists())

    def test_dry_run_plans_update_and_writes_nothing(self):
        self.assertEqual(self.run_sync("sync", "CG-150", "554", str(self.body), "--dry-run",
                                       "--comment-id", C554), 0)
        self.assertIn(f"would UPDATE {C554}", self.out[-1])
        self.assertEqual(self.plane.writes(), [])
        self.assertFalse(self.state.exists())

    def test_status_without_target_lists_pending_from_ledger(self):
        self.state.mkdir(mode=0o700)
        ps.Ledger(self.state).append({"type": "UNSYNCHRONIZED", "ticket": "CG-150", "pr": 554, "error": "x"})
        self.assertEqual(self.run_sync("status"), 1)
        self.assertIn("PENDING CG-150 #554", self.out[0])

    # ---------------------------------------------------------------- update and create

    def test_updates_only_the_verified_comment_and_reads_back(self):
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), "--comment-id", C554), 0)
        self.assertEqual(self.plane.writes(), [("PATCH", f"projects/{ps.PROJECT}/work-items/{ITEM}/comments/{C554}/")])
        self.assertIn("merged as abc123", self.plane.comments[C554]["comment_html"])
        self.assertEqual(self.plane.comments[C553]["comment_html"], marked(553))
        self.assertEqual(self.plane.calls[-1], ("GET", f"projects/{ps.PROJECT}/work-items/{ITEM}/comments/{C554}/"))
        types = [e["type"] for e in self.ledger()]
        self.assertEqual(types, ["ATTEMPT", "SYNCED"])
        self.assertEqual(self.ledger()[-1]["comment_id"], C554)
        self.assertNotIn("merged as abc123", (self.state / "ledger.jsonl").read_text())

    def test_retry_after_crash_is_a_noop_not_a_second_write(self):
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        self.plane.calls.clear()
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        self.assertEqual(self.plane.writes(), [])
        self.assertEqual(self.ledger()[-1]["action"], "noop")

    def test_creates_once_when_no_marker_exists(self):
        self.assertEqual(self.run_sync("CG-150", "560", str(self.body), gh=gh_for()), 0)
        self.assertEqual([m for m, _ in self.plane.writes()], ["POST"])
        self.assertTrue(self.plane.comments[NEW]["comment_html"].startswith(
            "<p><code>codegraph-coordinator-sync/v1 isink17/codegraph#560</code></p>"))
        self.plane.calls.clear()
        self.assertEqual(self.run_sync("CG-150", "560", str(self.body)), 0)
        self.assertEqual(self.plane.writes(), [])

    def test_lost_post_response_adopts_the_created_comment(self):
        self.plane.post_lost = True
        self.assertEqual(self.run_sync("CG-150", "560", str(self.body)), 0)
        self.assertEqual([m for m, _ in self.plane.writes()], ["POST"])
        self.assertEqual(self.ledger()[-1]["comment_id"], NEW)

    def test_paginates_every_comment_page(self):
        self.plane.page = 1
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        self.assertEqual(self.ledger()[-1]["comment_id"], C554)

    # ---------------------------------------------------------------- fail closed

    def assertRefusedWithoutWrites(self, rc, code):
        self.assertEqual(rc, 3, self.err)
        self.assertIn(code, self.err[-1])
        self.assertEqual(self.plane.writes(), [])

    def test_two_marker_comments_refuse(self):
        self.plane.comments["5e963fff-210c-4b7c-82e1-12b05b1de6b8"] = comment(
            "5e963fff-210c-4b7c-82e1-12b05b1de6b8", marked(554, "dup"))
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "AMBIGUOUS_MARKER")
        self.assertEqual(self.ledger()[-1]["type"], "UNSYNCHRONIZED")

    def test_marker_outside_leading_line_refuses(self):
        self.plane.comments[C553] = comment(C553, marked(553, "see codegraph-coordinator-sync/v1 "
                                                               "isink17/codegraph#554"))
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "AMBIGUOUS_MARKER")

    def test_expected_comment_id_must_be_the_match(self):
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body), "--comment-id", C553),
                                        "AMBIGUOUS_MARKER")

    def test_ledger_known_comment_missing_from_plane_is_not_recreated(self):
        self.state.mkdir(mode=0o700)
        ps.Ledger(self.state).append({"type": "RECONSTRUCTED", "comments": [
            {"ticket": "CG-150", "pr": 554, "comment_id": C554}]})
        del self.plane.comments[C554]
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "AMBIGUOUS_MARKER")

    def test_reply_or_hidden_marker_comment_refuses(self):
        self.plane.comments[C554]["hidden_at"] = "2026-11-22T00:00:00Z"
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "reply or hidden")

    def test_comment_of_another_work_item_refuses(self):
        self.plane.comments[C554]["work_item_id"] = NEW
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "IDENTITY")

    def test_wrong_project_or_ticket_refuses(self):
        self.plane.script = [(200, {}, json.dumps({"id": ITEM, "sequence_id": 151, "project_id": ps.PROJECT,
                                                   "identifier": "CG-151"}).encode())]
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "IDENTITY")
        self.plane.script = [(200, {}, json.dumps({"id": ITEM, "sequence_id": 150, "project_id": NEW,
                                                   "identifier": "CG-150"}).encode())]
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "IDENTITY")

    def test_pr_that_does_not_name_the_ticket_refuses(self):
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body), gh=gh_for("CG-1500")),
                                        "MAPPING")

    def test_body_with_its_own_marker_refuses(self):
        self.body.write_text(marked(553, "x"))
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "BODY")
        self.assertEqual(self.ledger()[-1]["type"], "UNSYNCHRONIZED")
        self.body.write_text("<p>see codegraph&#45;coordinator-sync/v1 isink17/codegraph#553</p>")
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "BODY")

    def test_malformed_ledger_refuses(self):
        self.state.mkdir(mode=0o700)
        (self.state / "ledger.jsonl").write_text("not json\n")
        (self.state / "ledger.jsonl").chmod(0o600)
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "malformed")

    def test_bad_usage(self):
        self.assertEqual(self.run_sync("sync", "CG-15x", "554", str(self.body)), 2)
        self.assertEqual(self.run_sync("CG-150", "-1", str(self.body)), 2)
        self.assertEqual(self.plane.calls, [])

    # ---------------------------------------------------------------- transient failures

    def test_plane_down_is_unsynchronized(self):
        self.plane.script = [ConnectionRefusedError("refused")] * 3
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 1)
        self.assertEqual(self.ledger()[-1]["type"], "UNSYNCHRONIZED")
        self.assertEqual(self.slept, [2, 4])

    def test_rate_limit_waits_retry_after_capped(self):
        self.plane.script = [(429, {"Retry-After": "2"}, b""), (429, {"retry-after": "9999"}, b"")]
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        self.assertEqual(self.slept, [2, ps.MAX_RETRY_AFTER])

    def test_rate_limit_exhausted_is_unsynchronized(self):
        self.plane.script = [(429, {}, b"")] * 3
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 1)
        self.assertIn("rate limited", self.err[-1])

    def test_readback_mismatch_is_unsynchronized(self):
        real = self.plane.__call__

        def tamper(method, url, headers, data):
            status, h, raw = real(method, url, headers, data)
            if method == "GET" and url.endswith(f"/comments/{C554}/"):
                v = json.loads(raw)
                v["comment_html"] += "<p>normalised</p>"
                raw = json.dumps(v).encode()
            return status, h, raw
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), plane=tamper), 1)
        self.assertIn("READBACK", self.err[-1])
        self.assertEqual(self.ledger()[-1]["type"], "UNSYNCHRONIZED")

    def test_unauthorized_refuses_and_redacts_secret_echo(self):
        self.plane.script = [(401, {}, json.dumps({"code": f"bad key {KEY}"}).encode())]
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 3)
        self.assertIn("AUTH", self.err[-1])

    def test_secret_in_any_message_is_redacted(self):
        def leak(pr):
            raise ps.Unsynced(f"GITHUB: {KEY}")
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), gh=leak), 1)
        self.assertIn("[REDACTED]", self.err[-1])

    # ---------------------------------------------------------------- concurrency

    def test_second_writer_is_refused_while_lock_is_held(self):
        fd = ps.lock(self.state)
        try:
            self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 4)
            self.assertEqual(self.plane.calls, [])
        finally:
            os.close(fd)

    def test_concurrent_syncs_write_once(self):
        gate, entered = threading.Event(), threading.Event()
        real = self.plane.__call__

        def slow(method, url, headers, data):
            if method == "PATCH":
                entered.set()
                gate.wait(5)
            return real(method, url, headers, data)
        codes = []
        t = threading.Thread(target=lambda: codes.append(self.run_sync("CG-150", "554", str(self.body), plane=slow)))
        t.start()
        self.assertTrue(entered.wait(5))
        codes.append(self.run_sync("CG-150", "554", str(self.body), plane=slow))
        gate.set()
        t.join()
        self.assertEqual(sorted(codes), [0, 4])
        self.assertEqual(len(self.plane.writes()), 1)

    # ---------------------------------------------------------------- bootstrap

    def test_bootstrap_records_snapshot_once(self):
        self.assertEqual(self.run_sync("bootstrap", "CG-150", "--dry-run"), 0)
        self.assertFalse((self.state / "ledger.jsonl").exists())
        self.assertEqual(self.run_sync("bootstrap", "CG-150"), 0)
        (snap,) = self.ledger()
        self.assertEqual(snap["type"], "RECONSTRUCTED")
        self.assertEqual([(r["pr"], r["comment_id"]) for r in snap["comments"]], [(55, C55), (553, C553), (554, C554)])
        self.assertEqual(oct((self.state / "ledger.jsonl").stat().st_mode & 0o777), "0o600")
        self.assertEqual(self.run_sync("bootstrap", "CG-150"), 3)
        self.assertEqual(self.plane.writes(), [])

    def test_bootstrap_refuses_duplicate_markers(self):
        self.plane.comments[NEW] = comment(NEW, marked(553, "again"))
        self.assertEqual(self.run_sync("bootstrap", "CG-150"), 3)
        self.assertFalse((self.state / "ledger.jsonl").exists())


    # ---------------------------------------------------------------- review round 1

    def sanitizing(self):
        real = self.plane.__call__

        def rewrite(method, url, headers, data):
            if data is not None:
                v = json.loads(data)
                v["comment_html"] = v["comment_html"].replace("<a href=", '<a rel="noopener noreferrer" href=')
                data = json.dumps(v).encode()
            return real(method, url, headers, data)
        return rewrite

    def test_sanitized_storage_reads_back_and_retries_as_noop(self):
        plane = self.sanitizing()
        self.assertEqual(self.run_sync("CG-150", "560", str(self.body), plane=plane), 0)
        self.assertIn('rel="noopener', self.plane.comments[NEW]["comment_html"])
        self.plane.calls.clear()
        self.assertEqual(self.run_sync("CG-150", "560", str(self.body), plane=plane), 0)
        self.assertEqual(self.plane.writes(), [])

    def test_unanswered_create_is_not_repeated_within_grace(self):
        self.plane.script = [None, None, (500, {}, b"")]  # work item, comments, POST
        self.assertEqual(self.run_sync("CG-150", "560", str(self.body)), 1)
        self.assertEqual([m for m, _ in self.plane.writes()], ["POST"])
        self.assertEqual(self.run_sync("CG-150", "560", str(self.body)), 1)
        self.assertIn("CREATE_PENDING", self.err[-1])
        self.assertEqual([m for m, _ in self.plane.writes()], ["POST"])
        old = ps.CREATE_GRACE
        ps.CREATE_GRACE = 0
        self.addCleanup(setattr, ps, "CREATE_GRACE", old)
        self.assertEqual(self.run_sync("CG-150", "560", str(self.body)), 0)
        self.assertEqual([m for m, _ in self.plane.writes()], ["POST", "POST"])
        self.assertEqual(self.ledger()[-1]["comment_id"], NEW)

    def test_patch_network_error_is_retried(self):
        real = self.plane.__call__
        failed = []

        def flaky(method, url, headers, data):
            if method == "PATCH" and not failed:
                failed.append(1)
                raise ConnectionResetError("reset")
            return real(method, url, headers, data)
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), plane=flaky), 0)
        self.assertEqual(self.slept, [2])

    def test_oversized_response_is_unsynchronized(self):
        self.plane.script = [(200, {}, b" " * (ps.MAX_RESPONSE + 1))]
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 1)
        self.assertIn("response over", self.err[-1])

    def test_repeated_cursor_is_unsynchronized(self):
        page = json.dumps({"data": [], "has_more": True, "next_cursor": "c1"}).encode()
        self.plane.script = [None, (200, {}, page), (200, {}, page)]
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 1)
        self.assertIn("pagination", self.err[-1])

    def test_torn_ledger_line_refuses(self):
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        with open(self.state / "ledger.jsonl", "a") as f:
            f.write('{"type": "ATT')
        self.plane.calls.clear()
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "LEDGER")

    def test_loose_permissions_refuse(self):
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        (self.state / "ledger.jsonl").chmod(0o644)
        self.plane.calls.clear()
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "0600")
        (self.state / "ledger.jsonl").chmod(0o600)
        self.state.chmod(0o755)
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "0700")
        self.state.chmod(0o700)

    def test_owner_comment_id_supersedes_ledger_pin(self):
        self.state.mkdir(mode=0o700)
        ps.Ledger(self.state).append({"type": "RECONSTRUCTED", "comments": [
            {"ticket": "CG-150", "pr": 554, "comment_id": C553}]})
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "the ledger")
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), "--comment-id", C554), 0)
        self.assertEqual(self.ledger()[-1]["comment_id"], C554)
        self.plane.calls.clear()
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)

    def test_marker_lost_on_readback_has_its_own_message(self):
        real = self.plane.__call__

        def drop(method, url, headers, data):
            status, h, raw = real(method, url, headers, data)
            if method == "GET" and url.endswith(f"/comments/{C554}/"):
                v = json.loads(raw)
                v["comment_html"] = "<p>x</p>"
                raw = json.dumps(v).encode()
            return status, h, raw
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), plane=drop), 1)
        self.assertIn("no longer leads with", self.err[-1])

    def test_updated_at_compared_as_time_not_text(self):
        # DRF drops zero microseconds; as text "...01Z" sorts after "...01.500000Z".
        self.plane.comments[C554]["updated_at"] = "2026-11-22T10:00:01Z"
        self.plane.now = lambda: "2026-11-22T10:00:01.500000Z"
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)

    # ---------------------------------------------------------------- durability

    def patch_flush(self, fullfsync, fail=lambda call: None):
        """Record every flush as (call, "file" | "dir"); fail(call) may return an error to raise."""
        calls, real_fcntl = [], fcntl.fcntl

        def kind(fd):
            return "dir" if stat.S_ISDIR(os.fstat(fd).st_mode) else "file"

        def record(call, fd):
            calls.append((call, kind(fd)))
            error = fail(call)
            if error:
                raise error

        def fake_fcntl(fd, cmd, *a):
            if fullfsync is None or cmd != fullfsync:
                return real_fcntl(fd, cmd, *a)
            record("F_FULLFSYNC", fd)
            return 0
        for target, name, value in ((ps, "FULLFSYNC", fullfsync), (fcntl, "fcntl", fake_fcntl),
                                    (os, "fsync", lambda fd: record("fsync", fd))):
            patcher = mock.patch.object(target, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        return calls

    def test_darwin_flushes_file_then_directory_with_full_fsync(self):
        if sys.platform == "darwin":
            self.assertEqual(ps.FULLFSYNC, fcntl.F_FULLFSYNC)
        calls = self.patch_flush(51)
        os.close(ps.lock(self.state))
        self.assertEqual(calls, [("F_FULLFSYNC", "dir")])
        calls.clear()
        for _ in range(2):
            ps.Ledger(self.state).append({"type": "UNSYNCHRONIZED", "ticket": "CG-150", "pr": 554, "error": "x"})
        self.assertEqual(calls, [("F_FULLFSYNC", "file"), ("F_FULLFSYNC", "dir")] * 2)

    def test_unsupported_full_fsync_falls_back_to_fsync(self):
        calls = self.patch_flush(51, lambda call: OSError(errno.ENOTSUP, "x") if call == "F_FULLFSYNC" else None)
        self.state.mkdir(mode=0o700)
        ps.Ledger(self.state).append({"type": "UNSYNCHRONIZED", "ticket": "CG-150", "pr": 554, "error": "x"})
        self.assertEqual(calls, [("F_FULLFSYNC", "file"), ("fsync", "file"), ("F_FULLFSYNC", "dir"), ("fsync", "dir")])

    def test_without_full_fsync_flushes_file_then_directory_with_fsync(self):
        calls = self.patch_flush(None)
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        self.assertEqual(calls, [("fsync", "dir")] + [("fsync", "file"), ("fsync", "dir")] * 2)

    def test_failed_full_fsync_surfaces_before_any_write(self):
        only_ledger = []
        calls = self.patch_flush(51, lambda call: None if only_ledger and not (self.state / "ledger.jsonl").exists()
                                 else OSError(errno.EIO, "x"))
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 1)
        self.assertIn("local I/O error", self.err[-1])
        only_ledger.append(1)
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 1)
        self.assertEqual(self.out, [])
        self.assertEqual(self.plane.writes(), [])
        self.assertIn("UNSYNCHRONIZED event could not be written", self.err[-1])
        self.assertNotIn("fsync", [c for c, _ in calls])

    def test_unflushed_synced_event_is_not_claimed_and_retry_is_a_noop(self):
        healthy = []
        self.patch_flush(None, lambda call: None if healthy or not self.plane.writes() else OSError(errno.EIO, "x"))
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 1)
        self.assertEqual(self.out, [])
        self.assertIn("SYNCED event could not be written", self.err[-1])
        healthy.append(1)
        self.plane.calls.clear()
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        self.assertEqual(self.plane.writes(), [])
        self.assertEqual(self.ledger()[-1]["type"], "SYNCED")

    def test_live_reconstructed_snapshot_line_still_loads_and_pins(self):
        self.state.mkdir(mode=0o700)
        line = {"type": "RECONSTRUCTED", "source": "plane-cloud v2 read-back",
                "note": "point-in-time snapshot; no prior event history exists",
                "comments": [{"ticket": "CG-150", "pr": 554, "comment_id": C554,
                              "updated_at": "2026-11-21T15:02:07.328673Z", "body_sha256": "0" * 64}],
                "utc": "2026-11-22T12:00:00.000000Z"}
        p = self.state / "ledger.jsonl"
        p.write_text(json.dumps(line, sort_keys=True) + "\n")
        p.chmod(0o600)
        self.assertEqual(ps.Ledger(self.state).known_comment("CG-150", 554), C554)
        self.plane.comments[C554]["comment_html"] = marked(554, "x")
        self.plane.comments.pop(C553)
        self.plane.comments["5e963fff-210c-4b7c-82e1-12b05b1de6b8"] = comment(
            "5e963fff-210c-4b7c-82e1-12b05b1de6b8", marked(554, "dup"))
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "AMBIGUOUS_MARKER")
        self.plane.comments.pop("5e963fff-210c-4b7c-82e1-12b05b1de6b8")
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        self.assertEqual(self.plane.writes(), [("PATCH", f"projects/{ps.PROJECT}/work-items/{ITEM}/comments/{C554}/")])
        self.assertEqual(self.ledger()[0], line)

    # ---------------------------------------------------------------- canonical marker

    def test_noncanonical_marker_comment_refuses_and_is_never_updated(self):
        for html in ["<p><code>codegraph-coordinator-sync/v1 isink17/codegraph#0554</code></p><p>old</p>",
                     "<p><code>codegraph-coordinator-sync/v1&nbsp;isink17/codegraph#554</code></p><p>old</p>"]:
            self.plane.comments[C554] = comment(C554, html)
            self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "AMBIGUOUS_MARKER")
            self.assertEqual(self.plane.comments[C554]["comment_html"], html)

    def test_marker_naming_no_pr_does_not_block_the_canonical_comment(self):
        self.plane.comments[NEW] = comment(NEW, "<p><code>codegraph-coordinator-sync/v1 isink17/codegraph#00</code></p>")
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 0)
        self.assertEqual(self.plane.writes(), [("PATCH", f"projects/{ps.PROJECT}/work-items/{ITEM}/comments/{C554}/")])

    # ---------------------------------------------------------------- redaction

    def test_server_code_echoing_the_key_late_is_redacted_before_truncation(self):
        self.plane.script = [(401, {}, json.dumps({"code": "x" * 51 + KEY}).encode())]
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body)), 3)
        self.assertIn("AUTH", self.err[-1])
        self.assertEqual(fragments(self.err[-1] + (self.state / "ledger.jsonl").read_text()), [])

    def test_long_error_message_is_redacted_before_truncation(self):
        def leak(pr):
            raise ps.Unsynced("GITHUB: " + "y" * 280 + KEY)
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), gh=leak), 1)
        self.assertEqual(fragments((self.state / "ledger.jsonl").read_text()), [])

    def test_escaped_spellings_of_the_key_are_redacted(self):
        key = 'k"\\\u00e9-SECRET-0123456789'
        self.state.mkdir(mode=0o700)
        ps.Ledger(self.state, ps.Plane(key).redact).append({"type": "UNSYNCHRONIZED", "error": "x " + key})
        self.assertEqual(fragments((self.state / "ledger.jsonl").read_text(), key), [])
        self.assertEqual(fragments(ps.redact(f"got {key!r} and {json.dumps(key)}", key), key), [])

    # ---------------------------------------------------------------- unexpected errors

    def test_unexpected_transport_error_is_unsynchronized_without_its_message(self):
        def bad(method, url, headers, data):
            raise ValueError(f"Invalid header value {headers['X-API-Key']!r}")
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), plane=bad), 1)
        self.assertIn("transport failure (ValueError)", self.err[-1])
        self.assertNotIn("Invalid header", self.err[-1])
        self.assertEqual(self.ledger()[-1]["type"], "UNSYNCHRONIZED")
        self.assertEqual(self.slept, [])

    def test_unexpected_error_after_post_adopts_instead_of_posting_again(self):
        real = self.plane.__call__

        def lost(method, url, headers, data):
            r = real(method, url, headers, data)
            if method == "POST":
                raise RuntimeError(f"boom {KEY}")
            return r
        self.assertEqual(self.run_sync("CG-150", "560", str(self.body), plane=lost), 0)
        self.assertEqual([m for m, _ in self.plane.writes()], ["POST"])
        self.assertEqual(self.ledger()[-1]["comment_id"], NEW)

    def test_unexpected_error_never_escapes_main(self):
        def broken(pr):
            raise KeyError(KEY)
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), gh=broken), 1)
        self.assertEqual(self.err, ["plane-sync: UNSYNCHRONIZED: internal error (KeyError)"])
        self.assertEqual(self.ledger()[-1]["error"], "internal error (KeyError)")

    def test_unwritable_unsynchronized_event_keeps_the_refusal_and_says_so(self):
        real = ps.Ledger.append

        def append(ledger, event):
            if event["type"] == "UNSYNCHRONIZED":
                raise OSError(errno.ENOSPC, "full")
            return real(ledger, event)
        with mock.patch.object(ps.Ledger, "append", append):
            self.assertEqual(self.run_sync("CG-150", "554", str(self.body), gh=gh_for("CG-1500")), 3)
        self.assertEqual(self.out, [])
        self.assertIn("UNSYNCHRONIZED event could not be written", self.err[-1])
        self.assertIn("MAPPING", self.err[-1])


class Matching(unittest.TestCase):
    def test_marker_boundaries(self):
        m = "codegraph-coordinator-sync/v1 isink17/codegraph"
        for markup, pr in [(f"<p><code>{m}#554</code></p><p>PR: x</p>", 554),
                           (f"<p>{m}#533 synced 2026-11-07</p>", 533),
                           (f"<p><code>{m}#5540</code></p>", 5540),
                           (f"<p><code>{m}#55</code></p><p>0 items</p>", 55),
                           (f"<p><code>{m} design-decisions</code></p>", None),
                           (f"<p>note</p><p>{m}#554</p>", None),
                           (f"<p>note<br>{m}#554</p>", None)]:
            self.assertEqual(ps.marker_pr(comment(C554, markup)), pr, markup)

    def test_only_the_canonical_spelling_is_a_marker(self):
        m = "codegraph-coordinator-sync/v1 isink17/codegraph"
        real = f"<p><code>{m}#554</code></p>"
        self.assertEqual(ps.marker_pr(comment(C554, real)), 554)
        self.assertEqual(ps.matches([comment(C554, real)], 554)[0]["id"], C554)
        self.assertEqual(ps.marker_pr(comment(C554, "<p><code>codegraph-coordinator-sync/v1\n\t isink17/codegraph#554"
                                                   "</code></p>")), 554)
        blank_first = f"<p>&nbsp;</p><p>\u2003</p><p><code>{m}#554</code></p>"
        self.assertEqual(ps.marker_pr(comment(C554, blank_first)), 554)
        self.assertEqual(ps.matches([comment(C554, blank_first)], 554)[0]["id"], C554)
        for markup, names_554 in [(f"<p><code>{m}#0554</code></p>", True),
                                  (f"<p><code>{m}#00554</code></p>", True),
                                  (f"<p><code>{m}#+554</code></p>", True),
                                  (f"<p><code>{m}#-554</code></p>", True),
                                  (f"<p><code>{m}# 554</code></p>", True),
                                  (f"<p><code>{m}#&nbsp;554</code></p>", True),
                                  (f"<p>&nbsp;<code>{m}#554</code></p>", True),
                                  ("<p><code>codegraph-coordinator-sync/v1&nbsp;isink17/codegraph#554</code></p>", True),
                                  ("<p><code>codegraph-coordinator-sync/v1\u00a0isink17/codegraph#554</code></p>", True),
                                  ("<p><code>codegraph-coordinator-sync/v1 isink17/codegraph&#160;#554</code></p>", True),
                                  (f"<p><code>{m}#00</code></p>", False),
                                  (f"<p><code>{m}#05540</code></p>", False)]:
            c = comment(C554, markup)
            self.assertIsNone(ps.marker_pr(c), markup)
            if names_554:
                with self.assertRaises(ps.Refused, msg=markup):
                    ps.matches([c], 554)
            else:
                self.assertEqual(ps.matches([c], 554), [], markup)

    def test_text_ignores_sanitizer_rewrites(self):
        sent = '<p>PR: <a href="https://x/1">#1</a> a &amp; b</p>'
        stored = '<p>PR: <a href="https://x/1" rel="noopener noreferrer nofollow" target="_blank">#1</a> a &#38; b</p>'
        self.assertEqual(ps.text_of(sent), ps.text_of(stored))
        self.assertNotEqual(ps.text_of(sent), ps.text_of(sent.replace("a &amp; b", "a &amp; c")))
        self.assertEqual(ps.text_of("<p>a\n  b</p><p>c</p>"), ps.text_of("<p>a b</p>\n<p>c</p>"))
        self.assertEqual(ps.text_of("<p>a b</p><p>c</p>"), "a b\nc")

    def test_utc_fractions_of_any_length(self):
        self.assertLess(ps.parse_utc("2026-11-21T15:02:07Z"), ps.parse_utc("2026-11-21T15:02:07.3286Z"))
        self.assertIsNone(ps.parse_utc("2026-11-21T15:02:07"))


if __name__ == "__main__":
    unittest.main()
