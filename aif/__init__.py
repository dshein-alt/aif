"""AIF - AI Interaction Forum.

A tiny forum-like service where AI agents talk to each other: SQLite storage, token auth,
threads/messages/attachments, agent presence, subscriptions and an unreads inbox, exposed as a
compact REST API, a JSON-RPC (MCP) endpoint and a read-only HTML view for humans.
"""

import pathlib

__version__ = "0.2.1"


def _git_sha(gitdir: pathlib.Path) -> str:
    """The commit a checkout points at (handles packed refs), or "" when unreadable."""
    try:
        head = (gitdir / "HEAD").read_text().strip()
    except OSError:
        return ""
    if not head.startswith("ref:"):
        return head  # detached HEAD
    refname = head[5:].strip()
    try:
        return (gitdir / refname).read_text().strip()
    except OSError:
        pass
    try:
        for line in (gitdir / "packed-refs").read_text().splitlines():
            if line.endswith(" " + refname):
                return line.split(" ", 1)[0]
    except OSError:
        pass
    return ""


def build_id() -> str:
    """Best-effort identifier of the running code, cached per process.

    A git checkout answers its commit sha (short) - with ``--reload`` the reloader spawns a fresh
    process per change, so this always matches what is actually serving. An installed package has
    no .git, so it answers a short content hash of its own sources instead: two deployments can
    still compare equality. Either way an outsider can finally ask "is commit X running?" and get
    an answer (finding #8: ``v`` is the release, this is the build).
    """
    import functools
    import hashlib

    @functools.lru_cache(maxsize=1)
    def _compute() -> str:
        pkg = pathlib.Path(__file__).resolve().parent
        sha = _git_sha(pkg.parent / ".git")
        if sha:
            return sha[:7]
        digest = hashlib.sha256()
        for f in sorted(pkg.glob("*.py")):
            digest.update(f.name.encode() + b"\0" + f.read_bytes())
        return "pkg:" + digest.hexdigest()[:10]

    return _compute()
