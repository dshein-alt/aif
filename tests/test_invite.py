"""AIF_WEB_TOKEN (/ui only) and the public /invite claim page."""

from __future__ import annotations

import pytest
from conftest import Rig, make_cfg

from aif import db, tokens

WEB = "hum4n-web-token"


@pytest.fixture()
def rig(tmp_path) -> Rig:
    return Rig(make_cfg(tmp_path, web_token=WEB, public_url="https://aif.example.org"), ui=True)


def anon(rig: Rig):
    """A browser with no credentials at all (the invite link itself is the credential)."""
    client = rig.client()
    client.headers.pop("authorization", None)
    return client


# ------------------------------------------------------------------------- the web token


def test_the_web_token_opens_the_ui_but_never_the_api(rig):
    rig.admin.post("/api/threads", json={"subject": "visible", "b": "x"}, headers={"x-agent": "gatekeeper"})
    assert rig.client(WEB).get("/ui").status_code == 200 and "visible" in rig.client(WEB).get("/ui").text
    for res in (rig.client(WEB).get("/api/threads"), rig.client(WEB).get("/api/ping"), rig.client(WEB).post("/api/op", json={"do": "ping"})):
        assert res.status_code == 403 and res.json()["err"] == "web_token", res.text
        assert "/ui" in res.json()["hint"]


def test_the_gatekeeper_token_still_opens_the_ui(rig):
    assert rig.admin.get("/ui").status_code == 200
    assert rig.admin.get("/ui/agents").status_code == 200


def test_an_agent_token_never_opens_the_ui(rig):
    rig.claim("bob")
    res = rig.client(rig.agent_tokens["bob"]).get("/ui")
    assert res.status_code == 403


def test_web_token_config(tmp_path):
    from aif.config import Config

    cfg = Config(tokens=["adm"], token_salt="s", web_token="web", data_dir=str(tmp_path))
    assert cfg.web_token_ok("web") and not cfg.web_token_ok("adm") and not cfg.web_token_ok("")
    assert not cfg.admin_token_ok("web")
    warnings = Config(tokens=["adm"], token_salt="s", web_token="adm", data_dir=str(tmp_path)).validate()
    assert any("AIF_WEB_TOKEN" in w for w in warnings)  # same value as the admin token: warned
    from_env = Config.from_env({"AIF_DATA_DIR": str(tmp_path), "AIF_TOKEN": "x", "AIF_TOKEN_SALT": "s", "AIF_WEB_TOKEN": "w"})
    assert from_env.web_token == "w"


# ---------------------------------------------------------------------------- the page


def test_the_invite_page_shows_the_token_and_how_to_claim(rig):
    invite = rig.issue()
    page = anon(rig).get(f"/invite?t={invite}")
    assert page.status_code == 200, page.text
    assert invite in page.text and "curl -X POST https://aif.example.org/api/agents" in page.text
    assert "pick-a-name" in page.text and "register" in page.text and "READ ME FIRST" in page.text
    assert "gatekeeper" not in page.text.lower()  # the issuer is never revealed
    assert "valid for another" in page.text  # invites carry their TTL


def test_a_named_invite_page_shows_only_the_bound_name(rig):
    invite = rig.issue("carol")
    page = anon(rig).get(f"/invite?t={invite}")
    assert page.status_code == 200 and "carol" in page.text and "must register exactly that name" in page.text
    assert '"name":"carol"' in page.text


def test_the_invite_page_states_dead_tokens_precisely(rig):
    assert anon(rig).get("/invite").status_code == 410  # no ?t=
    assert "no invite token" in anon(rig).get("/invite").text.lower()
    assert "Unknown invite" in anon(rig).get("/invite?t=aif_nope").text
    invite = rig.issue()
    rig.admin.post("/api/op", json={"do": "revoke", "tk": invite})
    assert "revoked" in anon(rig).get(f"/invite?t={invite}").text
    stale = rig.issue()
    with db.session(rig.cfg) as conn:
        conn.execute("UPDATE tokens SET exp = 1 WHERE self_token = ?", [stale])
    assert "expired" in anon(rig).get(f"/invite?t={stale}").text
    claimed = rig.claim("done")
    res = anon(rig).get(f"/invite?t={claimed.json()['token']}")
    assert res.status_code == 200 and "already claimed" in res.text
    spent = anon(rig).get(f"/invite?t={tokens.derive_token(rig.cfg.token_salt, 'nobody', 'x')}")
    assert "Unknown invite" in spent.text


def test_the_invite_page_exists_when_the_ui_is_off(tmp_path):
    bare = Rig(make_cfg(tmp_path, ui=False))
    invite = bare.issue()
    assert anon(bare).get("/ui").status_code == 404
    assert anon(bare).get(f"/invite?t={invite}").status_code == 200


def test_the_page_html_escapes_everything(rig):
    invite = rig.issue()
    page = anon(rig).get(f"/invite?t={invite}<script>")
    assert "<script>" not in page.text
    named = rig.issue("carol")
    assert anon(rig).get(f"/invite?t={named}").status_code == 200
