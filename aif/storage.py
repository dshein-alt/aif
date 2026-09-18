"""Attachment blobs on disk.

The database stores only metadata plus a generated uuid (``key``); the bytes live in
``cfg.attachments_dir/<key[:2]>/<key>`` so the whole folder can sit on a volume.
"""

from __future__ import annotations

import hashlib
import os
import re
import shutil
import uuid
from typing import BinaryIO

from . import sanitize
from .config import Config

CHUNK = 256 * 1024
_EXT_RE = re.compile(r"[^a-z0-9]")


class StorageError(RuntimeError):
    pass


class TooLarge(StorageError):
    pass


def sanitize_name(name: object, limit: int = 200) -> str:
    """Keep an uploaded name displayable: basename, no control/invisible chars, bounded length.

    Display only - blobs are always named by a generated uuid, never by client text.
    """
    raw = sanitize.fold(name).replace("\\", "/").rsplit("/", 1)[-1]
    return " ".join(raw.split())[:limit] or "file"


def sanitize_ext(name: str, limit: int = 8) -> str:
    ext = os.path.splitext(name)[1].lstrip(".").lower()[:limit]
    ext = _EXT_RE.sub("", ext)
    return f".{ext}" if ext else ""


def new_key() -> str:
    return uuid.uuid4().hex


def blob_path(cfg: Config, key: str) -> str:
    """Absolute path of a blob; rejects anything that is not a generated key."""
    if not re.fullmatch(r"[0-9a-f]{32}", key):
        raise StorageError(f"invalid blob key: {key!r}")
    return os.path.join(cfg.attachments_dir, key[:2], key)


def save(cfg: Config, stream: BinaryIO, key: str, limit: int) -> tuple[int, str]:
    """Stream ``stream`` into the blob store. Returns ``(size, sha256)``.

    Raises :class:`TooLarge` (and removes the partial blob) when ``limit`` is exceeded.
    """
    path = blob_path(cfg, key)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    digest = hashlib.sha256()
    size = 0
    try:
        with open(path, "wb") as fh:
            while True:
                block = stream.read(CHUNK)
                if not block:
                    break
                size += len(block)
                if size > limit:
                    raise TooLarge(f"file exceeds limit of {limit} bytes")
                digest.update(block)
                fh.write(block)
    except BaseException:
        shutil.rmtree(os.path.dirname(path), ignore_errors=True) if not os.path.isdir(path) else None
        os.remove(path) if os.path.exists(path) else None
        raise
    return size, digest.hexdigest()


def open_blob(cfg: Config, key: str) -> BinaryIO:
    path = blob_path(cfg, key)
    if not os.path.isfile(path):
        raise StorageError(f"blob missing for key {key}")
    return open(path, "rb")


def remove(cfg: Config, key: str) -> bool:
    """Delete a blob, tolerating an already missing file."""
    path = blob_path(cfg, key)
    try:
        os.remove(path)
        return True
    except FileNotFoundError:
        return False
    except OSError as exc:  # pragma: no cover - depends on host FS
        raise StorageError(f"cannot delete blob {key}: {exc}") from exc


def stats(cfg: Config) -> dict[str, int]:
    total = 0
    count = 0
    for dirpath, _, files in os.walk(cfg.attachments_dir):
        for name in files:
            try:
                total += os.path.getsize(os.path.join(dirpath, name))
                count += 1
            except OSError:  # pragma: no cover - race with a purge
                continue
    return {"blobs": count, "bytes": total}
