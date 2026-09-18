"""The /ui cookie login: derived sessions, flags, expiry semantics, and the API boundary."""

from __future__ import annotations

import pytest
from conftest import ADMIN, Rig, make_cfg
from fastapi.testclient import TestClient

from aif import db
from aif.app import create_app

WEB = "hum4n-web-token"


@pytest.fixture()
def rig(tmp_path) -> Rig:
    return Rig(make_cfg(tmp_path, web_token=WEB, ui=True), ui=True)


def browser(rig: Rig) -> TestClient:
    return TestClient(create_app(rig.cfg, mount_ui=True))


def login(client: TestClient, password: str, next_: str = "/ui"):
    return client.post("/ui/login", data={"password": password, "next": next_}, follow_redirects=False)


# ----------------------------------------------------------------------------- the login


def test_the_login_page_is_a_post_form(rig):
    res = browser(rig).get("/ui")
    assert res.status_code == 401
    assert 'method=post action="/ui/login"' in res.text and 'type=password name=password' in res.text
    assert res.headers["referrer-policy"] == "no-referrer"


def test_a_wrong_password_is_a_clear_403(rig):
    res = login(browser(rig), "wrong")
    assert res.status_code == 403 and "password rejected" in res.text
    assert "aif_ui" not in res.headers.get("set-cookie", "")


def test_login_sets_a_derived_httponly_cookie(rig):
    res = login(browser(rig), WEB)
    assert res.status_code == 303 and res.headers["location"] == "/ui"
    cookie = res.headers["set-cookie"]
    assert "HttpOnly" in cookie and "SameSite=lax" in cookie and "Path=/ui" in cookie
    assert "secure" not in cookie.lower().replace("samesite", "")  # http here; Secure only on https
    value = cookie.split("aif_ui=", 1)[1].split(";", 1)[0]
    kind, subject, exp, sig = value.rsplit(":", 3)
    assert kind == "cfg" and len(subject) == 12 and subject not in WEB and WEB not in value
    assert int(exp) > 0 and len(sig) == 24


def test_the_cookie_never_authenticates_the_api(rig):
    client = browser(rig)
    login(client, WEB)
    assert client.get("/ui").status_code == 200
    res = client.get("/api/threads")  # only the cookie is sent here - no Authorization header
    assert res.status_code == 401 and res.json()["err"] == "need_token"


def test_login_next_is_restricted_to_ui_paths(rig):
    client = browser(rig)
    assert login(client, WEB, next_="https://evil.example/phish").headers["location"] == "/ui"
    assert login(client, WEB, next_="//evil.example").headers["location"] == "/ui"
    assert login(client, WEB, next_="/ui/agents").headers["location"] == "/ui/agents"


def test_logout_clears_the_session(rig):
    client = browser(rig)
    login(client, WEB)
    assert client.get("/ui").status_code == 200
    client.post("/ui/logout")
    assert client.get("/ui").status_code == 401


# ----------------------------------------------------------------- session lifetime semantics


def test_a_config_session_dies_when_its_token_leaves_the_config(rig):
    client = browser(rig)
    login(client, WEB)
    assert client.get("/ui").status_code == 200
    rig.cfg.web_token = "rotated-web-token"  # operator rotates the web token: old sessions must die
    assert client.get("/ui").status_code == 401


def test_an_agent_session_dies_with_the_credential(rig):
    rig.claim("bob")
    client = browser(rig)
    res = login(client, rig.agent_tokens["bob"], next_="/ui/agents")
    assert res.headers["location"] == "/ui/agents"
    assert "sign out (bob)" in client.get("/ui").text
    rig.admin.post("/api/op", json={"do": "revoke", "name": "bob"})
    assert client.get("/ui").status_code == 401


def test_a_forged_or_expired_cookie_is_rejected(rig):
    client = browser(rig)
    login(client, WEB)
    good = client.cookies.get("aif_ui")
    kind, subject, exp, sig = good.rsplit(":", 3)
    client.cookies.set("aif_ui", f"{kind}:{subject}:{exp}:{'0' * 24}")  # bad signature
    assert client.get("/ui").status_code == 401
    client.cookies.set("aif_ui", f"{kind}:{subject}:1:{sig}")  # expired
    assert client.get("/ui").status_code == 401
    client.cookies.set("aif_ui", "garbage")
    assert client.get("/ui").status_code == 401


def test_the_session_salt_is_created_once_and_reused(rig):
    login(browser(rig), WEB)
    with db.reader(rig.cfg) as conn:
        first = conn.execute("SELECT value FROM meta WHERE key = 'ui.session_salt'").fetchone()["value"]
    login(browser(rig), WEB)
    with db.reader(rig.cfg) as conn:
        assert conn.execute("SELECT value FROM meta WHERE key = 'ui.session_salt'").fetchone()["value"] == first
    assert len(first) == 32


# -------------------------------------------------------------------------- accept-once


def test_legacy_token_links_are_cookied_and_cleaned(rig):
    rig.admin.post("/api/threads", json={"subject": "legacy check", "b": "x"})
    client = browser(rig)
    res = client.get(f"/ui?token={WEB}&q=legacy", follow_redirects=False)
    assert res.status_code == 303
    assert res.headers["location"] == "/ui?q=legacy"  # the token is gone from the URL
    assert "aif_ui" in res.headers["set-cookie"]
    page = client.get(res.headers["location"])
    assert page.status_code == 200 and "legacy check" in page.text and "token=" not in page.text


def test_a_rejected_legacy_token_gets_a_clear_error(rig):
    res = browser(rig).get("/ui?token=nope")
    assert res.status_code == 403 and "token was rejected" in res.text


# ------------------------------------------------------------------------------ hygiene


def test_no_mutating_ui_route_answers_get(rig):
    """Cookies ride top-level GET navigations (SameSite=Lax), so nothing may mutate via GET."""
    app = create_app(rig.cfg, mount_ui=True)
    for route in app.routes:
        methods = getattr(route, "methods", set()) or set()
        path = getattr(route, "path", "")
        if path.startswith("/ui") and methods - {"GET", "HEAD"}:
            assert "GET" not in methods, f"{path} mutates and answers GET"


def test_the_gatekeeper_password_is_downgraded_to_readonly(rig):
    client = browser(rig)
    login(client, ADMIN)
    assert client.get("/ui").status_code == 200
    res = client.get("/api/threads")  # the session grants nothing beyond /ui
    assert res.status_code == 401
    value = client.cookies.get("aif_ui")
    assert ADMIN not in value
