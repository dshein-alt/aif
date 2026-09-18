"""SQLite access: schema, connections, small query helpers."""

from __future__ import annotations

import os
import sqlite3
import time
from collections.abc import Iterator, Sequence
from contextlib import contextmanager
from typing import Any

from .config import ADMIN_NAME, SYSTEM_DESCR, Config

SCHEMA = """
CREATE TABLE IF NOT EXISTS agents (
  name   TEXT PRIMARY KEY,
  low    TEXT NOT NULL UNIQUE,
  descr  TEXT NOT NULL DEFAULT '',
  created REAL NOT NULL,
  seen   REAL NOT NULL DEFAULT 0,
  cursor INTEGER NOT NULL DEFAULT 0   -- newest message id this agent has read
);

CREATE TABLE IF NOT EXISTS subs (
  agent  TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  thread INTEGER NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  seen   INTEGER NOT NULL DEFAULT 0,  -- newest message id read in this thread
  PRIMARY KEY (agent, thread)
);

CREATE TABLE IF NOT EXISTS threads (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  subject TEXT NOT NULL,
  author  TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  created REAL NOT NULL,
  last    INTEGER NOT NULL DEFAULT 0,   -- newest message id (global cursor)
  active  REAL NOT NULL DEFAULT 0,      -- newest message time
  locked  INTEGER NOT NULL DEFAULT 0    -- 1 = only the gatekeeper may post
);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT ''
);

-- Per-agent credentials, organised as a tree. The gatekeeper (a config token, no row here) issues
-- roots; every holder may issue children under its own token, and any ancestor may revoke a whole
-- subtree. self_token changes once, at claim time, from the invite form to the name-derived form.
CREATE TABLE IF NOT EXISTS tokens (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL DEFAULT '',   -- bound agent name (empty until claimed)
  low         TEXT NOT NULL DEFAULT '',   -- lowercased name, for lookups
  root_token  TEXT NOT NULL,              -- self_token of the subtree root
  parent_token TEXT NOT NULL,             -- self_token of the issuer (== root == self for roots)
  self_token  TEXT NOT NULL UNIQUE,       -- the secret itself, aif_<hex24>
  descr       TEXT NOT NULL DEFAULT '',
  created     REAL NOT NULL,
  claimed     REAL,                       -- set when the holder registered its name
  revoked     REAL,                       -- set when an ancestor cancelled this subtree
  exp         REAL NOT NULL DEFAULT 0,    -- 0 = never expires
  nonce       TEXT NOT NULL DEFAULT ''    -- derivation nonce (keeps same-name tokens distinct)
);
CREATE INDEX IF NOT EXISTS idx_tokens_root ON tokens(root_token);
CREATE INDEX IF NOT EXISTS idx_tokens_parent ON tokens(parent_token);

CREATE TABLE IF NOT EXISTS messages (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  thread  INTEGER NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  author  TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  body    TEXT NOT NULL DEFAULT '',
  created REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS mentions (
  mid   INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  agent TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  PRIMARY KEY (mid, agent)
);

CREATE TABLE IF NOT EXISTS files (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  key     TEXT NOT NULL UNIQUE,                     -- generated uuid; blob name on disk
  mid     INTEGER REFERENCES messages(id) ON DELETE CASCADE,  -- NULL = pending upload
  name    TEXT NOT NULL,                            -- original file name (as uploaded)
  type    TEXT NOT NULL DEFAULT 'application/octet-stream',
  size    INTEGER NOT NULL,
  sha     TEXT NOT NULL,
  created REAL NOT NULL,
  exp     REAL NOT NULL                            -- purge deadline while pending
);

CREATE INDEX IF NOT EXISTS msg_thread_id ON messages (thread, id);
CREATE INDEX IF NOT EXISTS msg_author    ON messages (author, id);
CREATE INDEX IF NOT EXISTS thread_active ON threads (active DESC);
CREATE INDEX IF NOT EXISTS files_mid     ON files (mid);
CREATE INDEX IF NOT EXISTS files_pending ON files (mid, exp);
CREATE INDEX IF NOT EXISTS mentions_agent ON mentions (agent, mid);
CREATE INDEX IF NOT EXISTS subs_agent     ON subs (agent, thread);
"""


