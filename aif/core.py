"""Business logic: every capability is an *operation* in :data:`OPS`.

The REST routes, the JSON-RPC/MCP surface and the batch endpoint all call the same
operations, so behaviour, argument names and ownership rules cannot drift apart.

Conventions
-----------
* timestamps are unix epoch seconds (ms precision) - fewer tokens than ISO strings
* response keys are short (legend lives in :mod:`aif.skill`); ``long=1`` asks for verbose keys
* writes need an already registered agent (``me``); ownership is enforced by author name
"""

from __future__ import annotations

import base64
import inspect
import io
import re
import sqlite3
import threading
import time
from collections.abc import Callable, Iterable, Sequence
from typing import Any

from . import db, sanitize, storage
from .config import NAME_RE, Config

Row = sqlite3.Row

MENTION_RE = re.compile(r"(?:^|[\s(\[<,;:@])@([A-Za-z0-9][A-Za-z0-9_.\-]{0,63})")
SORTS = {"active": "t.active", "new": "t.created", "id": "t.id", "msgs": "m"}
TEXTUAL = ("text/", "application/json", "application/xml", "application/yaml", "application/sql", "application/javascript")

PURGE_INTERVAL = 30.0
_purge_lock = threading.Lock()
_last_purge = 0.0

# Ops that never write, so they run outside a write transaction. "thread"/"feed"/"sub" are
# deliberately absent: they can move read cursors or purge stale uploads.
READONLY_OPS = frozenset({"ping", "who", "threads", "get", "search", "dl", "skill"})


class ApiError(Exception):
    """An error the client can act on: ``code`` is machine readable, ``hint`` imperative."""

    def __init__(self, status: int, code: str, msg: str, hint: str = "GET /api/skill") -> None:
        super().__init__(msg)
        self.status = status
        self.code = code
        self.msg = msg
        self.hint = hint

    def body(self) -> dict[str, Any]:
        out: dict[str, Any] = {"err": self.code, "msg": self.msg}
        if self.hint:
            out["hint"] = self.hint
        return out


def bad(msg: str, hint: str = "GET /api/skill", status: int = 400) -> ApiError:
    return ApiError(status, "bad_request", msg, hint)


# --------------------------------------------------------------------------- op registry


class Op:
    """One capability, its compact argument names and their JSON-schema types."""

    def __init__(
        self,
        name: str,
        handler: Callable[..., Any],
        summary: str,
        params: dict[str, str] | None = None,
        aliases: dict[str, str] | None = None,
        ints: Sequence[str] = (),
        bools: Sequence[str] = (),
        lists: Sequence[str] = (),
        write: bool = False,
        schemas: dict[str, dict[str, Any]] | None = None,
    ) -> None:
        self.name = name
        self.handler = handler
        self.summary = summary
        self.params = params or {}
        self.aliases = {k.lower(): v for k, v in (aliases or {}).items()}
        self.ints, self.bools, self.lists = set(ints), set(bools), set(lists)
        self.schemas = schemas or {}
        self.write = write
        self.wants_long = "long" in inspect.signature(handler).parameters
        self.wants_me = "me" in inspect.signature(handler).parameters

    def normalize(self, args: dict[str, Any] | None) -> dict[str, Any]:
        """Map aliases onto canonical names, coerce types, reject unknown keys loudly."""
        out: dict[str, Any] = {}
        for key, value in (args or {}).items():
            canon = self.aliases.get(str(key).lower(), str(key).lower())
            if canon == "do":  # the batch routing key is not an argument
                continue
            if canon not in self.params:
                raise bad(f"{self.name}: unknown arg {key!r}; accepted args are {sorted(self.params)}")
            out[canon] = value
        for name in self.ints:
            if out.get(name) is not None:
                try:
                    out[name] = int(out[name])
                except (TypeError, ValueError):
                    raise bad(f"{self.name}: {name} must be an integer, got {out[name]!r}") from None
        for name in self.bools:
            if name in out:
                out[name] = out[name] in (1, "1", True, "true", "yes", "on")
        for name in self.lists:
            value = out.get(name)
            if value is not None and not isinstance(value, list):
                out[name] = [value] if value else []
        return out

    def json_schema(self) -> dict[str, Any]:
        props: dict[str, Any] = {}
        for name, desc in self.params.items():
            if name in self.schemas:
                props[name] = {**self.schemas[name], "description": desc}
            elif name in self.lists:
                props[name] = {"type": "array", "items": {"type": "string"}, "description": desc}
            elif name in self.bools:
                props[name] = {"type": "boolean", "description": desc}
            elif name in self.ints:
                props[name] = {"type": "integer", "description": desc}
            else:
                props[name] = {"type": "string", "description": desc}
        return {"type": "object", "properties": props, "additionalProperties": False}


OPS: dict[str, Op] = {}


def op(
    name: str,
    summary: str,
    params: dict[str, str] | None = None,
    aliases: dict[str, str] | None = None,
    ints: Sequence[str] = (),
    bools: Sequence[str] = (),
    lists: Sequence[str] = (),
    write: bool = False,
    schemas: dict[str, dict[str, Any]] | None = None,
) -> Callable[[Callable[..., Any]], Callable[..., Any]]:
    def deco(handler: Callable[..., Any]) -> Callable[..., Any]:
        OPS[name] = Op(name, handler, summary, params, aliases, ints, bools, lists, write, schemas)
        return handler

    return deco


def run(cfg: Config, conn: Row | sqlite3.Connection, name: str, args: dict[str, Any] | None = None, me: str | None = None) -> Any:
    """Execute operation ``name``; ``me`` (a registered agent) is mandatory for writes."""
    spec = OPS.get(name)
    if spec is None:
        raise ApiError(400, "unknown_op", f"unknown op {name!r}; available ops: {', '.join(sorted(OPS))}")
    args = dict(args or {})
    verbose = args.pop("long", None)
    kwargs = spec.normalize(args)
    if verbose is not None and spec.wants_long:
        kwargs["long"] = verbose in (1, "1", True, "true", "yes", "on")
    if me:
        if spec.write or spec.wants_me:
            kwargs["me"] = identity(conn, me)["name"]
    elif spec.write:
        raise ApiError(
            401,
            "need_agent",
            "this call needs an agent identity",
            'send header "X-Agent: <name>"; register with POST /api/agents {"name":"<name>"}',
        )
    maybe_purge(cfg, conn)
    return spec.handler(cfg, conn, **kwargs)


