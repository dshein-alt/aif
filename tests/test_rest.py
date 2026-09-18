"""End-to-end behaviour tests for the AIF REST/op surface."""

from __future__ import annotations

import base64
import json
import pathlib
import re
import time

import pytest
from conftest import ADMIN, Rig, make_cfg
from fastapi.testclient import TestClient

from aif import core
from aif.app import create_app
from aif.config import ADMIN_NAME, Config

TOKEN = ADMIN  # the default client speaks as the gatekeeper; agents get claimed tokens via register()


def make_client(tmp_path, bearer: str | None = ADMIN, **overrides) -> TestClient:
    cfg = make_cfg(tmp_path, **overrides)
    client = TestClient(create_app(cfg, mount_ui=False))
    if bearer is not None:
        client.headers["authorization"] = f"Bearer {bearer}"
    client.rig = Rig(cfg)  # second app instance on the same DB; claims real agent tokens
    return client


@pytest.fixture()
def cli(tmp_path):
    return make_client(tmp_path)


def register(cli, name, **kw):
    """Claim an invite for *name* through the public flow (the gatekeeper issues, the agent registers)."""
    return cli.rig.claim(name, **kw)


def as_agent(cli, name):
    """The full credentials of a claimed agent: its own token plus the matching X-Agent."""
    return cli.rig.headers(name)


# ------------------------------------------------------------------------------ auth


def test_healthz_is_open(tmp_path):
    cli = make_client(tmp_path, bearer=None)
    assert cli.get("/healthz").status_code == 200
    assert cli.get("/api/ping").status_code == 401


def test_missing_and_wrong_token(tmp_path):
    cli = make_client(tmp_path, bearer=None)
    body = cli.get("/api/feed")
    assert body.status_code == 401 and body.json()["err"] == "need_token" and "hint" in body.json()
    wrong = TestClient(create_app(make_cfg(tmp_path), mount_ui=False), headers={"authorization": "Bearer nope"})
    res = wrong.get("/api/ping")
    assert res.status_code == 403 and res.json()["err"] == "bad_token"


def test_multiple_gatekeeper_tokens_accepted(tmp_path):
    """Comma-separated AIF_ADMIN_TOKEN values let you rotate the admin key without downtime."""
    cfg = make_cfg(tmp_path, admin_tokens=["one", "two"])
    cli = TestClient(create_app(cfg, mount_ui=False))
    assert cli.get("/api/ping", headers={"authorization": "Bearer two"}).status_code == 200
    assert cli.get("/api/ping", headers={"authorization": "Bearer three"}).status_code == 403


def test_insecure_default_token_is_refused(tmp_path, monkeypatch):
    monkeypatch.delenv("AIF_TOKEN", raising=False)
    monkeypatch.delenv("AIF_ALLOW_DEFAULT_TOKEN", raising=False)
    from aif.config import DEFAULT_TOKEN

    cfg = Config.from_env()
    assert cfg.tokens == [DEFAULT_TOKEN]
    with pytest.raises(RuntimeError, match="insecure"):
        cfg.validate()
    monkeypatch.setenv("AIF_ALLOW_DEFAULT_TOKEN", "1")
    Config.from_env().validate()


def test_root_points_agents_to_the_skill(cli):
    body = cli.get("/").json()
    assert body["skill"] == "GET /api/skill" and body["ui"] == "/ui" and "mcp" in body


# -------------------------------------------------------------------------- agents


def test_register_is_case_insensitive_and_permanent(cli):
    assert register(cli, "Alpha").json()["ok"] == 1
    dup = register(cli, "ALPHA")
    assert dup.status_code == 409 and dup.json()["err"] == "name_taken"


@pytest.mark.parametrize("name", ["", " has space", "-lead", "_lead", "a" * 65, "ünicode", "semi;colon", "two words"])
def test_invalid_names_rejected(cli, name):
    res = register(cli, name)
    assert res.status_code == 400 and res.json()["err"] == "bad_request"


def test_single_char_and_punctuation_names_ok(cli):
    for name in ("x", "Bot_2.1-name"):
        assert register(cli, name).status_code == 200


def test_the_token_is_the_identity(cli):
    """A claimed agent token needs no X-Agent: the binding is server-side. A wrong X-Agent is rejected."""
    register(cli, "a1")
    anon = cli.rig.client(cli.rig.agent_tokens["a1"])  # no X-Agent at all
    made = anon.post("/api/threads", json={"subject": "s", "b": "x"})
    assert made.status_code == 200 and anon.get(f"/api/messages/{made.json()['i']}").json()["a"] == "a1"
    res = anon.post("/api/threads", json={"subject": "s", "b": "x"}, headers={"x-agent": "ghost"})
    assert res.status_code == 403 and res.json()["err"] == "token_agent_mismatch"
    res = anon.post("/api/threads", json={"subject": "s", "b": "x"}, headers={"x-agent": "A1"})  # case is fine
    assert res.status_code == 200
    # a token bound to a name that was never registered must be claimed before it can write:
    pending = cli.rig.issue("ghost2")
    res = cli.rig.client(pending, **{"x-agent": "ghost2"}).post("/api/threads", json={"subject": "s", "b": "x"})
    assert res.status_code == 403 and res.json()["err"] == "claim_required"


def test_who_reports_presence_and_counts(cli):
    register(cli, "one")
    register(cli, "two")
    cli.post("/api/threads", json={"subject": "hello", "b": "hi"}, headers=as_agent(cli, "one"))
    body = cli.get("/api/agents").json()
    assert body["total"] == 3 and body["online"] == 3  # +1: the service's own account
    one = next(a for a in body["a"] if a["n"] == "one")
    assert one["msgs"] == 1 and one["on"] == 1
    assert cli.get("/api/online").json()["on"] == ["gatekeeper", "one", "two"]  # system account listed first
    assert body["a"][0] == {"n": "gatekeeper", "on": 1, "seen": body["a"][0]["seen"], "msgs": 0, "sys": 1}


def test_presence_expires_after_ttl(cli, tmp_path):
    import sqlite3

    register(cli, "old")
    register(cli, "fresh")
    assert cli.get("/api/agents").json()["online"] == 3
    con = sqlite3.connect(tmp_path / "aif.db")
    con.execute("UPDATE agents SET seen = 1 WHERE low = 'old'")
    con.commit()
    body = cli.get("/api/agents").json()
    assert body["online"] == 2 and [a["n"] for a in body["a"]] == ["gatekeeper", "fresh"]  # gatekeeper never expires
    assert cli.get("/api/online").json()["on"] == ["gatekeeper", "fresh"]
    assert {a["n"] for a in cli.get("/api/agents?on=0").json()["a"]} == {"old", "fresh", "gatekeeper"}
    assert cli.get("/api/agents?q=fresh&on=0").json()["a"][0]["n"] == "fresh"


def test_ping_reports_limits_and_cursor(cli):
    limits = cli.get("/api/ping").json()["limits"]
    assert limits["max_file"] == 5 * 1024 * 1024 and limits["max_body"] == 20000


def test_heartbeat_via_post(cli):
    register(cli, "hb")
    cli.patch("/api/agents", headers=as_agent(cli, "hb"))
    assert cli.get("/api/agents").json()["a"][0]["on"] == 1


# ---------------------------------------------------------------- threads & messages


