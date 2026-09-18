"""Tests for the other surfaces: MCP (JSON-RPC), the human HTML view, the skill card and the CLI."""

from __future__ import annotations

import json
import re
import subprocess
import sys

import pytest
from conftest import ADMIN, Rig, make_cfg
from fastapi.testclient import TestClient

from aif.app import create_app
from aif.core import OPS
from aif.mcp import INSTRUCTIONS

TOKEN = ADMIN  # these fixtures speak with the gatekeeper token; agents are claimed via rig


@pytest.fixture()
def cli(tmp_path):
    rig = Rig(make_cfg(tmp_path, ui=True), ui=True)
    rig.claim("alice", descr="first")
    rig.claim("bob")
    client = rig.admin
    client.rig = rig
    return client


def rpc(client, method, params=None, rid=1, headers=None):
    body = {"jsonrpc": "2.0", "id": rid, "method": method}
    if params is not None:
        body["params"] = params
    return client.post("/mcp", json=body, headers=headers or {})


# ---------------------------------------------------------------------------- MCP


def test_mcp_initialize_teaches_the_client(cli):
    res = rpc(cli, "initialize", {"protocolVersion": "2025-06-18"}).json()["result"]
    assert res["protocolVersion"] == "2025-06-18"
    assert {"tools", "resources", "prompts"} <= set(res["capabilities"])
    assert res["serverInfo"]["name"] == "aif"
    assert "WORK LOOP" in res["instructions"] and "MCP NOTES" in res["instructions"]
    assert rpc(cli, "initialize", {"protocolVersion": "9999-01-01"}, rid=2).json()["result"]["protocolVersion"] == "2025-06-18"


def test_mcp_tools_mirror_the_ops(cli):
    tools = rpc(cli, "tools/list").json()["result"]["tools"]
    assert {t["name"] for t in tools} == set(OPS)
    post = next(t for t in tools if t["name"] == "post")
    assert post["annotations"]["readOnlyHint"] is False and post["annotations"]["destructiveHint"] is False
    files = post["inputSchema"]["properties"]["files"]
    assert files["type"] == "array" and files["items"]["type"] == "object" and "k" in files["items"]["properties"]
    ping = next(t for t in tools if t["name"] == "ping")
    assert ping["annotations"]["readOnlyHint"] is True
    thread = next(t for t in tools if t["name"] == "thread")
    assert thread["inputSchema"]["properties"]["id"]["type"] == "integer"
    assert thread["inputSchema"]["additionalProperties"] is False


def test_mcp_tool_call_read_and_write(cli):
    out = rpc(cli, "tools/call", {"name": "who", "arguments": {}}).json()["result"]
    assert out["isError"] is False and {a["n"] for a in out["structuredContent"]["a"]} == {"alice", "bob", "gatekeeper"}
    made = rpc(cli, "tools/call", {"name": "post", "arguments": {"agent": "alice", "subject": "from mcp", "b": "hi @bob"}}, headers={"x-agent": "alice"}).json()["result"]
    assert made["isError"] is False and made["structuredContent"]["ok"] == 1
    assert rpc(cli, "tools/call", {"name": "threads", "arguments": {}}).json()["result"]["isError"] is False


def test_mcp_an_invite_token_must_be_claimed_first(cli):
    invite = cli.rig.issue()
    out = rpc(cli.rig.client(invite), "tools/call", {"name": "post", "arguments": {"subject": "x", "b": "y"}}).json()["result"]
    assert out["isError"] is True
    assert json.loads(out["content"][0]["text"])["err"] == "claim_required"
    joined = rpc(cli.rig.client(invite), "tools/call", {"name": "register", "arguments": {"name": "mcpjoin"}}).json()["result"]
    assert joined["isError"] is False and joined["structuredContent"]["token"].startswith("aif_")


def test_mcp_tool_errors_carry_hints(cli):
    out = rpc(cli, "tools/call", {"name": "thread", "arguments": {"id": 4242}}).json()["result"]
    assert out["isError"] is True
    body = json.loads(out["content"][0]["text"])
    assert body["err"] == "no_thread" and body["hint"]
    unknown = rpc(cli, "tools/call", {"name": "nope", "arguments": {}}).json()["result"]
    assert unknown["isError"] is True and "tools" in json.loads(unknown["content"][0]["text"])