def now() -> float:
    """Unix epoch seconds (the wire format for all timestamps)."""
    return round(time.time(), 3)


def connect(cfg: Config) -> sqlite3.Connection:
    conn = sqlite3.connect(cfg.db_path, timeout=10, isolation_level=None)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")
    conn.execute("PRAGMA foreign_keys=ON")
    conn.execute("PRAGMA busy_timeout=10000")
    return conn


def init(cfg: Config) -> None:
    os.makedirs(os.path.dirname(cfg.db_path) or ".", exist_ok=True)
    os.makedirs(cfg.attachments_dir, exist_ok=True)
    with connect(cfg) as conn:
        conn.executescript(SCHEMA)
        migrate(conn)
        ensure_system(conn)


def migrate(conn: sqlite3.Connection) -> None:
    """Additive schema evolution for databases created by an older release."""
    cols = {r["name"] for r in conn.execute("PRAGMA table_info(threads)")}
    if "locked" not in cols:  # threads gained the locked flag in 0.2
        conn.execute("ALTER TABLE threads ADD COLUMN locked INTEGER NOT NULL DEFAULT 0")
        conn.commit()


def get_meta(conn: sqlite3.Connection, key: str, default: str | None = None) -> str | None:
    row = conn.execute("SELECT value FROM meta WHERE key = ?", [key]).fetchone()
    return row["value"] if row else default


def set_meta(conn: sqlite3.Connection, key: str, value: str) -> None:
    conn.execute("INSERT INTO meta (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", [key, value])


def ensure_system(conn: sqlite3.Connection) -> None:
    """Create (or refresh) the service's own :data:`ADMIN_NAME` account. Idempotent."""
    ts = now()
    conn.execute(
        "INSERT INTO agents (name, low, descr, created, seen) VALUES (?,?,?,?,?) ON CONFLICT(low) DO UPDATE SET descr = excluded.descr",
        [ADMIN_NAME, ADMIN_NAME.lower(), SYSTEM_DESCR, ts, ts],
    )
    conn.commit()


@contextmanager
def reader(cfg: Config) -> Iterator[sqlite3.Connection]:
    """Plain read connection (no write transaction) for ops that cannot mutate."""
    conn = connect(cfg)
    try:
        yield conn
    finally:
        conn.close()


@contextmanager
def session(cfg: Config) -> Iterator[sqlite3.Connection]:
    """One transactional unit of work: commits on success, rolls back on error."""
    conn = connect(cfg)
    try:
        conn.execute("BEGIN IMMEDIATE")
        yield conn
        conn.execute("COMMIT")
    except BaseException:
        try:
            conn.execute("ROLLBACK")
        except sqlite3.Error:  # pragma: no cover - rollback of a dead txn
            pass
        raise
    finally:
        conn.close()


def like_arg(term: str) -> str:
    """Escape LIKE wildcards so user text is matched literally (pair with ``ESCAPE '\\'``)."""
    return "%" + term.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_") + "%"


# --- SQL composition helpers ---------------------------------------------------------------
#
# Values never become SQL: they are always bound parameters. The only dynamic text allowed inside
# a statement is built here, from constant fragments, so the AST audit in tests/test_sanitize.py
# can whitelist exactly these names and reject interpolation anywhere else.


def where(clauses: Sequence[str]) -> str:
    """Join constant filter fragments (``"t.id > ?"``) into a WHERE clause; values stay bound."""
    return "WHERE " + " AND ".join(clauses) if clauses else ""


def marks(values: Sequence[Any] | int) -> str:
    """``?, ?, ?`` placeholders for an ``IN (…)`` list of bound parameters."""
    return ",".join("?" * (values if isinstance(values, int) else len(values)))


def desc(flag: object) -> str:
    """Sort direction from a boolean - both branches are constants."""
    return "DESC" if flag else "ASC"


def sort_expr(table: dict[str, str], key: str) -> str:
    """Look an ORDER BY expression up in a caller-owned constant table; never accept caller text."""
    try:
        return table[key]
    except KeyError as exc:  # pragma: no cover - callers validate first
        raise ValueError(f"unknown sort key {key!r}") from exc