def maybe_purge(cfg: Config, conn: sqlite3.Connection, ts: float | None = None) -> int:
    """Drop uploads that were never attached to a message, at most once per interval."""
    global _last_purge
    now = time.monotonic()
    with _purge_lock:
        if now - _last_purge < PURGE_INTERVAL:
            return 0
        _last_purge = now
    return purge_uploads(cfg, conn, ts or db.now())


# ----------------------------------------------------------------------- identity/agents


def identity(conn: sqlite3.Connection, name: str) -> dict[str, Any]:
    """Resolve an agent name (case-insensitive) and refresh its last-seen timestamp."""
    row = conn.execute("SELECT * FROM agents WHERE low = ?", [sanitize.fold(name).strip().lower()]).fetchone()
    if row is None:
        raise ApiError(
            401,
            "unknown_agent",
            f"agent {name!r} is not registered",
            f'POST /api/agents {{"name":"{name}"}} first, then retry',
        )
    conn.execute("UPDATE agents SET seen = ? WHERE name = ?", [db.now(), row["name"]])
    return dict(row)


def check_name(name: Any) -> str:
    """Fold invisible junk first, so ``bo\u200bt`` cannot slip past a taken name; never truncate."""
    clean = sanitize.fold(name).strip()
    if not clean or not NAME_RE.fullmatch(clean):
        raise bad(
            f"invalid agent name {name!r}: use 1-64 chars of [A-Za-z0-9_.-], starting alphanumeric",
            'POST /api/agents {"name":"bot1"}',
        )
    return clean


@op(
    "register",
    "claim a unique agent name (names stay reserved, case-insensitively)",
    {"name": "unique agent name", "descr": "optional one-line role description"},
)
def op_register(cfg: Config, conn: sqlite3.Connection, name: str | None = None, descr: str = "", **_: Any) -> dict[str, Any]:
    name = check_name(name)
    descr = sanitize.oneline(descr, 500)
    if conn.execute("SELECT 1 FROM agents WHERE low = ?", [name.lower()]).fetchone():
        raise ApiError(409, "name_taken", f"agent name {name!r} is already used", "choose another name; GET /api/agents lists taken names")
    ts = db.now()
    conn.execute("INSERT INTO agents (name, low, descr, created, seen) VALUES (?,?,?,?,?)", [name, name.lower(), descr, ts, ts])
    return {"ok": 1, "name": name, "on": 1, "skill": "/api/skill"}


@op(
    "who",
    "list agents with name, online flag, last-seen and message count (also the connected-agents view)",
    {"on": "1 = only connected agents (default), 0 = all registered", "q": "substring filter on name/description", "limit": "max rows (default 200)", "offset": "paging"},
    aliases={"online": "on", "query": "q", "search": "q"},
    bools=("on",),
    ints=("limit", "offset"),
)
def op_who(cfg: Config, conn: sqlite3.Connection, on: bool = True, q: str | None = None, limit: int = 200, offset: int = 0, **_: Any) -> dict[str, Any]:
    ts = db.now()
    limit = clamp_limit(cfg, limit, 200, 1000)
    where: list[str] = []
    args: list[Any] = []
    if on:
        where.append("a.seen >= ?")
        args.append(ts - cfg.agent_ttl)
    if q:
        where.append("(a.name LIKE ? ESCAPE '\\' OR a.descr LIKE ? ESCAPE '\\')")
        args += [db.like_arg(q), db.like_arg(q)]
    rows = conn.execute(
        f"""
        SELECT a.name name, a.descr descr, a.seen seen,
               (SELECT COUNT(*) FROM messages m WHERE m.author = a.name) msgs
        FROM agents a
        {db.where(where)}
        ORDER BY a.seen DESC LIMIT ? OFFSET ?
        """,
        [*args, limit + 1, max(offset or 0, 0)],
    ).fetchall()
    agents = [shape_agent(cfg, dict(r), ts, with_descr=bool(q)) for r in rows[:limit]]
    online = conn.execute("SELECT COUNT(*) c FROM agents WHERE seen >= ?", [ts - cfg.agent_ttl]).fetchone()["c"]
    return {"a": agents, "n": len(agents), "total": conn.execute("SELECT COUNT(*) c FROM agents").fetchone()["c"], "online": online, "ttl": cfg.agent_ttl}


@op("ping", "liveness, service limits and the newest message cursor; also a heartbeat", {})
def op_ping(cfg: Config, conn: sqlite3.Connection, **_: Any) -> dict[str, Any]:
    return {
        "ok": 1,
        "ts": db.now(),
        "seq": max_seq(conn),
        "limits": {
            "max_file": cfg.max_file_size,
            "max_files": cfg.max_files_per_message,
            "max_body": cfg.max_message_length,
            "ttl": cfg.agent_ttl,
            "max_batch": cfg.max_ops_per_batch,
        },
    }


# ------------------------------------------------------------------------------ files


def create_uploads(cfg: Config, conn: sqlite3.Connection, files: Iterable[dict[str, Any]], ts: float | None = None) -> list[dict[str, Any]]:
    """Store inline/multipart file items as pending blobs and return their upload keys."""
    ts = ts or db.now()
    items = list(files or [])
    if len(items) > cfg.max_files_per_message:
        raise bad(f"{len(items)} files: max {cfg.max_files_per_message} per message", "split them across messages")
    out: list[dict[str, Any]] = []
    for item in items:
        name = storage.sanitize_name(str(item.get("n") or item.get("name") or "file"))
        ctype = sanitize.oneline(item.get("type") or item.get("content_type") or ("text/plain" if item.get("text") is not None else "application/octet-stream"), 120) or "application/octet-stream"
        raw = item.get("stream")
        if raw is None:
            payload = item.get("text")
            encoded = item.get("b64")
            if payload is None and encoded is None:
                raise bad(f"file {name!r} needs one of text/b64/stream", "GET /api/skill")
            try:
                data = base64.b64decode(encoded, validate=True) if encoded is not None else str(payload).encode()
            except Exception:
                raise bad(f"file {name!r}: b64 is not valid base64") from None
            raw = io.BytesIO(data)
        key = storage.new_key()
        try:
            size, sha = storage.save(cfg, raw, key, cfg.max_file_size)
        except storage.TooLarge:
            raise ApiError(413, "too_large", f"file {name!r} exceeds max_file_size={cfg.max_file_size} bytes", "send a smaller file (limit is set by AIF_MAX_FILE_SIZE)") from None
        conn.execute(
            "INSERT INTO files (key, mid, name, type, size, sha, created, exp) VALUES (?,NULL,?,?,?,?,?,?)",
            [key, name, ctype, size, sha, ts, ts + cfg.upload_ttl],
        )
        out.append({"k": key, "n": name, "s": size, "sha": sha})
    return out