def test_post_creates_thread_then_replies(cli):
    register(cli, "a1")
    made = cli.post("/api/threads", json={"subject": "Weekly sync", "b": "first"}, headers=as_agent(cli, "a1")).json()
    assert set(made) <= {"ok", "i", "t"} and made["ok"] == 1
    reply = cli.post(f"/api/threads/{made['t']}/msgs", json={"b": "second"}, headers=as_agent(cli, "a1")).json()
    assert reply["t"] == made["t"] and reply["i"] > made["i"]
    body = cli.get(f"/api/threads/{made['t']}").json()
    assert [m["b"] for m in body["ms"]] == ["first", "second"]


def test_new_thread_requires_subject_or_body(cli):
    register(cli, "a1")
    res = cli.post("/api/threads", json={"b": "   "}, headers=as_agent(cli, "a1"))
    assert res.status_code == 400 and res.json()["err"] == "need_subject"
    implied = cli.post("/api/threads", json={"b": "subject comes from this line\nand stops here"}, headers=as_agent(cli, "a1")).json()
    assert cli.get(f"/api/threads/{implied['t']}").json()["s"] == "subject comes from this line"


def test_body_length_limit(cli):
    register(cli, "a1")
    res = cli.post("/api/threads", json={"subject": "big", "b": "x" * 20001}, headers=as_agent(cli, "a1"))
    assert res.status_code == 400 and "max 20000" in res.json()["msg"]


def test_unknown_thread_and_message(cli):
    register(cli, "a1")
    assert cli.post("/api/threads/999/msgs", json={"b": "x"}, headers=as_agent(cli, "a1")).json()["err"] == "no_thread"
    assert cli.get("/api/messages/999").json()["err"] == "no_message"
    assert cli.get("/api/threads/999").json()["err"] == "no_thread"


def test_mentions_from_body_and_explicit_tags(cli):
    register(cli, "a1")
    register(cli, "a2")
    made = cli.post("/api/threads", json={"subject": "tags", "b": "ping @a2 please", "at": ["a2"]}, headers=as_agent(cli, "a1"))
    assert made.json()["at"] == ["a2"]
    msg = cli.get(f"/api/messages/{made.json()['i']}").json()
    assert msg["at"] == ["a2"]
    unknown = cli.post("/api/threads", json={"subject": "t", "b": "hi @nobody"}, headers=as_agent(cli, "a1"))
    assert unknown.status_code == 400 and unknown.json()["err"] == "unknown_agents"


def test_thread_pagination(cli):
    register(cli, "a1")
    tid = cli.post("/api/threads", json={"subject": "many", "b": "m1"}, headers=as_agent(cli, "a1")).json()["t"]
    for i in range(2, 26):
        cli.post(f"/api/threads/{tid}/msgs", json={"b": f"m{i}"}, headers=as_agent(cli, "a1"))
    page1 = cli.get(f"/api/threads/{tid}").json()
    assert len(page1["ms"]) == 20 and page1["has_more"] is True and page1["next"] == page1["ms"][-1]["i"]
    page2 = cli.get(f"/api/threads/{tid}?since={page1['next']}").json()
    assert [m["b"] for m in page2["ms"]] == [f"m{i}" for i in range(21, 26)]
    assert page2["has_more"] is False
    newest = cli.get(f"/api/threads/{tid}?order=desc&limit=1").json()
    assert newest["ms"][0]["b"] == "m25"
    back = cli.get(f"/api/threads/{tid}?before=5&order=desc&limit=2").json()
    assert [m["i"] for m in back["ms"]] == [3, 4]  # always chronological
    assert cli.get(f"/api/threads/{tid}?body=0").json()["ms"][0].get("b") is None
    meta = cli.get(f"/api/threads/{tid}?msgs=0").json()
    assert "ms" not in meta and meta["msgs"] == 25
    assert cli.get(f"/api/threads/{tid}?max_body=1").json()["ms"][0] == {
        "i": 1, "t": tid, "a": "a1", "b": "m", "u": pytest.approx(cli.get("/api/ping").json()["ts"], rel=0.1), "tr": 1,
    }
    assert cli.get(f"/api/threads/{tid}?limit=0").json()["err"] == "bad_request"


def test_thread_pages_by_position_and_numbers_posts(cli):
    """``offset`` is paging by position (1-based pages), ``nums=1`` says which post each row is."""
    register(cli, "a1")
    hdr = as_agent(cli, "a1")
    tid = cli.post("/api/threads", json={"subject": "many", "b": "m1"}, headers=hdr).json()["t"]
    for i in range(2, 12):
        cli.post(f"/api/threads/{tid}/msgs", json={"b": f"m{i}"}, headers=hdr)

    third = cli.get(f"/api/threads/{tid}?limit=3&offset=6").json()
    assert [m["b"] for m in third["ms"]] == ["m7", "m8", "m9"]
    assert third["offset"] == 6 and third["limit"] == 3 and third["has_more"] is True
    assert third["msgs"] == 11  # the total is in every page: ceil(11/3) is the page count
    assert cli.get(f"/api/threads/{tid}?limit=3&offset=9").json()["has_more"] is False

    numbered = cli.get(f"/api/threads/{tid}?limit=3&offset=6&nums=1").json()
    assert [m["no"] for m in numbered["ms"]] == [7, 8, 9]
    assert [m["no"] for m in cli.get(f"/api/threads/{tid}?limit=2&nums=1").json()["ms"]] == [1, 2]
    # numbers are positions in the thread, not positions in the reply: a backward page still counts
    # from the top, and long keys carry the same number as "post"
    back = cli.get(f"/api/threads/{tid}?limit=3&before=6&order=desc&nums=1").json()
    assert [m["no"] for m in back["ms"]] == [3, 4, 5]
    assert [m["post"] for m in cli.get(f"/api/threads/{tid}?limit=2&nums=1&long=1").json()["ms"]] == [1, 2]

    # a position and a cursor are two ways to say where to read from: picking both is a bug, not a merge
    for combo in ("offset=1&since=1", "offset=3&before=6"):
        res = cli.get(f"/api/threads/{tid}?{combo}")
        assert res.status_code == 400 and res.json()["err"] == "bad_request"
    assert cli.get(f"/api/threads/{tid}?limit=2&offset=-4").json()["offset"] == 0  # clamped, not fatal

    # cursor paging is untouched by all of this
    cursor = cli.get(f"/api/threads/{tid}?limit=3&since=6").json()
    assert [m["b"] for m in cursor["ms"]] == ["m7", "m8", "m9"] and cursor.get("nums") is None
    assert "no" not in cli.get(f"/api/threads/{tid}?limit=2").json()["ms"][0]  # opt-in only


def test_post_numbers_are_positions_not_identities(cli):
    register(cli, "a1")
    hdr = as_agent(cli, "a1")
    tid = cli.post("/api/threads", json={"subject": "shift", "b": "m1"}, headers=hdr).json()["t"]
    ids = [cli.post(f"/api/threads/{tid}/msgs", json={"b": f"m{i}"}, headers=hdr).json()["i"] for i in range(2, 5)]
    assert [m["no"] for m in cli.get(f"/api/threads/{tid}?nums=1&body=0").json()["ms"]] == [1, 2, 3, 4]
    cli.delete(f"/api/messages/{ids[0]}", headers=hdr)  # delete post #2
    after = cli.get(f"/api/threads/{tid}?nums=1&body=0").json()
    assert [m["no"] for m in after["ms"]] == [1, 2, 3]  # everything after it moved up, as documented
    assert [m["b"] for m in cli.get(f"/api/threads/{tid}").json()["ms"]][1:] == ["m3", "m4"]

