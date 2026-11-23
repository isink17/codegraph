#!/usr/bin/env python3
"""Synchronise one coordinator comment per (ticket, PR) on Plane Cloud.

    plane_sync.py status [CG-N PR]               read-only
    plane_sync.py sync CG-N PR BODY.html [--comment-id UUID] [--dry-run]
    plane_sync.py CG-N PR BODY.html              same as sync (campaign.py calls this form)
    plane_sync.py bootstrap CG-N [CG-N ...] [--dry-run]

Policy: docs/agent-campaign.md, "Plane synchronisation". The comment is found
by its marker line `codegraph-coordinator-sync/v1 isink17/codegraph#<PR>`;
one match is updated in place, none is created once, anything else refuses.
Success is reported only after a read-back. The API key is read from the
macOS Keychain at run time and never printed, logged or written.

Exit codes: 0 synced or read-only check passed, 1 UNSYNCHRONIZED (retry),
2 usage, 3 refused (owner must look), 4 another writer holds the lock.
A retry after exit 1 re-reads Plane and the ledger: it adopts a comment that
an unanswered create made and never re-sends a create inside CREATE_GRACE.
"""

import argparse
import errno
import fcntl
import hashlib
import html
import http.client
import json
import os
import re
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlencode

API = "https://api.plane.so/api/v2/workspaces/ivan-dev"
PROJECT = "adc3082b-dc0d-4b7f-aa14-a19d2f248680"
REPO_SLUG = "isink17/codegraph"
KEYCHAIN_SERVICE = "plane-cloud-mac"
KEYCHAIN_ACCOUNT = "api-token"
MARKER_PREFIX = f"codegraph-coordinator-sync/v1 {REPO_SLUG}#"
STATE_DIR = Path(os.environ.get("CG_PLANE_SYNC_STATE",
                                str(Path.home() / "Projekti/codegraph-ops/plane-sync")))

TIMEOUT = 20
MAX_RESPONSE = 8 << 20
MAX_BODY = 64 << 10
MAX_LEDGER = 16 << 20
MAX_PAGES = 50
PER_PAGE = 200
MAX_TRIES = 3
MAX_RETRY_AFTER = 60
# An unanswered create is not repeated until any request still in flight has landed.
CREATE_GRACE = 300

UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
TICKET = re.compile(r"^CG-([1-9][0-9]{0,5})$")
PR_NUMBER = re.compile(r"^[1-9][0-9]{0,6}$")
CANONICAL_MARKER = re.compile(re.escape(MARKER_PREFIX) + r"([1-9][0-9]*)(?![0-9])")
# Any spelling a reader could take for a PR's marker: zero padding, a sign,
# spaces around '#', or a Unicode space where the marker has a plain one.
LOOSE_MARKER = re.compile(r"codegraph-coordinator-sync/v1 ?" + re.escape(REPO_SLUG) + r" ?# ?[+-]? ?([0-9]+)(?![0-9])")
FULLFSYNC = getattr(fcntl, "F_FULLFSYNC", None)
# F_FULLFSYNC errors that mean the file system does not support it, not that the flush failed.
NO_FULLFSYNC = {errno.EINVAL, errno.ENOTSUP, errno.EOPNOTSUPP, errno.ENOTTY}


class Refused(Exception):
    """Needs the owner: ambiguous or mismatched identity, auth, bad input."""


class Unsynced(Exception):
    """Transient: Plane or GitHub unreachable, throttled, or read-back failed."""


class Locked(Exception):
    pass


def utc():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def sha256(text):
    return hashlib.sha256(text.encode()).hexdigest()


def marker(pr):
    return f"{MARKER_PREFIX}{pr}"


def redact(text, key):
    """Every spelling of the key that str(), repr() or json.dumps() can emit."""
    for form in sorted({key, repr(key)[1:-1], json.dumps(key)[1:-1]}, key=len, reverse=True):
        text = text.replace(form, "[REDACTED]")
    return text


def flush(fd):
    """Durable or raise. On macOS fsync leaves data in the drive's cache, so
    F_FULLFSYNC is used; plain fsync only where it is unsupported."""
    if FULLFSYNC is not None:
        try:
            fcntl.fcntl(fd, FULLFSYNC)
            return
        except OSError as e:
            if e.errno not in NO_FULLFSYNC:
                raise
    os.fsync(fd)


