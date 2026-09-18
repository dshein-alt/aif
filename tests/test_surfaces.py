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
from aif.config import ConfigError
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
    assert "<script>alert" not in index and "&lt;script&gt;" in index  # the payload stays text
    assert index.count("<script>") == 1 and "<script src=" not in index  # only the inline auto-refresh
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
    assert "<p>m20</p>" in page and "<p>m21</p>" not in page and "<p>m1</p>" in page
    older = cli.get(f"/ui/thread/{tid}?token={TOKEN}&before=5").text
    assert "m4" in older and "m5" not in older
    assert "page 1 of 2" in page  # numbered navigation, not just older/newer


def test_ui_thread_pages_are_numbered_and_navigable(cli):
    opened = cli.post("/api/threads", json={"subject": "long", "b": "m1"}, headers={"x-agent": "alice"}).json()
    tid, pin_id = opened["t"], opened["i"]
    ids = [cli.post(f"/api/threads/{tid}/msgs", json={"b": f"m{i}"}, headers={"x-agent": "alice"}).json()["i"] for i in range(2, 31)]

    first = cli.get(f"/ui/thread/{tid}?token={TOKEN}&limit=10").text  # accept-once cookies us
    assert "<p>m10</p>" in first and "<p>m11</p>" not in first
    assert first.count("<p>m1</p>") == 1  # the description shows once, not twice
    assert f"#{1} <span class=id>[{pin_id}]" in first  # ... and says which post it is, by both numbers
    assert "page 1 of 3" in first and first.count("class=pager") == 2  # above *and* below the list
    assert f'id="m-{ids[1]}"' in first and f'id="m-{ids[8]}"' in first and f'id="m-{ids[9]}"' not in first
    assert "page=1" not in first  # page 1 is the default: its links stay clean

    second = cli.get(f"/ui/thread/{tid}?page=2&limit=10").text
    assert "<p>m11</p>" in second and "<p>m20</p>" in second and "<p>m21</p>" not in second
    assert "page 2 of 3" in second and f'id="m-{ids[9]}"' in second and f'href="#m-{ids[9]}"' in second
    assert "prev" in second and "page=3" in second and "first" not in second  # page 1 is one click back
    assert f'href="/ui/thread/{tid}?limit=10"' in second  # back to page 1 without a page= in the URL

    last = cli.get(f"/ui/thread/{tid}?page=3&limit=10").text
    assert "<p>m21</p>" in last and "<p>m30</p>" in last
    assert "page 3 of 3" in last and "next" not in last and "first" in last and f'id="m-{ids[28]}"' in last

    stale = cli.get(f"/ui/thread/{tid}?page=99&limit=10").text  # a page that deletions left behind
    assert "page 3 of 3" in stale and "<p>m30</p>" in stale

    whole = cli.get(f"/ui/thread/{tid}?limit=500").text
    assert "class=pager" not in whole and "page 1 of 1" not in whole  # one page needs no bar
    assert "<p>m1</p>" in whole and "<p>m30</p>" in whole  # oldest first, newest last


def test_ui_shows_both_the_position_and_the_message_id(cli):
    """``#15`` is the place in this thread, ``[154]`` is the id every cross-reference quotes. One
    number alone reads as a contradiction against the text of a reply, so both are always shown
    and the link follows the id, which never moves."""
    opened = cli.post("/api/threads", json={"subject": "two numbers", "b": "m1"}, headers={"x-agent": "alice"}).json()
    tid = opened["t"]
    ids = [cli.post(f"/api/threads/{tid}/msgs", json={"b": f"m{i}"}, headers={"x-agent": "alice"}).json()["i"] for i in range(2, 6)]

    page = cli.get(f"/ui/thread/{tid}?token={TOKEN}&limit=50").text
    assert f"#1 <span class=id>[{opened['i']}]" in page  # the pinned description carries them too
    for pos, mid in enumerate(ids, start=2):
        assert f"#{pos} <span class=id>[{mid}]" in page

    cli.delete(f"/api/messages/{ids[0]}")  # m2 goes away and every later position shifts down
    after = cli.get(f"/ui/thread/{tid}?token={TOKEN}&limit=50").text
    assert f"[{ids[0]}]" not in after and f'id="m-{ids[0]}"' not in after  # the deleted post is gone
    assert f"#2 <span class=id>[{ids[1]}]" in after  # its neighbour kept its id and took the freed place
    assert f"href=\"#m-{ids[1]}\"" in after and f"#3 <span class=id>[{ids[2]}]" in after
    assert f"#3 <span class=id>[{ids[1]}]" not in after  # the old pairing is what must not survive


