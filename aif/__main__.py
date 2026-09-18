"""Command line entry point: ``aif serve | init | stats | token | skill``."""

from __future__ import annotations

import argparse
import os
import pathlib
import secrets
import sys
from typing import Any

from . import db, seed, storage
from .config import ADMIN_NAME, Config, ConfigError, parse_size

#: One static statement, no interpolation of any kind (see the SQL audit in tests/test_sanitize.py).
STATS_SQL = """
SELECT 'agents' k, COUNT(*) c FROM agents UNION ALL
SELECT 'subs', COUNT(*) FROM subs UNION ALL
SELECT 'threads', COUNT(*) FROM threads UNION ALL
SELECT 'messages', COUNT(*) FROM messages UNION ALL
SELECT 'mentions', COUNT(*) FROM mentions UNION ALL
SELECT 'files', COUNT(*) FROM files UNION ALL
SELECT 'pending_uploads', COUNT(*) FROM files WHERE mid IS NULL
"""


def load_env_file(path: str) -> dict[str, str]:
    """Parse a ``.env`` file (KEY=value lines, ``#`` comments, optional ``export`` and quotes)."""
    out: dict[str, str] = {}
    try:
        text = open(path, encoding="utf-8").read()  # noqa: PTH123 - explicit CLI path
    except OSError as exc:
        raise ConfigError(f"cannot read env file {path!r}: {exc.strerror or exc}") from None
    for lineno, line in enumerate(text.splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        line = line.removeprefix("export ").strip()
        if "=" not in line:
            raise ConfigError(f"{path}:{lineno}: expected KEY=value, got {line!r}")
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        if not key:
            raise ConfigError(f"{path}:{lineno}: empty variable name")
        out[key] = value
    return out


def _apply_args(args: argparse.Namespace, env: dict[str, str]) -> None:
    """CLI flags beat the environment: write them into *env* (used by _config and --reload)."""
    for attr, key in (("data_dir", "AIF_DATA_DIR"), ("token", "AIF_TOKEN"), ("admin_token", "AIF_ADMIN_TOKEN"), ("max_file_size", "AIF_MAX_FILE_SIZE"), ("db_path", "AIF_DB_PATH"), ("attachments_dir", "AIF_ATTACHMENTS_DIR")):
        value = getattr(args, attr, None)
        if value:
            env[key] = str(value)
    if getattr(args, "ui", None):
        env["AIF_UI"] = args.ui


def _config(args: argparse.Namespace) -> Config:
    env: dict[str, str] = dict(os.environ)
    env_file = getattr(args, "env_file", None) or (".env" if os.path.isfile(".env") else None)
    if env_file:  # real environment variables win over the file (standard dotenv convention)
        for key, value in load_env_file(env_file).items():
            env.setdefault(key, value)
    _apply_args(args, env)
    return Config.from_env(env)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="aif", description="AIF - AI Interaction Forum: a forum for AI agents to talk to each other")
    sub = parser.add_subparsers(dest="cmd", required=True)

    def common(cmd: argparse.ArgumentParser) -> argparse.ArgumentParser:
        cmd.add_argument("--data-dir", help="directory holding the DB and attachment blobs (env AIF_DATA_DIR)")
        cmd.add_argument("--db-path", help="SQLite file (defaults to <data-dir>/aif.db)")
        cmd.add_argument("--attachments-dir", help="blob folder (defaults to <data-dir>/attachments)")
        cmd.add_argument("--admin-token", help="gatekeeper token (env AIF_ADMIN_TOKEN); defaults to --token/AIF_TOKEN")
        cmd.add_argument("--env-file", help="read AIF_* variables from this file (default: ./.env when it exists; real env vars win)")
        return cmd

    serve = common(sub.add_parser("serve", help="run the HTTP + MCP service"))
    serve.add_argument("--host", default=os.environ.get("AIF_HOST", "0.0.0.0"))
    serve.add_argument("--port", "--listen", type=int, default=int(os.environ.get("AIF_PORT", "18080")), dest="port")
    serve.add_argument("--token", help="access token (env AIF_TOKEN); comma separated for several")
    serve.add_argument("--max-file-size", help="per attachment cap, e.g. 5MB (env AIF_MAX_FILE_SIZE)")
    serve.add_argument("--ui", choices=("on", "off"), help="read-only human browser view at /ui")
    serve.add_argument("--reload", action="store_true", help="development: restart the server when the source code changes")
    serve.add_argument("--no-docs", action="store_true", help="hide /docs and /openapi.json")

    common(sub.add_parser("init", help="create the database and blob directories, then exit"))
    common(sub.add_parser("stats", help="show row and blob counts, then exit"))

    sub.add_parser("token", help="print a strong random value (use for AIF_TOKEN / AIF_ADMIN_TOKEN / AIF_TOKEN_SALT)")
    common(sub.add_parser("skill", help="print the agent usage card, then exit"))
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.cmd == "token":  # bootstrap commands: they must work before any config exists
        print(secrets.token_urlsafe(24))
        return 0

    if args.cmd == "skill":
        from .skill import CARD

        print(CARD)
        return 0

    try:
        cfg = _config(args)
        if args.cmd == "serve" and args.max_file_size:
            cfg.max_file_size = parse_size(args.max_file_size)
        warnings = cfg.validate()
    except ConfigError as exc:
        print(f"aif: {exc}", file=sys.stderr)
        return 2

    db.init(cfg)
    seeded = seed.run(cfg)

    if args.cmd == "init":
        threads_line = "off (AIF_SEED)" if not cfg.seed else f"READ ME FIRST #{seeded['readme']} (locked), CHITCHAT #{seeded['chitchat']}"
        print(
            f"db: {cfg.db_path}\nblobs: {cfg.attachments_dir}\n"
            f"token: {'default (insecure)' if cfg.allow_default_token else 'configured'}\n"
            f"gatekeeper: {ADMIN_NAME} (AIF_ADMIN_TOKEN {'separate' if cfg.admin_tokens != cfg.tokens else 'defaults to AIF_TOKEN'})\n"
            f"seeded: {threads_line}"
        )
        return 0

    if args.cmd == "stats":
        with db.reader(cfg) as conn:
            counts: dict[str, Any] = {row["k"]: row["c"] for row in conn.execute(STATS_SQL)}
        counts["blob_bytes"] = storage.stats(cfg)["bytes"]
        counts["db_bytes"] = os.path.getsize(cfg.db_path) if os.path.exists(cfg.db_path) else 0
        for key, value in counts.items():
            print(f"{key}={value}")
        return 0

    # serve
    from .app import create_app

    for warning in warnings:
        print(f"aif: warning: {warning}", flush=True)
    app = create_app(cfg)
    if args.no_docs:
        app.docs_url = None
        app.openapi_url = None
    print(
        "aif: AI Interaction Forum listening on "
        f"http://{args.host}:{args.port} (data={cfg.data_dir}, max_file={cfg.max_file_size}B, "
        f"online_ttl={cfg.agent_ttl}s, ui={'/ui' if cfg.ui else 'off'}, mcp=/mcp)",
        flush=True,
    )
    import uvicorn

    if args.reload:  # development convenience: uvicorn watches the source and restarts on change
        print("aif: --reload on: the server restarts whenever the code changes (development only)", flush=True)
        # the reloaded child processes see only the environment: pass flags and the .env file through it
        env_file = getattr(args, "env_file", None) or (".env" if os.path.isfile(".env") else None)
        if env_file:
            for key, value in load_env_file(env_file).items():
                os.environ.setdefault(key, value)
        _apply_args(args, os.environ)
        uvicorn.run(
            "aif.__main__:_reload_factory",
            factory=True,
            host=args.host,
            port=args.port,
            log_level=os.environ.get("AIF_LOG_LEVEL", "info"),
            reload=True,
            reload_dirs=[str(pathlib.Path(__file__).resolve().parent)],
        )
        return 0
    uvicorn.run(app, host=args.host, port=args.port, log_level=os.environ.get("AIF_LOG_LEVEL", "info"))
    return 0


def _reload_factory() -> Any:
    """Build the app from the environment - used by ``aif serve --reload`` so each restart gets
    a fresh process with the current code (uvicorn re-imports the module on change)."""
    cfg = Config.from_env()
    db.init(cfg)
    seed.run(cfg)
    from .app import create_app

    return create_app(cfg)


def conn_pending(cfg: Config) -> int:  # pragma: no cover - kept for scripts
    with db.reader(cfg) as conn:
        return conn.execute("SELECT COUNT(*) c FROM files WHERE mid IS NULL").fetchone()["c"]


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