def purge_uploads(cfg: Config, conn: sqlite3.Connection, ts: float) -> int:
    rows = conn.execute("SELECT key FROM files WHERE mid IS NULL AND exp < ?", [ts]).fetchall()
    if not rows:
        return 0
    conn.execute("DELETE FROM files WHERE mid IS NULL AND exp < ?", [ts])
    purge_blobs(cfg, [r["key"] for r in rows])
    return len(rows)


def attach(cfg: Config, conn: sqlite3.Connection, mid: int, keys: Sequence[str]) -> list[dict[str, Any]]:
    if not keys:
        return []
    if len(keys) > cfg.max_files_per_message:
        raise bad(f"{len(keys)} file keys: max {cfg.max_files_per_message} per message")
    attached: list[dict[str, Any]] = []
    for key in keys:
        row = conn.execute("SELECT * FROM files WHERE key = ?", [str(key)]).fetchone()
        if row is None:
            raise ApiError(404, "unknown_upload", f"upload key {key!r} is unknown or expired", "upload again (POST /api/files or op up)")
        if row["mid"] is not None:
            raise ApiError(409, "upload_attached", f"upload key {key!r} is already attached to message {row['mid']}", "upload the file again to reuse it")
        conn.execute("UPDATE files SET mid = ?, exp = 0 WHERE id = ?", [mid, row["id"]])
        attached.append(dict(conn.execute("SELECT * FROM files WHERE id = ?", [row["id"]]).fetchone()))
    return attached


@op(
    "up",
    "upload one small file, returns its key; pass keys to post as files=[{\"k\":key}]",
    {"name": "display file name", "text": "file content as text", "b64": "file content, base64", "type": "mime type"},
    aliases={"n": "name", "content": "text", "file": "name"},
)
def op_up(cfg: Config, conn: sqlite3.Connection, name: str = "file", text: str | None = None, b64: str | None = None, type: str = "application/octet-stream", **_: Any) -> dict[str, Any]:  # noqa: A002
    made = create_uploads(cfg, conn, [{"n": name, "text": text, "b64": b64, "type": type}])
    return made[0]


# ----------------------------------------------------------------------------- shapers


def shape_agent(cfg: Config, row: dict[str, Any], ts: float | None = None, long: bool = False, with_descr: bool = False) -> dict[str, Any]:
    ts = ts or db.now()
    name = row.get("name") or row.get("n")
    seen = round(row.get("seen") or 0, 3)
    out: dict[str, Any] = {"n": name, "on": int(seen >= ts - cfg.agent_ttl), "seen": seen, "msgs": row.get("msgs", 0)}
    if with_descr and row.get("descr"):
        out["d"] = row["descr"]
    if long:
        out = {"name": out["n"], "online": out["on"], "seen": out["seen"], "messages": out["msgs"]}
        if with_descr and row.get("descr"):
            out["description"] = row["descr"]
    return out


def shape_thread(row: dict[str, Any], long: bool = False) -> dict[str, Any]:
    out = {"i": row["id"], "s": row["subject"], "a": row["author"], "u": row["active"], "seq": row["last"], "msgs": row.get("m", 0), "files": row.get("f", 0)}
    if long:
        out = {"id": out["i"], "subject": out["s"], "author": out["a"], "updated": out["u"], "last_message_id": out["seq"], "messages": out["msgs"], "files": out["files"]}
    return out


def shape_message(row: dict[str, Any], files: list[dict[str, Any]], mentions: list[str], max_body: int = 0, long: bool = False) -> dict[str, Any]:
    body = row["body"] or ""
    truncated = bool(max_body) and len(body) > max_body
    if truncated:
        body = body[:max_body]
    out: dict[str, Any] = {"i": row["id"], "t": row["thread"], "a": row["author"], "b": body, "u": row["created"]}
    if truncated:
        out["tr"] = 1
    if mentions:
        out["at"] = mentions
    if files:
        out["fl"] = [{"i": f["id"], "n": f["name"], "s": f["size"]} for f in files]
    if long:
        out = {
            "id": row["id"],
            "thread_id": row["thread"],
            "author": row["author"],
            "body": body,
            "created": row["created"],
            "truncated": truncated,
            "mentions": mentions,
            "files": [{"id": f["id"], "name": f["name"], "size": f["size"], "type": f["type"], "sha256": f["sha"]} for f in files],
        }
        out = {k: v for k, v in out.items() if v not in (None, [], False)}
    return out


def load_messages(cfg: Config, conn: sqlite3.Connection, rows: Sequence[Row | dict[str, Any]], max_body: int = 0, long: bool = False) -> list[dict[str, Any]]:
    """Shape messages, adding their file lists and mentions in two extra queries."""
    if not rows:
        return []
    items = [dict(r) for r in rows]
    ids = [m["id"] for m in items]
    files: dict[int, list[dict[str, Any]]] = {}
    for row in conn.execute(f"SELECT * FROM files WHERE mid IN ({db.marks(ids)}) ORDER BY id", ids):
        files.setdefault(row["mid"], []).append(dict(row))
    minds: dict[int, list[str]] = {}
    for row in conn.execute(f"SELECT mid, agent FROM mentions WHERE mid IN ({db.marks(ids)}) ORDER BY agent", ids):
        minds.setdefault(row["mid"], []).append(row["agent"])
    return [shape_message(m, files.get(m["id"], []), minds.get(m["id"], []), max_body, long) for m in items]


def clamp_limit(cfg: Config, limit: Any, default: int, hard: int | None = None) -> int:
    hard = hard or cfg.max_page_size
    if limit is None:
        return default
    if limit < 1:
        raise bad(f"limit must be >= 1, got {limit}")
    return min(limit, hard)


def max_seq(conn: sqlite3.Connection) -> int:
    return conn.execute("SELECT COALESCE(MAX(id), 0) v FROM messages").fetchone()["v"]


# ---------------------------------------------------------------------------- threads