def test_seeded_threads_stay_at_the_top_of_the_thread_list(tmp_path):
    """A reader who lands on /ui must find the manual and the lobby without paging, whatever the
    activity order does to them."""
    rig = Rig(make_cfg(tmp_path, ui=True, seed=True), ui=True)
    rig.claim("alice")
    cli = rig.admin
    cli.get("/api/ping")  # first request seeds READ ME FIRST and CHITCHAT
    for i in range(3, 14):  # busy threads that would otherwise out-rank a quiet manual
        tid = cli.post("/api/threads", json={"subject": f"busy {i}", "b": "open"}, headers={"x-agent": "alice"}).json()["t"]
        cli.post(f"/api/threads/{tid}/msgs", json={"b": "latest"}, headers={"x-agent": "alice"})

    page = cli.get(f"/ui?token={ADMIN}").text
    rows = re.findall(r'<a href="/ui/thread/(\d+)">', page)
    assert rows[:2] == ["1", "2"], rows  # READ ME FIRST, then CHITCHAT, whatever the sort says
    assert "pinned threads first" in page
    assert page.count("READ ME FIRST") == 1 and page.count(">CHITCHAT<") == 1  # never listed twice

    quiet = cli.get("/ui?limit=5").text  # the seeded pair is shown even when the window is small
    assert re.findall(r'<a href="/ui/thread/(\d+)">', quiet)[:2] == ["1", "2"]
    assert "busy 13" in quiet and "pinned threads first" in quiet

    assert "READ ME FIRST" not in cli.get("/ui?q=busy").text  # a search returns what was asked for


def test_ui_cursor_links_still_number_posts(cli):
    opened = cli.post("/api/threads", json={"subject": "long", "b": "m1"}, headers={"x-agent": "alice"}).json()
    tid = opened["t"]
    ids = [cli.post(f"/api/threads/{tid}/msgs", json={"b": f"m{i}"}, headers={"x-agent": "alice"}).json()["i"] for i in range(2, 31)]
    cli.get(f"/ui?token={TOKEN}")  # accept-once: the cookie carries the session from here on
    older = cli.get(f"/ui/thread/{tid}?before=15&limit=5").text
    assert "<p>m10</p>" in older and "<p>m14</p>" in older and "<p>m15</p>" not in older
    assert f'id="m-{ids[8]}"' in older and f'id="m-{ids[12]}"' in older  # numbered by position, cursor or not
    assert "earlier" in older and "page " not in older  # the old two-link pager, no page bar


def test_ui_reading_views_reload_in_place(tmp_path):
    """A thread left open follows the conversation without throwing the reader back to the top -
    and without stranding a reader who was at the bottom, which is where a live thread is read."""
    rig = Rig(make_cfg(tmp_path, ui=True, ui_refresh=90), ui=True)
    rig.claim("alice")
    cli = rig.admin
    tid = cli.post("/api/threads", json={"subject": "reader", "b": "m1"}, headers={"x-agent": "alice"}).json()["t"]
    cli.get(f"/ui/thread/{tid}?token={ADMIN}")  # accept-once, then the cookie is enough
    for url in ("/ui", f"/ui/thread/{tid}", "/ui/agents"):
        reading = cli.get(url).text
        assert "sessionStorage" in reading and "90000" in reading and "document.hidden" in reading
        assert "'bottom'" in reading  # a reader at the bottom is stored as a place, not a pixel
        assert "pagehide" in reading and "beforeunload" not in reading  # the hook that survives bfcache
        assert "visibilitychange" in reading  # a tab hidden past the interval catches up on return
        assert "reloads every 90s" in reading  # and the page admits it does this, or it looks dead
        assert "<script src=" not in reading  # inline only: still no assets and nothing third-party
    assert "<script" not in cli.get("/ui/files/99999").text  # an error page ships nothing


