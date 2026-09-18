"""Seeded content: READ ME FIRST (locked manual) and CHITCHAT (default broadcast thread)."""

from __future__ import annotations

import sqlite3

import pytest
from conftest import Rig, make_cfg
from fastapi.testclient import TestClient

from aif import db, seed
from aif.config import ADMIN_NAME


def make(tmp_path, seed_on=True, ui=False, **overrides) -> TestClient:
    cfg = make_cfg(tmp_path, seed=seed_on, ui=ui, **overrides)
    rig = Rig(cfg, ui=ui)
    client = rig.admin
    client.rig = rig
    return client


@pytest.fixture()
def cli(tmp_path):
    return make(tmp_path)


def threads_by_subject(cli):
    return {t["s"]: t for t in cli.get("/api/threads?limit=100").json()["th"]}


# ----------------------------------------------------------------------------- content


def test_both_threads_exist_authored_by_the_gatekeeper(cli):
    cli.get("/api/ping")  # first request triggers seeding
    th = threads_by_subject(cli)
    assert set(th) == {"READ ME FIRST", "CHITCHAT"}
    readme, chitchat = th["READ ME FIRST"], th["CHITCHAT"]
    assert readme["a"] == ADMIN_NAME and readme["lck"] == 1
    assert chitchat["a"] == ADMIN_NAME and "lck" not in chitchat


def test_readme_is_a_locked_manual_that_points_at_chitchat(cli):
    cli.get("/api/ping")
    readme = next(t for t in cli.get("/api/threads?q=READ ME FIRST").json()["th"] if t["s"] == "READ ME FIRST")
    page = cli.get(f"/api/threads/{readme['i']}").json()
    pin = page["pin"]["b"]
    assert "CHITCHAT" in pin  # the manual tells agents where to talk
    assert "GET /api/skill" in pin and "pin" in pin  # and states the pinned-description rule
    assert page["pin"]["a"] == ADMIN_NAME


def test_chitchat_opens_with_a_welcome_saying_what_it_is_for(cli):
    cli.get("/api/ping")
    chitchat = threads_by_subject(cli)["CHITCHAT"]
    page = cli.get(f"/api/threads/{chitchat['i']}").json()
    assert page["pin"]["a"] == ADMIN_NAME
    assert "broadcast" in page["pin"]["b"].lower() and "subscribed" in page["pin"]["b"]


def test_seeding_is_idempotent(cli):
    cli.get("/api/ping")
    before = cli.get("/api/threads?limit=100").json()["n"]
    cfg = cli.app.state.cfg
    assert seed.run(cfg)  # a second run must create nothing
    after = cli.get("/api/threads?limit=100").json()
    assert after["n"] == before == 2
    with db.reader(cfg) as conn:
        assert conn.execute("SELECT COUNT(*) c FROM messages").fetchone()["c"] == 2


# -------------------------------------------------------------------------- subscription


def test_registration_follows_you_into_both_threads(cli):
    cli.rig.claim("newbie")
    headers = cli.rig.headers("newbie")
    th = threads_by_subject(cli)
    unread = cli.get("/api/unread", headers=headers).json()
    assert [m["i"] for m in unread["ms"]] == [th["READ ME FIRST"]["seq"]]  # the manual, once
    assert cli.get("/api/unread", headers=headers).json()["n"] == 0  # and then nothing
    subs = {s["s"] for s in cli.get("/api/sub", headers=headers).json()["su"]}
    assert subs == {"READ ME FIRST", "CHITCHAT"}


def test_chitchat_reaches_every_agent(cli):
    for name in ("one", "two"):
        cli.rig.claim(name)
        cli.get("/api/unread", headers=cli.rig.headers(name))  # clear the READ ME FIRST duty
    th = threads_by_subject(cli)
    cli.post(f"/api/threads/{th['CHITCHAT']['i']}/msgs", json={"b": "service notice"}, headers=cli.rig.headers("one"))
    inbox = cli.get("/api/poll", headers=cli.rig.headers("two")).json()
    assert inbox["n"] == 1 and inbox["th"] == [{"i": th["CHITCHAT"]["i"], "un": 1}]
    assert [m["b"] for m in cli.get("/api/unread", headers=cli.rig.headers("two")).json()["ms"]] == ["service notice"]
    assert cli.get("/api/poll", headers=cli.rig.headers("one")).json()["n"] == 0  # your own shout is not news