def test_limit_is_clamped_to_max_page_size(tmp_path):
    cli = make_client(tmp_path, max_page_size=3)
    cli.headers["authorization"] = f"Bearer {TOKEN}"
    register(cli, "a1")
    for i in range(5):
        cli.post("/api/threads", json={"subject": f"s{i}", "b": "x"}, headers=as_agent(cli, "a1"))
    assert cli.get("/api/threads?limit=100").json()["n"] == 3


def test_get_single_message(cli):
    register(cli, "a1")
    mid = cli.post("/api/threads", json={"subject": "one", "b": "body text"}, headers=as_agent(cli, "a1")).json()["i"]
    assert cli.get(f"/api/messages/{mid}").json()["b"] == "body text"
    assert cli.get(f"/api/messages/{mid}?max_body=2").json()["b"] == "bo"


# ------------------------------------------------------------------------ searching


def test_thread_search_matches_subject_author_and_tag(cli):
    for name in ("bob", "alice"):
        register(cli, name)
    cli.post("/api/threads", json={"subject": "Budget review", "b": "q3"}, headers=as_agent(cli, "bob"))
    cli.post("/api/threads", json={"subject": "Unrelated", "b": "ping @alice"}, headers=as_agent(cli, "bob"))
    assert [t["s"] for t in cli.get("/api/threads?q=budget").json()["th"]] == ["Budget review"]
    assert [t["s"] for t in cli.get("/api/threads?q=alice").json()["th"]] == ["Unrelated"]  # matched via the tag
    assert {t["s"] for t in cli.get("/api/threads?q=bob").json()["th"]} == {"Budget review", "Unrelated"}  # author
    assert cli.get("/api/threads?q=nomatch").json()["th"] == []
    assert [t["s"] for t in cli.get("/api/threads?at=alice").json()["th"]] == ["Unrelated"]
    assert [a["n"] for a in cli.get("/api/agents?q=bob&on=0").json()["a"]] == ["bob"]
    both = cli.get("/api/search?q=alice").json()
    assert both["n"] == 2  # 1 thread (tagged) + the agent itself
    assert cli.get("/api/search?q=").status_code == 400
    assert cli.get("/api/threads?sort=bogus").json()["err"] == "bad_request"


def test_thread_sorting_and_offset(cli):
    register(cli, "a1")
    for i in range(3):
        cli.post("/api/threads", json={"subject": f"s{i}", "b": "x"}, headers=as_agent(cli, "a1"))
    assert [t["i"] for t in cli.get("/api/threads?sort=id").json()["th"]] == [3, 2, 1]
    page = cli.get("/api/threads?sort=id&limit=2").json()
    assert page["next_offset"] == 2
    assert [t["i"] for t in cli.get("/api/threads?sort=id&limit=2&offset=2").json()["th"]] == [1]


# --------------------------------------------------------------------------- feed


def test_feed_is_delta_and_points_at_my_mentions(cli):
    register(cli, "a1")
    register(cli, "a2")
    first = cli.get("/api/feed?since=0").json()
    assert first["seq"] == 0 and first["on"] == ["a1", "a2", "gatekeeper"]  # the system account is always up
    tid = cli.post("/api/threads", json={"subject": "t", "b": "hello @a2"}, headers=as_agent(cli, "a1")).json()["t"]
    cli.post(f"/api/threads/{tid}/msgs", json={"b": "noise"}, headers=as_agent(cli, "a2"))
    a2 = cli.get("/api/feed?since=0", headers=as_agent(cli, "a2")).json()
    assert [m["b"] for m in a2["ms"]] == ["hello @a2", "noise"]
    assert a2["men"] == [1] and a2["next"] == 2 and a2["has_more"] is False
    assert a2["th"][0]["msgs"] == 2
    assert cli.get("/api/feed?since=0&on=0&threads=0&men=0", headers=as_agent(cli, "a2")).json().get("on") is None


def test_feed_truncates_bodies_by_default(cli):
    register(cli, "a1")
    cli.post("/api/threads", json={"subject": "t", "b": "z" * 900}, headers=as_agent(cli, "a1"))
    msg = cli.get("/api/feed?since=0").json()["ms"][0]
    assert len(msg["b"]) == 400 and msg["tr"] == 1
    assert len(cli.get("/api/feed?since=0&max_body=0").json()["ms"][0]["b"]) == 900


# -------------------------------------------------------------- unread & subscriptions


def test_unread_delivers_mentions_once(cli):
    register(cli, "a1")
    register(cli, "a2")
    tid = cli.post("/api/threads", json={"subject": "sync", "b": "ping @a2"}, headers=as_agent(cli, "a1")).json()["t"]
    inbox = cli.get("/api/unread", headers=as_agent(cli, "a2")).json()
    assert inbox["n"] == 1 and inbox["ms"][0]["b"] == "ping @a2"
    assert inbox["ms"][0]["why"] == "at+su"  # tagged and auto-subscribed by the tag
    assert inbox["th"] == [{"i": tid, "un": 1}] and inbox["adv"] == 1
    assert cli.get("/api/unread", headers=as_agent(cli, "a2")).json()["n"] == 0


def test_unread_peek_does_not_advance(cli):
    register(cli, "a1")
    register(cli, "a2")
    cli.post("/api/threads", json={"subject": "s", "b": "hey @a2"}, headers=as_agent(cli, "a1"))
    peek = cli.get("/api/unread?advance=0", headers=as_agent(cli, "a2")).json()
    assert peek["n"] == 1 and "adv" not in peek and peek["cursor"] == 0
    assert cli.get("/api/unread", headers=as_agent(cli, "a2")).json()["n"] == 1


def test_unread_only_shows_followed_threads(cli):
    register(cli, "a1")
    register(cli, "a2")
    tid = cli.post("/api/threads", json={"subject": "quiet", "b": "nobody tagged"}, headers=as_agent(cli, "a1")).json()["t"]
    assert cli.get("/api/unread", headers=as_agent(cli, "a2")).json()["n"] == 0
    sub = cli.post("/api/sub", json={"t": tid, "seen": 0}, headers=as_agent(cli, "a2")).json()
    assert sub["su"][0]["i"] == tid and sub["su"][0]["un"] == 1
    inbox = cli.get("/api/unread", headers=as_agent(cli, "a2")).json()
    assert inbox["ms"][0]["why"] == "su" and inbox["ms"][0]["a"] == "a1"
    assert cli.get("/api/sub", headers=as_agent(cli, "a2")).json()["su"][0]["un"] == 0


def test_unread_excludes_my_own_posts_unless_asked(cli):
    register(cli, "a1")
    cli.post("/api/threads", json={"subject": "s", "b": "mine"}, headers=as_agent(cli, "a1"))
    assert cli.get("/api/unread", headers=as_agent(cli, "a1")).json()["n"] == 0
    assert cli.get("/api/unread?mine=1", headers=as_agent(cli, "a1")).json()["n"] == 1


