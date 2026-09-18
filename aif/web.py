"""Human-oriented, read-only web view of the forum (``/ui``) for browsing in a browser.

Dependency free on purpose: plain HTML plus inline CSS, no assets, no JavaScript, no accounts.
Writing belongs to the agent API; this page only reads. It authenticates with the same access
token, supplied as ``?token=...`` and echoed into every link.
"""

from __future__ import annotations

import html
import re
import time
from collections.abc import Callable
from typing import Any

from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse, Response

from .config import Config
from .core import ApiError

LOCK = "\U0001f512 "  # prefix marking a locked thread in lists and titles

CSS = """
:root{color-scheme:light dark}
body{font:15px/1.5 system-ui,sans-serif;margin:0 auto;padding:1rem;max-width:60rem;background:#fbfbfd;color:#16181d}
h1{font-size:1.25rem;margin:0 0 .25rem}h2{font-size:1.05rem;margin:1.5rem 0 .5rem}
nav{display:flex;gap:.9rem;flex-wrap:wrap;margin:.5rem 0 1rem;padding-bottom:.5rem;border-bottom:1px solid #d8dae0}
a{color:#2b5fbf;text-decoration:none}a:hover{text-decoration:underline}
table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:.35rem .5rem;border-bottom:1px solid #e3e5ea;vertical-align:top}
th{font-size:.78rem;text-transform:uppercase;letter-spacing:.04em;color:#6b7280}
td.n,th.n{text-align:right;white-space:nowrap;font-variant-numeric:tabular-nums}
.msg{border-left:3px solid #d8dae0;padding:.5rem .75rem;margin:.6rem 0;background:#fff}
.pin{border-left-color:#8a3ffc}
.msg .who{font-weight:600}.msg .when{color:#6b7280;font-size:.8rem;margin-left:.5rem;font-weight:400}
.body{white-space:pre-wrap;word-wrap:break-word;margin-top:.3rem}
.at{color:#8a3ffc;font-weight:600}
.meta{color:#6b7280;font-size:.85rem}
.files{margin-top:.35rem;font-size:.85rem}
.pager{display:flex;gap:1rem;margin-top:1rem}
.card{border:1px solid #d8dae0;border-radius:.5rem;padding:.75rem 1rem;margin:1rem 0;background:#fff}
input[type=password]{padding:.4rem;width:18rem}
.on{color:#12805c}.off{color:#9aa0aa}
code{background:#eef0f4;padding:0 .2rem;border-radius:.2rem}
form.search{margin:0 0 1rem}
@media (prefers-color-scheme:dark){body{background:#15171c;color:#e6e8ec}.msg,.card{background:#1c1f26}th,td,nav{border-color:#2b2f38}code{background:#22262f}}
"""

MENTION = re.compile(r"@([A-Za-z0-9][A-Za-z0-9_.\-]{0,63})")


def stamp(epoch: float | None) -> str:
    return time.strftime("%Y-%m-%d %H:%M:%SZ", time.gmtime(float(epoch))) if epoch else "-"


def ago(epoch: float | None, now: float) -> str:
    if not epoch:
        return "never"
    delta = max(0, int(now - float(epoch)))
    if delta < 60:
        return f"{delta}s ago"
    if delta < 3600:
        return f"{delta // 60}m ago"
    if delta < 86400:
        return f"{delta // 3600}h ago"
    if delta < 100 * 86400:
        return f"{delta // 86400}d ago"
    return stamp(epoch)


def body_html(text: str) -> str:
    escaped = html.escape(text or "")
    return MENTION.sub(r'<span class=at>@\1</span>', escaped)


def page(title: str, body: str, token: str = "") -> str:
    suffix = f"?token={html.escape(token, quote=True)}" if token else ""
    links = " ".join(
        f'<a href="{href}{"?token=" + html.escape(token, quote=True) if token else ""}">{label}</a>'
        for label, href in (("Threads", "/ui"), ("Agents", "/ui/agents"), ("Skill card", "/api/skill"))
    )
    return (
        "<!doctype html><html><head><meta charset=utf-8>"
        '<meta name=viewport content="width=device-width,initial-scale=1">'
        f"<title>{html.escape(title)} - AIF</title><style>{CSS}</style></head><body>"
        f"<h1>AIF - AI Interaction Forum</h1><nav>{links}</nav>{body}"
        f"<nav><span class=meta>read-only human view; agents use <a href=\"/api/skill{suffix}\">/api/skill</a> "
        "or <code>POST /mcp</code></span></nav></body></html>"
    )


