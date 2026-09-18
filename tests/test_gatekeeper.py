"""The `gatekeeper` system account: who may act as it, and what an admin token may do."""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from aif import db
from aif.app import create_app
from aif.config import ADMIN_NAME, Config

ADMIN = "sup3r-secret-admin"
AGENT = "ag3nt-token"


def cfg_for(tmp_path, **overrides) -> Config:
    return Config(
        tokens=[AGENT],
        admin_tokens=[ADMIN],
        seed=False,
        data_dir=str(tmp_path),
        db_path=str(tmp_path / "aif.db"),
        attachments_dir=str(tmp_path / "attachments"),
        **overrides,
    )


@pytest.fixture()
def admin(tmp_path):
    """Holds the gatekeeper token."""
    return TestClient(create_app(cfg_for(tmp_path), mount_ui=False), headers={"authorization": f"Bearer {ADMIN}"})


@pytest.fixture()
def agent(tmp_path):
    """Holds an ordinary agent token (no privileges)."""

    class Pair:
        def __init__(self, client):
            self.client = client
            self.admin = TestClient(create_app(cfg_for(tmp_path), mount_ui=False), headers={"authorization": f"Bearer {ADMIN}"})

        def register(self, name, **kw):
            r = self.client.post("/api/agents", json={"name": name, **kw})
            assert r.status_code == 200, r.text
            return r

        def as_agent(self, name):
            return {"x-agent": name}

    return Pair(TestClient(create_app(cfg_for(tmp_path), mount_ui=False), headers={"authorization": f"Bearer {AGENT}"}))


# ------------------------------------------------------------------- the account exists


def test_the_system_account_is_seeded_and_idempotent(tmp_path):
    cfg = cfg_for(tmp_path)
    db.init(cfg)
    db.init(cfg)  # starting twice must not create a second row or reset anything
    with db.reader(cfg) as conn:
        rows = list(conn.execute("SELECT name, low, descr FROM agents"))
    assert [r["name"] for r in rows] == [ADMIN_NAME]
    assert "system account" in rows[0]["descr"]


def test_system_account_is_listed_as_a_special_agent(agent):
    agent.register("bot1")
    body = agent.client.get("/api/agents").json()
    top = body["a"][0]
    assert top["n"] == ADMIN_NAME and top["sys"] == 1 and top["on"] == 1
    assert {a["n"] for a in body["a"]} == {ADMIN_NAME, "bot1"}
    assert ADMIN_NAME in agent.client.get("/api/online").json()["on"]


def test_no_one_may_register_the_reserved_name(agent, admin):
    for caller in (agent.client, admin):
        for candidate in [ADMIN_NAME, "GateKeeper", "GATEKEEPER", " gatekeeper "]:
            res = caller.post("/api/agents", json={"name": candidate})
            assert res.status_code == 403 and res.json()["err"] == "name_reserved", (candidate, res.text)
    assert agent.client.post("/api/agents", json={"name": "gate_keeper"}).status_code == 200  # a different name is fine


# ------------------------------------------------------------------- impersonation guard


def test_an_agent_token_cannot_act_as_the_system_account(agent):
    for call in (
        agent.client.get("/api/unread", headers=agent.as_agent(ADMIN_NAME)),
        agent.client.post("/api/threads", json={"subject": "fake", "b": "x"}, headers=agent.as_agent(ADMIN_NAME)),
        agent.client.post("/api/op", json={"do": "poll"}, headers=agent.as_agent(ADMIN_NAME)),
    ):
        assert call.status_code == 403 and call.json()["err"] == "system_account", call.text
        assert "AIF_ADMIN_TOKEN" in call.json()["hint"]


def test_an_agent_token_without_a_name_still_has_no_identity(agent):
    assert agent.client.post("/api/threads", json={"subject": "s", "b": "x"}).status_code == 401
    assert agent.client.get("/api/poll").status_code == 401


def test_the_system_account_can_still_be_read_by_anyone(agent):
    agent.admin.post("/api/threads", json={"subject": "notice", "b": "from the server"}, headers=agent.as_agent(ADMIN_NAME))
    tid = agent.client.get("/api/threads?q=notice").json()["th"][0]["i"]
    page = agent.client.get(f"/api/threads/{tid}").json()
    assert page["a"] == ADMIN_NAME and page["ms"][0]["a"] == ADMIN_NAME


# ----------------------------------------------------------------------- privileges


def test_a_gatekeeper_token_without_a_name_acts_as_the_system_account(admin):
    made = admin.post("/api/threads", json={"subject": "pinned rules", "b": "read me"}).json()
    assert made["ok"] == 1
    assert admin.get(f"/api/messages/{made['i']}").json()["a"] == ADMIN_NAME


def test_a_gatekeeper_token_may_act_as_any_agent(admin, agent):
    agent.register("worker")
    tid = agent.client.post("/api/threads", json={"subject": "work", "b": "yours"}, headers=agent.as_agent("worker")).json()["t"]
    made = admin.post(f"/api/threads/{tid}/msgs", json={"b": "posted on your behalf"}, headers=agent.as_agent("worker"))
    assert made.status_code == 200, made.text
    assert admin.get(f"/api/messages/{made.json()['i']}").json()["a"] == "worker"
    assert admin.post("/api/threads", json={"subject": "as nobody", "b": "x"}, headers=agent.as_agent("ghost")).status_code == 401


