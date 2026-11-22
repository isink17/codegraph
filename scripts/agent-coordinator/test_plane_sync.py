#!/usr/bin/env python3
"""Tests for plane_sync.py against a fake Plane transport, a fake gh and a
fake key. Never touches the Keychain, Plane or GitHub.

    python3 scripts/agent-coordinator/test_plane_sync.py
"""

import json
import os
import sys
import tempfile
import threading
import unittest
from pathlib import Path
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


def comment(cid, html, stripped=None, updated="2026-11-21T15:02:07.328673Z", **kw):
    return {"id": cid, "work_item_id": ITEM, "comment_html": html,
            "comment_stripped": stripped if stripped is not None else html.replace("<p>", "").replace("</p>", "\n")
            .replace("<code>", "").replace("</code>", ""),
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
            c["comment_stripped"] = comment(cid, c["comment_html"])["comment_stripped"]
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
        self.state = self.tmp / "state"
        self.old_state = ps.STATE_DIR
        ps.STATE_DIR = self.state
        self.addCleanup(setattr, ps, "STATE_DIR", self.old_state)
        self.body = self.tmp / "body.html"
        self.body.write_text("<p>PR #554 merged as abc123.</p>\n")
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
        self.assertNotIn(KEY, everything)

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
        self.state.mkdir()
        ps.Ledger(self.state).append({"type": "UNSYNCHRONIZED", "ticket": "CG-150", "pr": 554, "error": "x"})
        self.assertEqual(self.run_sync("status"), 1)
        self.assertIn("PENDING CG-150 #554", self.out[0])

    # ---------------------------------------------------------------- update and create

    def test_updates_only_the_verified_comment_and_reads_back(self):
        self.assertEqual(self.run_sync("CG-150", "554", str(self.body), "--comment-id", C554), 0)
        self.assertEqual(self.plane.writes(), [("PATCH", f"projects/{ps.PROJECT}/work-items/{ITEM}/comments/{C554}/")])
        self.assertTrue(self.plane.comments[C554]["comment_html"].endswith("<p>PR #554 merged as abc123.</p>"))
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
        self.state.mkdir()
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

    def test_malformed_ledger_refuses(self):
        self.state.mkdir()
        (self.state / "ledger.jsonl").write_text("not json\n")
        self.assertRefusedWithoutWrites(self.run_sync("CG-150", "554", str(self.body)), "LEDGER")

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
        gate = threading.Event()
        real = self.plane.__call__

        def slow(method, url, headers, data):
            if method == "PATCH":
                gate.wait(5)
            return real(method, url, headers, data)
        codes = []
        t = threading.Thread(target=lambda: codes.append(self.run_sync("CG-150", "554", str(self.body), plane=slow)))
        t.start()
        for _ in range(200):
            if any(m == "PATCH" for m, _ in self.plane.calls):
                break
            threading.Event().wait(0.01)
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


class Matching(unittest.TestCase):
    def test_marker_boundaries(self):
        for text, pr in [("codegraph-coordinator-sync/v1 isink17/codegraph#554", 554),
                         ("codegraph-coordinator-sync/v1 isink17/codegraph#533 synced 2026-11-07", 533),
                         ("codegraph-coordinator-sync/v1 isink17/codegraph#5540", 5540),
                         ("codegraph-coordinator-sync/v1 isink17/codegraph#554x", None),
                         ("codegraph-coordinator-sync/v1 isink17/codegraph design-decisions", None),
                         ("note\ncodegraph-coordinator-sync/v1 isink17/codegraph#554", None)]:
            self.assertEqual(ps.marker_pr(comment(C554, "", stripped=text)), pr, text)


if __name__ == "__main__":
    unittest.main()