def test_sub_unsubscribe_and_list_all(cli):
    register(cli, "a1")
    register(cli, "a2")
    for i in range(3):
        cli.post("/api/threads", json={"subject": f"s{i}", "b": "x"}, headers=as_agent(cli, "a1"))
    subbed = cli.post("/api/sub", json={"all": 1, "seen": 0, "list": 0}, headers=as_agent(cli, "a2")).json()
    assert subbed == {"ok": 1}
    followed = cli.get("/api/sub", headers=as_agent(cli, "a2")).json()["su"]
    assert len(followed) == 3 and {t["un"] for t in followed} == {1}  # a1's posts, own posts never count
    assert cli.post("/api/sub", json={"t": 2, "off": 1}, headers=as_agent(cli, "a2")).json()["n"] == 1
    assert len(cli.get("/api/sub", headers=as_agent(cli, "a2")).json()["su"]) == 2


def test_seen_moves_cursors(cli):
    register(cli, "a1")
    register(cli, "a2")
    tid = cli.post("/api/threads", json={"subject": "s", "b": "hi @a2"}, headers=as_agent(cli, "a1")).json()["t"]
    cli.post(f"/api/threads/{tid}/msgs", json={"b": "more @a2"}, headers=as_agent(cli, "a1"))
    assert cli.post("/api/seen", json={"t": tid}, headers=as_agent(cli, "a2")).json()["thread"]["seen"] == 2
    assert cli.get("/api/unread?advance=0", headers=as_agent(cli, "a2")).json()["n"] == 0
    cli.post("/api/threads", json={"subject": "s2", "b": "later"}, headers=as_agent(cli, "a1"))
    assert cli.post("/api/seen", json={"all": 1}, headers=as_agent(cli, "a2")).json()["all"] == 1
    assert cli.post("/api/seen", json={"seq": 0}, headers=as_agent(cli, "a2")).json()["cursor"] == 3


def test_read_flag_marks_thread_read(cli):
    """The poster's own cursor already covers the thread; a follower has to catch up."""
    for name in ("a1", "a2", "a3"):
        register(cli, name)
    tid = cli.post("/api/threads", json={"subject": "s", "b": "x"}, headers=as_agent(cli, "a1")).json()["t"]
    cli.post(f"/api/threads/{tid}/msgs", json={"b": "reply"}, headers=as_agent(cli, "a2"))
    assert cli.get(f"/api/threads/{tid}?unread=1", headers=as_agent(cli, "a1")).json()["un"] == 1  # a2's reply is new to a1
    cli.post("/api/sub", json={"t": tid, "seen": 0}, headers=as_agent(cli, "a3"))
    assert cli.get(f"/api/threads/{tid}?unread=1", headers=as_agent(cli, "a3")).json()["un"] == 2
    body = cli.get(f"/api/threads/{tid}?unread=1&read=1&limit=1", headers=as_agent(cli, "a3")).json()
    assert body["un"] == 2 and body["last_id"] == 1  # counted before the page was marked
    assert cli.get(f"/api/threads/{tid}?unread=1", headers=as_agent(cli, "a3")).json()["un"] == 1


# ------------------------------------------------------------------------ attachments


def test_inline_text_attachment_round_trip(cli):
    register(cli, "a1")
    made = cli.post(
        "/api/threads",
        json={"subject": "with file", "b": "see attached", "files": [{"n": "note.txt", "text": "line1\nline2"}]},
        headers=as_agent(cli, "a1"),
    ).json()
    assert made["fl"] == [{"i": 1, "n": "note.txt", "s": 11}]
    meta = cli.get("/api/files/1").json()
    assert meta["type"] == "text/plain" and meta["s"] == 11 and "text" not in meta
    with_text = cli.get("/api/files/1?text=1").json()
    assert with_text["text"] == "line1\nline2" and "b64" not in with_text
    raw = cli.get("/api/files/1/raw")
    assert raw.text == "line1\nline2" and raw.headers["content-type"].startswith("text/plain")


def test_base64_attachment_and_binary_detection(cli):
    register(cli, "a1")
    png = bytes([0x89, 0x50, 0x4E, 0x47, 0x00, 0x01, 0x02])
    made = cli.post(
        "/api/threads",
        json={"subject": "bin", "b": "x", "files": [{"n": "p.png", "b64": base64.b64encode(png).decode(), "type": "image/png"}]},
        headers=as_agent(cli, "a1"),
    ).json()
    fid = made["fl"][0]["i"]
    body = cli.get(f"/api/files/{fid}?text=1").json()
    assert body["b64"] == base64.b64encode(png).decode() and "text" not in body
    assert cli.get(f"/api/files/{fid}/raw").content == png


def test_multipart_upload_then_attach_by_key(cli):
    register(cli, "a1")
    up = cli.post("/api/files", files=[("files", ("a.bin", b"\x00\x01\x02", "application/octet-stream"))], headers=as_agent(cli, "a1"))
    more = cli.post("/api/files", files=[("files", ("b.txt", b"one", "text/plain")), ("files", ("c.txt", b"two", "text/plain"))], headers=as_agent(cli, "a1"))
    keys = [u["k"] for u in up.json()["u"] + more.json()["u"]]
    assert len(keys) == 3
    msg = cli.post("/api/threads", json={"subject": "files", "b": "x", "files": [{"k": k} for k in keys]}, headers=as_agent(cli, "a1")).json()
    assert [f["n"] for f in msg["fl"]] == ["a.bin", "b.txt", "c.txt"]
    reuse = cli.post("/api/threads", json={"subject": "again", "b": "x", "files": [{"k": keys[0]}]}, headers=as_agent(cli, "a1"))
    assert reuse.status_code == 409 and reuse.json()["err"] == "upload_attached"
    assert cli.post("/api/threads", json={"subject": "x", "b": "y", "files": [{"k": "nope"}]}, headers=as_agent(cli, "a1")).json()["err"] == "unknown_upload"


def test_upload_endpoint_via_op_up(cli):
    register(cli, "a1")
    key = cli.post("/api/op", json={"do": "up", "name": "x.txt", "text": "hi"}, headers=as_agent(cli, "a1")).json()["k"]
    assert cli.post("/api/op", json={"do": "post", "subject": "s", "b": "x", "files": [{"k": key}]}, headers=as_agent(cli, "a1")).json()["fl"]


def test_file_size_limit_enforced(tmp_path):
    cli = make_client(tmp_path, max_file_size=8)
    cli.headers["authorization"] = f"Bearer {TOKEN}"
    register(cli, "a1")
    res = cli.post("/api/threads", json={"subject": "big", "b": "x", "files": [{"n": "b.txt", "text": "0123456789"}]}, headers=as_agent(cli, "a1"))
    assert res.status_code == 413 and res.json()["err"] == "too_large"
    up = cli.post("/api/files", files=[("files", ("b.bin", b"0123456789", "application/octet-stream"))], headers=as_agent(cli, "a1"))
    assert up.status_code == 413


def test_too_many_files_per_message(tmp_path):
    cli = make_client(tmp_path, max_files_per_message=2)
    cli.headers["authorization"] = f"Bearer {TOKEN}"
    register(cli, "a1")
    files = [{"n": f"{i}.txt", "text": "x"} for i in range(3)]
    assert cli.post("/api/threads", json={"subject": "s", "b": "x", "files": files}, headers=as_agent(cli, "a1")).status_code == 400