def test_mcp_accepts_agent_inside_arguments(cli):
    made = rpc(cli, "tools/call", {"name": "post", "arguments": {"agent": "bob", "subject": "via args", "b": "x"}}).json()["result"]
    assert made["isError"] is False
    assert cli.get("/api/threads?sort=id").json()["th"][0]["a"] == "bob"


def test_mcp_resources_and_prompts(cli):
    assert {r["uri"] for r in rpc(cli, "resources/list").json()["result"]["resources"]} == {"aif://skill", "aif://limits"}
    skill = rpc(cli, "resources/read", {"uri": "aif://skill"}).json()["result"]["contents"][0]
    assert skill["mimeType"] == "text/plain" and "WORK LOOP" in skill["text"]
    limits = json.loads(rpc(cli, "resources/read", {"uri": "aif://limits"}).json()["result"]["contents"][0]["text"])
    assert limits["max_file_bytes"] == 5 * 1024 * 1024
    prompt = rpc(cli, "prompts/get", {"name": "aif-agent", "arguments": {"name": "zoe", "descr": "watcher"}}).json()["result"]
    text = prompt["messages"][0]["content"]["text"]
    assert '"name":"zoe"' in text and "YOUR TASK" in text
    assert rpc(cli, "prompts/list").json()["result"]["prompts"][0]["arguments"][0]["name"] == "name"


def test_mcp_protocol_errors(cli):
    assert rpc(cli, "bogus/method").json()["error"]["code"] == -32601
    assert rpc(cli, "resources/read", {"uri": "aif://nope"}).json()["error"]["code"] == -32002
    assert rpc(cli, "prompts/get", {"name": "nope"}).json()["error"]["code"] == -32602
    broken = cli.post("/mcp", content=b"{oops", headers={"content-type": "application/json"})
    assert broken.status_code == 400 and broken.json()["error"]["code"] == -32700
    assert cli.post("/mcp", json={"jsonrpc": "2.0", "method": "ping"}).status_code == 202  # notification
    assert cli.get("/mcp").status_code == 405
    assert cli.post("/mcp", json={"jsonrpc": "2.0", "id": 1, "method": "ping"}, headers={"authorization": "Bearer nope"}).status_code == 403


def test_mcp_batch_request(cli):
    out = cli.post("/mcp", json=[{"jsonrpc": "2.0", "id": 1, "method": "ping"}, {"jsonrpc": "2.0", "method": "notifications/cancelled"}]).json()
    assert isinstance(out, list) and len(out) == 1 and out[0]["id"] == 1


def test_mcp_instructions_match_the_card():
    assert INSTRUCTIONS.startswith("AIF - AI Interaction Forum") and len(INSTRUCTIONS) < 6000


# ------------------------------------------------------------------- human web view


def test_ui_requires_a_login(tmp_path):
    anon = TestClient(create_app(make_cfg(tmp_path, ui=True), mount_ui=True))
    res = anon.get("/ui")
    assert res.status_code == 401 and "Sign in to read the forum" in res.text
    assert anon.get("/ui?token=nope").status_code == 403  # accept-once rejects clearly
    assert anon.get(f"/ui?token={TOKEN}").status_code == 200  # accept-once -> cookie -> content
    fresh = TestClient(create_app(make_cfg(tmp_path, ui=True), mount_ui=True))
    assert fresh.get("/ui", headers={"authorization": f"Bearer {TOKEN}"}).status_code == 401  # the cookie, not a header


def test_ui_lists_threads_and_links_with_token(tmp_path):
    client = TestClient(create_app(make_cfg(tmp_path, ui=True), mount_ui=True), headers={"authorization": f"Bearer {TOKEN}"})
    client.post("/api/agents", json={"name": "alice"})
    client.post("/api/threads", json={"subject": "Quarterly plans", "b": "hello"}, headers={"x-agent": "alice"})
    page = client.get(f"/ui?token={TOKEN}").text  # accept-once: cookied from here on
    assert "Quarterly plans" in page and "alice" in page
    assert f"token={TOKEN}" not in page and "?token=" not in page  # no credential survives in any link
    assert 'href="/ui/thread/1"' in page
    detail = client.get("/ui/thread/1").text  # clean URL, the cookie carries the session
    assert "hello" in detail and "alice" in detail and "?token=" not in detail
    assert "Agents" in detail
    assert "alice" in client.get("/ui/agents").text


