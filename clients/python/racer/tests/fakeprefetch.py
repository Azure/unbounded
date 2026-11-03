# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""In-memory prefetch tickets for the fake Racer (designs/racer-prefetch.md).

This mirrors pkg/racersdk/internal/fakeracer/prefetch.go and follows the
Rust server's client-socket contract (validation, error codes, idempotency,
expiry, retention, and JSON shapes). It has no pages, owners, or scheduling:
refs move from pending to an outcome as the outcome hook decides, and an
unsettled ticket reports RUNNING, never QUEUED.
"""

from __future__ import annotations

import hashlib
import json
import re
import threading
import time
from dataclasses import dataclass, field
from urllib.parse import parse_qs

PATH = "/v1/prefetches"
PAGE = 16 * 1024 * 1024
MAX_COUNTER = (1 << 53) - 1
_ID = re.compile(r"[A-Za-z0-9._-]{1,128}")
_KEY = re.compile(r"[0-9a-f]{64}")
_WAIT_PART = re.compile(r"(\d*)(?:\.(\d*))?(ns|us|\u00b5s|\u03bcs|ms|s|m|h)")
_WAIT_UNITS = {"ns": 1, "us": 1_000, "\u00b5s": 1_000, "\u03bcs": 1_000, "ms": 1_000_000, "s": 10**9, "m": 60 * 10**9, "h": 3600 * 10**9}
_CURSOR = re.compile(r"[A-Za-z0-9._-]{1,256}")
_TOP = {"cache_uid", "priority", "generation", "sequence", "ttl_seconds", "max_pending_bytes", "refs"}
_REF = {"key", "etag", "offset", "length", "metadata"}


class Fail(Exception):
    def __init__(self, status, code, message="", retry_after=None):
        super().__init__(message)
        self.status, self.code, self.message, self.retry_after = status, code, message, retry_after


def _bad(message):
    return Fail(400, "invalid_request", message)


@dataclass
class Outcome:
    """Scripted result of one ref. The default succeeds."""

    pending: bool = False
    error: str | None = None
    etag: str | None = None
    size: int = 0
    # empty marks a zero-length object: a ref with offset 0 and no length
    # succeeds with no pages, any other ref fails invalid_range.
    empty: bool = False


@dataclass
class _Ref:
    ref: dict
    outcome: str = "pending"
    error: str | None = None
    etag: str | None = None
    length: int = 0


@dataclass
class _Ticket:
    id: str
    created: float
    priority: str = "normal"
    generation: int = 0
    sequence: int = 0
    hash: bytes = b""
    ttl: int = 0
    updated: float = 0.0
    expires: float = 0.0
    canceled: str | None = None
    # released: canceled or expired; outcomes are frozen until remove_at.
    released: bool = False
    remove_at: float = 0.0
    refs: list = field(default_factory=list)


def _uint(v):
    return isinstance(v, int) and not isinstance(v, bool) and v >= 0


class FakePrefetches:
    """Ticket registry. Tests set outcome(ticket_id, ref_dict) -> Outcome and
    admit(ticket_id, body) -> Fail | None, and may replace now()."""

    def __init__(self) -> None:
        self.cache_uid = None
        self.passthrough = False
        self.max_body = 4194304
        self.max_refs = 16384
        self.max_tickets = 1024
        self.max_ttl = 3600
        self.max_wait = 60.0
        self.retention = 300.0
        self.refs_page = 256
        self.outcome = None
        self.admit = None
        self.now = time.time
        self.tickets: dict[str, _Ticket] = {}
        self.lock = threading.Lock()

    # HTTP --------------------------------------------------------------

    def handle(self, method, target, headers, body):
        """Return (status, [(name, value)], body bytes)."""
        path, _, query = target.partition("?")
        try:
            status, data = self._route(method, path, query, headers, body)
            extra = []
        except Fail as f:
            status, data = f.status, {"code": f.code, "message": f.message}
            extra = []
            if f.status == 405:
                extra.append(("Allow", "GET" if path == PATH else "GET, PUT, DELETE"))
            if f.retry_after:
                extra.append(("Retry-After", str(f.retry_after)))
        raw = json.dumps(data).encode()
        # Racer closes the connection after every prefetch response.
        headers = [("Content-Type", "application/json"), ("Content-Length", str(len(raw))), ("Connection", "close")]
        return status, extra + headers, raw

    def _route(self, method, path, query, headers, body):
        if path == PATH:
            if method != "GET":
                raise Fail(405, "method_not_allowed", "method not allowed")
            self._no_body(headers, body)
            if query:
                raise _bad("list takes no parameters")
            return 200, self._list()
        if method not in ("GET", "PUT", "DELETE"):
            raise Fail(405, "method_not_allowed", "method not allowed")
        pid = path[len(PATH) + 1 :]
        if not _ID.fullmatch(pid) or pid in (".", ".."):
            raise _bad("invalid id")
        if method == "PUT":
            return self._put(pid, query, headers, body)
        self._no_body(headers, body)
        if method == "DELETE":
            if query:
                raise _bad("cancel takes no parameters")
            return self._cancel(pid)
        return self._status(pid, query)

    @staticmethod
    def _no_body(headers, body):
        if body or headers.get("content-length", "0") != "0" or "transfer-encoding" in headers:
            raise _bad("unexpected body")
        if "authorization" in headers:
            raise _bad("authorization is accepted on PUT only")

    # PUT ---------------------------------------------------------------

    def _put(self, pid, query, headers, body):
        for h in ("transfer-encoding", "expect", "content-encoding"):
            if h in headers:
                raise _bad("unsupported body framing")
        if query:
            raise _bad("PUT takes no parameters")
        if headers.get("content-type", "").split(";")[0].strip().lower() != "application/json":
            raise _bad("content type must be application/json")
        declared = headers.get("content-length", "")
        if not declared.isdigit() or int(declared) == 0 or not body:
            raise _bad("missing body")
        if int(declared) > self.max_body:
            raise Fail(413, "too_large", "body too large")
        # Racer rejects a passthrough cache before it parses the body.
        if self.passthrough:
            raise Fail(409, "passthrough", "cache is passthrough")
        request, canonical = self._decode(body)
        if self.cache_uid and request.get("cache_uid") and request["cache_uid"] != self.cache_uid:
            raise Fail(409, "cache_uid_mismatch", "cache uid mismatch")
        # Like Racer, the hash covers the body as sent (absent fields omitted,
        # defaults not applied) plus the presence of Authorization.
        auth = "authorization" in headers
        digest = hashlib.sha256(canonical + (b"\1" if auth else b"\0")).digest()
        g, s = request["generation"], request["sequence"]
        with self.lock:
            now = self.now()
            self._gc(now)
            t = self.tickets.get(pid)
            if t is not None:
                if g < t.generation:
                    raise Fail(409, "stale_generation", f"generation {g} is older than current {t.generation}")
                if g == t.generation and s < t.sequence:
                    raise Fail(409, "stale_sequence", f"sequence {s} is older than current {t.sequence}")
                if (g, s) == (t.generation, t.sequence):
                    if digest != t.hash:
                        raise Fail(409, "payload_mismatch", f"generation {g} sequence {s} was submitted with another payload")
                    # A replay renews the TTL of a live ticket only.
                    if not t.released:
                        t.expires = now + t.ttl
                    return 200, self._render(t, {})
            if self.admit:
                fail = self.admit(pid, request)
                if fail:
                    raise fail
            code = 200
            if t is None:
                if len(self.tickets) >= self.max_tickets and not self._evict_oldest_released():
                    raise Fail(429, "ticket_limit", "node prefetch ticket limit reached", 5)
                t = _Ticket(pid, now)
                self.tickets[pid] = t
                code = 201
            t.priority, t.generation, t.sequence, t.hash = request["priority"], g, s, digest
            t.ttl, t.updated, t.expires, t.canceled = request["ttl_seconds"], now, now + request["ttl_seconds"], None
            t.released, t.remove_at = False, 0.0
            t.refs = [_Ref(r, etag=r.get("etag"), length=r.get("length", 0)) for r in request["refs"]]
            return code, self._render(t, {})

    def _evict_oldest_released(self):
        """Drop the retained ticket due for removal first, as Racer does
        before answering 429 ticket_limit."""
        released = [t for t in self.tickets.values() if t.released]
        if not released:
            return False
        del self.tickets[min(released, key=lambda t: t.remove_at).id]
        return True

    def _decode(self, body):
        try:
            r = json.loads(body)
        except ValueError:
            raise _bad("invalid JSON") from None
        if not isinstance(r, dict) or set(r) - _TOP:
            raise _bad("unknown field")
        if not isinstance(r.get("refs"), list) or any(not isinstance(ref, dict) for ref in r["refs"]):
            raise _bad("invalid refs")
        # Canonical form: present fields in Racer's field order, compact.
        canonical = json.dumps(
            {k: r[k] for k in ("cache_uid", "priority", "generation", "sequence", "ttl_seconds", "max_pending_bytes") if k in r}
            | {"refs": [{k: ref[k] for k in ("key", "etag", "offset", "length", "metadata") if k in ref} for ref in r["refs"]]},
            separators=(",", ":"),
        ).encode()
        r = json.loads(body)
        refs = r.get("refs")
        if not isinstance(refs, list) or not refs:
            raise _bad("refs must not be empty")
        if len(refs) > self.max_refs:
            raise Fail(413, "too_large", "too many refs")
        if "cache_uid" in r and (not isinstance(r["cache_uid"], str) or not 0 < len(r["cache_uid"]) <= 64):
            raise _bad("invalid cache_uid")
        r.setdefault("priority", "normal")
        if r["priority"] not in ("high", "normal", "low"):
            raise _bad("invalid priority")
        for name in ("generation", "sequence"):
            r.setdefault(name, 0)
            if not _uint(r[name]) or r[name] > MAX_COUNTER:
                raise _bad("counter out of range")
        ttl = r.setdefault("ttl_seconds", 600)
        if not _uint(ttl) or ttl == 0:
            raise _bad("invalid ttl_seconds")
        r["ttl_seconds"] = min(ttl, self.max_ttl)
        if not _uint(r.setdefault("max_pending_bytes", 0)):
            raise _bad("invalid max_pending_bytes")
        for ref in refs:
            if not isinstance(ref, dict) or set(ref) - _REF or not isinstance(ref.get("key"), str) or not _KEY.fullmatch(ref["key"]):
                raise _bad("invalid ref")
            if "length" in ref and (not _uint(ref["length"]) or ref["length"] == 0):
                raise _bad("invalid length")
            if not _uint(ref.setdefault("offset", 0)):
                raise _bad("invalid offset")
            if "etag" in ref and not (isinstance(ref["etag"], str) and len(ref["etag"]) >= 2 and ref["etag"][0] == ref["etag"][-1] == '"'):
                raise _bad("invalid etag")
            if "metadata" in ref and not _opaque(ref["metadata"]):
                raise _bad("invalid metadata")
        return r, canonical

    # Tickets -----------------------------------------------------------

    def _gc(self, now):
        """TTL expiry like Racer: an unsettled ticket becomes CANCELLED
        (expired), a settled one keeps its outcome; retention counts from
        expiry or cancel."""
        for pid, t in list(self.tickets.items()):
            if not t.released and now >= t.expires:
                self._evaluate(t)
                if not self._settled(t):
                    t.canceled, t.updated = "expired", t.expires
                self._release(t, t.expires)
            if t.released and now >= t.remove_at:
                del self.tickets[pid]

    def _release(self, t, at):
        if not t.released:
            t.released, t.remove_at = True, at + self.retention

    def _evaluate(self, t):
        if t.released:
            return
        for r in t.refs:
            if r.outcome != "pending":
                continue
            o = self.outcome(t.id, r.ref) if self.outcome else Outcome()
            if o.pending:
                continue
            if r.etag is None and o.etag:
                r.etag = o.etag
            off = r.ref.get("offset", 0)
            error = o.error
            if o.empty:
                r.length = 0
                if off or "length" in r.ref:
                    error = error or "invalid_range"
            elif o.size:
                if off >= o.size:
                    error = error or "invalid_range"
                else:
                    r.length = min(r.length or o.size - off, o.size - off)
            r.outcome, r.error = ("failed", error) if error else ("succeeded", None)

    @staticmethod
    def _settled(t):
        return t.released or all(r.outcome != "pending" for r in t.refs)

    def _render(self, t, q):
        self._evaluate(t)
        settled = self._settled(t)
        b = dict.fromkeys(
            ("logical_requested", "aligned_requested", "already_resident", "queued", "in_flight", "joined", "fetched_from_origin", "admitted", "failed"),
            0,
        )
        counts = {"succeeded": 0, "failed": 0, "pending": 0}
        pages = set()
        for r in t.refs:
            b["logical_requested"] += r.length
            aligned = 0
            off = r.ref.get("offset", 0)
            if r.length:
                for p in range(off // PAGE, (off + r.length - 1) // PAGE + 1):
                    if (r.ref["key"], p) not in pages:
                        pages.add((r.ref["key"], p))
                        aligned += PAGE
            b["aligned_requested"] += aligned
            counts[r.outcome] += 1
            if r.outcome == "succeeded":
                b["admitted"] += aligned
                b["fetched_from_origin"] += aligned
            elif r.outcome == "failed":
                b["failed"] += aligned
            elif not t.released:
                b["in_flight"] += aligned
        if t.canceled is not None:
            state = "CANCELLED"
        elif not settled:
            state = "RUNNING"
        elif counts["failed"] == 0:
            state = "SUCCEEDED"
        elif counts["failed"] == len(t.refs):
            state = "FAILED"
        else:
            state = "PARTIAL"
        out = {
            "id": t.id,
            "cache_uid": self.cache_uid or "",
            "state": state,
            "reason": t.canceled,
            "priority": t.priority,
            "generation": t.generation,
            "sequence": t.sequence,
            "settled": settled,
            "ttl_seconds": t.ttl,
            "expires_at_ms": int(t.expires * 1000),
            "created_at_ms": int(t.created * 1000),
            "updated_at_ms": int(t.updated * 1000),
            "refs_total": len(t.refs),
            "refs_succeeded": counts["succeeded"],
            "refs_failed": counts["failed"],
            "refs_pending": counts["pending"],
            "bytes": b,
        }
        if q.get("coverage"):
            out["coverage"] = {"resident_bytes": b["admitted"], "observed_at_ms": int(self.now() * 1000)}
        if q.get("refs"):
            # Like Racer, the cursor is an offset into the filtered refs.
            start = q.get("start", 0)
            selected = [(i, r) for i, r in enumerate(t.refs) if q["refs"] == "all" or r.outcome == "failed"]
            page = selected[start : start + self.refs_page]
            items = []
            for i, r in page:
                item = {"index": i, "key": r.ref["key"], "etag": r.etag, "offset": r.ref.get("offset", 0), "length": r.length, "outcome": r.outcome}
                if r.error:
                    item["error"] = r.error
                items.append(item)
            more = len(selected) > start + self.refs_page
            out["refs"] = items
            out["next_cursor"] = f"{t.generation}.{t.sequence}.{start + self.refs_page}" if more else None
        return out

    def _status(self, pid, query):
        q = self._query(query)
        deadline = time.monotonic() + q.get("wait", 0.0)
        while True:
            with self.lock:
                self._gc(self.now())
                t = self.tickets.get(pid)
                if t is None:
                    raise Fail(404, "not_found", f"no prefetch {pid}")
                if "cursor" in q:
                    g, s, n = q["cursor"]
                    if (g, s) != (t.generation, t.sequence):
                        raise _bad("cursor belongs to another generation or sequence")
                    q["start"] = n
                self._evaluate(t)
                if self._settled(t) or time.monotonic() >= deadline or not q.get("wait"):
                    return 200, self._render(t, q)
            time.sleep(0.005)

    def _query(self, raw):
        q = {}
        try:
            values = parse_qs(raw, keep_blank_values=True, strict_parsing=bool(raw))
        except ValueError:
            raise _bad("invalid query") from None
        for name, v in values.items():
            if len(v) != 1:
                raise _bad("duplicate parameter")
            v = v[0]
            if name == "wait":
                seconds = _parse_wait(v)
                if seconds is None:
                    raise _bad("invalid wait")
                if seconds > self.max_wait:
                    raise _bad("wait too long")
                q["wait"] = seconds
            elif name == "coverage":
                if v not in ("0", "1", "true", "false"):
                    raise _bad("invalid coverage")
                q["coverage"] = v in ("1", "true")
            elif name == "refs":
                if v not in ("failed", "all"):
                    raise _bad("invalid refs")
                q["refs"] = v
            elif name == "cursor":
                parts = v.split(".")
                if not _CURSOR.fullmatch(v) or len(parts) < 3 or not all(x.isdigit() for x in parts[:3]):
                    raise _bad("invalid cursor")
                g, s, n = (int(x) for x in parts[:3])
                q["cursor"] = (g, s, n)
            else:
                raise _bad("unknown parameter")
        if "cursor" in q and "refs" not in q:
            raise _bad("cursor without refs")
        return q

    def _cancel(self, pid):
        with self.lock:
            now = self.now()
            self._gc(now)
            t = self.tickets.get(pid)
            if t is None:
                raise Fail(404, "not_found", f"no prefetch {pid}")
            # An already canceled ticket keeps its reason.
            if t.canceled is None:
                self._evaluate(t)
                t.canceled, t.updated = "client", now
                self._release(t, now)
            return 200, self._render(t, {})

    def _list(self):
        with self.lock:
            self._gc(self.now())
            out = []
            for pid in sorted(self.tickets)[:1024]:
                s = self._render(self.tickets[pid], {})
                out.append({k: s[k] for k in ("id", "state", "reason", "priority", "generation", "sequence", "settled", "expires_at_ms", "bytes")})
            return {"prefetches": out}


def _opaque(v):
    return isinstance(v, str) and 0 < len(v) <= 8192 and v[0] != " " and v[-1] != " " and all(ord(c) >= 0x20 and ord(c) != 0x7F for c in v)


def _parse_wait(v):
    """Parse a Go-style duration as Racer does; return seconds or None."""
    if v == "0":
        return 0.0
    if not v:
        return None
    nanos, pos = 0, 0
    while pos < len(v):
        m = _WAIT_PART.match(v, pos)
        if not m or (not m.group(1) and not m.group(2)):
            return None
        per = _WAIT_UNITS[m.group(3)]
        nanos += int(m.group(1) or 0) * per
        scale = per
        for digit in (m.group(2) or "")[:18]:
            scale //= 10
            nanos += int(digit) * scale
        pos = m.end()
    return nanos / 1e9