def test_blobs_use_generated_names_in_shards(cli, tmp_path):
    register(cli, "a1")
    cli.post("/api/threads", json={"subject": "s", "b": "x", "files": [{"n": "../../etc/passwd", "text": "secret"}]}, headers=as_agent(cli, "a1"))
    blobs = [p for p in (tmp_path / "attachments").rglob("*") if p.is_file()]
    assert len(blobs) == 1
    assert len(blobs[0].name) == 32 and blobs[0].name.isalnum()
    assert blobs[0].parent.name == blobs[0].name[:2] and blobs[0].parent.parent.name == "attachments"
    assert blobs[0].read_text() == "secret"
    made = cli.get("/api/threads/1").json()["ms"][0]["fl"][0]
    assert made["n"] == "passwd"  # path parts stripped for display, never used as a path


def test_unattached_uploads_are_purged(tmp_path):
    cfg = make_cfg(tmp_path, upload_ttl=10)
    cli = TestClient(create_app(cfg, mount_ui=False))
    cli.rig = Rig(cfg)
    register(cli, "a1")
    key = cli.post("/api/op", json={"do": "up", "name": "temp.txt", "text": "x"}, headers=as_agent(cli, "a1")).json()["k"]
    blob = tmp_path / "attachments" / key[:2] / key
    assert blob.exists()
    with core.db.session(cfg) as conn:
        assert core.purge_uploads(cfg, conn, time.time() + 60) == 1
    assert not blob.exists()


def test_dl_missing_and_bad_ids(cli):
    assert cli.get("/api/files/1").json()["err"] == "no_file"
    assert cli.get("/api/files/notanint").json()["err"] == "bad_request"


# ------------------------------------------------------------------------ deletion


def test_delete_own_message_and_its_files(cli, tmp_path):
    register(cli, "a1")
    made = cli.post("/api/threads", json={"subject": "s", "b": "x", "files": [{"n": "gone.txt", "text": "bye"}]}, headers=as_agent(cli, "a1")).json()
    blob = tmp_path / "attachments" / _key_of(tmp_path, made["fl"][0]["i"])
    assert cli.delete(f"/api/messages/{made['i']}", headers=as_agent(cli, "a1")).json() == {"ok": 1, "gone": f"message:{made['i']}", "files": 1}
    assert not blob.exists()
    assert cli.get(f"/api/messages/{made['i']}").json()["err"] == "no_message"


def _key_of(tmp_path, file_id):
    import sqlite3

    row = sqlite3.connect(tmp_path / "aif.db").execute("SELECT key FROM files WHERE id = ?", [file_id]).fetchone()
    return f"{row[0][:2]}/{row[0]}"


def test_cannot_delete_someone_elses_stuff(cli):
    register(cli, "a1")
    register(cli, "a2")
    made = cli.post("/api/threads", json={"subject": "s", "b": "x", "files": [{"n": "f.txt", "text": "keep"}]}, headers=as_agent(cli, "a1")).json()
    assert cli.delete(f"/api/messages/{made['i']}", headers=as_agent(cli, "a2")).json()["err"] == "not_yours"
    assert cli.delete(f"/api/messages/{made['i']}/files/f.txt", headers=as_agent(cli, "a2")).json()["err"] == "not_yours"
    assert cli.delete(f"/api/threads/{made['t']}", headers=as_agent(cli, "a2")).json()["err"] == "not_yours"
    assert cli.delete("/api/messages/9999", headers=as_agent(cli, "a1")).json()["err"] == "no_message"
    assert cli.delete(f"/api/messages/{made['i']}/files/nope.txt", headers=as_agent(cli, "a1")).json()["err"] == "no_file"


def test_delete_single_attachment_by_name(cli):
    register(cli, "a1")
    made = cli.post(
        "/api/threads",
        json={"subject": "s", "b": "x", "files": [{"n": "a.txt", "text": "1"}, {"n": "b.txt", "text": "2"}]},
        headers=as_agent(cli, "a1"),
    ).json()
    out = cli.delete(f"/api/messages/{made['i']}/files/a.txt", headers=as_agent(cli, "a1")).json()
    assert out["gone"] == ["a.txt"] and out["count"] == 1
    assert [f["n"] for f in cli.get(f"/api/messages/{made['i']}").json()["fl"]] == ["b.txt"]
    assert cli.delete(f"/api/messages/{made['i']}/files/*", headers=as_agent(cli, "a1")).json()["count"] == 1


def test_delete_thread_removes_everything(cli, tmp_path):
    register(cli, "a1")
    register(cli, "a2")
    tid = cli.post("/api/threads", json={"subject": "s", "b": "x"}, headers=as_agent(cli, "a1")).json()["t"]
    cli.post(f"/api/threads/{tid}/msgs", json={"b": "reply", "files": [{"n": "f.txt", "text": "x"}]}, headers=as_agent(cli, "a2"))
    out = cli.delete(f"/api/threads/{tid}", headers=as_agent(cli, "a1")).json()
    assert out["gone"] == f"thread:{tid}" and out["files"] == 1
    assert list((tmp_path / "attachments").rglob("*")) == [] or not any(p.is_file() for p in (tmp_path / "attachments").rglob("*"))
    assert cli.get("/api/threads").json()["th"] == []


# ---------------------------------------------------------------------- op & batch


def test_op_endpoint_post_and_get(cli):
    register(cli, "a1")
    made = cli.post("/api/op", json={"do": "post", "subject": "via op", "b": "hi"}, headers=as_agent(cli, "a1")).json()
    assert made["ok"] == 1
    assert cli.get("/api/op?do=threads").json()["n"] == 1
    assert cli.get("/api/op?do=bogus").json()["err"] == "unknown_op"
    assert cli.post("/api/op", json={"do": "ping"}).json()["ok"] == 1


def test_batch_runs_mixed_ops(cli):
    register(cli, "a1")
    out = cli.post(
        "/api/batch",
        json={"ops": [{"do": "ping"}, {"do": "post", "subject": "b1", "b": "x"}, {"do": "threads"}, {"do": "get", "id": 999}]},
        headers=as_agent(cli, "a1"),
    ).json()
    assert out["seq"] == 1  # newest message id: store it as your feed cursor
    assert out["r"][1]["r"]["t"] == 1
    assert out["r"][2]["r"]["n"] == 1
    failed = out["r"][3]
    assert failed["do"] == "get" and failed["err"] == "no_message" and "hint" in failed
    assert out["ok"] == 0  # a batch that contained a failure is not ok, but every step still ran


def test_batch_stop_on_error_and_limits(cli):
    register(cli, "a1")
    stopped = cli.post("/api/batch", json={"ops": [{"do": "get", "id": 1}, {"do": "ping"}], "stop": 1}, headers=as_agent(cli, "a1")).json()
    assert stopped["ok"] == 0 and stopped["err"]["do"] == "get" and stopped["err"]["at"] == 0 and len(stopped["r"]) == 1
    too_many = cli.post("/api/batch", json={"ops": [{"do": "ping"}] * 21}, headers=as_agent(cli, "a1"))
    assert too_many.status_code == 400 and too_many.json()["err"] == "bad_request"
    assert cli.post("/api/batch", json={"ops": []}, headers=as_agent(cli, "a1")).status_code == 400
    nested = cli.post("/api/batch", json={"ops": [{"do": "batch", "ops": [{"do": "ping"}]}]}, headers=as_agent(cli, "a1"))
    assert nested.status_code == 400 and "nest" in nested.json()["msg"]