def test_a_gatekeeper_token_may_delete_anything(agent):
    agent.register("owner")
    agent.register("nosey")
    mid = agent.client.post("/api/threads", json={"subject": "spam", "b": "x"}, headers=agent.as_agent("owner")).json()["i"]
    tid = agent.client.get(f"/api/messages/{mid}").json()["t"]
    blocked = agent.client.post("/api/op", json={"do": "rm", "what": "thread", "id": tid}, headers=agent.as_agent("nosey"))
    assert blocked.status_code == 403 and blocked.json()["err"] == "not_yours"
    gone = agent.admin.post("/api/op", json={"do": "rm", "what": "thread", "id": tid}, headers=agent.as_agent(ADMIN_NAME))
    assert gone.status_code == 200 and gone.json()["gone"] == f"thread:{tid}"
    assert agent.client.get(f"/api/threads/{tid}").status_code == 404


def test_a_gatekeeper_token_registers_on_behalf_of_others(agent):
    res = agent.admin.post("/api/agents", json={"name": "issued-one"})
    assert res.status_code == 200 and "by" not in res.json()  # gatekeeper itself is the issuer
    on_behalf = agent.admin.post("/api/agents", json={"name": "issued-two"}, headers=agent.as_agent("issued-one"))
    assert on_behalf.json()["by"] == "issued-one"
    assert agent.client.post("/api/agents", json={"name": "issued-two"}).json()["err"] == "name_taken"


def test_an_ordinary_agent_cannot_register_a_second_name(agent):
    agent.register("onlyme")
    res = agent.client.post("/api/agents", json={"name": "secondone"}, headers=agent.as_agent("onlyme"))
    assert res.status_code == 409 and res.json()["err"] == "already_registered"


def test_ping_reports_who_the_token_thinks_you_are(agent, admin):
    shown = admin.get("/api/ping").json()
    assert shown["as"] == ADMIN_NAME and shown["admin"] == 1
    plain = agent.client.get("/api/ping").json()
    assert "admin" not in plain and "as" not in plain
    agent.register("bot9")
    named = agent.client.get("/api/ping", headers=agent.as_agent("bot9")).json()
    assert named["as"] == "bot9" and "admin" not in named


# --------------------------------------------------------------------------- config


def test_admin_token_defaults_to_the_service_token(tmp_path):
    cfg = Config(tokens=["shared"], data_dir=str(tmp_path))
    assert cfg.admin_tokens == ["shared"] and cfg.admin_token_ok("shared")
    assert Config(tokens=["a", "b"], data_dir=str(tmp_path)).admin_tokens == ["a", "b"]


def test_admin_token_is_a_separate_secret(tmp_path):
    cfg = Config(tokens=["agent-token"], admin_tokens=["admin-token"], data_dir=str(tmp_path))
    assert cfg.agent_token_ok("agent-token") and cfg.agent_token_ok("admin-token")
    assert cfg.admin_token_ok("admin-token") and not cfg.admin_token_ok("agent-token")
    assert not cfg.agent_token_ok("nope") and not cfg.admin_token_ok("nope")


def test_from_env_reads_both_variables(tmp_path):
    cfg = Config.from_env({"AIF_DATA_DIR": str(tmp_path), "AIF_TOKEN": "a1,a2", "AIF_ADMIN_TOKEN": "root1,root2"})
    assert cfg.tokens == ["a1", "a2"] and cfg.admin_tokens == ["root1", "root2"]
    only = Config.from_env({"AIF_DATA_DIR": str(tmp_path), "AIF_TOKEN": "a1"})
    assert only.admin_tokens == ["a1"]
    empty = Config.from_env({"AIF_DATA_DIR": str(tmp_path), "AIF_TOKEN": "a1", "AIF_ADMIN_TOKEN": ""})  # compose passes unset vars as ""
    assert empty.admin_tokens == ["a1"]


def test_the_insecure_default_check_covers_the_admin_token(tmp_path):
    from aif.config import DEFAULT_TOKEN, ConfigError

    with pytest.raises(ConfigError):
        Config(tokens=["real"], admin_tokens=[DEFAULT_TOKEN], data_dir=str(tmp_path)).validate()
    Config(tokens=["real"], admin_tokens=["admin"], data_dir=str(tmp_path)).validate()


# ------------------------------------------------------------------------------ mcp


def test_mcp_respects_the_same_privileges(agent):
    def call(name, args, token, headers=None):
        return agent.client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": name, "arguments": args}},
            headers={"authorization": f"Bearer {token}", **(headers or {})},
        ).json()

    as_system = call("post", {"subject": "mcp notice", "b": "hi"}, ADMIN)
    assert as_system["result"]["isError"] is False and as_system["result"]["structuredContent"]["ok"] == 1
    spoofed = call("unread", {}, AGENT, {"x-agent": ADMIN_NAME})
    assert spoofed["result"]["isError"] is True and "system_account" in spoofed["result"]["content"][0]["text"]
    agent.register("mcpbot")
    own = call("post", {"subject": "mcp own", "b": "hi"}, AGENT, {"x-agent": "mcpbot"})
    assert own["result"]["structuredContent"]["ok"] == 1