def flush_dir(path):
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        flush(fd)
    finally:
        os.close(fd)


def keychain_key():
    try:
        p = subprocess.run(["/usr/bin/security", "find-generic-password", "-s", KEYCHAIN_SERVICE,
                            "-a", KEYCHAIN_ACCOUNT, "-w"], capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.TimeoutExpired):
        raise Refused("AUTH: the macOS Keychain could not be read")
    key = p.stdout.strip()
    if p.returncode != 0 or not key:
        raise Refused(f"AUTH: no Plane API key in the Keychain ({KEYCHAIN_SERVICE}/{KEYCHAIN_ACCOUNT})")
    return key


def urllib_transport(method, url, headers, data):
    """(status, headers, bytes); raises OSError when nothing came back."""
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            return r.status, dict(r.headers), r.read(MAX_RESPONSE + 1)
    except urllib.error.HTTPError as e:
        try:
            return e.code, dict(e.headers or {}), e.read(MAX_RESPONSE + 1)
        finally:
            e.close()
    except http.client.HTTPException as e:
        raise OSError(f"incomplete HTTP exchange ({type(e).__name__})")


def gh_pr(pr):
    try:
        p = subprocess.run(["gh", "pr", "view", str(pr), "--repo", REPO_SLUG,
                            "--json", "number,title,body,headRefName"],
                           capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.TimeoutExpired) as e:
        raise Unsynced(f"GITHUB: gh pr view failed: {type(e).__name__}")
    if p.returncode != 0:
        raise Unsynced(f"GITHUB: gh pr view exit {p.returncode}")
    try:
        return json.loads(p.stdout)
    except ValueError:
        raise Unsynced("GITHUB: malformed gh output")