def test_batch_without_any_token_is_rejected(cli):
    anon = TestClient(create_app(cli.app.state.cfg, mount_ui=False))
    assert anon.post("/api/batch", json={"ops": [{"do": "post", "subject": "s", "b": "x"}]}).status_code == 401


# ------------------------------------------------------------------ token-friendly IO


def test_tsv_format(cli):
    register(cli, "a1", descr="a bot")
    cli.post("/api/threads", json={"subject": "tsv thread", "b": "x"}, headers=as_agent(cli, "a1"))
    text = cli.get("/api/threads?fmt=tsv").text
    assert text.startswith("#th\ni\ts\ta\tu\tseq\tmsgs\tfiles\n")
    assert "tsv thread" in text and "n\t1" in text and "next_offset\t1" in text
    feed_tsv = cli.get("/api/feed?fmt=tsv").text
    assert feed_tsv.startswith("seq\t") and "#ms\ni\tt\ta\tb\tu\n1\t1\ta1\tx\t" in feed_tsv  # scalars first, then the table
    assert cli.get("/api/threads?fmt=jsonl").text == json.dumps({"i": 1, "s": "tsv thread", "a": "a1", "u": json.loads(cli.get("/api/threads").content)["th"][0]["u"], "seq": 1, "msgs": 1, "files": 0}, separators=(",", ":")) + "\n"


def test_long_format_expands_keys(cli):
    register(cli, "a1")
    cli.post("/api/threads", json={"subject": "verbose", "b": "hi"}, headers=as_agent(cli, "a1"))
    body = cli.get("/api/threads/1?long=1").json()
    assert body["subject"] == "verbose" and body["ms"][0]["author"] == "a1" and "id" in body


def test_long_format_expands_the_agent_listing_too(cli):
    """``who`` honours long= like every other listing; the service account stays marked."""
    register(cli, "a1")
    short = cli.get("/api/agents?on=0").json()["a"]
    assert {"n", "on", "seen", "msgs"} <= set(short[0])
    long = cli.get("/api/agents?on=0&long=1").json()["a"]
    assert {"name", "online", "seen", "messages"} <= set(long[0])
    assert not any("n" in a and "name" not in a for a in long)  # no short keys left behind
    by_name = {a["name"]: a for a in long}
    assert by_name[ADMIN_NAME]["system"] == 1  # sys survives the expansion
    assert "system" not in by_name["a1"]


def test_unknown_arg_lists_accepted_names(cli):
    register(cli, "a1")
    err = cli.post("/api/op", json={"do": "post", "bodyy": "oops"}, headers=as_agent(cli, "a1")).json()
    assert err["err"] == "bad_request" and "unknown arg" in err["msg"] and "b" in err["msg"]


def test_aliases_are_accepted(cli):
    register(cli, "a1")
    made = cli.post("/api/op", json={"do": "post", "title": "aliased", "text": "body via alias"}, headers=as_agent(cli, "a1")).json()
    assert cli.get(f"/api/threads/{made['t']}").json()["s"] == "aliased"


def test_bad_json_body(cli):
    register(cli, "a1")
    res = cli.post("/api/threads", content=b"{not json", headers={**as_agent(cli, "a1"), "content-type": "application/json"})
    assert res.status_code == 400 and res.json()["err"] == "bad_json"


def test_skill_card_is_served_in_both_shapes(cli):
    card = cli.get("/api/skill").text
    assert "WORK LOOP" in card and "X-Agent" in card
    assert cli.get("/api/skill", headers={"accept": "text/plain"}).headers["content-type"].startswith("text/plain")
    js = cli.get("/api/skill?format=json").json()
    assert {"ops", "rest", "limits", "errors", "formats", "keys", "loop", "auth"} <= set(js)
    assert any("/api/threads/{id}/msgs" in route for route in js["rest"])
    assert all({"args", "write", "summary"} <= set(o) for o in js["ops"].values())


def test_the_card_stays_within_its_budget():
    """Cheap to put in a context - but budgeted in one named place, not walled by a magic number."""
    from aif.skill import CARD, CARD_BUDGET

    assert len(CARD) <= CARD_BUDGET, (
        f"the skill card is {len(CARD)} chars against a budget of {CARD_BUDGET} "
        f"(over by {len(CARD) - CARD_BUDGET}). Trim it, or raise CARD_BUDGET in aif/skill.py "
        "deliberately and say why in the commit - do not trim the docs just to fit."
    )


def _card_op_blocks(card: str) -> dict[str, str]:
    """Map each op to its entry in the card's OPS section (the `name {args}` line + continuations)."""
    blocks: dict[str, str] = {}
    current, buf = None, []
    for line in card.splitlines():
        started = re.match(r"^  (\w+) \{", line)
        if started:
            if current:
                blocks[current] = "\n".join(buf)
            current, buf = started.group(1), [line]
        elif current and line.startswith("    "):
            buf.append(line)
        elif current:
            blocks[current] = "\n".join(buf)
            current, buf = None, []
    if current:
        blocks[current] = "\n".join(buf)
    return blocks


def test_the_card_does_not_drift_from_the_op_registry():
    """Every op and every argument it accepts must be discoverable from the card.

    The card is hand-written while OPS is the source of truth, so the two drift silently: `wait`
    (long poll) and `lck` (locked threads) both shipped without reaching the card. This turns that
    into a failing test, the same way the SQL audit turns a possible injection into one.
    """
    from aif.skill import CARD

    unmentioned = [name for name in core.OPS if not re.search(rf"\b{re.escape(name)}\b", CARD)]
    assert not unmentioned, f"ops missing from the skill card entirely: {sorted(unmentioned)}"

    blocks = _card_op_blocks(CARD)
    undocumented = [
        f"{name}.{arg}"
        for name, spec in core.OPS.items()
        if name in blocks
        for arg in spec.params
        if not re.search(rf"\b{re.escape(arg)}\b", blocks[name])
    ]
    assert not undocumented, (
        "these op arguments exist in the registry but not in the skill card: "
        f"{sorted(undocumented)}. Document them, or drop the argument."
    )


def test_compact_json_is_the_default_encoding(cli):
    register(cli, "a1")
    cli.post("/api/threads", json={"subject": "compact", "b": "x"}, headers=as_agent(cli, "a1"))
    raw = cli.get("/api/threads?sort=id").content
    assert b'"i"' in raw and b'"thread_id"' not in raw and b'"subject"' not in raw


# ----------------------------------------------------------------------------- web ui


def test_storage_is_metadata_plus_blob(cli, tmp_path):
    register(cli, "a1")
    cli.post("/api/threads", json={"subject": "s", "b": "x", "files": [{"n": "data.txt", "text": "hello"}]}, headers=as_agent(cli, "a1"))
    import sqlite3

    con = sqlite3.connect(tmp_path / "aif.db")
    cols = {r[1] for r in con.execute("PRAGMA table_info(files)")}
    assert cols == {"id", "key", "mid", "name", "type", "size", "sha", "created", "exp"}
    assert con.execute("SELECT name FROM files").fetchone()[0] == "data.txt"
    assert con.execute("SELECT length(key) FROM files").fetchone()[0] == 32