def test_ui_escapes_hostile_content(cli):
    cli.post("/api/agents", json={"name": "evil"})
    cli.post("/api/threads", json={"subject": "<script>alert(1)</script>", "b": "<img src=x onerror=alert(1)> @bob"}, headers={"x-agent": "evil"})
    index = cli.get(f"/ui?token={TOKEN}").text
    detail = cli.get(f"/ui/thread/1?token={TOKEN}").text
    assert "<script>" not in index and "&lt;script&gt;" in index
    assert "<img" not in detail and "onerror" in detail  # escaped, but visible as text
    assert 'class=at' in detail and "@bob" in detail  # mentions highlighted


def test_ui_shows_attachments_and_pages_them(cli, tmp_path):
    cli.post("/api/agents", json={"name": "alice"})
    made = cli.post(
        "/api/threads",
        json={"subject": "with file", "b": "see attach", "files": [{"n": "report.txt", "text": "the payload"}]},
        headers={"x-agent": "alice"},
    ).json()
    detail = cli.get(f"/ui/thread/{made['t']}?token={TOKEN}").text  # accept-once cookies us
    href = re.search(r'href="(/ui/files/\d+)"', detail)
    assert href and "report.txt" in detail and "?token=" not in href.group(1)
    meta = cli.get(href.group(1))
    assert 'href="/ui/files/1/raw"' in meta.text  # the metadata page links the cookie-authenticated blob
    raw = cli.get("/ui/files/1/raw")
    assert raw.status_code == 200 and raw.text == "the payload"
    anon = TestClient(create_app(cli.app.state.cfg, mount_ui=True))
    assert anon.get("/ui/files/1/raw").status_code == 401  # but never without the cookie


def test_ui_paging(cli):
    cli.post("/api/agents", json={"name": "alice"})
    tid = cli.post("/api/threads", json={"subject": "long", "b": "m1"}, headers={"x-agent": "alice"}).json()["t"]
    for i in range(2, 31):
        cli.post(f"/api/threads/{tid}/msgs", json={"b": f"m{i}"}, headers={"x-agent": "alice"})
    page = cli.get(f"/ui/thread/{tid}?token={TOKEN}").text
    assert "class=body>m20<" in page and "class=body>m21<" not in page and "class=body>m1<" in page
    older = cli.get(f"/ui/thread/{tid}?token={TOKEN}&before=5").text
    assert "m4" in older and "m5" not in older
    assert "older" in page or "newer" in page


def test_root_redirects_browsers_to_the_ui(cli):
    res = cli.get("/", headers={"accept": "text/html"}, follow_redirects=False)
    assert res.status_code == 303 and res.headers["location"].startswith("/ui")
    landed = cli.get("/", headers={"accept": "text/html"})
    assert landed.status_code == 401 and "Sign in to read the forum" in landed.text  # browsers land on the login page
    assert cli.get("/").json()["ui"] == "/ui"


def test_ui_can_be_switched_off(tmp_path):
    cfg = make_cfg(tmp_path / "uioff", ui=False)
    client = TestClient(create_app(cfg, mount_ui=False))
    client.headers["authorization"] = f"Bearer {TOKEN}"
    assert client.get(f"/ui?token={TOKEN}").status_code == 404
    assert client.get("/api/ping").status_code == 200


def test_ui_search(cli):
    cli.post("/api/agents", json={"name": "alice"})
    cli.post("/api/threads", json={"subject": "Mars rover budget", "b": "x"}, headers={"x-agent": "alice"})
    cli.post("/api/threads", json={"subject": "Coffee", "b": "x"}, headers={"x-agent": "alice"})
    page = cli.get(f"/ui?token={TOKEN}&q=mars").text
    assert "Mars rover budget" in page and "Coffee" not in page


# ---------------------------------------------------------------------------- CLI


def run_cli(*argv, env=None):
    import os
    import tempfile

    # cwd is an empty directory on purpose: ./.env must never leak the developer's real one into tests
    return subprocess.run(
        [sys.executable, "-m", "aif", *argv],
        capture_output=True,
        text=True,
        env={**os.environ, "AIF_TOKEN": "t0ken", "AIF_TOKEN_SALT": "cli-salt", **(env or {})},
        cwd=tempfile.mkdtemp(prefix="aif-cli-"),
    )


def test_cli_token_and_skill():
    token = run_cli("token").stdout.strip()
    assert len(token) >= 24 and re.fullmatch(r"[A-Za-z0-9_-]+", token)
    card = run_cli("skill")
    assert card.returncode == 0 and "WORK LOOP" in card.stdout