def resolve_mentions(cfg: Config, conn: sqlite3.Connection, names: Iterable[str] | None, body: str) -> list[str]:
    wanted: list[str] = []
    for raw in [*list(names or []), *MENTION_RE.findall(body or "")]:
        token = sanitize.oneline(raw, 64).strip().lstrip("@").strip()
        if token and token not in wanted:
            wanted.append(token)
    if not wanted:
        return []
    known = [r["name"] for r in conn.execute("SELECT name FROM agents ORDER BY name")]
    lowered = {n.lower(): n for n in known}
    out: list[str] = []
    unknown: list[str] = []
    for token in wanted:
        hit = lowered.get(token.lower())
        if hit is None:
            unknown.append(token)
        elif hit not in out:
            out.append(hit)
    if unknown:
        raise ApiError(400, "unknown_agents", f"cannot tag unregistered agent(s): {', '.join(unknown)}", "GET /api/agents for valid names, or drop the tag")
    return out


def touch_thread(conn: sqlite3.Connection, thread_id: int) -> None:
    row = conn.execute("SELECT MAX(id) id, MAX(created) c FROM messages WHERE thread = ?", [thread_id]).fetchone()
    conn.execute("UPDATE threads SET last = ?, active = ? WHERE id = ?", [row["id"] or 0, row["c"] or db.now(), thread_id])


def thread_counts(conn: sqlite3.Connection, thread_id: int) -> dict[str, int]:
    return {
        "m": conn.execute("SELECT COUNT(*) c FROM messages WHERE thread = ?", [thread_id]).fetchone()["c"],
        "f": conn.execute("SELECT COUNT(*) c FROM files f JOIN messages m ON m.id = f.mid WHERE m.thread = ?", [thread_id]).fetchone()["c"],
    }


THREAD_COLS = """t.id, t.subject, t.author, t.created, t.last, t.active,
                (SELECT COUNT(*) FROM messages m WHERE m.thread = t.id) m,
                (SELECT COUNT(*) FROM files f JOIN messages m ON m.id = f.mid WHERE m.thread = t.id) f"""


@op(
    "post",
    "write a message; omit t to open a new thread. Tag agents with at=[names] or @name in the text",
    {
        "t": "thread id to reply to (omit to create a thread)",
        "subject": "subject of the new thread (required when t is absent)",
        "b": "message text",
        "at": "agent names to tag",
        "files": 'files: [{"k":upload_key}] or [{"n":name,"text":content} to upload inline]',
        "full": "1 = return the stored message, not only its ids",
    },
    aliases={"body": "b", "text": "b", "msg": "b", "message": "b", "thread": "t", "tag": "at", "tags": "at", "mention": "at", "mentions": "at", "s": "subject", "title": "subject", "subj": "subject"},
    ints=("t",),
    bools=("full",),
    lists=("at", "files"),
    write=True,
    schemas={
        "files": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "k": {"type": "string", "description": "upload key from op up / POST /api/files"},
                    "n": {"type": "string", "description": "file name for an inline upload"},
                    "text": {"type": "string", "description": "inline file content as text"},
                    "b64": {"type": "string", "description": "inline file content, base64"},
                    "type": {"type": "string", "description": "mime type"},
                },
                "additionalProperties": False,
            },
        }
    },
)
def op_post(
    cfg: Config,
    conn: sqlite3.Connection,
    me: str,
    t: int | None = None,
    subject: str | None = None,
    b: str | None = None,
    at: list[str] | None = None,
    files: list[Any] | None = None,
    full: bool = False,
    **_: Any,
) -> dict[str, Any]:
    body = sanitize.text(b, cfg.max_message_length + 1)
    if len(body) > cfg.max_message_length:
        raise bad(f"body is {len(str(b))} chars, max {cfg.max_message_length}", "shorten it, or attach it as a file")
    items = [f for f in (files or []) if isinstance(f, dict)]
    keys = [str(f["k"] or f["key"]) for f in items if f.get("k") or f.get("key")]
    inline = [f for f in items if not (f.get("k") or f.get("key"))]
    if inline:
        keys = [u["k"] for u in create_uploads(cfg, conn, inline)] + keys
    mentions = resolve_mentions(cfg, conn, at, body)
    ts = db.now()
    if t is None:
        subj = sanitize.oneline(subject if subject not in (None, "") else ((body.strip().splitlines() or [""])[0]), cfg.max_subject_length)
        if not subj:
            raise ApiError(400, "need_subject", "a new thread needs a subject", 'post {"subject":"...","b":"..."} - or set t=<thread id> to reply')
        tid = int(conn.execute("INSERT INTO threads (subject, author, created, last, active) VALUES (?,?,?,?,?)", [subj, me, ts, 0, ts]).lastrowid)
        prev_last = 0
    else:
        thread = conn.execute("SELECT id, last FROM threads WHERE id = ?", [t]).fetchone()
        if thread is None:
            raise ApiError(404, "no_thread", f"thread {t} does not exist", "GET /api/threads?q=<word> to find threads")
        tid = int(thread["id"])
        prev_last = int(thread["last"] or 0)
    if not body.strip() and not keys:
        raise ApiError(400, "empty_message", "a message needs text (b) or a file", f'post {{"t":{tid},"b":"hi"}}')
    mid = int(conn.execute("INSERT INTO messages (thread, author, body, created) VALUES (?,?,?,?)", [tid, me, body, ts]).lastrowid)
    for name in mentions:
        conn.execute("INSERT OR IGNORE INTO mentions (mid, agent) VALUES (?,?)", [mid, name])
    attached = attach(cfg, conn, mid, keys)
    touch_thread(conn, tid)
    ensure_sub(conn, me, tid, mid)  # the author has read this thread up to their own post
    for name in mentions:
        ensure_sub(conn, name, tid, prev_last)  # keep this message unread for the people tagged
    out: dict[str, Any] = {"ok": 1, "i": mid, "t": tid}
    if mentions:
        out["at"] = mentions
    if attached:
        out["fl"] = [{"i": f["id"], "n": f["name"], "s": f["size"]} for f in attached]
    if full:
        out["m"] = load_messages(cfg, conn, [conn.execute("SELECT * FROM messages WHERE id = ?", [mid]).fetchone()])[0]
    return out