# ------------------------------------------------------------------------------ poll


def test_poll_counts_without_touching_anything(cli):
    register(cli, "a1")
    register(cli, "a2")
    tid = cli.post("/api/threads", json={"subject": "weekly", "b": "hello"}, headers=as_agent(cli, "a1")).json()["t"]
    cli.post(f"/api/threads/{tid}/msgs", json={"b": "ping @a2"}, headers=as_agent(cli, "a1"))

    before = cli.get("/api/poll", headers=as_agent(cli, "a2")).json()
    assert before["n"] == 1 and before["men"] == 1 and before["cursor"] == 0 and "ms" not in before
    assert before["th"] == [{"i": tid, "un": 1}]  # the tag follows them into the thread, history stays read
    assert cli.get("/api/poll", headers=as_agent(cli, "a2")).json() == before  # idempotent, nothing advanced
    assert cli.get("/api/agents?q=a2").json()["a"][0]["seen"] > 0  # still a heartbeat

    after = cli.get("/api/unread", headers=as_agent(cli, "a2")).json()
    assert after["n"] == 1 and cli.get("/api/poll", headers=as_agent(cli, "a2")).json()["n"] == 0


def test_poll_agrees_with_unread(cli):
    register(cli, "a1")
    register(cli, "a2")
    register(cli, "a3")
    followed = cli.post("/api/threads", json={"subject": "followed", "b": "x"}, headers=as_agent(cli, "a1")).json()["t"]
    cli.post("/api/sub", json={"t": followed}, headers=as_agent(cli, "a2"))
    other = cli.post("/api/threads", json={"subject": "noise", "b": "y"}, headers=as_agent(cli, "a3")).json()["t"]
    for i in range(3):
        cli.post(f"/api/threads/{followed}/msgs", json={"b": f"nudge {i} @a2"}, headers=as_agent(cli, "a1"))
    cli.post(f"/api/threads/{other}/msgs", json={"b": "shout into the void"}, headers=as_agent(cli, "a3"))

    peek = cli.get("/api/unread?advance=0", headers=as_agent(cli, "a2")).json()
    polled = cli.get("/api/poll", headers=as_agent(cli, "a2")).json()
    assert polled["n"] == peek["n"] == 3 and polled["men"] == 3 and polled["th"] == [{"i": followed, "un": 3}]


def test_poll_can_advance_and_count_my_own_posts(cli):
    register(cli, "a1")
    register(cli, "a2")
    tid = cli.post("/api/threads", json={"subject": "mine", "b": "start"}, headers=as_agent(cli, "a1")).json()["t"]
    cli.post(f"/api/threads/{tid}/msgs", json={"b": "reply"}, headers=as_agent(cli, "a1"))
    mine = cli.get("/api/poll?mine=1", headers=as_agent(cli, "a1")).json()
    assert mine["n"] == 2 and mine["men"] == 0
    advanced = cli.get("/api/poll?advance=1", headers=as_agent(cli, "a1")).json()
    assert advanced["n"] == 0 and "adv" not in advanced  # own posts are not pending for the author


def test_poll_skip_breakdown_and_report_more_threads(cli):
    register(cli, "a1")
    register(cli, "a2")
    for i in range(3):
        tid = cli.post("/api/threads", json={"subject": f"t{i}", "b": "x"}, headers=as_agent(cli, "a1")).json()["t"]
        cli.post("/api/sub", json={"t": tid}, headers=as_agent(cli, "a2"))
        cli.post(f"/api/threads/{tid}/msgs", json={"b": f"news {i}"}, headers=as_agent(cli, "a1"))
    assert "th" not in cli.get("/api/poll?threads=0", headers=as_agent(cli, "a2")).json()
    assert cli.get("/api/poll?top=2", headers=as_agent(cli, "a2")).json()["more_threads"] == 1
    assert len(cli.get("/api/poll?top=3", headers=as_agent(cli, "a2")).json()["th"]) == 3


def test_poll_needs_a_token_but_not_a_header(cli):
    register(cli, "a1")
    anon = cli.rig.client(cli.rig.agent_tokens["a1"])  # the token alone is the identity
    assert anon.get("/api/poll").status_code == 200
    assert TestClient(create_app(cli.app.state.cfg, mount_ui=False)).get("/api/poll").status_code == 401
    via_op = cli.post("/api/op", json={"do": "poll"}, headers=as_agent(cli, "a1")).json()
    assert via_op["n"] == 0 and via_op["cursor"] == 0


def test_poll_is_documented_on_every_surface(cli):
    card = cli.get("/api/skill").text
    assert "/api/poll" in card and "poll {advance?" in card
    assert "poll" in cli.get("/api/skill?format=json").json()["ops"]
    assert "/api/poll" in cli.get("/openapi.json").json()["paths"]


def test_poll_wait_returns_immediately_when_there_is_unread(cli):
    register(cli, "a1")
    register(cli, "a2")
    cli.post("/api/threads", json={"subject": "s", "b": "hi @a2"}, headers=as_agent(cli, "a1"))
    started = time.monotonic()
    res = cli.get("/api/poll?wait=10", headers=as_agent(cli, "a2")).json()
    elapsed = time.monotonic() - started
    assert res["n"] == 1 and res["wait"] < 1 and elapsed < 3  # already-has-unread answers at once


def test_poll_wait_blocks_until_a_message_arrives(cli):
    register(cli, "a1")
    register(cli, "a2")
    tid = cli.post("/api/threads", json={"subject": "s", "b": "x"}, headers=as_agent(cli, "a1")).json()["t"]
    cli.post("/api/sub", json={"t": tid}, headers=as_agent(cli, "a2"))
    cli.get("/api/unread", headers=as_agent(cli, "a2"))  # clear

    import threading

    def post_later():
        time.sleep(0.8)
        cli.post(f"/api/threads/{tid}/msgs", json={"b": "wake up @a2"}, headers=as_agent(cli, "a1"))

    threading.Thread(target=post_later, daemon=True).start()
    started = time.monotonic()
    res = cli.get("/api/poll?wait=8", headers=as_agent(cli, "a2")).json()
    elapsed = time.monotonic() - started
    assert res["n"] == 1 and res["men"] == 1 and res["th"] == [{"i": tid, "un": 1}]
    assert 0.6 < elapsed < 6 and res["wait"] > 0.5  # woke when the message landed, not at the deadline


def test_poll_wait_times_out_with_zero(cli):
    register(cli, "a1")
    started = time.monotonic()
    res = cli.get("/api/poll?wait=1", headers=as_agent(cli, "a1")).json()
    elapsed = time.monotonic() - started
    assert res["n"] == 0 and res["wait"] >= 0.9 and elapsed >= 0.9
    capped = cli.get("/api/poll?wait=9999", headers=as_agent(cli, "a1")).json()  # clamped, returns at once with n>0 absent
    assert capped["n"] == 0 and capped["wait"] <= 60


def test_poll_wait_cannot_combine_with_advance(cli):
    register(cli, "a1")
    res = cli.get("/api/poll?wait=5&advance=1", headers=as_agent(cli, "a1"))
    assert res.status_code == 400 and "do not combine" in res.json()["msg"]


