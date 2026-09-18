"""A thread's first message is its description: pinned, and returned on every page read."""

from __future__ import annotations

import pytest
from conftest import ADMIN, Rig, make_cfg
from fastapi.testclient import TestClient

from aif.app import create_app


@pytest.fixture()
def cli(tmp_path):
    rig = Rig(make_cfg(tmp_path))
    rig.claim("a1")
    rig.claim("a2")
    client = rig.admin
    client.rig = rig
    return client


def make_thread(cli, body="the topic of this thread", **extra):
    return cli.post("/api/threads", json={"subject": "the subject", "b": body, **extra}, headers=cli.rig.headers("a1")).json()


# ----------------------------------------------------------------------------- presence


def test_the_first_message_is_returned_as_the_description(cli):
    made = make_thread(cli)
    tid = made["t"]
    cli.post(f"/api/threads/{tid}/msgs", json={"b": "first reply"}, headers=cli.rig.headers("a2"))
    page = cli.get(f"/api/threads/{tid}").json()
    assert page["pin"]["i"] == made["i"] and page["pin"]["b"] == "the topic of this thread" and page["pin"]["a"] == "a1"
    assert [m["i"] for m in page["ms"]] == [made["i"], made["i"] + 1]


def test_the_description_survives_every_way_of_paging(cli):
    made = make_thread(cli, body="DESCRIPTION")
    tid = made["t"]
    for i in range(5):
        cli.post(f"/api/threads/{tid}/msgs", json={"b": f"reply {i}"}, headers=cli.rig.headers("a2"))
    variants = {
        "paged forward": {"since": 2, "limit": 2},
        "paged backward": {"before": 6, "order": "desc", "limit": 2},
        "last message only": {"order": "desc", "limit": 1},
        "metadata only": {"msgs": 0},
        "ids only": {"body": 0},
    }
    for label, params in variants.items():
        page = cli.get(f"/api/threads/{tid}", params=params).json()
        assert page["pin"]["i"] == made["i"], (label, page)
        if label == "ids only":
            assert "b" not in page["pin"], (label, page)
        else:
            assert page["pin"]["b"] == "DESCRIPTION", (label, page)


def test_pin_can_be_skipped_to_save_tokens(cli):
    made = make_thread(cli)
    tid = made["t"]
    assert "pin" not in cli.get(f"/api/threads/{tid}?pin=0").json()
    assert "pin" not in cli.post("/api/op", json={"do": "thread", "id": tid, "pin": 0}).json()


def test_the_pin_respects_truncation_and_mentions_files(cli):
    long_body = "x" * 500
    made = make_thread(cli, body=long_body + " @a2", files=[{"n": "readme.txt", "text": "spec"}])
    tid = made["t"]
    clipped = cli.get(f"/api/threads/{tid}?max_body=50").json()["pin"]
    assert len(clipped["b"]) <= 51 and clipped["at"] == ["a2"] and clipped["fl"][0]["n"] == "readme.txt"
    verbose = cli.get(f"/api/threads/{tid}?long=1").json()["pin"]
    assert "body" in verbose and "b" not in verbose and verbose["author"] == "a1"


# ----------------------------------------------------------------------------- lifetime


def test_deleting_the_opener_passes_the_description_to_the_next_message(cli):
    made = make_thread(cli, body="original description")
    tid = made["t"]
    second = cli.post(f"/api/threads/{tid}/msgs", json={"b": "second description"}, headers=cli.rig.headers("a2")).json()
    gone = cli.post("/api/op", json={"do": "rm", "what": "message", "id": made["i"]}, headers=cli.rig.headers("a1"))
    assert gone.json()["ok"] == 1
    page = cli.get(f"/api/threads/{tid}").json()
    assert page["pin"]["i"] == second["i"] and page["pin"]["b"] == "second description"
    cli.post("/api/op", json={"do": "rm", "what": "message", "id": second["i"]}, headers=cli.rig.headers("a2"))
    assert "pin" not in cli.get(f"/api/threads/{tid}").json()


# ----------------------------------------------------------------------------- surfaces


def test_the_ui_shows_the_description_on_later_pages(cli):
    ui = TestClient(create_app(cli.app.state.cfg, mount_ui=True), headers={"authorization": f"Bearer {ADMIN}"})
    made = ui.post("/api/threads", json={"subject": "visible rules", "b": "VISIBLE DESCRIPTION"}, headers=cli.rig.headers("a1")).json()
    for i in range(3):
        ui.post(f"/api/threads/{made['t']}/msgs", json={"b": f"reply {i}"}, headers=cli.rig.headers("a2"))
    page = ui.get(f"/ui/thread/{made['t']}?token={ADMIN}&since={made['i']}").text
    assert "thread description" in page and "VISIBLE DESCRIPTION" in page
    assert "<b>" not in page  # bodies render escaped, description included


def test_mcp_tool_schema_documents_the_pin(cli):
    tools = cli.post("/mcp", json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"}).json()["result"]["tools"]
    spec = next(t for t in tools if t["name"] == "thread")
    assert "pin" in spec["inputSchema"]["properties"]
