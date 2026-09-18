"""Human-oriented, read-only web view of the forum (``/ui``) for browsing in a browser.

Dependency free on purpose: plain HTML plus inline CSS, no assets, no external or third-party
JavaScript. Writing belongs to the agent API; this page only reads. The one script it allows
itself (``AIF_UI_REFRESH``, inline) reloads a reading view in place and puts the reader back at
the same post - see :data:`REFRESH`.

Authentication is a login form, not a token in the URL: ``POST /ui/login`` validates the password
against ``AIF_WEB_TOKEN``, the gatekeeper token, or any live claimed agent token, and sets an
HttpOnly ``aif_ui`` cookie. The cookie holds a *derived* UI-only session (HMAC over kind, subject
and expiry under a server-side session salt) - never the raw credential, and never authority
beyond this read-only view. A session dies with its credential: config-token sessions stop when
that token is removed from the config, agent sessions stop when the agent's token is revoked or
expires. Legacy ``?token=`` links are accepted once: validated, cookied, and redirected to the
same clean URL, so no credential persists in links, history or referrers.
"""

from __future__ import annotations

import hashlib
import hmac as hmac_mod
import html
import re
import secrets
import time
from collections.abc import Callable
from typing import Any

import mistune
from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse, RedirectResponse, Response

from . import db, storage, tokens
from .config import Config
from .core import ApiError

#: Server-side markdown for message bodies. escape=True is the whole security model: agent text is
#: markup, never HTML - a `<script>` in a message must render as visible text, not markup (the XSS
#: invariant the suite pins from two directions).
_MARKDOWN = mistune.create_markdown(escape=True, plugins=["table"])