class Plane:
    def __init__(self, key, transport=urllib_transport, sleep=time.sleep):
        self.key, self.transport, self.sleep = key, transport, sleep

    def redact(self, text):
        return redact(text, self.key)

    def call(self, method, path, body=None):
        """Bounded: 429 waits Retry-After (capped) and retries; 5xx and
        network errors retry only idempotent methods; POST never retries."""
        data = None if body is None else json.dumps(body).encode()
        headers = {"X-API-Key": self.key, "Accept": "application/json"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        last = "no attempt"
        for attempt in range(1, MAX_TRIES + 1):
            try:
                status, rheaders, raw = self.transport(method, f"{API}/{path}", headers, data)
            except OSError as e:
                last = f"network error ({type(e).__name__})"
                if method == "POST":
                    raise Unsynced(f"PLANE: {method} {path}: {last}")
                if attempt < MAX_TRIES:
                    self.sleep(2 ** attempt)
                continue
            except Exception as e:
                # Its message may quote request headers, the key among them.
                raise Unsynced(f"PLANE: {method} {path}: transport failure ({type(e).__name__})") from None
            if status == 429:
                last = "rate limited"
                if attempt < MAX_TRIES:
                    self.sleep(retry_after(rheaders))
                continue
            if status >= 500:
                last = f"HTTP {status}"
                if method == "POST":
                    raise Unsynced(f"PLANE: {method} {path}: {last}")
                if attempt < MAX_TRIES:
                    self.sleep(2 ** attempt)
                continue
            if len(raw) > MAX_RESPONSE:
                raise Unsynced(f"PLANE: {method} {path}: response over {MAX_RESPONSE} bytes")
            try:
                value = json.loads(raw.decode()) if raw else {}
            except ValueError:
                value = None
            if status in (401, 403):
                raise Refused(f"AUTH: {method} {path}: HTTP {status} {error_code(value, self.redact)}")
            if status >= 400:
                raise Refused(f"PLANE: {method} {path}: HTTP {status} {error_code(value, self.redact)}")
            if not isinstance(value, dict):
                raise Unsynced(f"PLANE: {method} {path}: malformed response")
            return value
        raise Unsynced(f"PLANE: {method} {path}: {last} after {MAX_TRIES} attempts")

    def work_item(self, ticket):
        number = int(TICKET.match(ticket).group(1))
        w = self.call("GET", f"projects/{PROJECT}/work-items/{ticket}/")
        if (w.get("project_id") != PROJECT or w.get("sequence_id") != number
                or w.get("identifier") != ticket or not is_uuid(w.get("id"))):
            raise Refused(f"IDENTITY: {ticket} did not resolve to one work item in project {PROJECT}")
        return w["id"]

    def comments(self, item):
        out, seen, cursors, cursor = [], set(), set(), None
        for _ in range(MAX_PAGES):
            q = {"per_page": PER_PAGE, "paginate": "cursor", **({"cursor": cursor} if cursor else {})}
            page = self.call("GET", f"projects/{PROJECT}/work-items/{item}/comments/?{urlencode(q)}")
            data, more = page.get("data"), page.get("has_more")
            if not isinstance(data, list) or type(more) is not bool:
                raise Unsynced("PLANE: malformed comment page")
            for c in data:
                check_comment(c, item)
                if c["id"] in seen:
                    raise Refused(f"IDENTITY: comment {c['id']} listed twice")
                seen.add(c["id"])
                out.append(c)
            if not more:
                return out
            cursor = page.get("next_cursor")
            if not isinstance(cursor, str) or not cursor or cursor in cursors:
                raise Unsynced("PLANE: malformed comment pagination")
            cursors.add(cursor)
        raise Unsynced(f"PLANE: more than {MAX_PAGES} comment pages")

    def comment(self, item, cid):
        c = self.call("GET", f"projects/{PROJECT}/work-items/{item}/comments/{cid}/")
        check_comment(c, item)
        if c["id"] != cid:
            raise Refused(f"IDENTITY: read-back returned comment {c['id']}, expected {cid}")
        return c


def retry_after(headers):
    value = {k.lower(): v for k, v in headers.items()}.get("retry-after", "")
    try:
        return min(max(int(value), 1), MAX_RETRY_AFTER)
    except ValueError:
        return 5


def error_code(value, scrub):
    code = value.get("code") if isinstance(value, dict) else None
    return re.sub(r"[^A-Za-z0-9_.-]", "", scrub(code))[:60] if isinstance(code, str) else ""


def is_uuid(value):
    return isinstance(value, str) and bool(UUID.match(value))


def check_comment(c, item):
    if (not isinstance(c, dict) or not is_uuid(c.get("id")) or c.get("work_item_id") != item
            or not isinstance(c.get("comment_html"), str)):
        raise Refused("IDENTITY: malformed comment or comment of another work item")


BLOCK_END = re.compile(r"<br\s*/?>|</(?:p|div|li|h[1-6]|pre|blockquote|tr|ul|ol)\s*>", re.I)


def text_of(markup):
    """Visible text, one line per block, whitespace collapsed. Derived here
    rather than from Plane's comment_stripped, whose block separators are not
    part of any contract; it is also what read-back compares, so attribute
    rewrites and entity re-encoding by Plane's sanitizer do not count."""
    flat = html.unescape(re.sub(r"<[^>]*>", "", BLOCK_END.sub("\n", re.sub(r"\s+", " ", markup))))
    return "\n".join(" ".join(line.split()) for line in flat.splitlines() if line.strip())


def marker_pr(c):
    """The PR named by the comment's leading marker line, or None. Only HTML's
    own whitespace is collapsed here: a marker spelled with a non-breaking or
    other Unicode space, or a noncanonical number, is not one."""
    flat = html.unescape(re.sub(r"<[^>]*>", "", BLOCK_END.sub("\n", re.sub(r"[ \t\n\r\f]+", " ", c["comment_html"]))))
    first = next((line.strip(" ") for line in flat.split("\n") if line.strip()), "")
    m = CANONICAL_MARKER.match(first)
    return int(m.group(1)) if m else None


def matches(comments, pr):
    """Every comment that carries this PR's marker anywhere. A marker that is
    not the comment's canonical leading line, or that spells this PR in any
    other way (#0554, # 554, a non-breaking space), is never a safe target, so
    it makes the selection ambiguous instead of being ignored."""
    want = marker(pr)
    hit = []
    for c in comments:
        if marker_pr(c) == pr:
            hit.append(c)
        elif any(int(m.group(1)) == pr for m in LOOSE_MARKER.finditer(text_of(c["comment_html"]))):
            raise Refused(f"AMBIGUOUS_MARKER: comment {c['id']} names {want} outside a canonical leading marker line")
    return hit


def prepare_body(text, pr):
    """The HTML sent to Plane: the caller's body behind the marker paragraph."""
    if len(text.encode()) > MAX_BODY:
        raise Refused(f"BODY: over {MAX_BODY} bytes")
    head = f"<p><code>{marker(pr)}</code></p>"
    body = text.strip()
    if body.startswith(head):
        body = body[len(head):].lstrip()
    if "codegraph-coordinator-sync/" in body or "codegraph-coordinator-sync/" in text_of(body):
        raise Refused("BODY: carries a coordinator marker of its own")
    if not body:
        raise Refused("BODY: empty")
    return head + body


class Ledger:
    """Append-only JSON lines, mode 0600. append() returns only once the line
    and the directory entry are flushed (see flush), and raises otherwise."""

    def __init__(self, state, redact=lambda s: s):
        self.path, self.redact = state / "ledger.jsonl", redact

    def events(self):
        try:
            fd = os.open(self.path, os.O_RDONLY | os.O_NOFOLLOW)
        except FileNotFoundError:
            return []
        except OSError as e:
            raise Refused(f"LEDGER: {self.path} unreadable ({errno.errorcode.get(e.errno, e.errno)})")
        with os.fdopen(fd, "rb") as f:
            st = os.fstat(f.fileno())
            if not stat.S_ISREG(st.st_mode) or st.st_mode & 0o077 or st.st_uid != os.getuid():
                raise Refused(f"LEDGER: {self.path} must be a regular file of this user with mode 0600")
            raw = f.read(MAX_LEDGER + 1)
        if len(raw) > MAX_LEDGER:
            raise Refused(f"LEDGER: over {MAX_LEDGER} bytes")
        out = []
        try:
            lines = raw.decode().splitlines()
        except UnicodeDecodeError:
            raise Refused("LEDGER: not UTF-8")
        for n, line in enumerate(lines, 1):
            try:
                e = json.loads(line)
            except ValueError:
                e = None
            if not isinstance(e, dict) or e.get("type") not in ("RECONSTRUCTED", "ATTEMPT", "SYNCED",
                                                                 "UNSYNCHRONIZED"):
                raise Refused(f"LEDGER: line {n} malformed")
            out.append(e)
        return out

    def append(self, event):
        data = (self.redact(json.dumps({"utc": utc(), **event}, sort_keys=True)) + "\n").encode()
        fd = os.open(self.path, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            if os.write(fd, data) != len(data):
                raise OSError(errno.EIO, "short ledger write")
            flush(fd)
        finally:
            os.close(fd)
        # Every time, not just on create: an earlier append may have created the
        # file and then failed before its directory entry was flushed.
        flush_dir(self.path.parent)

    def known_comment(self, ticket, pr):
        cid = None
        for e in self.events():
            rows = e.get("comments", []) if e["type"] == "RECONSTRUCTED" else [e]
            for r in rows:
                if r.get("ticket") == ticket and r.get("pr") == pr and r.get("comment_id"):
                    cid = r["comment_id"]
        return cid

    def unresolved_create(self, ticket, pr):
        """utc of the last create attempt not yet followed by SYNCED, or None."""
        when = None
        for e in self.events():
            if e.get("ticket") == ticket and e.get("pr") == pr:
                if e["type"] == "ATTEMPT" and e.get("action") == "create":
                    when = e.get("utc")
                elif e["type"] == "SYNCED":
                    when = None
        return when

    def pending(self):
        last = {}
        for e in self.events():
            if e["type"] != "RECONSTRUCTED":
                last[(e.get("ticket"), e.get("pr"))] = e
        return {k: e for k, e in last.items() if e["type"] != "SYNCED"}


def lock(state):
    missing = [p for p in state.parents if not p.exists()]
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    # The state directory's own entry on every run, as in Ledger.append.
    for p in (state, *missing):
        flush_dir(p.parent)
    st = os.stat(state, follow_symlinks=False)
    if not stat.S_ISDIR(st.st_mode) or st.st_mode & 0o077 or st.st_uid != os.getuid():
        raise Refused(f"LEDGER: {state} must be a directory of this user with mode 0700")
    fd = os.open(state / "plane-sync.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        os.close(fd)
        raise Locked(f"another plane-sync holds {state / 'plane-sync.lock'}")
    return fd


def check_pr_maps_to_ticket(pr, ticket, gh):
    info = gh(pr)
    if not isinstance(info, dict) or info.get("number") != pr:
        raise Refused(f"MAPPING: gh did not return {REPO_SLUG}#{pr}")
    text = " ".join(str(info.get(k) or "") for k in ("title", "body", "headRefName"))
    if not re.search(rf"(?<![A-Za-z0-9]){re.escape(ticket)}(?![0-9])", text):
        raise Refused(f"MAPPING: {REPO_SLUG}#{pr} does not name {ticket} in its title, body or branch")


def parse_target(ticket, pr):
    if not TICKET.match(ticket or ""):
        raise Refused(f"USAGE: ticket must look like CG-150, got {ticket!r}")
    if not PR_NUMBER.match(str(pr)):
        raise Refused(f"USAGE: PR must be a positive number, got {pr!r}")
    return ticket, int(pr)


def select(plane, ledger, ticket, pr, expect):
    """(work item id, the one existing comment or None), failing closed."""
    item = plane.work_item(ticket)
    hit = matches(plane.comments(item), pr)
    # An owner-given id that Plane confirms supersedes the one the ledger pinned.
    label, cid = ("--comment-id", expect) if expect else ("the ledger", ledger.known_comment(ticket, pr))
    if cid and [c["id"] for c in hit] != [cid]:
        raise Refused(f"AMBIGUOUS_MARKER: {label} names {cid}, Plane has "
                      f"{[c['id'] for c in hit] or 'no'} marker comment for {ticket} #{pr}")
    if len(hit) > 1:
        raise Refused(f"AMBIGUOUS_MARKER: {len(hit)} comments carry {marker(pr)}: {[c['id'] for c in hit]}")
    if hit and (hit[0].get("parent_id") or hit[0].get("hidden_at")):
        raise Refused(f"AMBIGUOUS_MARKER: comment {hit[0]['id']} is a reply or hidden")
    return item, (hit[0] if hit else None)


def read_body(args, pr):
    if args.comment_id and not is_uuid(args.comment_id):
        raise Refused("USAGE: --comment-id must be a lowercase UUID")
    try:
        text = Path(args.body).read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as e:
        raise Refused(f"BODY: {args.body} unreadable ({type(e).__name__})")
    return prepare_body(text, pr)


def plan(cur, body):
    if cur is None:
        return "create"
    return "noop" if text_of(cur["comment_html"]) == text_of(body) else "update"


def parse_utc(value):
    # Python 3.9's fromisoformat takes only 3- or 6-digit fractions.
    text = re.sub(r"\.(\d+)", lambda m: "." + (m.group(1) + "000000")[:6], str(value).replace("Z", "+00:00"))
    try:
        t = datetime.fromisoformat(text)
    except ValueError:
        return None
    return t if t.tzinfo else None


def cmd_sync(args, plane, gh, out):
    ticket, pr = parse_target(args.ticket, args.pr)
    ledger = Ledger(STATE_DIR, plane.redact)
    if args.dry_run:
        body = read_body(args, pr)
        check_pr_maps_to_ticket(pr, ticket, gh)
        _, cur = select(plane, ledger, ticket, pr, args.comment_id)
        action = plan(cur, body).upper() + ("" if cur is None else f" {cur['id']}")
        out(f"plane-sync: DRY-RUN {ticket} #{pr}: would {action} (body sha256 {sha256(body)}); nothing written")
        return 0
    fd = lock(STATE_DIR)
    try:
        ledger.events()
        record = {"ticket": ticket, "pr": pr}
        cid = None
        try:
            body = read_body(args, pr)
            record["body_sha256"] = sha256(body)
            check_pr_maps_to_ticket(pr, ticket, gh)
            item, cur = select(plane, ledger, ticket, pr, args.comment_id)
            cid = cur and cur["id"]
            action = plan(cur, body)
            pending = ledger.unresolved_create(ticket, pr)
            if action == "create" and pending:
                age = (datetime.now(timezone.utc) - (parse_utc(pending) or datetime.now(timezone.utc))).total_seconds()
                if age < CREATE_GRACE:
                    raise Unsynced(f"CREATE_PENDING: the create attempted at {pending} has no comment on Plane "
                                   f"yet; not creating again before {CREATE_GRACE} s have passed")
            ledger.append({"type": "ATTEMPT", **record, "comment_id": cid, "action": action})
            if action == "create":
                cid = create(plane, item, pr, body)
            elif action == "update":
                plane.call("PATCH", f"projects/{PROJECT}/work-items/{item}/comments/{cid}/", {"comment_html": body})
            back = plane.comment(item, cid)
            if marker_pr(back) != pr:
                raise Unsynced(f"READBACK: comment {cid} no longer leads with {marker(pr)}")
            if text_of(back["comment_html"]) != text_of(body):
                raise Unsynced(f"READBACK: comment {cid} text sha256 {sha256(text_of(back['comment_html']))} "
                               f"!= sent {sha256(text_of(body))}")
            if action == "update":
                before, after = parse_utc(cur.get("updated_at")), parse_utc(back.get("updated_at"))
                if before is None or after is None or after <= before:
                    raise Unsynced(f"READBACK: comment {cid} updated_at did not advance")
        except Exception as e:
            error = str(e) if isinstance(e, (Refused, Unsynced)) else f"internal error ({type(e).__name__})"
            try:
                ledger.append({"type": "UNSYNCHRONIZED", **record, "comment_id": cid,
                               "error": ledger.redact(error)[:300]})
            except Exception as le:
                raise (Refused if isinstance(e, Refused) else Unsynced)(
                    f"LEDGER: the UNSYNCHRONIZED event could not be written ({type(le).__name__}); {error}") from None
            raise
        try:
            ledger.append({"type": "SYNCED", **record, "comment_id": cid, "action": action,
                           "updated_at": back.get("updated_at"), "readback": "ok"})
        except OSError as e:
            raise Unsynced(f"LEDGER: comment {cid} read back, but the SYNCED event could not be written "
                           f"({type(e).__name__}); a retry records it") from None
        out(f"plane-sync: SYNCED {ticket} #{pr} comment {cid} ({action}, read back)")
        return 0
    finally:
        os.close(fd)


def create(plane, item, pr, body):
    """POST is not retried: when its outcome is unknown, Plane is re-read and
    the comment it may have made is adopted instead of made twice."""
    try:
        made = plane.call("POST", f"projects/{PROJECT}/work-items/{item}/comments/",
                          {"comment_html": body, "access": "INTERNAL"})
    except Unsynced:
        hit = matches(plane.comments(item), pr)
        if len(hit) == 1:
            return hit[0]["id"]
        raise
    check_comment(made, item)
    return made["id"]


def cmd_status(args, plane, gh, out):
    ledger = Ledger(STATE_DIR)
    if args.ticket is None:
        pending = ledger.pending()
        for (ticket, pr), e in sorted(pending.items(), key=lambda kv: str(kv[0])):
            out(f"plane-sync: PENDING {ticket} #{pr}: last event {e['type']} at {e.get('utc')}")
        out(f"plane-sync: ledger {ledger.path}: {len(ledger.events())} events, {len(pending)} pending")
        return 1 if pending else 0
    if args.pr is None:
        raise Refused("USAGE: status takes no arguments or CG-N PR")
    ticket, pr = parse_target(args.ticket, args.pr)
    check_pr_maps_to_ticket(pr, ticket, gh)
    item, cur = select(plane, ledger, ticket, pr, args.comment_id)
    where = f"comment {cur['id']} updated {cur.get('updated_at')}" if cur else "no marker comment yet"
    out(f"plane-sync: STATUS {ticket} #{pr}: work item {item}; {where}; "
        f"ledger knows {ledger.known_comment(ticket, pr) or 'nothing'}")
    return 0


def cmd_bootstrap(args, plane, gh, out):
    """One RECONSTRUCTED line built from a live read: a point-in-time
    snapshot, never history. Refused once the ledger has any event."""
    for t in args.tickets:
        if not TICKET.match(t):
            raise Refused(f"USAGE: ticket must look like CG-150, got {t!r}")
    tickets = list(dict.fromkeys(args.tickets))
    rows = []
    for ticket in tickets:
        item = plane.work_item(ticket)
        by_pr = {}
        for c in plane.comments(item):
            pr = marker_pr(c)
            if pr is not None:
                by_pr.setdefault(pr, []).append(c)
        for pr, cs in sorted(by_pr.items()):
            if len(cs) != 1:
                raise Refused(f"AMBIGUOUS_MARKER: {len(cs)} comments for {ticket} #{pr}")
            rows.append({"ticket": ticket, "pr": pr, "comment_id": cs[0]["id"],
                         "updated_at": cs[0].get("updated_at"), "body_sha256": sha256(cs[0]["comment_html"])})
    event = {"type": "RECONSTRUCTED", "source": "plane-cloud v2 read-back",
             "note": "point-in-time snapshot; no prior event history exists", "comments": rows}
    if args.dry_run:
        out(f"plane-sync: DRY-RUN bootstrap would record {len(rows)} comments; nothing written")
        return 0
    fd = lock(STATE_DIR)
    try:
        ledger = Ledger(STATE_DIR, plane.redact)
        if ledger.events():
            raise Refused(f"LEDGER: {ledger.path} already has events; bootstrap runs once")
        ledger.append(event)
    finally:
        os.close(fd)
    out(f"plane-sync: RECONSTRUCTED snapshot of {len(rows)} comments written to {Ledger(STATE_DIR).path}")
    return 0


def parser():
    p = argparse.ArgumentParser(prog="plane_sync.py")
    sub = p.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("status")
    s.add_argument("ticket", nargs="?")
    s.add_argument("pr", nargs="?")
    s.add_argument("--comment-id")
    s = sub.add_parser("sync")
    s.add_argument("ticket")
    s.add_argument("pr")
    s.add_argument("body")
    s.add_argument("--comment-id")
    s.add_argument("--dry-run", action="store_true")
    s = sub.add_parser("bootstrap")
    s.add_argument("tickets", nargs="+")
    s.add_argument("--dry-run", action="store_true")
    return p


def main(argv=None, transport=urllib_transport, key=keychain_key, gh=gh_pr, sleep=time.sleep,
         out=print, err=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if argv and TICKET.match(argv[0]):
        argv.insert(0, "sync")
    err = err or (lambda s: print(s, file=sys.stderr))
    secret = None
    try:
        args = parser().parse_args(argv)
    except SystemExit as e:
        return 2 if e.code else 0

    def safe(s):
        return redact(s, secret) if secret else s

    try:
        if args.cmd == "status" and args.ticket is None:
            return cmd_status(args, None, gh, lambda s: out(safe(s)))
        secret = key()
        plane = Plane(secret, transport, sleep)
        return {"status": cmd_status, "sync": cmd_sync, "bootstrap": cmd_bootstrap}[args.cmd](
            args, plane, gh, lambda s: out(safe(s)))
    except Locked as e:
        err(safe(f"plane-sync: LOCKED: {e}"))
        return 4
    except Refused as e:
        err(safe(f"plane-sync: REFUSED: {e}"))
        return 2 if str(e).startswith("USAGE") else 3
    except Unsynced as e:
        err(safe(f"plane-sync: UNSYNCHRONIZED: {e}"))
        return 1
    except OSError as e:
        err(safe(f"plane-sync: UNSYNCHRONIZED: local I/O error ({type(e).__name__})"))
        return 1
    except Exception as e:
        # Never a traceback: it could quote the key.
        err(f"plane-sync: UNSYNCHRONIZED: internal error ({type(e).__name__})")
        return 1


if __name__ == "__main__":
    sys.exit(main())