def test_a_waiting_poll_does_not_block_writes(cli):
    """The whole point of routing wait>0 to a reader: other agents can still write meanwhile."""
    register(cli, "a1")
    register(cli, "a2")
    register(cli, "a3")
    tid = cli.post("/api/threads", json={"subject": "s", "b": "x"}, headers=as_agent(cli, "a1")).json()["t"]
    cli.post("/api/sub", json={"t": tid}, headers=as_agent(cli, "a2"))
    cli.get("/api/unread", headers=as_agent(cli, "a2"))

    import threading

    out: dict = {}

    def wait_for_news():
        out.update(cli.rig.client(cli.rig.agent_tokens["a2"], **{"x-agent": "a2"}).get("/api/poll?wait=6").json())

    watcher = threading.Thread(target=wait_for_news, daemon=True)
    watcher.start()
    time.sleep(0.5)  # the wait is now parked on a reader connection
    made = cli.post(f"/api/threads/{tid}/msgs", json={"b": "meanwhile @a2"}, headers=as_agent(cli, "a1"))
    assert made.status_code == 200  # a write went through while a poll was waiting
    watcher.join(timeout=6)
    assert out.get("n") == 1 and out.get("men") == 1  # and the waiter saw it


def test_mcp_poll_supports_wait(cli):
    register(cli, "a1")
    res = cli.rig.client(cli.rig.agent_tokens["a1"]).post(
        "/mcp",
        json={"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "poll", "arguments": {"wait": 1}}},
    ).json()["result"]
    assert res["isError"] is False and res["structuredContent"]["n"] == 0 and res["structuredContent"]["wait"] >= 0.9


def test_ping_reports_release_and_build(cli):
    out = cli.get("/api/ping").json()
    assert out["v"] and out["build"]
    import subprocess

    sha = subprocess.run(["git", "rev-parse", "--short", "HEAD"], capture_output=True, text=True, cwd=pathlib.Path(__file__).parent.parent).stdout.strip()
    assert out["build"].startswith(sha)  # the wire names the commit (and -dirty when the tree is mid-edit, as now)


def test_build_id_reads_detached_and_packed_refs(tmp_path):
    from aif import _git_sha

    gitdir = tmp_path / ".git"
    (gitdir / "refs" / "heads").mkdir(parents=True)
    (gitdir / "HEAD").write_text("ref: refs/heads/master\n")
    (gitdir / "refs" / "heads" / "master").write_text("a" * 40 + "\n")
    assert _git_sha(gitdir) == "a" * 40
    (gitdir / "refs" / "heads" / "master").unlink()
    (gitdir / "packed-refs").write_text("b" * 40 + " refs/heads/master\n")
    assert _git_sha(gitdir) == "b" * 40
    (gitdir / "HEAD").write_text("c" * 40 + "\n")  # detached
    assert _git_sha(gitdir) == "c" * 40
    assert _git_sha(tmp_path / "nope") == ""  # unreadable is not an error


def test_build_id_marks_a_dirty_checkout(tmp_path):
    import subprocess


    def git(*argv):
        return subprocess.run(["git", *argv], cwd=tmp_path, capture_output=True, text=True).stdout.strip()

    git("init", "-q")
    git("config", "user.email", "t@t")
    git("config", "user.name", "t")
    (tmp_path / "aif").mkdir()
    (tmp_path / "aif" / "x.py").write_text("X = 1\n")
    git("add", ".")
    git("commit", "-qm", "init")
    assert "-dirty" not in build_id_for_test(tmp_path)
    (tmp_path / "aif" / "x.py").write_text("X = 2\n")  # tracked edit
    assert "-dirty" in build_id_for_test(tmp_path)
    git("checkout", "-q", ".")
    (tmp_path / "aif" / "new.py").write_text("X = 3\n")  # untracked INSIDE the package: would be imported
    assert "-dirty" in build_id_for_test(tmp_path)
    git("clean", "-qfd")
    (tmp_path / "notes.txt").write_text("outside the package\n")  # untracked outside: irrelevant
    assert "-dirty" not in build_id_for_test(tmp_path)


def build_id_for_test(repo):
    from aif import _git_dirty, _git_sha

    return _git_sha(repo / ".git")[:7] + ("-dirty" if _git_dirty(repo / "aif") else "")


def test_build_id_stays_stable_within_a_process(cli):
    first = cli.get("/api/ping").json()["build"]
    assert cli.get("/api/ping").json()["build"] == first  # the per-process cache holds


def test_build_id_is_computed_once_per_process(monkeypatch):
    import aif

    aif._compute_build.cache_clear()
    calls = []
    orig = aif._git_sha
    monkeypatch.setattr(aif, "_git_sha", lambda p: (calls.append(p), orig(p))[1])
    assert aif.build_id() == aif.build_id() and len(calls) == 1  # genuinely cached, not just equal


def test_build_id_marks_undeterminable_dirtiness(monkeypatch):
    import aif

    aif._compute_build.cache_clear()
    monkeypatch.setattr(aif, "_git_sha", lambda p: "d" * 40)
    monkeypatch.setattr(aif, "_git_dirty", lambda p: None)  # git could not tell
    assert aif.build_id().endswith("-unknown")  # a bare sha would have been a coin flip
    aif._compute_build.cache_clear()
def test_build_id_follows_a_worktree_gitdir_file(tmp_path):
    from aif import _git_sha

    real = tmp_path / "main" / ".git" / "worktrees" / "wt1"
    real.mkdir(parents=True)
    (real / "HEAD").write_text("ref: refs/heads/master\n")
    (real / "refs" / "heads").mkdir(parents=True)
    (real / "refs" / "heads" / "master").write_text("e" * 40 + "\n")
    wt = tmp_path / "wt1"
    wt.mkdir()
    (wt / ".git").write_text(f"gitdir: {real}\n")
    assert _git_sha(wt / ".git") == "e" * 40  # the file is followed to the real gitdir


def test_threads_can_be_fetched_by_id(tmp_path):
    """``threads {ids}`` answers with exactly those headers, in id order, ignoring sort and paging -
    what a reader-facing view needs to pin a couple of threads it cannot assume the position of."""
    cli = make_client(tmp_path)
    register(cli, "a1")
    made = [cli.post("/api/threads", json={"subject": f"t{i}", "b": "x"}, headers=as_agent(cli, "a1")).json()["t"] for i in range(3)]
    picked = cli.post("/api/op", json={"do": "threads", "ids": [made[2], made[0], 9999]}).json()
    assert [t["i"] for t in picked["th"]] == sorted([made[0], made[2]]) and picked["n"] == 2 and picked["sort"] == "ids"
    assert all("s" in t and "msgs" in t for t in picked["th"])  # real headers, not stubs
    assert cli.post("/api/op", json={"do": "threads", "ids": ["nope"]}).status_code == 400


def test_thread_list_names_the_threads_a_view_should_keep_in_sight(tmp_path):
    """The op's own order stays untouched; ``pinned`` only reports the seeded ids (empty if seeding is off)."""
    cli = make_client(tmp_path, seed=True)
    cli.get("/api/ping")  # first request seeds
    assert cli.post("/api/op", json={"do": "threads"}).json()["pinned"] == [1, 2]

    plain = make_client(tmp_path / "plain")
    plain.post("/api/threads", json={"subject": "only", "b": "x"}, headers={"x-agent": "a1"})
    assert plain.post("/api/op", json={"do": "threads"}).json()["pinned"] == []  # nothing seeded, nothing pinned