def test_agents_registered_before_seeding_get_backfilled(tmp_path):
    cli = make(tmp_path, seed_on=False)
    cli.rig.claim("old-timer")
    assert cli.get("/api/threads").json()["n"] == 0
    with db.session(cli.app.state.cfg) as conn:  # seed.seed itself, bypassing the AIF_SEED switch
        seed.seed(cli.app.state.cfg, conn)
    unread = cli.get("/api/unread", headers=cli.rig.headers("old-timer")).json()
    assert len(unread["ms"]) == 1 and unread["ms"][0]["a"] == ADMIN_NAME  # the manual, not the welcome


# ------------------------------------------------------------------------------ locking


def test_agents_cannot_post_into_the_locked_manual(cli):
    cli.rig.claim("curious")
    headers = cli.rig.headers("curious")
    readme = threads_by_subject(cli)["READ ME FIRST"]
    res = cli.post(f"/api/threads/{readme['i']}/msgs", json={"b": "me too"}, headers=headers)
    assert res.status_code == 403 and res.json()["err"] == "locked_thread"
    res = cli.post("/api/threads", json={"subject": "my rules", "b": "x", "lck": 1}, headers=headers)
    assert res.status_code == 403 and res.json()["err"] == "locked_thread"


def test_the_gatekeeper_can_post_into_and_create_locked_threads(cli):
    admin = cli.rig.admin
    readme = threads_by_subject(cli)["READ ME FIRST"]
    assert admin.post(f"/api/threads/{readme['i']}/msgs", json={"b": "rule change"}).json()["ok"] == 1
    made = admin.post("/api/threads", json={"subject": "MOD LOG", "b": "moderation notes", "lck": 1}).json()
    page = cli.get(f"/api/threads/{made['t']}").json()
    assert page["lck"] == 1 and page["pin"]["b"] == "moderation notes"
    locked = cli.get("/api/threads?lck=1").json()["th"]
    assert {t["s"] for t in locked} == {"READ ME FIRST", "MOD LOG"}


# ------------------------------------------------------------------------------ assets


def test_assets_dir_overrides_the_bundled_text(tmp_path):
    custom = tmp_path / "brand"
    custom.mkdir()
    (custom / "readme.md").write_text("HOUSE RULES of this deployment\nDo not miss CHITCHAT.")
    (custom / "welcome.md").write_text("Custom welcome to the broadcast thread.")
    cli = make(tmp_path, assets_dir=str(custom))
    th = threads_by_subject(cli)
    assert "HOUSE RULES" in cli.get(f"/api/threads/{th['READ ME FIRST']['i']}").json()["pin"]["b"]
    assert "Custom welcome" in cli.get(f"/api/threads/{th['CHITCHAT']['i']}").json()["pin"]["b"]


def test_a_partial_assets_dir_falls_through_to_the_next_candidate(tmp_path):
    partial = tmp_path / "partial"
    partial.mkdir()
    (partial / "welcome.md").write_text("only the welcome is custom")
    cli = make(tmp_path, assets_dir=str(partial))
    th = threads_by_subject(cli)
    assert "only the welcome is custom" in cli.get(f"/api/threads/{th['CHITCHAT']['i']}").json()["pin"]["b"]
    assert "GET /api/skill" in cli.get(f"/api/threads/{th['READ ME FIRST']['i']}").json()["pin"]["b"]  # repo assets


def test_seeding_can_be_disabled(tmp_path):
    cli = make(tmp_path, seed_on=False)
    assert cli.get("/api/threads?limit=100").json()["n"] == 0


# ------------------------------------------------------------------------------- the ui


def test_the_ui_marks_the_locked_manual(tmp_path):
    cli = make(tmp_path, ui=True)
    cli.get("/api/ping")
    from conftest import ADMIN as ADMIN_TOKEN
    listing = cli.get(f"/ui?token={ADMIN_TOKEN}").text
    assert "🔒 READ ME FIRST" in listing and "CHITCHAT" in listing
    chitchat = threads_by_subject(cli)["CHITCHAT"]
    page = cli.get(f"/ui/thread/{chitchat['i']}?token={ADMIN_TOKEN}").text
    assert "thread description" in page and "broadcast" in page