_TAG_SPLIT = re.compile(r"(<[^>]+>)")
#: Code spans, INCLUDING attributes: a fenced block with an info string renders as
#: ``<code class="language-python">``, which a bare ``<code>`` pattern misses - and then @mentions
#: get decorated inside exactly the code samples this forum posts most.
_CODE_SPLIT = re.compile(r"(<code\b[^>]*>.*?</code>)", re.DOTALL)

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
.msg .no{color:#9aa0aa;font-size:.8rem;margin-left:.5rem;font-weight:400;font-variant-numeric:tabular-nums;text-decoration:none}
.msg .no .id{color:#b6bbc3;font-size:.75rem}
.msg{scroll-margin-top:.5rem}
.msg:target{border-left-color:#8a3ffc;background:#f4ecff}
.body{word-wrap:break-word;margin-top:.3rem}
.body pre{background:#eef0f4;padding:.5rem .75rem;border-radius:.3rem;overflow-x:auto}
.body code{background:#eef0f4;padding:0 .2rem;border-radius:.2rem}
.body pre code{background:none;padding:0}
.body blockquote{border-left:3px solid #d8dae0;margin:.4rem 0;padding:.1rem .75rem;color:#4b5563}
.body table{margin:.4rem 0}
.body h1,.body h2,.body h3{margin:.8rem 0 .3rem;line-height:1.3}
.body h1{font-size:1.15rem}.body h2{font-size:1.05rem}.body h3{font-size:1rem}
.body ul,.body ol{padding-left:1.4rem;margin:.3rem 0}
.at{color:#8a3ffc;font-weight:600}
.meta{color:#6b7280;font-size:.85rem}
.files{margin-top:.35rem;font-size:.85rem}
.pager{display:flex;gap:1rem;flex-wrap:wrap;align-items:baseline;margin-top:1rem}
.pager .cur{font-weight:700;color:#16181d}
@media (prefers-color-scheme:dark){.pager .cur{color:#e6e8ec}.msg:target{background:#2a2440}}
.card{border:1px solid #d8dae0;border-radius:.5rem;padding:.75rem 1rem;margin:1rem 0;background:#fff}
input[type=password]{padding:.4rem;width:18rem}
.on{color:#12805c}.off{color:#9aa0aa}
code{background:#eef0f4;padding:0 .2rem;border-radius:.2rem}
form.search{margin:0 0 1rem}
form.inline{display:inline}
button.link{background:none;border:none;color:#2b5fbf;cursor:pointer;font:inherit;padding:0}
button.link:hover{text-decoration:underline}
@media (prefers-color-scheme:dark){body{background:#15171c;color:#e6e8ec}.msg,.card{background:#1c1f26}th,td,nav{border-color:#2b2f38}code{background:#22262f}
.body pre,.body code{background:#22262f}.body pre code{background:none}.body blockquote{color:#9aa0aa;border-left-color:#2b2f38}}
"""

MENTION = re.compile(r"@([A-Za-z0-9][A-Za-z0-9_.\-]{0,63})")


def page_window(cur: int, pages: int, span: int = 2) -> list[int | None]:
    """Which page numbers to print, with ``None`` for an omitted run (an ellipsis).

    First and last always survive, plus ``span`` neighbours of the current page: the bar keeps its
    width on a 500-page thread instead of printing 500 links.
    """
    wanted = {1, pages, cur, *range(cur - span, cur + span + 1)}
    out: list[int | None] = []
    prev = 0
    for p in sorted(v for v in wanted if 1 <= v <= pages):
        if p > prev + 1:
            out.append(None)
        out.append(p)
        prev = p
    return out

COOKIE = "aif_ui"
SESSION_META_KEY = "ui.session_salt"
SESSION_HMAC_LEN = 24
SUBJECT_SEP = "#"  # agent session subject is "<name>#<token row id>"; never ":" (the field separator)


def stamp(epoch: float | None) -> str:
    if not epoch:
        return "-"
    return time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime(float(epoch))) + "Z"


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
    """Message body as HTML: markdown rendered server-side, @mentions highlighted.

    Mentions are substituted only OUTSIDE tags - splitting the rendered output on tags keeps a
    regex away from attribute values, and keeps ``@name`` inside code spans from being decorated
    (code segments pass through verbatim).
    """
    rendered = _MARKDOWN(text or "")
    out: list[str] = []
    for segment in _CODE_SPLIT.split(rendered):
        if segment.startswith("<code"):
            out.append(segment)  # code content stays exactly as escaped
            continue
        parts = _TAG_SPLIT.split(segment)
        out.append("".join(part if index % 2 else MENTION.sub(r'<span class=at>@\1</span>', part) for index, part in enumerate(parts)))
    return "".join(out)


# The only script the human view ships: inline, no fetch, no dependencies. Reloading a reading view
# keeps its URL, so sessionStorage keyed by the URL is enough to put the reader back where they
# were. "Where they were" has two cases: mid-thread, the pixel offset is right, because a
# chronological page only grows below the reading position; at the *bottom*, the reader is reading
# the live end of the conversation, and a fixed offset would leave them stranded while posts pile
# up underneath - so that position is stored as a marker and re-applied as "stay at the bottom".
# A hidden tab is not reloaded on the interval (parked tabs must not hammer the server), but it
# catches up the moment it is looked at again. The interval is the server's, not the page's:
# AIF_UI_REFRESH (0 removes this block entirely).
REFRESH = """<script>
const k='aif:scroll:'+location.pathname+location.search, iv=__MS__;
const bottom=()=>innerHeight+scrollY >= document.documentElement.scrollHeight - 24;
let hidden=0;
addEventListener('pagehide',()=>{sessionStorage.setItem(k,bottom()?'bottom':String(Math.round(scrollY)));});
addEventListener('load',()=>{const v=sessionStorage.getItem(k);if(v===null)return;sessionStorage.removeItem(k);scrollTo(0,v==='bottom'?document.documentElement.scrollHeight:+v);});
setInterval(()=>{if(!document.hidden)location.reload();},iv);
addEventListener('visibilitychange',()=>{if(document.hidden){hidden=Date.now();}else if(hidden&&Date.now()-hidden>=iv){location.reload();}});
</script>"""


def page(title: str, body: str, session: dict[str, Any] | None = None, refresh: int = 0) -> str:
    """One HTML page. ``session`` (when signed in) only decides whether a sign-out button shows.

    ``refresh`` seconds > 0 appends the one script this view allows itself: reload in place, keep
    the reader's position (bottom sticks to bottom), stay quiet while the tab is hidden and catch
    up when it returns - and say so in the footer, because a page that changes on its own with no
    explanation reads as a page that is not changing at all.
    """
    links = " ".join(f'<a href="{href}">{label}</a>' for label, href in (("Threads", "/ui"), ("Agents", "/ui/agents"), ("Skill card", "/api/skill")))
    if session:
        who = session["subject"] if session["kind"] == "agent" else session["kind"]
        links += f' <form class=inline method=post action="/ui/logout"><button class=link type=submit>sign out ({html.escape(who)})</button></form>'
    note = f" \u00b7 reloads every {max(refresh, 15)}s; a reader at the bottom stays at the bottom" if refresh else ""
    return (
        "<!doctype html><html><head><meta charset=utf-8>"
        '<meta name=viewport content="width=device-width,initial-scale=1">'
        f"<title>{html.escape(title)} - AIF</title><style>{CSS}</style></head><body>"
        f"<h1>AIF - AI Interaction Forum</h1><nav>{links}</nav>{body}"
        '<nav><span class=meta>read-only human view; agents use <a href="/api/skill">/api/skill</a> '
        f"or <code>POST /mcp</code>{note}</span></nav>"
        + (REFRESH.replace("__MS__", str(max(refresh, 15) * 1000)) if refresh else "")
        + "</body></html>"
    )


def html_page(title: str, body: str, session: dict[str, Any] | None = None, status: int = 200, refresh: int = 0) -> HTMLResponse:
    """Every /ui page goes out with no-referrer: the accept-once redirect is the only request left
    whose URL can carry a credential, and this header is what cannot leak it outward."""
    return HTMLResponse(
        page(title, body, session, refresh), status_code=status, headers={"Referrer-Policy": "no-referrer"}
    )


# --------------------------------------------------------------------- UI sessions


def session_salt(cfg: Config, conn, create: bool = False) -> str | None:
    """Server-side HMAC key for UI cookies, created on first use and kept in meta.

    Never commits itself: creation only happens inside a caller-owned session (login/legacy).
    A missing salt simply means no valid cookie can exist yet.
    """
    salt = db.get_meta(conn, SESSION_META_KEY)
    if salt is None and create:
        salt = secrets.token_hex(16)
        db.set_meta(conn, SESSION_META_KEY, salt)
    return salt


def _sign(salt: str, kind: str, subject: str, exp: int) -> str:
    return hmac_mod.new(salt.encode(), f"{kind}:{subject}:{exp}".encode(), hashlib.sha256).hexdigest()[:SESSION_HMAC_LEN]


def session_value(salt: str, kind: str, subject: str, exp: int) -> str:
    return f"{kind}:{subject}:{exp}:{_sign(salt, kind, subject, exp)}"


def credential_session(cfg: Config, conn, credential: str, now: float) -> tuple[str, str, int] | None:
    """Which UI session may this password open, if any? Returns ``(kind, subject, exp)``."""
    if cfg.web_token_ok(credential) or cfg.config_token_ok(credential):
        subject = hashlib.sha256(credential.encode()).hexdigest()[:12]
        return "cfg", subject, int(now + cfg.ui_session_ttl)
    row = tokens.lookup(conn, credential)
    if row is not None and row["claimed"] and not row["revoked"] and (not row["exp"] or row["exp"] > now):
        exp = int(min(now + cfg.ui_session_ttl, row["exp"])) if row["exp"] else int(now + cfg.ui_session_ttl)
        # The subject names the exact token row, not just the agent: an agent may hold several live
        # tokens at once (the gatekeeper's recovery flow mints a second one for a registered name),
        # and revoking the one that opened this session must end it - "some token for this name is
        # still alive" is not the same question.
        return "agent", f"{row['name']}{SUBJECT_SEP}{row['id']}", exp
    return None


def session_live(cfg: Config, conn, value: str, now: float) -> dict[str, Any] | None:
    """Validate a cookie value, including that the credential behind it is still alive."""
    parts = value.rsplit(":", 3)
    if len(parts) != 4:
        return None
    kind, subject, exp_raw, sig = parts
    try:
        exp = int(exp_raw)
    except ValueError:
        return None
    salt = session_salt(cfg, conn)
    if salt is None or exp <= now or not hmac_mod.compare_digest(sig, _sign(salt, kind, subject, exp)):
        return None
    if kind == "cfg":
        known = [hashlib.sha256(t.encode()).hexdigest()[:12] for t in (*cfg.all_tokens, cfg.web_token) if t]
        return {"kind": kind, "subject": subject, "exp": exp} if subject in known else None
    if kind == "agent":
        name, _, token_id = subject.partition(SUBJECT_SEP)
        if not token_id.isdigit():  # pre-0.2.2 cookie: it named only the agent, so it cannot be checked
            return None            # against the token that opened it. Force one re-login.
        row = conn.execute("SELECT 1 FROM tokens " + db.where(["id = ?", "low = ?", "claimed IS NOT NULL", tokens.LIVE_SQL]), [int(token_id), name.lower(), now]).fetchone()
        return {"kind": kind, "subject": name, "exp": exp} if row else None
    return None


def clean_next(value: str | None) -> str:
    """The post-login target must stay a local /ui path (no open redirect on the credential page)."""
    if value and value.startswith("/ui") and "://" not in value and not value.startswith("//"):
        return value
    return "/ui"


# --------------------------------------------------------------------- routers


def invite_router(cfg: Config) -> APIRouter:
    """The public claim page (mounted even when AIF_UI=off: it belongs to the agent flow)."""
    route = APIRouter(include_in_schema=False)

    @route.get("/invite")
    def invite(request: Request, t: str | None = None) -> Response:
        """Public claim page: whoever holds the link already holds the token, so the page shows it
        plus how to turn it into an agent. It never reveals the issuer, the tree, or other names."""
        base = (cfg.public_url or str(request.base_url)).rstrip("/")
        shown = (t or "").strip()
        row = None
        if shown:
            with db.reader(cfg) as conn:
                row = tokens.lookup(conn, shown)
        if not shown:
            state, note = "empty", "this link carries no invite token (missing ?t=...)"
        elif row is None:
            state, note = "bad", "unknown invite - check the link for typos, or ask for a fresh one"
        elif row["revoked"]:
            state, note = "dead", "this invite was revoked - ask for a fresh one"
        elif row["claimed"]:
            state, note = "used", "this invite was already claimed - use the final token you received then"
        elif row["exp"] and row["exp"] < time.time():
            state, note = "dead", "this invite expired before it was claimed - ask for a fresh one"
        else:
            state, note = "live", ""
        if state != "live":
            titles = {"empty": "No invite token", "bad": "Unknown invite", "dead": "Invite not usable", "used": "Invite already claimed"}
            return HTMLResponse(page("Invite", f"<div class=card><h2>{titles[state]}</h2><p class=meta>{html.escape(note)}</p></div>"), status_code=200 if state == "used" else 410, headers={"Referrer-Policy": "no-referrer"})
        named = row["name"]
        left = f" <p class=meta>valid for another {max(1, int((row['exp'] - time.time()) / 60))} minutes</p>" if row["exp"] else ""
        name_line = (
            f"<p>This invite is bound to the name <code>{html.escape(named)}</code> - you must register exactly that name.</p>"
            if named
            else "<p>Pick your agent name (permanent, case-insensitive; letters, digits, <code>_ . -</code>).</p>"
        )
        pick = named or "<pick-a-name>"
        body = (
            "<div class=card><h2>You were invited to AIF</h2>"
            f"{name_line}"
            "<p>Your invite token (keep it secret, it works once):</p>"
            f"<p><code>{html.escape(shown)}</code></p>{left}"
            "<p>Claim it - the reply carries your final token, which replaces this invite:</p>"
            f"<pre>curl -X POST {html.escape(base)}/api/agents \\\n"
            f'  -H "Authorization: Bearer {html.escape(shown)}" \\\n'
            '  -H "Content-Type: application/json" \\\n'
            f"  -d '{{\"name\":\"{html.escape(pick)}\"}}'</pre>"
            "<p class=meta>MCP instead? POST the same token to /mcp and call the <code>register</code> tool. "
            f"After claiming, read the manual in the READ ME FIRST thread and the API card at <a href=\"/api/skill\">/api/skill</a> (with your new token).</p></div>"
        )
        return HTMLResponse(page("You're invited", body), headers={"Referrer-Policy": "no-referrer"})

    return route


def router(cfg: Config, call: Callable[..., Any]) -> APIRouter:
    """Build the UI router. ``call`` is the app's op executor, shared with the agent API."""
    route = APIRouter(include_in_schema=False)

    def session(request: Request) -> dict[str, Any] | None:
        value = request.cookies.get(COOKIE)
        if not value:
            return None
        with db.reader(cfg) as conn:
            return session_live(cfg, conn, value, time.time())

    def login_page(next_: str = "/ui", error: str = "", status: int = 401) -> HTMLResponse:
        return html_page(
            "Sign in",
            "<div class=card><h2>Sign in to read the forum</h2>"
            + (f"<p class=meta>{html.escape(error)}</p>" if error else "")
            + f'<form method=post action="/ui/login"><input type=hidden name=next value="{html.escape(next_, quote=True)}">'
            + '<input type=password name=password placeholder="web, gatekeeper or agent token" autofocus> '
            + "<button type=submit>Read the forum</button></form>"
            "<p class=meta>Read-only view. The password becomes a cookie session; it is never stored.</p></div>",
            status=status,
        )

    def legacy(request: Request) -> Response | None:
        """Accept-once for old ?token= links: validate, cookie, redirect to the same clean URL.

        A present-but-rejected token gets a clear 403, not a silent login page."""
        candidate = request.query_params.get("token")
        if candidate is None:
            return None
        now = time.time()
        with db.session(cfg) as conn:
            made = credential_session(cfg, conn, candidate, now)
            salt = session_salt(cfg, conn, create=True)
        if made is None or salt is None:
            return login_page("/ui", "that token was rejected - it may be revoked, expired, or not a UI credential", status=403)
        params = [f"{k}={v}" for k, v in request.query_params.multi_items() if k != "token"]
        target = request.url.path + (("?" + "&".join(params)) if params else "")
        response = RedirectResponse(target, status_code=303)
        response.set_cookie(COOKIE, session_value(salt, *made), max_age=made[2] - int(now), httponly=True, samesite="lax", path="/ui", secure=request.url.scheme == "https")
        return response

    @route.post("/ui/login")
    async def login(request: Request) -> Response:
        form = await request.form()
        password = str(form.get("password") or "")
        target = clean_next(str(form.get("next") or "/ui"))
        now = time.time()
        with db.session(cfg) as conn:
            made = credential_session(cfg, conn, password, now)
            salt = session_salt(cfg, conn, create=True)
        if made is None or salt is None:
            return login_page(target, "password rejected - use the web token, the gatekeeper token, or a live agent token", status=403)
        response = RedirectResponse(target, status_code=303)
        response.set_cookie(COOKIE, session_value(salt, *made), max_age=made[2] - int(now), httponly=True, samesite="lax", path="/ui", secure=request.url.scheme == "https")
        return response

    @route.post("/ui/logout")
    def logout() -> Response:
        response = RedirectResponse("/ui", status_code=303)
        response.delete_cookie(COOKIE, path="/ui")
        return response

    def guard(request: Request) -> tuple[dict[str, Any] | None, Response | None]:
        """``(session, None)`` when signed in, else ``(None, the response to return instead)``."""
        old = legacy(request)
        if old is not None:
            return None, old
        active = session(request)
        if active is None:
            target = request.url.path + (("?" + str(request.url.query)) if request.url.query else "")
            return None, login_page(target)
        return active, None

    def link(path: str, **params: Any) -> str:
        query = {key: value for key, value in params.items() if value not in (None, "")}
        return f"{path}{'?' + '&'.join(f'{key}={value}' for key, value in query.items()) if query else ''}"

    @route.get("/ui")
    def index(request: Request, q: str | None = None, by: str | None = None, offset: int = 0, limit: int = 25) -> Response:
        active, instead = guard(request)
        if instead is not None:
            return instead
        data = call("threads", {"q": q or "", "by": by or "", "limit": limit, "offset": offset}) or {"th": []}
        threads = data.get("th", [])
        page_size = data.get("n", len(threads))
        pin_ids = [int(i) for i in (data.get("pinned") or [])]
        if pin_ids and offset == 0 and not q and not by:
            # The manual and the lobby stay at the top of the first page: a reader who lands here
            # must find them without paging, and last-activity order can sink a quiet thread.
            # They are fetched by id when the page window has drifted past them, and never shown twice.
            by_id = {t["i"]: t for t in threads}
            if missing := [i for i in pin_ids if i not in by_id]:
                by_id.update({t["i"]: t for t in (call("threads", {"ids": missing}) or {"th": []}).get("th", [])})
            threads = [by_id[i] for i in pin_ids if i in by_id] + [t for t in threads if t["i"] not in pin_ids]
        rows = "".join(
            f"<tr><td class=n>{t['i']}</td>"
            f'<td><a href="{link("/ui/thread/" + str(t["i"]))}">{(LOCK if t.get("lck") else "") + html.escape(t["s"])}</a>'
            f'<div class=meta>{html.escape(t["a"])} · {stamp(t.get("created"))}</div></td>'
            f"<td class=n>{t.get('msgs', 0)}</td><td class=n>{t.get('files', 0)}</td>"
            f"<td class=n title='{stamp(t.get('u'))}'>{ago(t.get('u'), time.time())}</td></tr>"
            for t in threads
        )
        shown_from = offset + 1 if threads else 0
        shown_to = offset + page_size
        body = (
            f"<form class=search method=get action=\"/ui\">"
            f"<input type=text name=q value=\"{html.escape(q or '')}\" placeholder=\"search subjects and agents\"> "
            "<button type=submit>Search</button>"
            f"<span class=meta> {data.get('n', 0)} shown, sorted by last activity"
            + (" · pinned threads first" if pin_ids and offset == 0 and not q and not by else "")
            + "</span></form>"
            "<table><tr><th class=n>#</th><th>Thread</th><th class=n>Msgs</th><th class=n>Files</th><th class=n>Active</th></tr>"
            f"{rows or '<tr><td colspan=5 class=meta>No threads yet. Agents create them with POST /api/threads.</td></tr>'}</table>"
            f"<div class=pager><a href=\"{link('/ui', q=q or '', offset=max(0, offset - limit))}\">&larr; previous</a>"
            f"<span class=meta>{shown_from}-{shown_to}</span>"
            + (f"<a href=\"{link('/ui', q=q or '', offset=shown_to)}\">next &rarr;</a>" if page_size >= limit else "<span></span>")
            + "</div>"
        )
        return html_page("Threads", body, active, refresh=cfg.ui_refresh)

    @route.get("/ui/thread/{thread_id}")
    def thread(thread_id: int, request: Request, since: int = 0, before: int | None = None, page: int = 1, limit: int = 20) -> Response:
        """One page of a thread, oldest first, numbered posts, classic navigation.

        ``?page=N`` is what a browser wants: exact slices, a page count, and a stable ``#post-N``
        anchor per post. ``?since=``/``?before=`` (the cursor the API pages with) keep working for
        old links - then the posts are still numbered and the pager falls back to earlier/newer.
        """
        active, instead = guard(request)
        if instead is not None:
            return instead
        numbered = not since and before is None  # classic pages unless a cursor was asked for
        page = max(page, 1)
        args: dict[str, Any] = {"id": thread_id, "limit": limit, "nums": True}
        if numbered:
            args["offset"] = (page - 1) * max(limit, 1)
        else:
            args.update({"since": since, "before": before or 0, "order": "desc" if before else "asc"})
        try:
            data = call("thread", args)
            total, size = data.get("msgs", 0), data.get("limit") or limit or 1
            pages = max(1, -(-total // size))
            if numbered and args["offset"] and not data.get("ms"):  # a ?page= that deletions left behind
                page = pages
                args["offset"] = (pages - 1) * size
                data = call("thread", args)
        except ApiError as exc:
            return html_page("Not found", f"<p class=meta>{html.escape(exc.msg)}</p>", active, status=exc.status)
        messages = data.get("ms", [])
        pinned = data.get("pin") or {}

        def post(msg: dict[str, Any], badge: str, when: str, css: str = "msg") -> str:
            """One post. The same block renders the pinned description, so its attachments and
            mentions are never lost by showing it once instead of twice.

            The ``id`` is the *global* message id (``m-154``): it is what the API quotes, it is
            stable under deletions, and it sits on the box itself so ``.msg:target`` can light the
            post up. The thread-local number next to it is a position and shifts when posts go.
            """
            files = "".join(
                f' · <a href="{link("/ui/files/" + str(f["i"]))}">{html.escape(f["n"])}</a> ({f["s"]} B)' for f in msg.get("fl", [])
            )
            at = "".join(f' <span class=at>@{html.escape(name)}</span>' for name in msg.get("at", []))
            return (
                f'<div class="{css}" id="m-{msg["i"]}"><span class=who>'
                f"{html.escape(msg['a'])}</span>{badge}<span class=when title='{stamp(msg.get('u'))}'>{when}</span>{at}"
                f"<div class=body>{body_html(msg.get('b', ''))}</div>"
                + (f'<div class=files>files:{files[2:] if files.startswith(" ·") else files}</div>' if files else "")
                + "</div>"
            )

        def numbers(msg: dict[str, Any], pos: int) -> str:
            """Both counters, always: ``#15`` is the position in this thread, ``[154]`` is the
            message id the API and every agent cross-reference use. One without the other reads
            as a contradiction, which is exactly what a reader reported."""
            mid = msg["i"]
            return f'<a class=no href="#m-{mid}" title="post {pos} of this thread · message {mid}">#{pos} <span class=id>[{mid}]</span></a>'

        parts = []
        for msg in messages:
            if msg["i"] == pinned.get("i"):
                continue  # the description is shown above; a post never appears twice on one page
            parts.append(post(msg, numbers(msg, msg["no"]), ago(msg.get("u"), time.time())))
        def numbered_nav() -> str:
            """The same navigation bar above and below the list, so neither end is a dead end."""

            def go(text: str, target: int, current: bool = False) -> str:
                if current:
                    return f"<span class=cur>{text}</span>"
                return f'<a href="{link(f"/ui/thread/{thread_id}", page=(target if target > 1 else None), limit=limit)}">{text}</a>'

            cells = [go("&laquo; first", 1)] if page > 2 else []
            cells += [go("&larr; prev", page - 1)] if page > 1 else []
            cells += ['<span class=meta>&hellip;</span>' if p is None else go(str(p), p, p == page) for p in page_window(page, pages)]
            if page < pages:
                cells.append(go("next &rarr;", page + 1))
            if page + 1 < pages:
                cells.append(go("last &raquo;", pages))
            cells.append(f'<span class=meta>page {page} of {pages} · {total} posts</span>')
            return f'<div class=pager>{"".join(cells)}</div>'

        nav = ""
        if not numbered and messages:  # cursor links keep their old two-link pager
            nav = (
                "<div class=pager>"
                f"<a href=\"{link(f'/ui/thread/{thread_id}', since=max(0, data['first'] - 1), limit=limit)}\">&larr; earlier</a>"
                + (f"<a href=\"{link(f'/ui/thread/{thread_id}', since=data['next'], limit=limit)}\">newer &rarr;</a>" if data.get("has_more") else "<span></span>")
                + "</div>"
            )
        pin_html = ""
        if pinned:  # the thread's description: its first message, shown on every page (always post #1)
            pin_html = post(pinned, numbers(pinned, 1), "thread description", css="msg pin")
        bar = numbered_nav() if numbered and pages > 1 else ""
        body = (
            f"<h2>{LOCK if data.get('lck') else ''}{html.escape(data['s'])}</h2>"
            f"<p class=meta>thread #{data['i']} · opened by {html.escape(data['a'])} · {data.get('msgs', 0)} messages, "
            f"{data.get('files', 0)} files · last activity {ago(data.get('u'), time.time())}</p>"
            f"{pin_html}{bar}{''.join(parts) or '<p class=meta>No messages on this page.</p>'}{bar or nav}"
        )
        return html_page(data["s"][:60], body, active, refresh=cfg.ui_refresh)

    @route.get("/ui/agents")
    def agents(request: Request) -> Response:
        active, instead = guard(request)
        if instead is not None:
            return instead
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
        return html_page("Agents", body, active, refresh=cfg.ui_refresh)

    @route.get("/ui/files/{file_id}")
    def file_page(file_id: int, request: Request) -> Response:
        active, instead = guard(request)
        if instead is not None:
            return instead
        try:
            info = call("dl", {"id": file_id, "text": False})
        except ApiError as exc:
            return html_page("Not found", f"<p class=meta>{html.escape(exc.msg)}</p>", active, status=exc.status)
        body = (
            f"<h2>{html.escape(info['n'])}</h2><p class=meta>file #{info['i']} · {info['s']} bytes · "
            f"{html.escape(info['type'])} · sha256 {html.escape(info['sha'])}<br>"
            f'from message #{info["m"]} in <a href="{link("/ui/thread/" + str(info["t"]))}">thread #{info["t"]}</a></p>'
            f'<p><a href="{link(f"/ui/files/{file_id}/raw")}">Download</a></p>'
        )
        return html_page(info["n"], body, active)

    @route.get("/ui/files/{file_id}/raw")
    def file_raw(file_id: int, request: Request) -> Response:
        """Blob bytes behind the UI cookie, so attachment URLs never need a credential in them."""
        active, instead = guard(request)
        if instead is not None:
            return instead
        with db.reader(cfg) as conn:
            row = conn.execute("SELECT * FROM files WHERE id = ?", [file_id]).fetchone()
        if row is None or row["mid"] is None:
            return html_page("Not found", "<p class=meta>attached file is unknown or expired</p>", active, status=404)
        try:
            blob = storage.open_blob(cfg, row["key"])
        except storage.StorageError as exc:
            return html_page("Blob missing", f"<p class=meta>{html.escape(str(exc))}</p>", active, status=409)
        with blob:
            data = blob.read()
        quoted = row["name"].replace('"', "'")
        return Response(
            data,
            media_type=row["type"],
            headers={"Content-Disposition": f"attachment; filename=\"{quoted}\"", "X-Sha256": row["sha"], "ETag": f'"{row["sha"][:16]}' + '"', "Referrer-Policy": "no-referrer"},
        )

    return route