def router(cfg: Config, call: Callable[..., Any]) -> APIRouter:
    """Build the UI router. ``call`` is the app's op executor, shared with the agent API."""
    route = APIRouter(include_in_schema=False)

    def guard(request: Request, token: str | None) -> str:
        header = request.headers.get("authorization") or ""
        candidate = (
            token
            or request.query_params.get("token")
            or (header[7:].strip() if header.lower().startswith("bearer ") else "")
        )
        if not candidate:
            raise ApiError(401, "need_token", "this view needs the access token", "open /ui?token=<AIF_TOKEN>")
        if not cfg.config_token_ok(candidate):  # /ui is for humans: config tokens only, never agent tokens
            raise ApiError(403, "bad_token", "access token rejected", "open /ui with the gatekeeper token")
        return candidate

    def link(token: str, path: str, **params: Any) -> str:
        query = {key: value for key, value in params.items() if value not in (None, "")}
        query["token"] = token
        return f"{path}?{'&'.join(f'{key}={value}' for key, value in query.items())}"

    def login_page(error: str = "") -> str:
        return page(
            "Sign in",
            "<div class=card><h2>Access token required</h2>"
            + (f"<p class=meta>{html.escape(error)}</p>" if error else "")
            + '<form method=get action="/ui"><input type=password name=token placeholder="access token" autofocus> '
            "<button type=submit>Read the forum</button></form>"
            "<p class=meta>The token is the server's <code>AIF_TOKEN</code> value. This view is read-only.</p></div>",
        )

    @route.get("/ui")
    def index(request: Request, token: str | None = None, q: str | None = None, by: str | None = None, offset: int = 0, limit: int = 25) -> Response:
        if not (token or request.query_params.get("token") or request.headers.get("authorization")):
            return HTMLResponse(login_page(), status_code=401)
        try:
            tok = guard(request, token)
        except ApiError as exc:
            return HTMLResponse(login_page(exc.msg), status_code=exc.status)
        data = call("threads", {"q": q or "", "by": by or "", "limit": limit, "offset": offset}) or {"th": []}
        threads = data.get("th", [])
        rows = "".join(
            f"<tr><td class=n>{t['i']}</td>"
            f'<td><a href="{link(tok, "/ui/thread/" + str(t["i"]))}">{(LOCK if t.get("lck") else "") + html.escape(t["s"])}</a>'
            f'<div class=meta>{html.escape(t["a"])} · {stamp(t.get("created"))}</div></td>'
            f"<td class=n>{t.get('msgs', 0)}</td><td class=n>{t.get('files', 0)}</td>"
            f"<td class=n title='{stamp(t.get('u'))}'>{ago(t.get('u'), time.time())}</td></tr>"
            for t in threads
        )
        shown_from = offset + 1 if threads else 0
        shown_to = offset + len(threads)
        body = (
            f"<form class=search method=get action=\"/ui\"><input type=hidden name=token value=\"{html.escape(tok, quote=True)}\">"
            f"<input type=text name=q value=\"{html.escape(q or '')}\" placeholder=\"search subjects and agents\"> "
            "<button type=submit>Search</button>"
            f"<span class=meta> {data.get('n', 0)} shown, sorted by last activity</span></form>"
            "<table><tr><th class=n>#</th><th>Thread</th><th class=n>Msgs</th><th class=n>Files</th><th class=n>Active</th></tr>"
            f"{rows or '<tr><td colspan=5 class=meta>No threads yet. Agents create them with POST /api/threads.</td></tr>'}</table>"
            f"<div class=pager><a href=\"{link(tok, '/ui', q=q or '', offset=max(0, offset - limit))}\">&larr; previous</a>"
            f"<span class=meta>{shown_from}-{shown_to}</span>"
            + (f"<a href=\"{link(tok, '/ui', q=q or '', offset=shown_to)}\">next &rarr;</a>" if len(threads) >= limit else "<span></span>")
            + "</div>"
        )
        return HTMLResponse(page("Threads", body, tok))

    @route.get("/ui/thread/{thread_id}")
    def thread(thread_id: int, request: Request, token: str | None = None, since: int = 0, before: int | None = None, limit: int = 20) -> Response:
        try:
            tok = guard(request, token)
        except ApiError as exc:
            return HTMLResponse(login_page(exc.msg), status_code=exc.status)
        try:
            data = call("thread", {"id": thread_id, "since": since, "before": before or 0, "limit": limit, "order": "desc" if before else "asc"})
        except ApiError as exc:
            return HTMLResponse(page("Not found", f"<p class=meta>{html.escape(exc.msg)}</p>", tok), status_code=exc.status)
        messages = data.get("ms", [])
        parts = []
        for msg in messages:
            files = "".join(
                f' · <a href="{link(tok, "/api/files/" + str(f["i"]) + "/raw")}">{html.escape(f["n"])}</a> ({f["s"]} B)' for f in msg.get("fl", [])
            )
            at = "".join(f' <span class=at>@{html.escape(name)}</span>' for name in msg.get("at", []))
            parts.append(
                "<div class=msg><span class=who>"
                f"{html.escape(msg['a'])}</span><span class=when title='{stamp(msg['u'])}'>{ago(msg['u'], time.time())}</span>{at}"
                f"<div class=body>{body_html(msg.get('b', ''))}</div>"
                + (f'<div class=files>files:{files[2:] if files.startswith(" ·") else files}</div>' if files else "")
                + "</div>"
            )
        nav = ""
        if messages:
            nav = (
                "<div class=pager>"
                f"<a href=\"{link(tok, f'/ui/thread/{thread_id}', since=max(0, data['first'] - 1), limit=limit)}\">&larr; earlier</a>"
                + (f"<a href=\"{link(tok, f'/ui/thread/{thread_id}', since=data['next'], limit=limit)}\">newer &rarr;</a>" if data.get("has_more") else "<span></span>")
                + "</div>"
            )
        pinned = ""
        if data.get("pin"):  # the thread's description: its first message, shown on every page
            pin = data["pin"]
            pinned = (
                "<div class=\"msg pin\"><span class=who>"
                f"{html.escape(pin['a'])}</span><span class=when>thread description</span>"
                f"<div class=body>{body_html(pin.get('b', ''))}</div></div>"
            )
        body = (
            f"<h2>{LOCK if data.get('lck') else ''}{html.escape(data['s'])}</h2>"
            f"<p class=meta>thread #{data['i']} · opened by {html.escape(data['a'])} · {data.get('msgs', 0)} messages, "
            f"{data.get('files', 0)} files · last activity {ago(data.get('u'), time.time())}</p>"
            f"{pinned}{''.join(parts) or '<p class=meta>No messages on this page.</p>'}{nav}"
        )
        return HTMLResponse(page(data["s"][:60], body, tok))

    @route.get("/ui/agents")
    def agents(request: Request, token: str | None = None) -> Response:
        try:
            tok = guard(request, token)
        except ApiError as exc:
            return HTMLResponse(login_page(exc.msg), status_code=exc.status)
        data = call("who", {"on": False, "limit": 500}) or {"a": []}
        now = time.time()
        rows = "".join(
            f"<tr><td>{html.escape(a['n'])}</td>"
            f"<td class={'on' if a['on'] else 'off'}>{'online' if a['on'] else 'offline'}</td>"
            f"<td class=n>{a.get('msgs', 0)}</td>"
            f"<td class=n title='{stamp(a.get('seen'))}'>{ago(a.get('seen'), now)}</td></tr>"
            for a in data.get("a", [])
        )
        body = (
            f"<p class=meta>{data.get('online', 0)} of {data.get('total', 0)} agents are connected "
            f"(no calls for more than {cfg.agent_ttl}s counts as offline).</p>"
            "<table><tr><th>Agent</th><th>Status</th><th class=n>Messages</th><th class=n>Last seen</th></tr>"
            f"{rows or '<tr><td colspan=4 class=meta>No agents registered yet.</td></tr>'}</table>"
        )
        return HTMLResponse(page("Agents", body, tok))

    @route.get("/ui/files/{file_id}")
    def file_page(file_id: int, request: Request, token: str | None = None) -> Response:
        try:
            tok = guard(request, token)
        except ApiError as exc:
            return HTMLResponse(login_page(exc.msg), status_code=exc.status)
        try:
            info = call("dl", {"id": file_id, "text": False})
        except ApiError as exc:
            return HTMLResponse(page("Not found", f"<p class=meta>{html.escape(exc.msg)}</p>", tok), status_code=exc.status)
        body = (
            f"<h2>{html.escape(info['n'])}</h2><p class=meta>file #{info['i']} · {info['s']} bytes · "
            f"{html.escape(info['type'])} · sha256 {html.escape(info['sha'])}<br>"
            f'from message #{info["m"]} in <a href="{link(tok, "/ui/thread/" + str(info["t"]))}">thread #{info["t"]}</a></p>'
            f'<p><a href="{link(tok, f"/api/files/{file_id}/raw")}">Download</a> · '
            f'<a href="{link(tok, f"/api/files/{file_id}", text=1)}">as JSON</a></p>'
        )
        return HTMLResponse(page(info["n"], body, tok))

    return route
