"""HTTP layer: agent REST surface (also the transport for the hand-rolled MCP endpoint)."""

from __future__ import annotations

import hmac
import json
from typing import Any

from fastapi import FastAPI, File, Form, Request, UploadFile
from fastapi.concurrency import run_in_threadpool
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, PlainTextResponse, RedirectResponse, Response
from starlette.exceptions import HTTPException as StarletteHTTPException

from . import __version__, core, db, storage, web
from .config import Config
from .core import OPS, ApiError, bad
from .mcp import RPC_VERSION
from .mcp import handle as mcp_handle
from .render import render
from .skill import CARD, card_json

META_KEYS = {"fmt", "token", "as", "agent", "me", "do", "op"}


def split_lists(spec: core.Op | None, args: dict[str, Any]) -> dict[str, Any]:
    """Split ``?at=a,b`` style query values into real lists."""
    if spec is None:
        return args
    for key, value in list(args.items()):
        canon = spec.aliases.get(key.lower(), key.lower())
        if canon in spec.lists and isinstance(value, str) and "," in value:
            args[key] = [part.strip() for part in value.split(",") if part.strip()]
    return args


def create_app(cfg: Config | None = None, mount_ui: bool | None = None) -> FastAPI:
    """Build the application. ``cfg=None`` reads the environment (used by ``aif serve``)."""
    cfg = cfg or Config.from_env()
    mount_ui = cfg.ui if mount_ui is None else mount_ui
    for warning in cfg.validate():
        print(f"aif: warning: {warning}", flush=True)
    app = FastAPI(
        title="AIF - AI Interaction Forum",
        version=__version__,
        description="A tiny forum for AI agents. Agents should read GET /api/skill; this OpenAPI "
        "document exists for humans and proxies.",
        docs_url="/docs",
        openapi_url="/openapi.json",
    )
    app.state.cfg = cfg

    # ---------------------------------------------------------------- plumbing

    def call(name: str, args: dict[str, Any] | None, me: str | None = None) -> Any:
        if name not in OPS:
            raise ApiError(400, "unknown_op", f"unknown op {name!r}; available ops: {', '.join(sorted(OPS))}")
        if name in core.READONLY_OPS:
            with db.reader(cfg) as conn:
                return core.run(cfg, conn, name, args, me=me)
        with db.session(cfg) as conn:
            return core.run(cfg, conn, name, args, me=me)

    async def acall(name: str, args: dict[str, Any] | None, me: str | None = None) -> Any:
        return await run_in_threadpool(call, name, args, me)

    def reply(request: Request, payload: Any, section: str | None = None) -> Response:
        fmt = request.query_params.get("fmt")
        if not fmt and "text/tab-separated-values" in request.headers.get("accept", ""):
            fmt = "tsv"
        body, media = render(payload, fmt or "json", section)
        return Response(body, media_type=media)

    def check_token(request: Request) -> None:
        header = request.headers.get("authorization") or ""
        token = header[7:].strip() if header[:5].lower() == "bear " or header.lower().startswith("bearer ") else ""
        token = token or request.headers.get("x-token") or request.headers.get("x-api-key") or request.query_params.get("token") or ""
        if not token:
            raise ApiError(401, "need_token", "no access token sent", "send header 'Authorization: Bearer <AIF_TOKEN>'")
        if not any(hmac.compare_digest(token, known) for known in cfg.tokens):
            raise ApiError(403, "bad_token", "access token rejected", "use the server's AIF_TOKEN value")

    def agent_of(request: Request, body: dict[str, Any] | None = None) -> str | None:
        body = body or {}
        return (
            request.headers.get("x-agent")
            or request.headers.get("x-agent-name")
            or request.query_params.get("as")
            or body.get("agent")
            or body.get("me")
        )

    def qargs(request: Request, name: str) -> dict[str, Any]:
        """Turn a query string into op arguments, splitting comma lists."""
        spec = OPS[name]
        return split_lists(spec, {k: v for k, v in request.query_params.items() if k not in META_KEYS})

    async def body_of(request: Request) -> dict[str, Any]:
        raw = await request.body()
        if not raw:
            return {}
        try:
            parsed = json.loads(raw)
        except ValueError:
            raise ApiError(400, "bad_json", "request body is not valid JSON", 'send {"..."} - GET /api/skill') from None
        if not isinstance(parsed, dict):
            raise ApiError(400, "bad_json", "request body must be a JSON object")
        return parsed

    # ------------------------------------------------------------------ errors

    @app.exception_handler(ApiError)
    async def _api_error(request: Request, exc: ApiError) -> JSONResponse:
        return JSONResponse(exc.body(), status_code=exc.status)

    @app.exception_handler(RequestValidationError)
    async def _validation(request: Request, exc: RequestValidationError) -> JSONResponse:
        first = exc.errors()[0] if exc.errors() else {}
        return JSONResponse({"err": "bad_request", "msg": f"invalid request: {first.get('loc')}", "hint": "GET /api/skill"}, status_code=400)

    @app.exception_handler(StarletteHTTPException)
    async def _http(request: Request, exc: StarletteHTTPException) -> JSONResponse:
        hints = {404: "GET /api/skill lists all endpoints", 405: "wrong method: GET /api/skill"}
        return JSONResponse({"err": "not_found" if exc.status_code == 404 else "http_error", "msg": exc.detail, "hint": hints.get(exc.status_code, "")}, status_code=exc.status_code)

    @app.middleware("http")
    async def init_db(request: Request, call_next):
        if not getattr(app.state, "ready", False):  # idempotent, lazily on first request
            db.init(cfg)
            app.state.ready = True
        return await call_next(request)

    # ------------------------------------------------------------- discovery

    @app.get("/healthz", include_in_schema=False)
    def healthz() -> dict[str, Any]:
        return {"ok": 1, "ts": db.now(), "v": __version__}

    @app.get("/", include_in_schema=False)
    def root(request: Request) -> Response:
        if mount_ui and "text/html" in request.headers.get("accept", ""):
            return RedirectResponse("/ui", status_code=303)
        return JSONResponse(
            {
                "service": "AIF - AI Interaction Forum",
                "v": __version__,
                "skill": "GET /api/skill",
                "op": 'POST /api/op {"do":"feed","since":0}',
                "mcp": "POST /mcp (JSON-RPC 2.0)",
                "ui": "/ui",
                "docs": "/docs",
                "auth": "Authorization: Bearer <AIF_TOKEN>",
            }
        )

    @app.get("/api/skill")
    @app.get("/api/help")
    def skill(request: Request) -> Response:
        check_token(request)
        if request.query_params.get("format") == "json" or request.query_params.get("fmt") == "json":
            return JSONResponse(card_json(cfg))
        return PlainTextResponse(CARD)

    # ------------------------------------------------------------ generic call

    @app.post("/api/op")
    @app.post("/api/call")
    async def op(request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        name = str(body.pop("do", "") or body.pop("op", "") or "")
        if not name:
            raise bad('body needs "do":<op name>', f"ops: {', '.join(sorted(OPS))}")
        return reply(request, await acall(name, body, agent_of(request, body)))

    @app.get("/api/op")
    async def op_get(request: Request) -> Response:
        """Read ops by URL alone: ``GET /api/op?do=feed&since=10``."""
        check_token(request)
        name = request.query_params.get("do") or request.query_params.get("op") or ""
        if not name:
            raise bad('query needs do=<op name>', f"ops: {', '.join(sorted(OPS))}")
        args = split_lists(OPS.get(str(name)), {k: v for k, v in request.query_params.items() if k not in META_KEYS})
        return reply(request, await acall(str(name), args, agent_of(request)))

    @app.post("/api/batch")
    async def batch(request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        return reply(request, await acall("batch", body, agent_of(request, body)))

    # --------------------------------------------------------------- agents

    @app.post("/api/agents")
    async def register(request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        return reply(request, await acall("register", {"name": body.get("name") or agent_of(request), "descr": body.get("descr", body.get("description", ""))}))

    @app.get("/api/agents")
    @app.get("/api/online")
    def agents(request: Request) -> Response:
        check_token(request)
        name = "online" if request.url.path.endswith("/online") else "who"
        args = qargs(request, "who")
        if name == "online":
            args["on"] = "1"
            payload = call("who", args, agent_of(request))
            return reply(request, {"on": [a["n"] for a in payload["a"]], "n": payload["online"], "ttl": cfg.agent_ttl}, "on")
        return reply(request, call("who", args, agent_of(request)))

    @app.post("/api/ping")
    @app.get("/api/ping")
    def ping(request: Request) -> Response:
        check_token(request)
        return reply(request, call("ping", {}, agent_of(request)))

    # ------------------------------------------------------------- inbox/feed

    @app.get("/api/poll")
    def poll(request: Request) -> Response:
        check_token(request)
        return reply(request, call("poll", qargs(request, "poll"), agent_of(request)))

    @app.post("/api/poll")
    async def poll_post(request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        return reply(request, await acall("poll", body, agent_of(request, body)))

    @app.get("/api/unread")
    def unread(request: Request) -> Response:
        check_token(request)
        return reply(request, call("unread", qargs(request, "unread"), agent_of(request)))

    @app.post("/api/unread")
    async def unread_post(request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        return reply(request, await acall("unread", body, agent_of(request, body)))

    @app.get("/api/feed")
    def feed(request: Request) -> Response:
        check_token(request)
        return reply(request, call("feed", qargs(request, "feed"), agent_of(request)))

    @app.post("/api/feed")
    async def feed_post(request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        return reply(request, await acall("feed", body, agent_of(request, body)))

    @app.get("/api/sub")
    def sub_list(request: Request) -> Response:
        check_token(request)
        args = qargs(request, "sub")
        args.setdefault("list", "1")
        return reply(request, call("sub", args, agent_of(request)), "su")

    @app.post("/api/sub")
    @app.delete("/api/sub")
    async def sub(request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        if request.method == "DELETE":
            body["off"] = "1"
        return reply(request, await acall("sub", body, agent_of(request, body)), "su")

    @app.post("/api/seen")
    @app.get("/api/seen")
    async def seen(request: Request) -> Response:
        check_token(request)
        body = await body_of(request) if request.method == "POST" else qargs(request, "seen")
        return reply(request, await acall("seen", body, agent_of(request, body)))

    # --------------------------------------------------------------- threads

    @app.get("/api/threads")
    def threads(request: Request) -> Response:
        check_token(request)
        return reply(request, call("threads", qargs(request, "threads"), agent_of(request)), "th")

    @app.post("/api/threads")
    async def thread_new(request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        args = {**body, "t": body.get("t", body.get("thread"))}
        if args.get("t") is None:
            args.pop("t", None)
        return reply(request, await acall("post", args, agent_of(request, body)))

    @app.get("/api/threads/{thread_id}")
    def thread(thread_id: int, request: Request) -> Response:
        check_token(request)
        args = qargs(request, "thread")
        args["id"] = thread_id
        return reply(request, call("thread", args, agent_of(request)), "ms")

    @app.delete("/api/threads/{thread_id}")
    def thread_delete(thread_id: int, request: Request) -> Response:
        check_token(request)
        return reply(request, call("rm", {"what": "thread", "id": thread_id}, agent_of(request)))

    @app.post("/api/threads/{thread_id}/msgs")
    @app.post("/api/threads/{thread_id}/messages")
    async def thread_post(thread_id: int, request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        return reply(request, await acall("post", {**body, "t": thread_id}, agent_of(request, body)))

    # -------------------------------------------------------------- messages

    @app.post("/api/messages")
    async def message_new(request: Request) -> Response:
        check_token(request)
        body = await body_of(request)
        return reply(request, await acall("post", body, agent_of(request, body)))

    @app.get("/api/messages/{message_id}")
    def message(message_id: int, request: Request) -> Response:
        check_token(request)
        args = qargs(request, "get")
        args["id"] = message_id
        return reply(request, call("get", args, agent_of(request)))

    @app.delete("/api/messages/{message_id}")
    def message_delete(message_id: int, request: Request) -> Response:
        check_token(request)
        return reply(request, call("rm", {"what": "message", "id": message_id}, agent_of(request)))

    @app.delete("/api/messages/{message_id}/files/{name}")
    def file_delete(message_id: int, name: str, request: Request) -> Response:
        check_token(request)
        return reply(request, call("rm", {"what": "file", "id": message_id, "name": name}, agent_of(request)))

    # ----------------------------------------------------------------- files

    def upload(files: list[UploadFile] | None, payload: str | None, request: Request) -> dict[str, Any]:
        check_token(request)
        me = agent_of(request)
        if not me:
            raise ApiError(401, "need_agent", "upload needs an agent identity", 'send header "X-Agent: <name>"')
        items: list[dict[str, Any]] = []
        for handle in files or []:
            items.append({"n": handle.filename or "file", "type": handle.content_type or "application/octet-stream", "stream": handle.file})
        if payload:
            try:
                parsed = json.loads(payload)
            except ValueError:
                raise bad("form field payload must be JSON", 'payload={"files":[{"n":"a.txt","text":"..."}]}') from None
            for item in (parsed.get("files") or parsed if isinstance(parsed, list) else parsed.get("files") or []):
                if isinstance(item, dict):
                    items.append(item)
        if not items:
            raise bad("no files sent", 'multipart field "files", or op up / post files=[{"n","text"}]')
        with db.session(cfg) as conn:
            core.identity(conn, me)
            return {"u": core.create_uploads(cfg, conn, items)}

    @app.post("/api/files")
    def files_upload(
        request: Request,
        files: list[UploadFile] | None = File(None, description="one or more files"),
        payload: str | None = Form(None, description='optional JSON {"files":[{"n":name,"text":content}]}'),
    ) -> dict[str, Any]:
        return upload(files, payload, request)

    @app.get("/api/files/{file_id}")
    def file_meta(file_id: int, request: Request) -> Response:
        check_token(request)
        return reply(request, call("dl", {"id": file_id, "text": request.query_params.get("text", "0")}, agent_of(request)))

    @app.get("/api/files/{file_id}/raw")
    def file_raw(file_id: int, request: Request) -> Response:
        check_token(request)
        with db.reader(cfg) as conn:
            row = conn.execute("SELECT * FROM files WHERE id = ?", [file_id]).fetchone()
        if row is None or row["mid"] is None:
            raise ApiError(404, "no_file", f"attached file {file_id} is unknown or expired", "ids come from message field fl[].i")
        try:
            blob = storage.open_blob(cfg, row["key"])
        except storage.StorageError as exc:
            raise ApiError(409, "blob_missing", str(exc), "the blob is gone from disk; re-upload it") from None
        with blob:
            data = blob.read()
        quoted = row["name"].replace('"', "'")
        return Response(
            data,
            media_type=row["type"],
            headers={"Content-Disposition": f"attachment; filename=\"{quoted}\"", "X-Sha256": row["sha"], "ETag": f'"{row["sha"][:16]}' + '"'},
        )

    @app.post("/api/files/{file_id}/attach")
    def file_attach(file_id: int, request: Request, message_id: int | None = None) -> dict[str, Any]:
        """Attach an upload to a message by ids (the usual path is post files=[{\"k\":key}])."""
        check_token(request)
        if message_id is None:
            raise bad("?message_id=<id> is required")
        with db.session(cfg) as conn:
            me = core.identity(conn, agent_of(request) or "")["name"]
            msg = conn.execute("SELECT * FROM messages WHERE id = ?", [message_id]).fetchone()
            if msg is None:
                raise ApiError(404, "no_message", f"message {message_id} does not exist")
            if msg["author"] != me:
                raise ApiError(403, "not_yours", f"message {message_id} was written by {msg['author']!r}")
            key = conn.execute("SELECT key FROM files WHERE id = ?", [file_id]).fetchone()
            if key is None:
                raise ApiError(404, "no_file", f"upload {file_id} does not exist", "POST /api/files first")
            attached = core.attach(cfg, conn, message_id, [key["key"]])
            return {"ok": 1, "fl": [{"i": f["id"], "n": f["name"], "s": f["size"]} for f in attached]}

    # --------------------------------------------------------------- search

    @app.get("/api/search")
    def search(request: Request) -> Response:
        check_token(request)
        return reply(request, call("search", qargs(request, "search"), agent_of(request)))

    # ------------------------------------------------------------------ misc

    @app.post("/mcp")
    async def mcp(request: Request) -> Response:
        check_token(request)
        raw = await request.body()
        try:
            payload = json.loads(raw) if raw.strip() else {"method": "", "id": None}
        except ValueError:
            return JSONResponse({"jsonrpc": RPC_VERSION, "error": {"code": -32700, "message": "parse error: body is not JSON"}, "id": None}, status_code=400)
        result = await run_in_threadpool(mcp_handle, cfg, payload, agent_of(request))
        if result is None:  # JSON-RPC notification
            return Response(status_code=202)
        return JSONResponse(result)

    @app.get("/mcp", include_in_schema=False)
    def mcp_get() -> JSONResponse:
        return JSONResponse(
            {"err": "no_stream", "msg": "this MCP endpoint is request/response only (no SSE stream)", "hint": "POST JSON-RPC 2.0 to /mcp; tools/list then tools/call"},
            status_code=405,
        )

    if mount_ui:
        app.include_router(web.router(cfg, call))

    return app
