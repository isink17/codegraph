"""Display (r3 §7.3 + review N3, R5-1 §R4.3). render() is a function of the
document bytes alone; every value is escaped once with the injective esc()."""

import hashlib
import re

from . import protocol

HEADER = "CODEGRAPH OWNER AUTHORIZATION v1"
TRUNCATE = 120
UNPROVEN = ("UNPROVEN: consistent with an interrupted append; cannot be distinguished from corruption or "
            "tampering. These bytes contain no complete record. They will be preserved as evidence and "
            "chained into the log.")
CONSTANT_ORDER = ("v", "doc", "entry", "verb", "class", "trust", "mode", "slot", "op_id", "transition",
                  "identity_kind", "identity", "slot_generation", "seq")
UNTRUSTED_LAST = ("reason", "task", "attest")
_HEX64 = re.compile(r"^[0-9a-f]{64}\Z")
_ESCAPE = re.compile(r'\\x[0-9a-f]{2}|\\\\|\\"|.', re.S)


def esc(data):
    if type(data) is str:
        data = data.encode("utf-8", "surrogatepass")
    out = []
    for c in data:
        if c == 0x5C:
            out.append("\\\\")
        elif c == 0x22:
            out.append('\\"')
        elif 0x20 <= c <= 0x7E:
            out.append(chr(c))
        else:
            out.append(f"\\x{c:02x}")
    return "".join(out)


def unesc(text):
    out = bytearray()
    for tok in _ESCAPE.findall(text):
        if tok == "\\\\":
            out.append(0x5C)
        elif tok == '\\"':
            out.append(0x22)
        elif tok.startswith("\\x") and len(tok) == 4:
            out.append(int(tok[2:], 16))
        elif len(tok) == 1 and 0x20 <= ord(tok) <= 0x7E and tok not in '\\"':
            out.append(ord(tok))
        else:
            raise ValueError("not an esc() string")
    return bytes(out)


def _quoted(value):
    raw = value.encode("ascii") if type(value) is str else value
    e = esc(raw)
    if len(e) <= TRUNCATE:
        return f'"{e}"'
    cut, n = [], 0
    for tok in _ESCAPE.findall(e):
        if n + len(tok) > TRUNCATE:
            break
        cut.append(tok)
        n += len(tok)
    return f'"{"".join(cut)}..." [truncated: {len(raw)} bytes, sha256 {hashlib.sha256(raw).hexdigest()[:16]}]'


def _flatten(prefix, value, out):
    if type(value) is dict:
        for k in sorted(value):
            _flatten(f"{prefix}.{k}" if prefix else k, value[k], out)
    elif type(value) is list:
        if not value:
            out.append((prefix, "[]"))
        for i, v in enumerate(value):
            _flatten(f"{prefix}.{i}", v, out)
    elif type(value) is bool:
        out.append((prefix, "true" if value else "false"))
    elif type(value) is int:
        out.append((prefix, str(value)))
    else:
        out.append((prefix, _quoted(value)))


def _repair_lines(plan):
    lines = []
    for item in plan:
        fields = []
        _flatten("", item, fields)
        lines.append("REPAIR " + " ".join(f"{k}={v}" for k, v in fields))
        if item.get("action") == "trim":
            lines.append(UNPROVEN)
    return lines


def render(doc_bytes):
    """Returns (text, sha256 of doc_bytes). Fixed order: header, repair plan,
    broker constants, digests, other fields, untrusted fields last, nonce line."""
    doc = protocol.parse_doc(doc_bytes)
    digest = hashlib.sha256(doc_bytes).hexdigest()
    lines = [HEADER]
    lines += _repair_lines(doc.get("repair_plan", []))
    groups = ([], [], [], [])
    for key in sorted(doc, key=lambda k: (CONSTANT_ORDER.index(k) if k in CONSTANT_ORDER else 99, k)):
        if key == "repair_plan":
            continue
        fields = []
        _flatten(key, doc[key], fields)
        top = key in CONSTANT_ORDER
        for label, shown in fields:
            hexval = shown.startswith('"') and _HEX64.match(shown[1:-1] or "")
            group = 3 if key in UNTRUSTED_LAST else 0 if top else 1 if hexval else 2
            groups[group].append(f"{label.upper()} = {shown}")
    for g in groups:
        lines += g
    lines.append(f"DOCUMENT SHA256 = {digest}")
    lines.append(nonce_prompt(digest))
    return "\n".join(lines) + "\n", digest


def nonce_prompt(digest):
    return f"CONFIRM {digest}: type the nonce shown for this document"
