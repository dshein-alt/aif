"""The `gatekeeper` system account: who may act as it, and what a config (admin) token may do."""

from __future__ import annotations

import pytest
from conftest import ADMIN, Rig, make_cfg
from fastapi.testclient import TestClient

from aif import db
from aif.app import create_app
from aif.config import ADMIN_NAME, DEFAULT_SALT, DEFAULT_TOKEN, Config, ConfigError


@pytest.fixture()
def rig(tmp_path) -> Rig:
    return Rig(make_cfg(tmp_path))


# ------------------------------------------------------------------- the account exists


def test_the_system_account_is_seeded_and_idempotent(tmp_path):
    cfg = make_cfg(tmp_path)
    db.init(cfg)
    db.init(cfg)  # starting twice must not create a second row or reset anything
    with db.reader(cfg) as conn:
        rows = list(conn.execute("SELECT name, low, descr FROM agents"))
    assert [r["name"] for r in rows] == [ADMIN_NAME]
    assert "system account" in rows[0]["descr"]


def test_system_account_is_listed_as_a_special_agent(rig):
    rig.claim("bot1")
    body = rig.admin.get("/api/agents").json()
    top = body["a"][0]
    assert top["n"] == ADMIN_NAME and top["sys"] == 1 and top["on"] == 1
    assert {a["n"] for a in body["a"]} == {ADMIN_NAME, "bot1"}
    assert ADMIN_NAME in rig.admin.get("/api/online").json()["on"]


def test_no_one_may_register_the_reserved_name(rig):
    for candidate in [ADMIN_NAME, "GateKeeper", "GATEKEEPER", " gatekeeper "]:
        res = rig.claim(candidate)  # invite flow
        assert res.status_code == 403 and res.json()["err"] == "name_reserved", (candidate, res.text)
        res = rig.admin.post("/api/agents", json={"name": candidate})  # or directly by the admin
        assert res.status_code == 403 and res.json()["err"] == "name_reserved", (candidate, res.text)
    assert rig.claim("gate_keeper").status_code == 200  # a different name is fine


# ----------------------------------------------------------------------- acting as it