def test_cli_init_stats(tmp_path):
    init = run_cli("init", "--data-dir", str(tmp_path / "cli"))
    assert init.returncode == 0 and "aif.db" in init.stdout
    assert (tmp_path / "cli" / "aif.db").exists() and (tmp_path / "cli" / "attachments").is_dir()
    stats = run_cli("stats", "--data-dir", str(tmp_path / "cli"))
    assert stats.returncode == 0, stats.stderr
    assert re.search(r"agents=1", stats.stdout) and "db_bytes=" in stats.stdout  # 1 = the seeded system account


def test_cli_refuses_insecure_default_token(tmp_path):
    import os

    res = subprocess.run(
        [sys.executable, "-m", "aif", "init", "--data-dir", str(tmp_path / "nope")],
        capture_output=True,
        text=True,
        env={k: v for k, v in os.environ.items() if k not in ("AIF_TOKEN", "AIF_TOKEN_SALT", "AIF_ALLOW_DEFAULT_TOKEN")},
        cwd=str(tmp_path),
    )
    assert res.returncode == 2 and "insecure default" in res.stderr


def test_cli_rejects_bad_size(tmp_path):
    res = run_cli("serve", "--data-dir", str(tmp_path / "bad"), "--max-file-size", "5MBB")
    assert res.returncode == 2 and "invalid size" in res.stderr


def test_cli_help_mentions_every_command():
    out = run_cli("--help").stdout
    for cmd in ("serve", "init", "stats", "token", "skill"):
        assert cmd in out


# ------------------------------------------------------------------ docs / openapi


def test_openapi_stays_available_for_humans(cli):
    assert cli.get("/openapi.json").json()["info"]["title"].startswith("AIF")
    assert cli.get("/docs").status_code == 200
    assert cli.get("/healthz").json()["ok"] == 1 and cli.get("/healthz").json()["v"]


def test_mcp_instructions_are_the_card_plus_notes():
    from aif.skill import CARD

    assert INSTRUCTIONS.startswith(CARD)


def test_cli_reads_an_env_file(tmp_path):
    env_file = tmp_path / "custom.env"
    env_file.write_text("# comment\nAIF_TOKEN=from-file\nexport AIF_TOKEN_SALT=\"file-salt\"\nAIF_DATA_DIR='" + str(tmp_path / "filevar") + "'\n")
    res = run_cli("init", "--env-file", str(env_file), env={k: v for k, v in __import__("os").environ.items() if not k.startswith("AIF_")})
    assert res.returncode == 0, res.stderr
    assert (tmp_path / "filevar" / "aif.db").exists()


def test_cli_auto_loads_dot_env_from_the_cwd(tmp_path):
    (tmp_path / ".env").write_text("AIF_TOKEN=auto\nAIF_TOKEN_SALT=auto-salt\n")
    res = subprocess.run(
        [sys.executable, "-m", "aif", "init", "--data-dir", str(tmp_path / "autovar")],
        capture_output=True,
        text=True,
        env={k: v for k, v in __import__("os").environ.items() if not k.startswith("AIF_")},
        cwd=str(tmp_path),
    )
    assert res.returncode == 0, res.stderr
    assert (tmp_path / "autovar" / "aif.db").exists()


def test_real_environment_wins_over_the_env_file(tmp_path):
    env_file = tmp_path / ".env"
    env_file.write_text("AIF_TOKEN=from-file\nAIF_DATA_DIR=" + str(tmp_path / "fromfile") + "\n")
    res = run_cli("init", "--data-dir", str(tmp_path / "fromenv"), "--env-file", str(env_file))
    assert res.returncode == 0, res.stderr
    assert (tmp_path / "fromenv" / "aif.db").exists() and not (tmp_path / "fromfile").exists()  # flag beats file beats nothing


def test_a_broken_env_file_is_a_clear_error(tmp_path):
    bad = tmp_path / "bad.env"
    bad.write_text("NOT_A_LINE\n")
    res = run_cli("init", "--data-dir", str(tmp_path / "x"), "--env-file", str(bad))
    assert res.returncode == 2 and "expected KEY=value" in res.stderr
    missing = run_cli("init", "--data-dir", str(tmp_path / "y"), "--env-file", str(tmp_path / "nope.env"))
    assert missing.returncode == 2 and "cannot read env file" in missing.stderr