def test_ui_auto_refresh_can_be_turned_off(tmp_path):
    rig = Rig(make_cfg(tmp_path, ui=True, ui_refresh=0), ui=True)
    cli = rig.admin
    cli.get(f"/ui?token={ADMIN}")
    assert "<script" not in cli.get("/ui").text
    assert "reloads every" not in cli.get("/ui").text  # the footer does not promise what is switched off


def test_ui_refresh_below_the_floor_is_refused(tmp_path):
    """An interval of a few seconds would turn every open tab into a load generator."""
    with pytest.raises(ConfigError):
        make_cfg(tmp_path, ui=True, ui_refresh=5).validate()


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


# ----------------------------------------------------------------------- markdown in /ui


def test_ui_renders_message_bodies_as_markdown(cli):
    cli.post("/api/agents", json={"name": "alice"})
    made = cli.post(
        "/api/threads",
        json={"subject": "fmt", "b": "**bold** and `code`\n\n- one\n- two\n\n> a quote\n\n@bob look"},
        headers={"x-agent": "alice"},
    ).json()
    cli.get(f"/ui?token={TOKEN}")  # accept-once: cookie for the rest
    page = cli.get(f"/ui/thread/{made['t']}").text
    assert "<strong>bold</strong>" in page and "<code>code</code>" in page
    assert "<li>one</li>" in page and "<blockquote>" in page
    assert '<span class=at>@bob</span>' in page  # mentions still highlighted after rendering
    assert cli.get(f"/api/messages/{made['i']}").json()["b"].startswith("**bold**")  # the API keeps raw text


def test_markdown_never_turns_agent_text_into_markup(cli):
    cli.post("/api/agents", json={"name": "evil"})
    made = cli.post(
        "/api/threads",
        json={"subject": "xss", "b": "<script>alert(1)</script> and <img src=x onerror=alert(1)>", "at": []},
        headers={"x-agent": "evil"},
    ).json()
    cli.post(f"/api/threads/{made['t']}/msgs", json={"b": "and a parsed link: [click](javascript:alert(1))"}, headers={"x-agent": "evil"})
    cli.get(f"/ui?token={TOKEN}")
    page = cli.get(f"/ui/thread/{made['t']}").text
    assert "<script>alert" not in page and "<img" not in page  # no live tags at all
    assert "&lt;script&gt;" in page and "&lt;img" in page  # both visible as escaped text
    assert 'href="javascript:' not in page and "#harmful-link" in page  # parsed dangerous links are neutralised


def test_mentions_inside_code_spans_are_not_decorated(cli):
    cli.post("/api/agents", json={"name": "alice"})
    made = cli.post("/api/threads", json={"subject": "m", "b": "no mentions in the opener"}, headers={"x-agent": "alice"}).json()
    cli.post(f"/api/threads/{made['t']}/msgs", json={"b": "ping `@bob` in code, and @bob for real"}, headers={"x-agent": "alice"})
    cli.get(f"/ui?token={TOKEN}")
    page = cli.get(f"/ui/thread/{made['t']}").text
    assert "<code>@bob</code>" in page  # left alone inside code
    assert "and <span class=at>@bob</span> for real" in page  # decorated in prose
    assert page.count('<span class=at>@bob</span>') == 2  # the prose one + the header mention chip
