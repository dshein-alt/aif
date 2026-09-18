"""Per-agent tokens: derivation, issuance, lookup and cascade revocation.

The gatekeeper is a *config* token (``AIF_ADMIN_TOKEN``) and has no row here; everything it issues
becomes a tree root. Every claimed token holder may issue children under its own token, and any
ancestor (or the gatekeeper) may revoke an entire subtree - a parent answers for everything below it.

Token shape: ``aif_`` + 24 hex chars of ``sha256(salt \\0 name \\0 nonce)``. Tokens are stored
cleartext (they grant what they grant), never logged, and only ever *returned* at issue/claim time.
An invite carries no name: the holder registers a name with it, and the server then rewrites the
row to the final name-derived token, which is what the agent uses from then on.
"""

from __future__ import annotations

import hashlib
import secrets
import sqlite3
from typing import Any

from . import db
from .config import Config

TOKEN_LEN = 24


def derive_token(salt: str, name: str, nonce: str = "") -> str:
    """Deterministic token for a (name, nonce) pair under this server's salt."""
    digest = hashlib.sha256(f"{salt}\0{name}\0{nonce}".encode()).hexdigest()
    return f"aif_{digest[:TOKEN_LEN]}"


def new_nonce() -> str:
    return secrets.token_hex(8)


def lookup(conn: sqlite3.Connection, token: str) -> dict[str, Any] | None:
    row = conn.execute("SELECT * FROM tokens WHERE self_token = ?", [token]).fetchone()
    return dict(row) if row else None


def issue(cfg: Config, conn: sqlite3.Connection, issuer: dict[str, Any] | None, name: str = "", descr: str = "", days: float | None = None) -> dict[str, Any]:
    """Create a token row. ``issuer=None`` means the gatekeeper (a root); otherwise the issuer's
    row, so the child hangs under it. Named tokens skip the name-choice step but still get claimed
    on first use."""
    nonce = new_nonce()
    token = derive_token(cfg.token_salt, name, nonce)
    now = db.now()
    if issuer is None:  # a tree root: parent == root == self
        root = parent = token
    else:
        parent, root = issuer["self_token"], issuer["root_token"]
    exp = now + days * 86400 if days else (0.0 if name else now + cfg.invite_ttl)
    conn.execute(
        "INSERT INTO tokens (name, low, root_token, parent_token, self_token, descr, created, exp, nonce) VALUES (?,?,?,?,?,?,?,?,?)",
        [name, name.lower(), root, parent, token, descr, now, exp, nonce],
    )
    return {"token": token, "name": name, "parent": parent, "root": root, "exp": exp, "nonce": nonce}


def claim(cfg: Config, conn: sqlite3.Connection, row: dict[str, Any], name: str) -> str:
    """Bind *name* to a token row and return the final, name-derived token.

    The invite form of the token stops existing here: self_token is rewritten to
    ``derive(salt, name, nonce)`` (root tokens keep root == parent == self). No other row can
    reference the old value: children require a claimed parent, so none exist yet.
    """
    final = derive_token(cfg.token_salt, name, row["nonce"])
    if row["root_token"] == row["self_token"]:  # a root: its own root/parent pointers move too
        conn.execute(
            "UPDATE tokens SET name = ?, low = ?, self_token = ?, claimed = ?, root_token = ?, parent_token = ? WHERE self_token = ?",
            [name, name.lower(), final, db.now(), final, final, row["self_token"]],
        )
    else:
        conn.execute(
            "UPDATE tokens SET name = ?, low = ?, self_token = ?, claimed = ? WHERE self_token = ?",
            [name, name.lower(), final, db.now(), row["self_token"]],
        )
    return final


def check_live(conn: sqlite3.Connection, row: dict[str, Any] | None, code_unknown: str = "bad_token") -> dict[str, Any]:
    """Raise the precise reason a token row is unusable, or hand it back."""
    from .core import ApiError  # local import: core imports this module's helpers

    if row is None:
        raise ApiError(403, code_unknown, "access token rejected", "use the server's AIF_TOKEN value, or an issued agent token")
    if row["revoked"]:
        raise ApiError(403, "token_revoked", "this token was revoked", "ask your issuer (or the gatekeeper) for a fresh one")
    if row["exp"] and row["exp"] < db.now():
        if row["claimed"]:
            raise ApiError(403, "token_expired", "this token expired", "ask your issuer (or the gatekeeper) for a fresh one")
        raise ApiError(403, "invite_expired", "this invite was never claimed and expired", "ask your issuer for a fresh invite")
    return row


def children_of(conn: sqlite3.Connection, self_token: str) -> list[dict[str, Any]]:
    return [dict(r) for r in conn.execute("SELECT * FROM tokens WHERE parent_token = ? AND self_token != parent_token", [self_token])]


def subtree(conn: sqlite3.Connection, self_token: str) -> list[dict[str, Any]]:
    """The node itself plus every descendant, following parent_token downward."""
    me = lookup(conn, self_token)
    out: dict[str, dict[str, Any]] = {self_token: me} if me else {}
    frontier = [self_token]
    while frontier:
        batch = frontier
        frontier = []
        for row in conn.execute(f"SELECT * FROM tokens WHERE parent_token IN ({db.marks(batch)})", batch):
            if row["self_token"] in out:
                continue
            out[row["self_token"]] = dict(row)
            frontier.append(row["self_token"])
    return list(out.values())


def is_ancestor(conn: sqlite3.Connection, ancestor_token: str, target: dict[str, Any]) -> bool:
    """Walk *target*'s parent chain to its root; True if *ancestor_token* shows up on the way."""
    seen: set[str] = set()
    current = target
    while True:
        if current["self_token"] == ancestor_token:
            return True
        if current["parent_token"] in (current["self_token"],) or current["self_token"] in seen:
            return False
        seen.add(current["self_token"])
        parent = lookup(conn, current["parent_token"])
        if parent is None:
            return False
        current = parent


def revoke_subtree(conn: sqlite3.Connection, target: dict[str, Any]) -> list[str]:
    """Cascade: mark the target and its whole subtree revoked; returns the affected token rows' ids."""
    nodes = subtree(conn, target["self_token"])
    now = db.now()
    for node in nodes:
        if not node["revoked"]:
            conn.execute("UPDATE tokens SET revoked = ? WHERE self_token = ?", [now, node["self_token"]])
    return [n["self_token"] for n in nodes]