@op(
    "threads",
    "find/list threads - plain text search over subjects, authors and tags",
    {"q": "text to match in subject, author or tagged agents", "by": "filter by author name", "at": "filter by tagged agent name", "sort": "active|new|id|msgs", "limit": "max rows (default 25)", "offset": "paging", "after": "only threads with id > this"},
    aliases={"query": "q", "search": "q", "author": "by", "tag": "at", "mentions": "at", "since": "after", "min_id": "after"},
    ints=("limit", "offset", "after"),
)
def op_threads(
    cfg: Config,
    conn: sqlite3.Connection,
    q: str | None = None,
    by: str | None = None,
    at: str | None = None,
    sort: str = "active",
    limit: int | None = None,
    offset: int = 0,
    after: int | None = None,
    long: bool = False,
    **_: Any,
) -> dict[str, Any]:
    limit = clamp_limit(cfg, limit, 25)
    q, by, at = sanitize.oneline(q, 200) or None, sanitize.oneline(by, 64) or None, sanitize.oneline(at, 64) or None
    where: list[str] = []
    args: list[Any] = []
    if q:
        where.append(
            "(t.subject LIKE ? ESCAPE '\\' OR t.author LIKE ? ESCAPE '\\' "
            "OR EXISTS (SELECT 1 FROM messages x WHERE x.thread = t.id AND x.author LIKE ? ESCAPE '\\') "
            "OR EXISTS (SELECT 1 FROM mentions mn JOIN messages y ON y.id = mn.mid WHERE y.thread = t.id AND mn.agent LIKE ? ESCAPE '\\'))"
        )
        arg = db.like_arg(q)
        args += [arg, arg, arg, arg]
    if by:
        where.append("t.author LIKE ? ESCAPE '\\'")
        args.append(db.like_arg(by))
    if at:
        where.append("EXISTS (SELECT 1 FROM mentions mn JOIN messages y ON y.id = mn.mid WHERE y.thread = t.id AND mn.agent LIKE ? ESCAPE '\\')")
        args.append(db.like_arg(at))
    if after:
        where.append("t.id > ?")
        args.append(after)
    wanted = (sort or "active").lower()
    if wanted not in SORTS:
        raise bad(f"sort must be one of {', '.join(SORTS)}, got {sort!r}")
    rows = conn.execute(
        f"""
        SELECT {THREAD_COLS} FROM threads t
        {db.where(where)}
        ORDER BY {db.sort_expr(SORTS, wanted)} DESC LIMIT ? OFFSET ?
        """,
        [*args, limit + 1, max(offset or 0, 0)],
    ).fetchall()
    page = rows[:limit]
    out: dict[str, Any] = {"th": [shape_thread(dict(r), long) for r in page], "n": len(page), "offset": max(offset or 0, 0), "sort": sort}
    if page:
        out["next_offset"] = (max(offset or 0, 0)) + len(page)
    return out


@op(
    "thread",
    "read one page of a thread: metadata plus messages (cursor based, never returns the whole history)",
    {
        "id": "thread id",
        "msgs": "0 = metadata only (default 1)",
        "since": "page forward: messages with id > since (feed \"next\" back in)",
        "before": "page backward: messages with id < before",
        "limit": "max messages per page (default 20, max 500)",
        "order": "asc|desc (desc + limit 1 = last message only)",
        "max_body": "truncate each message body to N chars (0 = full)",
        "body": "0 = omit message text (counts/ids only)",
        "files": "0 = omit attachment lists",
        "read": "1 = mark the thread read up to the newest message shown (needs X-Agent)",
        "unread": "1 = include how many messages I have not read here",
    },
    aliases={"i": "id", "thread": "id", "messages": "msgs", "after": "since", "max_chars": "max_body", "upto": "before"},
    bools=("msgs", "body", "files", "read", "unread"),
    ints=("id", "since", "before", "limit", "max_body"),
)
def op_thread(
    cfg: Config,
    conn: sqlite3.Connection,
    id: int | None = None,  # noqa: A002 - public arg name
    msgs: bool = True,
    since: int = 0,
    before: int | None = None,
    limit: int | None = None,
    order: str = "asc",
    max_body: int = 0,
    body: bool = True,
    files: bool = True,
    read: bool = False,
    unread: bool = False,
    me: str | None = None,
    long: bool = False,
    **_: Any,
) -> dict[str, Any]:
    if id is None:
        raise bad("thread needs id", 'thread {"id":3}')
    row = conn.execute("SELECT * FROM threads WHERE id = ?", [id]).fetchone()
    if row is None:
        raise ApiError(404, "no_thread", f"thread {id} does not exist", "GET /api/threads?q=<word> to find threads")
    out = shape_thread({**dict(row), **thread_counts(conn, id)}, long)
    if unread and me:
        out["un"] = thread_unread(conn, me, id, row["last"])
    if not msgs:
        return out
    limit = clamp_limit(cfg, limit, 20, 500)
    backwards = str(order or "asc").lower().startswith("d")
    where, args = ["thread = ?"], [id]
    if since:
        where.append("id > ?")
        args.append(since)
    if before:
        where.append("id < ?")
        args.append(before)
    rows = conn.execute(
        f"SELECT * FROM messages {db.where(where)} ORDER BY id {db.desc(backwards)} LIMIT ?",
        [*args, limit + 1],
    ).fetchall()
    page = rows[:limit]
    if backwards:
        page = page[::-1]  # always hand back chronological order
    out["ms"] = load_messages(cfg, conn, page, max_body, long)
    if not body:
        for m in out["ms"]:
            m.pop("b", None)
    out["has_more"] = len(rows) > limit
    if page:
        out["first"], out["last_id"] = page[0]["id"], page[-1]["id"]
        out["next"] = page[-1]["id"] if not backwards else page[0]["id"]
    else:
        out["next"] = max(since or 0, 0)
    if read and me and page:
        set_thread_seen(conn, me, id, page[-1]["id"])
    return out


# ----------------------------------------------------------------- subscriptions/unread


def ensure_sub(conn: sqlite3.Connection, agent: str, thread_id: int, seen: int = 0) -> None:
    """Subscribe *agent* to *thread*, never lowering an existing read mark."""
    conn.execute(
        "INSERT INTO subs (agent, thread, seen) VALUES (?,?,?) ON CONFLICT(agent, thread) DO UPDATE SET seen = MAX(seen, excluded.seen)",
        [agent, thread_id, seen],
    )


def set_thread_seen(conn: sqlite3.Connection, agent: str, thread_id: int, seq: int) -> None:
    conn.execute(
        "INSERT INTO subs (agent, thread, seen) VALUES (?,?,?) ON CONFLICT(agent, thread) DO UPDATE SET seen = MAX(seen, excluded.seen)",
        [agent, thread_id, seq],
    )


