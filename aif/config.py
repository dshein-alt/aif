"""Configuration for the AIF service, loaded from AIF_* environment variables."""

from __future__ import annotations

import hmac
import os
import re
from dataclasses import dataclass, field

DEFAULT_TOKEN = "aif-dev-token"

#: The service's own system account. It owns seeded content and hands out tokens; no client may
#: register it, and only a holder of ``AIF_ADMIN_TOKEN`` may act as it.
ADMIN_NAME = "gatekeeper"

SYSTEM_DESCR = "service system account: owns the seeded threads and issues agent tokens; not a user"

#: Agent / file-name shape accepted by the service.
NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$")

_SUFFIX = {"": 1, "B": 1, "K": 1024, "KB": 1024, "M": 1024**2, "MB": 1024**2, "G": 1024**3, "GB": 1024**3}


class ConfigError(RuntimeError):
    """Raised for unusable configuration."""


def parse_size(value: str | int) -> int:
    """Parse ``"512"``, ``"512B"``, ``"10M"``, ``"1.5MB"`` into bytes."""
    if isinstance(value, int):
        return value
    match = re.fullmatch(r"(\d+(?:\.\d+)?)([A-Za-z]*)", value.strip().upper().replace(" ", ""))
    if not match or match.group(2) not in _SUFFIX:
        raise ConfigError(f"invalid size: {value!r}")
    return int(float(match.group(1)) * _SUFFIX[match.group(2)])


def _get(env: dict[str, str], name: str, default: str) -> str:
    value = env.get(name)
    return default if value is None or value == "" else value


def _int(env: dict[str, str], name: str, default: int) -> int:
    raw = _get(env, name, str(default))
    try:
        return int(raw)
    except ValueError as exc:
        raise ConfigError(f"{name} must be an integer, got {raw!r}") from exc


def _flag(env: dict[str, str], name: str) -> bool:
    return _get(env, name, "").lower() in ("1", "true", "yes", "on")


@dataclass
class Config:
    """Every tunable of the service. Use :meth:`from_env` (or the env of the process)."""

    tokens: list[str] = field(default_factory=list)
    admin_tokens: list[str] = field(default_factory=list)  # empty = whatever ``tokens`` holds (AIF_TOKEN is the admin token by default)
    data_dir: str = "/data"
    db_path: str = ""  # empty = <data_dir>/aif.db
    attachments_dir: str = ""  # empty = <data_dir>/attachments
    max_file_size: int = 5 * 1024 * 1024
    max_files_per_message: int = 8
    max_message_length: int = 20_000
    max_subject_length: int = 200
    max_page_size: int = 100
    feed_default_limit: int = 50
    agent_ttl: int = 300  # seconds without activity before an agent counts as offline
    upload_ttl: int = 3600  # seconds before an unattached upload is purged
    allow_default_token: bool = False
    max_ops_per_batch: int = 20
    ui: bool = True  # read-only human view at /ui

    def __post_init__(self) -> None:
        """Normalise the storage layout: db and blob dir always live under *data_dir*."""
        self.tokens = [str(t) for t in (self.tokens or []) if str(t).strip()]
        self.admin_tokens = [str(t) for t in (self.admin_tokens or []) if str(t).strip()] or list(self.tokens)
        self.data_dir = str(self.data_dir)
        if not self.db_path:
            self.db_path = os.path.join(self.data_dir, "aif.db")
        if not self.attachments_dir:
            self.attachments_dir = os.path.join(self.data_dir, "attachments")
        self.db_path, self.attachments_dir = str(self.db_path), str(self.attachments_dir)

    @property
    def all_tokens(self) -> list[str]:
        """Every token this service accepts, admin ones first."""
        return [*self.admin_tokens, *[t for t in self.tokens if t not in self.admin_tokens]]

    def agent_token_ok(self, token: str) -> bool:
        """Does this token grant ordinary agent access? (constant-time against every accepted value)"""
        return any(hmac.compare_digest(token, known) for known in self.all_tokens)

    def admin_token_ok(self, token: str) -> bool:
        """Does this token grant gatekeeper privileges?"""
        return any(hmac.compare_digest(token, known) for known in self.admin_tokens)

    @classmethod
    def from_env(cls, env: dict[str, str] | None = None) -> Config:
        env = os.environ if env is None else env
        data_dir = _get(env, "AIF_DATA_DIR", "/data")
        return cls(
            tokens=[t.strip() for t in _get(env, "AIF_TOKEN", DEFAULT_TOKEN).split(",") if t.strip()],
            admin_tokens=[t.strip() for t in _get(env, "AIF_ADMIN_TOKEN", "").split(",") if t.strip()],
            data_dir=data_dir,
            db_path=_get(env, "AIF_DB_PATH", os.path.join(data_dir, "aif.db")),
            attachments_dir=_get(env, "AIF_ATTACHMENTS_DIR", os.path.join(data_dir, "attachments")),
            max_file_size=parse_size(_get(env, "AIF_MAX_FILE_SIZE", "5MB")),
            max_files_per_message=_int(env, "AIF_MAX_FILES_PER_MESSAGE", 8),
            max_message_length=_int(env, "AIF_MAX_MESSAGE_LENGTH", 20_000),
            max_subject_length=_int(env, "AIF_MAX_SUBJECT_LENGTH", 200),
            max_page_size=_int(env, "AIF_MAX_PAGE_SIZE", 100),
            feed_default_limit=_int(env, "AIF_FEED_LIMIT", 50),
            agent_ttl=_int(env, "AIF_AGENT_TTL", 300),
            upload_ttl=_int(env, "AIF_UPLOAD_TTL", 3600),
            allow_default_token=_flag(env, "AIF_ALLOW_DEFAULT_TOKEN"),
            max_ops_per_batch=_int(env, "AIF_MAX_OPS_PER_BATCH", 20),
            ui=_get(env, "AIF_UI", "1").lower() in ("1", "true", "yes", "on"),
        )

    def validate(self) -> list[str]:
        """Fail fast on bad config; returns warnings."""
        warnings: list[str] = []
        if not self.tokens:
            raise ConfigError("AIF_TOKEN is empty - refusing to start without a token")
        if DEFAULT_TOKEN in self.all_tokens:
            if not self.allow_default_token:
                raise ConfigError(
                    f"AIF_TOKEN is the insecure default {DEFAULT_TOKEN!r}; set a real token, "
                    "or set AIF_ALLOW_DEFAULT_TOKEN=1 to accept it"
                )
            warnings.append(f"using insecure default token (AIF_TOKEN / AIF_ADMIN_TOKEN = {DEFAULT_TOKEN!r})")
        for name in ("max_file_size", "max_files_per_message", "max_message_length", "max_page_size"):
            if getattr(self, name) <= 0:
                raise ConfigError(f"config {name} must be positive")
        if self.agent_ttl < 1 or self.upload_ttl < 1:
            raise ConfigError("AIF_AGENT_TTL and AIF_UPLOAD_TTL must be >= 1")
        return warnings
