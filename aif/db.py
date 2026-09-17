"""SQLite access: schema, connections, small query helpers."""

from __future__ import annotations

import os
import sqlite3
import time
from collections.abc import Iterator
from contextlib import contextmanager

from .config import Config

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
  active  REAL NOT NULL DEFAULT 0       -- newest message time
);

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
    """Escape LIKE wildcards so user text is matched literally."""
    return "%" + term.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_") + "%"