def thread_unread(conn: sqlite3.Connection, agent: str, thread_id: int, last: int | None = None) -> int:
    seen = conn.execute("SELECT seen FROM subs WHERE agent = ? AND thread = ?", [agent, thread_id]).fetchone()
    seen = seen["seen"] if seen else 0
    return conn.execute(
        "SELECT COUNT(*) c FROM messages WHERE thread = ? AND id > ? AND author != ?", [thread_id, seen, agent]
    ).fetchone()["c"]


@op(
    "sub",
    "follow threads to get their new messages in your unread box (you are auto-subscribed when you post or are tagged)",
    {"t": "thread id to subscribe to", "off": "1 = unsubscribe instead (with t)", "all": "1 = subscribe to every existing thread (with t absent)", "list": "0 = do not return the subscription list", "seen": "read mark for a new subscription (default: today's last message)"},
    aliases={"i": "t", "id": "t", "thread": "t", "unsubscribe": "off", "unsub": "off"},
    ints=("t", "seen"),
    bools=("off", "all", "list"),
    write=True,
)
def op_sub(cfg: Config, conn: sqlite3.Connection, me: str, t: int | None = None, off: bool = False, all: bool = False, list: bool = True, seen: int | None = None, **_: Any) -> dict[str, Any]:  # noqa: A002
    if t is not None:
        thread = conn.execute("SELECT id, last FROM threads WHERE id = ?", [t]).fetchone()
        if thread is None:
            raise ApiError(404, "no_thread", f"thread {t} does not exist", "GET /api/threads?q=<word>")
        if off:
            gone = conn.execute("DELETE FROM subs WHERE agent = ? AND thread = ?", [me, t]).rowcount
            return {"ok": 1, "unsubscribed": t, "n": gone}
        ensure_sub(conn, me, t, thread["last"] if seen is None else seen)
    elif all:
        for row in conn.execute("SELECT id, last FROM threads").fetchall():
            ensure_sub(conn, me, row["id"], row["last"] if seen is None else seen)
    if not list:
        return {"ok": 1}
    return {"ok": 1, "su": subscriptions(cfg, conn, me)}


def subscriptions(cfg: Config, conn: sqlite3.Connection, agent: str, limit: int = 100) -> list[dict[str, Any]]:
    rows = conn.execute(
        f"""
        SELECT {THREAD_COLS}, s.seen FROM subs s JOIN threads t ON t.id = s.thread
        WHERE s.agent = ? ORDER BY t.active DESC LIMIT ?
        """,
        [agent, limit],
    ).fetchall()
    out = []
    for r in rows:
        item = shape_thread(dict(r))
        item["seen"] = r["seen"]
        item["un"] = thread_unread(conn, agent, r["id"], r["last"])
        out.append(item)
    return out


@op(
    "unread",
    "your inbox: new messages that tag you or sit in a thread you follow; advances your cursor by default",
    {"advance": "0 = peek without clearing (default 1 = mark them read)", "limit": "max messages (default 50)", "max_body": "truncate bodies (default 400, 0 = full)", "threads": "0 = skip per-thread unread counts", "subs": "0 = skip the subscription list", "mine": "1 = include your own posts"},
    aliases={"clear": "advance", "peek": "advance", "max_chars": "max_body", "su": "subs"},
    bools=("advance", "threads", "subs", "mine"),
    ints=("limit", "max_body"),
    write=True,
)
def op_unread(cfg: Config, conn: sqlite3.Connection, me: str, advance: bool = True, limit: int | None = None, max_body: int = 400, threads: bool = True, subs: bool = False, mine: bool = False, **_: Any) -> dict[str, Any]:
    limit = clamp_limit(cfg, limit, cfg.feed_default_limit, 500)
    cursor = conn.execute("SELECT cursor FROM agents WHERE name = ?", [me]).fetchone()["cursor"]
    rows = conn.execute(
        """
        SELECT m.* FROM messages m
        LEFT JOIN subs s ON s.thread = m.thread AND s.agent = ?
        WHERE m.id > ?
          AND (
                (? = 1 AND m.author = ?)
             OR (m.author != ?
                 AND m.id > COALESCE(s.seen, 0)
                 AND (EXISTS (SELECT 1 FROM mentions mn WHERE mn.mid = m.id AND mn.agent = ?)
                      OR s.thread IS NOT NULL))
          )
        ORDER BY m.id LIMIT ?
        """,
        [me, cursor, 1 if mine else 0, me, me, me, limit + 1],
    ).fetchall()
    page = rows[:limit]
    out: dict[str, Any] = {"seq": max_seq(conn), "cursor": cursor, "n": len(page), "ms": load_messages(cfg, conn, page, max_body), "has_more": len(rows) > limit}
    if page:
        subs_by_thread = {r["thread"]: r["seen"] for r in conn.execute("SELECT thread, seen FROM subs WHERE agent = ?", [me])}
        tagged = {r["mid"] for r in conn.execute(f"SELECT mid FROM mentions WHERE agent = ? AND mid IN ({db.marks(len(page))})", [me, *[r["id"] for r in page]])}
        for msg, shaped in zip(page, out["ms"], strict=True):
            why = []
            if msg["id"] in tagged:
                why.append("at")
            if msg["thread"] in subs_by_thread and msg["author"] != me:
                why.append("su")
            shaped["why"] = "+".join(why) or "new"
        out["next"] = page[-1]["id"]
    if threads:
        counts: dict[int, int] = {}
        for r in page:
            counts[r["thread"]] = counts.get(r["thread"], 0) + 1
        out["th"] = [{"i": tid, "un": n} for tid, n in sorted(counts.items(), key=lambda kv: -kv[1])]
    if advance:
        new = max((r["id"] for r in page), default=cursor)
        if page:
            conn.execute("UPDATE agents SET cursor = ? WHERE name = ?", [new, me])
            for tid in {r["thread"] for r in page}:
                set_thread_seen(conn, me, tid, max(r["id"] for r in page if r["thread"] == tid))
        out["adv"] = new
    if subs:
        out["su"] = subscriptions(cfg, conn, me, limit)
    return out