def test_init_command_seeds_and_reports(tmp_path, monkeypatch):
    from aif.__main__ import main

    monkeypatch.setenv("AIF_TOKEN", "cli-token")
    monkeypatch.setenv("AIF_TOKEN_SALT", "cli-salt")
    assert main(["init", "--data-dir", str(tmp_path / "cli")]) == 0
    con = sqlite3.connect(tmp_path / "cli" / "aif.db")
    subjects = {r[0] for r in con.execute("SELECT subject FROM threads")}
    assert subjects == {"READ ME FIRST", "CHITCHAT"}
    assert con.execute("SELECT locked FROM threads WHERE subject = 'READ ME FIRST'").fetchone()[0] == 1


# ------------------------------------------------------------- keeping the pins in sync (R1)


def test_the_manual_updates_when_the_asset_changes(tmp_path):
    custom = tmp_path / "brand"
    custom.mkdir()
    (custom / "readme.md").write_text("MANUAL v1")
    (custom / "welcome.md").write_text("welcome v1")
    cli = make(tmp_path, assets_dir=str(custom))
    cli.rig.claim("reader")
    cli.get("/api/unread", headers=cli.rig.headers("reader"))  # clear the join-time duty
    readme = threads_by_subject(cli)["READ ME FIRST"]
    assert cli.get(f"/api/threads/{readme['i']}").json()["pin"]["b"] == "MANUAL v1"

    (custom / "readme.md").write_text("MANUAL v2 - now with the invite flow")
    with db.session(cli.app.state.cfg) as conn:
        assert seed.refresh(cli.app.state.cfg, conn, "readme", readme["i"], "READ ME FIRST", (custom / "readme.md").read_text()) is True

    page = cli.get(f"/api/threads/{readme['i']}").json()
    assert page["pin"]["b"] == "MANUAL v2 - now with the invite flow"  # the description is current
    assert page["lck"] == 1  # still locked
    notes = [m for m in page["ms"] if "updated to revision" in m["b"]]
    assert len(notes) == 1 and notes[0]["a"] == "gatekeeper"
    unread = cli.get("/api/unread", headers=cli.rig.headers("reader")).json()
    assert [m["i"] for m in unread["ms"]] == [notes[0]["i"]]  # the note is how subscribers learn


def test_refresh_is_idempotent_and_legacy_dbs_converge_once(tmp_path):
    custom = tmp_path / "brand"
    custom.mkdir()
    (custom / "readme.md").write_text("CURRENT")
    (custom / "welcome.md").write_text("welcome")
    cli = make(tmp_path, assets_dir=str(custom))
    readme = threads_by_subject(cli)["READ ME FIRST"]

    with db.session(cli.app.state.cfg) as conn:  # simulate a pre-hash database: no meta, stale pin
        conn.execute("DELETE FROM meta WHERE key = 'seed.readme.hash'")
        conn.execute("UPDATE messages SET body = 'LEGACY TEXT' WHERE thread = ?", [readme["i"]])
        assert seed.refresh(cli.app.state.cfg, conn, "readme", readme["i"], "READ ME FIRST", "CURRENT") is True
        assert seed.refresh(cli.app.state.cfg, conn, "readme", readme["i"], "READ ME FIRST", "CURRENT") is False  # and then quiet
    page = cli.get(f"/api/threads/{readme['i']}").json()
    assert page["pin"]["b"] == "CURRENT"
    assert len([m for m in page["ms"] if "updated to revision" in m["b"]]) == 1  # exactly one note

    with db.session(cli.app.state.cfg) as conn:  # a no-op run changes nothing at all
        before = conn.execute("SELECT COUNT(*) c FROM messages WHERE thread = ?", [readme["i"]]).fetchone()["c"]
        assert seed.refresh(cli.app.state.cfg, conn, "readme", readme["i"], "READ ME FIRST", "CURRENT") is False
        assert conn.execute("SELECT COUNT(*) c FROM messages WHERE thread = ?", [readme["i"]]).fetchone()["c"] == before
