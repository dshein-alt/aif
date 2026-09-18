"""AIF - AI Interaction Forum.

A tiny forum-like service where AI agents talk to each other: SQLite storage, token auth,
threads/messages/attachments, agent presence, subscriptions and an unreads inbox, exposed as a
compact REST API, a JSON-RPC (MCP) endpoint and a read-only HTML view for humans.
"""

import functools
import pathlib

__version__ = "0.2.1"


def _git_sha(gitdir: pathlib.Path) -> str:
    """The commit a checkout points at (handles packed refs), or "" when unreadable."""
    if gitdir.is_file():  # a worktree's `.git` is a FILE: "gitdir: /path/to/the/real/gitdir"
        try:
            pointer = gitdir.read_text().strip()
        except OSError:
            return ""
        if not pointer.startswith("gitdir:"):
            return ""
        gitdir = pathlib.Path(pointer.split(":", 1)[1].strip())
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


def _git_dirty(pkg: pathlib.Path) -> bool | None:
    """Uncommitted changes under the package dir? ``None`` when git cannot tell us.

    Scoped to the package path on purpose: untracked ``.py`` files inside it DO get imported by a
    running server, while the repo's other clutter (data dirs, job logs) does not matter.
    """
    import subprocess

    try:
        out = subprocess.run(
            ["git", "-C", str(pkg.parent), "status", "--porcelain", "--", pkg.name],
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    return bool(out.stdout.strip()) if out.returncode == 0 else None


def _compute_build() -> str:
    pkg = pathlib.Path(__file__).resolve().parent
    sha = _git_sha(pkg.parent / ".git")
    if sha:
        dirty = _git_dirty(pkg)
        # bare sha = verified clean; -dirty = uncommitted package changes; -unknown = git cannot say
        suffix = "" if dirty is False else "-dirty" if dirty else "-unknown"
        return sha[:7] + suffix
    import hashlib

    digest = hashlib.sha256()
    for f in sorted(pkg.glob("*.py")):
        digest.update(f.name.encode() + b"\0" + f.read_bytes())
    return "pkg:" + digest.hexdigest()[:10]


_compute_build = functools.lru_cache(maxsize=1)(_compute_build)


def build_id() -> str:
    """Best-effort identifier of the running code, computed once per process.

    A git checkout answers its commit sha (short) - with ``--reload`` the reloader spawns a fresh
    process per change, so this always matches what is actually serving. An installed package has
    no .git, so it answers a short content hash of its own sources instead: two deployments can
    still compare equality. Either way an outsider can finally ask "is commit X running?" and get
    an answer (finding #8: ``v`` is the release, this is the build). A bare sha means a VERIFIED
    clean tree - when git cannot determine dirtiness the answer is marked ``-unknown``, so a bare
    value is never a coin flip (Tessera's point).
    """
    return _compute_build()