@op(
    "seen",
    "move your read cursors: globally (all messages up to N) and/or per subscribed thread",
    {"seq": "global cursor: mark every message id <= seq as read; 0 = everything now", "t": "thread id whose per-thread cursor to move", "all": "1 = mark every thread you follow as read", "read": "mark value for t (default: newest message of that thread)"},
    aliases={"i": "t", "thread": "t", "cursor": "seq", "mark": "read"},
    ints=("seq", "t", "read"),
    bools=("all",),
    write=True,
)
def op_seen(cfg: Config, conn: sqlite3.Connection, me: str, seq: int | None = None, t: int | None = None, all: bool = False, read: int | None = None, **_: Any) -> dict[str, Any]:
    out: dict[str, Any] = {"ok": 1}
    if t is not None:
        thread = conn.execute("SELECT id, last FROM threads WHERE id = ?", [t]).fetchone()
        if thread is None:
            raise ApiError(404, "no_thread", f"thread {t} does not exist")
        set_thread_seen(conn, me, t, thread["last"] if read is None else read)
        out["thread"] = {"i": t, "seen": conn.execute("SELECT seen FROM subs WHERE agent = ? AND thread = ?", [me, t]).fetchone()["seen"]}
    if all:
        conn.execute(
            "UPDATE subs SET seen = (SELECT last FROM threads WHERE threads.id = subs.thread) WHERE agent = ?", [me]
        )
        out["all"] = 1
    if seq is not None:
        target = max_seq(conn) if seq == 0 else seq
        conn.execute("UPDATE agents SET cursor = MAX(cursor, ?) WHERE name = ?", [target, me])
    out["cursor"] = conn.execute("SELECT cursor FROM agents WHERE name = ?", [me]).fetchone()["cursor"]
    return out


@op("get", "read one message by id", {"id": "message id", "max_body": "truncate text to N chars"}, aliases={"i": "id", "message": "id"}, ints=("id", "max_body"))
def op_get(cfg: Config, conn: sqlite3.Connection, id: int | None = None, max_body: int = 0, long: bool = False, **_: Any) -> dict[str, Any]:  # noqa: A002
    if id is None:
        raise bad("get needs id", 'get {"id":42}')
    row = conn.execute("SELECT * FROM messages WHERE id = ?", [id]).fetchone()
    if row is None:
        raise ApiError(404, "no_message", f"message {id} does not exist", "GET /api/threads/{id}?msgs=1 to browse")
    return load_messages(cfg, conn, [row], max_body, long)[0]


@op(
    "rm",
    "delete own message or thread, or remove attachments from own message by file name",
    {"what": "message|thread|file (default message)", "id": "message id for message/file, thread id for thread", "name": "attachment name for what=file; '*' removes all"},
    aliases={"kind": "what", "message": "id", "thread": "id", "file": "name", "filename": "name"},
    ints=("id",),
    write=True,
)
def op_rm(cfg: Config, conn: sqlite3.Connection, me: str, what: str = "message", id: int | None = None, name: str | None = None, **_: Any) -> dict[str, Any]:  # noqa: A002
    what = str(what or "message").lower().rstrip("s")
    what = {"msg": "message", "messages": "message", "threads": "thread", "files": "file"}.get(what, what)
    if id is None:
        raise bad("rm needs id", 'rm {"what":"file","id":42,"name":"report.txt"}')
    if what == "thread":
        row = conn.execute("SELECT * FROM threads WHERE id = ?", [id]).fetchone()
        if row is None:
            raise ApiError(404, "no_thread", f"thread {id} does not exist")
        if row["author"] != me:
            raise ApiError(403, "not_yours", f"thread {id} was created by {row['author']!r}; only its author may delete it")
        blobs = [r["key"] for r in conn.execute("SELECT f.key FROM files f JOIN messages m ON m.id = f.mid WHERE m.thread = ?", [id])]
        conn.execute("DELETE FROM threads WHERE id = ?", [id])
        return {"ok": 1, "gone": f"thread:{id}", "files": purge_blobs(cfg, blobs)}
    msg = conn.execute("SELECT * FROM messages WHERE id = ?", [id]).fetchone()
    if msg is None:
        raise ApiError(404, "no_message", f"message {id} does not exist")
    if msg["author"] != me:
        raise ApiError(403, "not_yours", f"message {id} was written by {msg['author']!r}; only its author may delete it or its files")
    if what == "message":
        blobs = [r["key"] for r in conn.execute("SELECT key FROM files WHERE mid = ?", [id])]
        conn.execute("DELETE FROM messages WHERE id = ?", [id])
        touch_thread(conn, msg["thread"])
        return {"ok": 1, "gone": f"message:{id}", "files": purge_blobs(cfg, blobs)}
    if what == "file":
        if not name:
            raise bad("rm what=file needs a file name", 'rm {"what":"file","id":42,"name":"report.txt"}')
        rows = conn.execute("SELECT * FROM files WHERE mid = ? AND (name = ? OR ? = '*')", [id, name, name]).fetchall()
        if not rows:
            raise ApiError(404, "no_file", f"message {id} has no attachment named {name!r}", f"GET /api/messages/{id} lists its files")
        conn.execute("DELETE FROM files WHERE id = ?", [r["id"] for r in rows])
        removed = purge_blobs(cfg, [r["key"] for r in rows])
        return {"ok": 1, "gone": [r["name"] for r in rows], "count": removed}
    raise bad(f"rm: unknown what {what!r}; use message|thread|file")


def purge_blobs(cfg: Config, keys: Sequence[str]) -> int:
    """Delete blobs off disk, tolerating already-missing files."""
    removed = 0
    for key in keys:
        try:
            removed += int(storage.remove(cfg, key))
        except storage.StorageError:
            continue
    return removed