def test_an_agent_token_cannot_act_as_the_system_account(rig):
    rig.claim("bot2")
    for call in (
        rig.client(rig.agent_tokens["bot2"], **{"x-agent": ADMIN_NAME}).get("/api/unread"),
        rig.client(rig.agent_tokens["bot2"], **{"x-agent": ADMIN_NAME}).post("/api/threads", json={"subject": "fake", "b": "x"}),
    ):
        assert call.status_code == 403 and call.json()["err"] == "token_agent_mismatch", call.text
    spoofed = rig.client(rig.agent_tokens["bot2"]).post(
        "/mcp",
        json={"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "unread", "arguments": {"agent": ADMIN_NAME}}},
    ).json()
    assert spoofed["result"]["isError"] is True
    assert "system_account" in spoofed["result"]["content"][0]["text"]  # the run() guard rejects it on any transport


def test_a_gatekeeper_token_without_a_name_acts_as_the_system_account(rig):
    made = rig.admin.post("/api/threads", json={"subject": "pinned rules", "b": "read me"}).json()
    assert made["ok"] == 1
    assert rig.admin.get(f"/api/messages/{made['i']}").json()["a"] == ADMIN_NAME


def test_a_gatekeeper_token_may_act_as_any_agent(rig):
    rig.claim("worker")
    tid = rig.client(rig.agent_tokens["worker"]).post("/api/threads", json={"subject": "work", "b": "yours"}).json()["t"]
    made = rig.admin.post(f"/api/threads/{tid}/msgs", json={"b": "posted on your behalf"}, headers={"x-agent": "worker"})
    assert made.status_code == 200 and rig.admin.get(f"/api/messages/{made.json()['i']}").json()["a"] == "worker"
    assert rig.admin.post("/api/threads", json={"subject": "as nobody", "b": "x"}, headers={"x-agent": "ghost"}).status_code == 401


def test_the_system_account_can_still_be_read_by_anyone(rig):
    rig.claim("reader")
    rig.admin.post("/api/threads", json={"subject": "notice", "b": "from the server"}, headers={"x-agent": ADMIN_NAME})
    tid = rig.client(rig.agent_tokens["reader"]).get("/api/threads?q=notice").json()["th"][0]["i"]
    page = rig.client(rig.agent_tokens["reader"]).get(f"/api/threads/{tid}").json()
    assert page["a"] == ADMIN_NAME and page["ms"][0]["a"] == ADMIN_NAME


# ----------------------------------------------------------------------- privileges


def test_a_gatekeeper_token_may_delete_anything(rig):
    rig.claim("owner")
    rig.claim("nosey")
    owner = rig.client(rig.agent_tokens["owner"])
    mid = owner.post("/api/threads", json={"subject": "spam", "b": "x"}).json()["i"]
    tid = owner.get(f"/api/messages/{mid}").json()["t"]
    blocked = rig.client(rig.agent_tokens["nosey"]).post("/api/op", json={"do": "rm", "what": "thread", "id": tid})
    assert blocked.status_code == 403 and blocked.json()["err"] == "not_yours"
    gone = rig.admin.post("/api/op", json={"do": "rm", "what": "thread", "id": tid}, headers={"x-agent": ADMIN_NAME})
    assert gone.status_code == 200 and gone.json()["gone"] == f"thread:{tid}"
    assert owner.get(f"/api/threads/{tid}").status_code == 404


def test_a_gatekeeper_token_registers_on_behalf_of_others(rig):
    res = rig.admin.post("/api/agents", json={"name": "issued-one"})
    assert res.status_code == 200 and "by" not in res.json() and "token" not in res.json()  # no claim flow here
    on_behalf = rig.admin.post("/api/agents", json={"name": "issued-two"}, headers={"x-agent": "issued-one"})
    assert on_behalf.json()["by"] == "issued-one"
    assert rig.claim("issued-two").json()["err"] == "name_taken"


def test_an_ordinary_agent_cannot_register_a_second_name(rig):
    rig.claim("onlyme")
    res = rig.client(rig.agent_tokens["onlyme"]).post("/api/agents", json={"name": "secondone"})
    assert res.status_code == 409 and res.json()["err"] == "already_registered"
    # ... and it cannot just register without a token either:
    anon = TestClient(create_app(rig.cfg, mount_ui=False))
    assert anon.post("/api/agents", json={"name": "thirdone"}).status_code == 401
    res = rig.client(rig.agent_tokens["onlyme"]).post("/api/agents", json={"name": "thirdone"}, headers={"x-agent": ""})
    assert res.json()["err"] == "already_registered"


def test_ping_reports_who_the_token_thinks_you_are(rig):
    shown = rig.admin.get("/api/ping").json()
    assert shown["as"] == ADMIN_NAME and shown["admin"] == 1
    rig.claim("bot9")
    named = rig.client(rig.agent_tokens["bot9"]).get("/api/ping").json()
    assert named["as"] == "bot9" and "admin" not in named


# --------------------------------------------------------------------------- config


def test_admin_token_defaults_to_the_service_token(tmp_path):
    cfg = Config(tokens=["shared"], token_salt="s", data_dir=str(tmp_path))
    assert cfg.admin_tokens == ["shared"] and cfg.admin_token_ok("shared")
    assert Config(tokens=["a", "b"], token_salt="s", data_dir=str(tmp_path)).admin_tokens == ["a", "b"]


def test_config_tokens_are_the_only_config_level_credentials(tmp_path):
    cfg = Config(tokens=["agent-token"], admin_tokens=["admin-token"], token_salt="s", data_dir=str(tmp_path))
    assert cfg.config_token_ok("agent-token") and cfg.config_token_ok("admin-token")  # AIF_TOKEN is an alias
    assert cfg.admin_token_ok("admin-token") and cfg.admin_token_ok("agent-token")
    assert not cfg.config_token_ok("nope") and not cfg.admin_token_ok("nope")


def test_from_env_reads_the_credentials_and_the_salt(tmp_path):
    cfg = Config.from_env({"AIF_DATA_DIR": str(tmp_path), "AIF_TOKEN": "a1,a2", "AIF_ADMIN_TOKEN": "root1,root2", "AIF_TOKEN_SALT": "salt"})
    assert cfg.tokens == ["a1", "a2"] and cfg.admin_tokens == ["root1", "root2"] and cfg.token_salt == "salt"
    only = Config.from_env({"AIF_DATA_DIR": str(tmp_path), "AIF_TOKEN": "a1", "AIF_TOKEN_SALT": "salt"})
    assert only.admin_tokens == ["a1"]
    empty = Config.from_env({"AIF_DATA_DIR": str(tmp_path), "AIF_TOKEN": "a1", "AIF_ADMIN_TOKEN": "", "AIF_TOKEN_SALT": "salt"})
    assert empty.admin_tokens == ["a1"]  # compose passes unset vars as ""


def test_the_insecure_defaults_are_refused(tmp_path):
    with pytest.raises(ConfigError, match="AIF_TOKEN is the insecure default"):
        Config(tokens=[DEFAULT_TOKEN], token_salt="real-salt", data_dir=str(tmp_path)).validate()
    with pytest.raises(ConfigError, match="AIF_TOKEN_SALT is the insecure default"):
        Config(tokens=["real"], token_salt=DEFAULT_SALT, data_dir=str(tmp_path)).validate()
    with pytest.raises(ConfigError, match="AIF_TOKEN_SALT is empty"):
        Config(tokens=["real"], token_salt="", data_dir=str(tmp_path)).validate()
    Config(tokens=["real"], token_salt="real-salt", data_dir=str(tmp_path)).validate()


# ------------------------------------------------------------------------------ mcp


def test_mcp_respects_the_same_privileges(rig):
    def call(name, args, token, headers=None):
        return rig.client(token, **(headers or {})).post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": name, "arguments": args}},
        ).json()

    as_system = call("post", {"subject": "mcp notice", "b": "hi"}, ADMIN)
    assert as_system["result"]["isError"] is False and as_system["result"]["structuredContent"]["ok"] == 1
    rig.claim("mcpbot")
    own = call("post", {"subject": "mcp own", "b": "hi"}, rig.agent_tokens["mcpbot"])
    assert own["result"]["structuredContent"]["ok"] == 1
    assert own["result"]["structuredContent"]["i"] != as_system["result"]["structuredContent"]["i"]
