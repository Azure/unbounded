# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""In-memory prefetch tickets for the fake Racer (designs/racer-prefetch.md).

This mirrors pkg/racersdk/internal/fakeracer/prefetch.go. It is not a
compatibility oracle: there are no pages, owners, or scheduling.
"""

from __future__ import annotations

import base64
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
_WAIT = re.compile(r"(\d+)(ms|s|m)")
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
        return status, extra + [("Content-Type", "application/json"), ("Content-Length", str(len(raw)))], raw

    def _route(self, method, path, query, headers, body):
        if path == PATH:
            if method != "GET":
                raise Fail(405, "invalid_request", "method not allowed")
            self._no_body(headers, body)
            if query:
                raise _bad("list takes no parameters")
            return 200, self._list()
        if method not in ("GET", "PUT", "DELETE"):
            raise Fail(405, "invalid_request", "method not allowed")
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
        request = self._decode(body)
        if self.passthrough:
            raise Fail(409, "passthrough", "cache is passthrough")
        if self.cache_uid and request.get("cache_uid") and request["cache_uid"] != self.cache_uid:
            raise Fail(409, "cache_uid_mismatch", "cache uid mismatch")
        if self.admit:
            fail = self.admit(pid, request)
            if fail:
                raise fail
        auth = "authorization" in headers
        digest = hashlib.sha256(json.dumps(request, sort_keys=True).encode() + (b"\1" if auth else b"\0")).digest()
        g, s = request["generation"], request["sequence"]
        with self.lock:
            now = self.now()
            self._gc(now)
            t = self.tickets.get(pid)
            code = 200
            if t is None:
                if len(self.tickets) >= self.max_tickets:
                    raise Fail(429, "ticket_limit", "ticket limit", 1)
                t = _Ticket(pid, now)
                self.tickets[pid] = t
                code = 201
            elif (g, s) == (t.generation, t.sequence):
                if digest != t.hash:
                    raise Fail(409, "payload_mismatch", f"payload differs at {g}/{s}")
                if t.canceled is None:
                    t.expires, t.updated = now + t.ttl, now
                return 200, self._render(t, {})
            elif g < t.generation:
                raise Fail(409, "stale_generation", f"generation {g} is older than current {t.generation}")
            elif g == t.generation and s < t.sequence:
                raise Fail(409, "stale_sequence", f"sequence {s} is older than current {t.sequence}")
            t.priority, t.generation, t.sequence, t.hash = request["priority"], g, s, digest
            t.ttl, t.updated, t.expires, t.canceled = request["ttl_seconds"], now, now + request["ttl_seconds"], None
            t.refs = [_Ref(r, etag=r.get("etag"), length=r.get("length", 0)) for r in request["refs"]]
            return code, self._render(t, {})

    def _decode(self, body):
        try:
            r = json.loads(body)
        except ValueError:
            raise _bad("invalid JSON") from None
        if not isinstance(r, dict) or set(r) - _TOP:
            raise _bad("unknown field")
        refs = r.get("refs")
        if not isinstance(refs, list) or not refs:
            raise _bad("refs must not be empty")
        if len(refs) > self.max_refs:
            raise Fail(413, "too_large", "too many refs")
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
        return r

    # Tickets -----------------------------------------------------------

    def _gc(self, now):
        for pid, t in list(self.tickets.items()):
            if t.canceled is None and now >= t.expires:
                t.canceled, t.updated = "expired", t.expires
            if t.canceled is not None and now >= t.updated + self.retention:
                del self.tickets[pid]

    def _evaluate(self, t):
        if t.canceled is not None:
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
            if o.size:
                if off >= o.size:
                    error = error or "invalid_range"
                else:
                    r.length = min(r.length or o.size - off, o.size - off)
            r.outcome, r.error = ("failed", error) if error else ("succeeded", None)

    @staticmethod
    def _settled(t):
        return t.canceled is not None or all(r.outcome != "pending" for r in t.refs)

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
            elif t.canceled is None:
                b["in_flight"] += aligned
        if t.canceled is not None:
            state = "CANCELLED"
        elif not settled:
            state = "RUNNING"
        elif counts["failed"] == 0:
            state = "SUCCEEDED"
        elif counts["succeeded"] == 0:
            state = "FAILED"
        else:
            state = "PARTIAL"
        out = {
            "id": t.id,
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
        if self.cache_uid:
            out["cache_uid"] = self.cache_uid
        if q.get("coverage"):
            out["coverage"] = {"resident_bytes": b["admitted"], "observed_at_ms": int(self.now() * 1000)}
        if q.get("refs"):
            start = q.get("start", 0)
            items, cursor = [], None
            for i in range(start, len(t.refs)):
                r = t.refs[i]
                if q["refs"] == "failed" and r.outcome != "failed":
                    continue
                if len(items) == self.refs_page:
                    cursor = self._cursor(t.generation, t.sequence, i)
                    break
                items.append(
                    {
                        "index": i,
                        "key": r.ref["key"],
                        "etag": r.etag,
                        "offset": r.ref.get("offset", 0),
                        "length": r.length,
                        "outcome": r.outcome,
                        "error": r.error,
                    }
                )
            out["refs"] = items
            out["next_cursor"] = cursor
        return out

    @staticmethod
    def _cursor(g, s, n):
        return base64.urlsafe_b64encode(f"{g}.{s}.{n}".encode()).decode().rstrip("=")

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
                        raise _bad("cursor from another window")
                    q["start"] = n
                self._evaluate(t)
                if self._settled(t) or time.monotonic() >= deadline:
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
                m = _WAIT.fullmatch(v)
                if not m:
                    raise _bad("invalid wait")
                seconds = int(m.group(1)) * {"ms": 0.001, "s": 1, "m": 60}[m.group(2)]
                if seconds > self.max_wait:
                    raise _bad("wait too long")
                q["wait"] = seconds
            elif name == "coverage":
                if v not in ("0", "1"):
                    raise _bad("invalid coverage")
                q["coverage"] = v == "1"
            elif name == "refs":
                if v not in ("failed", "all"):
                    raise _bad("invalid refs")
                q["refs"] = v
            elif name == "cursor":
                try:
                    g, s, n = (int(x) for x in base64.urlsafe_b64decode(v + "=" * (-len(v) % 4)).decode().split("."))
                except ValueError:
                    raise _bad("invalid cursor") from None
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
            if t.canceled is None:
                self._evaluate(t)
                t.canceled, t.updated = "client", now
            return 200, self._render(t, {})

    def _list(self):
        with self.lock:
            self._gc(self.now())
            out = []
            for pid in sorted(self.tickets):
                s = self._render(self.tickets[pid], {})
                out.append({k: s[k] for k in ("id", "state", "reason", "priority", "generation", "sequence", "settled", "expires_at_ms", "bytes")})
            return {"prefetches": out}