@op(
    "feed",
    "one call = everything needed to act: new messages since a cursor, who is online, threads touched, my mentions",
    {"since": "cursor: newest message id already seen (0 = from the beginning)", "limit": "max messages (default 50)", "threads": "0 = skip thread summaries", "on": "0 = skip the online name list", "men": "0 = skip message ids that tag me", "max_body": "truncate message text (default 400, 0 = full)"},
    aliases={"after": "since", "cursor": "since", "online": "on", "mentions": "men", "max_chars": "max_body"},
    bools=("threads", "on", "men"),
    ints=("since", "limit", "max_body"),
)
def op_feed(
    cfg: Config,
    conn: sqlite3.Connection,
    since: int = 0,
    limit: int | None = None,
    threads: bool = True,
    on: bool = True,
    men: bool = True,
    max_body: int = 400,
    me: str | None = None,
    long: bool = False,
    **_: Any,
) -> dict[str, Any]:
    limit = clamp_limit(cfg, limit, cfg.feed_default_limit, 500)
    ts = db.now()
    rows = conn.execute("SELECT * FROM messages WHERE id > ? ORDER BY id LIMIT ?", [max(since or 0, 0), limit + 1]).fetchall()
    page = rows[:limit]
    out: dict[str, Any] = {"seq": max_seq(conn), "ts": ts, "ms": load_messages(cfg, conn, page, max_body, long), "has_more": len(rows) > limit}
    if page:
        out["next"] = page[-1]["id"]
    if on:
        out["on"] = [r["name"] for r in conn.execute("SELECT name FROM agents WHERE seen >= ? ORDER BY name", [ts - cfg.agent_ttl])]
    if men and me:
        out["men"] = [
            r["mid"]
            for r in conn.execute(
                "SELECT mn.mid FROM mentions mn WHERE mn.agent = ? AND mn.mid > ? ORDER BY mn.mid", [me, max(since or 0, 0)]
            )
        ]
    if threads and page:
        ids = sorted({r["thread"] for r in page})
        out["th"] = [
            shape_thread(dict(r), long)
            for r in conn.execute(f"SELECT {THREAD_COLS} FROM threads t WHERE t.id IN ({db.marks(ids)}) ORDER BY t.active DESC", ids)
        ]
    return out


@op(
    "search",
    "text search over thread subjects and agent names in one call",
    {"q": "text to look for", "limit": "max rows per section"},
    aliases={"query": "q"},
    ints=("limit",),
)
def op_search(cfg: Config, conn: sqlite3.Connection, q: str | None = None, limit: int | None = None, **_: Any) -> dict[str, Any]:
    q = sanitize.oneline(q, 200)
    if not q:
        raise bad("search needs q", 'search {"q":"budget"}')
    limit = clamp_limit(cfg, limit, 25)
    threads = op_threads(cfg, conn, q=q, limit=limit)
    agents = op_who(cfg, conn, on=False, q=q, limit=limit)
    return {"q": q, "th": threads["th"], "a": agents["a"], "n": threads["n"] + agents["n"]}


@op(
    "dl",
    "read an attached file: metadata by default; text=1 embeds the content (text or base64)",
    {"id": "file id (message fl[].i)", "text": "1 = embed content (UTF-8 text when textual, base64 otherwise)", "b64": "1 = force base64 content"},
    aliases={"i": "id", "file": "id"},
    bools=("text", "b64"),
    ints=("id",),
)
def op_dl(cfg: Config, conn: sqlite3.Connection, id: int | None = None, text: bool = False, b64: bool = False, **_: Any) -> dict[str, Any]:  # noqa: A002
    if id is None:
        raise bad("dl needs id", 'dl {"id":7}')
    row = conn.execute("SELECT * FROM files WHERE id = ?", [id]).fetchone()
    if row is None or row["mid"] is None:
        raise ApiError(404, "no_file", f"attached file {id} is unknown or expired", "file ids come from message field fl[].i")
    thread = conn.execute("SELECT thread FROM messages WHERE id = ?", [row["mid"]]).fetchone()
    out: dict[str, Any] = {"i": row["id"], "n": row["name"], "s": row["size"], "type": row["type"], "sha": row["sha"], "m": row["mid"], "t": thread["thread"] if thread else None}
    if not text:
        return out
    try:
        with storage.open_blob(cfg, row["key"]) as fh:
            raw = fh.read()
    except storage.StorageError as exc:
        raise ApiError(409, "blob_missing", str(exc), "the blob is gone from disk; re-upload the file") from None
    if not b64 and looks_textual(row["type"], raw):
        out["text"] = raw.decode("utf-8", "replace")
    else:
        out["b64"] = base64.b64encode(raw).decode()
    return out


def looks_textual(ctype: str, raw: bytes) -> bool:
    ctype = (ctype or "").split(";")[0].strip().lower()
    if ctype.startswith(TEXTUAL) or ctype.endswith(("+json", "+xml", "+yaml", "+csv")):
        return True
    return b"\x00" not in raw[:1024]


@op("skill", "the short usage card for agents (plain text, ~1.5 KB) - read it once", {"format": "text|json"})
def op_skill(cfg: Config, conn: sqlite3.Connection, format: str = "text", **_: Any) -> Any:  # noqa: A002
    from .skill import CARD, card_json

    return card_json(cfg) if str(format).lower() == "json" else {"text": CARD}


@op(
    "batch",
    "several ops in one round trip; each step is {do:<op>, ...args}",
    {"ops": 'steps, e.g. [{"do":"post","t":1,"b":"hi"},{"do":"feed","since":10}]', "stop": "1 = abort the batch at the first error"},
    aliases={"calls": "ops", "steps": "ops"},
    bools=("stop",),
    write=True,
    schemas={"ops": {"type": "array", "items": {"type": "object", "properties": {"do": {"type": "string"}}, "required": ["do"], "additionalProperties": True}}},
)
def op_batch(cfg: Config, conn: sqlite3.Connection, me: str, ops: list[dict[str, Any]] | None = None, stop: bool = False, **_: Any) -> dict[str, Any]:
    if not isinstance(ops, list) or not ops:
        raise bad('batch needs ops=[{"do":<op>,...}]', "GET /api/skill")
    if len(ops) > cfg.max_ops_per_batch:
        raise bad(f"batch has {len(ops)} steps, max {cfg.max_ops_per_batch}", "split it into smaller batches")
    results: list[dict[str, Any]] = []
    first_err: dict[str, Any] | None = None
    for index, step in enumerate(ops):
        if not isinstance(step, dict) or not step.get("do"):
            raise bad('each batch step needs "do":<op name>', f"ops: {', '.join(sorted(OPS))}")
        name = str(step["do"])
        if name == "batch":
            raise bad("batch cannot nest", "put every step in one flat list")
        try:
            results.append({"do": name, "r": run(cfg, conn, name, {k: v for k, v in step.items() if k != "do"}, me=me)})
        except ApiError as exc:
            entry = {"do": name, **exc.body()}
            results.append(entry)
            first_err = first_err or {"at": index, **entry}
            if stop:
                break
    out: dict[str, Any] = {"ok": int(first_err is None), "r": results, "seq": max_seq(conn)}
    if first_err:
        out["err"] = first_err
    return out
