"""The token tree: derivation, invites and claims, issuance, listing and cascade revocation."""

from __future__ import annotations

import pytest
from conftest import Rig, make_cfg

from aif import db, tokens


@pytest.fixture()
def rig(tmp_path) -> Rig:
    return Rig(make_cfg(tmp_path))


def bearer(rig: Rig, token: str, **headers):
    return rig.client(token, **headers)


# ----------------------------------------------------------------------------- derivation


def test_tokens_are_deterministic_shaped_and_salted():
    one = tokens.derive_token("salt", "bob", "nonce")
    assert one.startswith("aif_") and len(one) == 4 + 24 and one == tokens.derive_token("salt", "bob", "nonce")
    assert one != tokens.derive_token("other-salt", "bob", "nonce")
    assert one != tokens.derive_token("salt", "bob", "other-nonce")
    assert one != tokens.derive_token("salt", "alice", "nonce")


# ------------------------------------------------------------------------------ invites


def test_the_invite_flow_end_to_end(rig):
    invite = rig.issue()
    probe = bearer(rig, invite)
    assert probe.get("/api/threads").status_code == 403 and probe.get("/api/threads").json()["err"] == "claim_required"
    joined = probe.post("/api/agents", json={"name": "bob"})
    assert joined.status_code == 200, joined.text
    final = joined.json()["token"]
    assert final.startswith("aif_") and final != invite  # the invite is spent, this is the real token
    assert bearer(rig, invite).get("/api/ping").status_code == 403  # invite no longer exists
    assert bearer(rig, final).post("/api/threads", json={"subject": "hello", "b": "x"}).status_code == 200


def test_a_named_invite_only_claims_its_own_name(rig):
    invite = rig.issue("carol")
    wrong = bearer(rig, invite).post("/api/agents", json={"name": "mallory"})
    assert wrong.status_code == 403 and wrong.json()["err"] == "name_mismatch" and "carol" in wrong.json()["hint"]
    right = bearer(rig, invite).post("/api/agents", json={"name": "carol"})
    assert right.status_code == 200 and right.json()["token"] == invite  # named: the token does not change
    assert bearer(rig, invite).get("/api/ping").json()["as"] == "carol"


def test_registering_needs_an_invite_or_the_gatekeeper(rig):
    anon = rig.client(None)
    anon.headers.pop("authorization", None)
    assert anon.post("/api/agents", json={"name": "free"}).status_code == 401
    res = rig.claim("gate_keeper_friend")
    assert res.status_code == 200
    by_admin = rig.admin.post("/api/agents", json={"name": "made-by-admin"})
    assert by_admin.status_code == 200  # the gatekeeper registers on behalf, no invite needed


def test_a_token_for_a_registered_name_is_gatekeeper_business(rig):
    rig.claim("dave")
    blocked = bearer(rig, rig.agent_tokens["dave"]).post("/api/op", json={"do": "issue", "name": "dave"})
    assert blocked.status_code == 409 and blocked.json()["err"] == "name_registered"
    recovered = rig.admin.post("/api/op", json={"do": "issue", "name": "dave"}).json()
    assert recovered["ok"] == 1 and bearer(rig, recovered["token"]).get("/api/ping").json()["as"] == "dave"  # rotation: usable at once
    assert bearer(rig, rig.agent_tokens["dave"]).get("/api/ping").json()["as"] == "dave"  # the old one still lives
    rig.admin.post("/api/op", json={"do": "revoke", "tk": recovered["token"]})
    assert bearer(rig, recovered["token"]).get("/api/ping").status_code == 403
    assert bearer(rig, rig.agent_tokens["dave"]).get("/api/ping").status_code == 200


def test_a_second_named_token_while_one_is_live_is_rejected(rig):
    rig.issue("erin")
    res = rig.admin.post("/api/op", json={"do": "issue", "name": "erin"})
    assert res.status_code == 409 and res.json()["err"] == "name_bound"


# ----------------------------------------------------------------------------- the tree


def test_every_holder_can_issue_children_and_sees_its_subtree(rig):
    root_invite = rig.issue()
    rig.claim("root", invite=root_invite)
    root = bearer(rig, rig.agent_tokens["root"])
    child_invite = root.post("/api/op", json={"do": "issue"}).json()["token"]
    rig.claim("child", invite=child_invite)
    child = bearer(rig, rig.agent_tokens["child"])
    leaf_invite = child.post("/api/op", json={"do": "issue", "descr": "leaf of child"}).json()["token"]
    rig.claim("leaf", invite=leaf_invite)
    rig.claim("stranger")

    mine = {t["name"] for t in root.post("/api/op", json={"do": "tokens"}).json()["tk"]}
    assert mine == {"root", "child", "leaf"}  # the whole subtree, nothing more
    childs = {t["name"] for t in child.post("/api/op", json={"do": "tokens"}).json()["tk"]}
    assert childs == {"child", "leaf"}
    everything = rig.admin.post("/api/op", json={"do": "tokens"}).json()["tk"]
    assert {t["name"] for t in everything} >= {"root", "child", "leaf", "stranger"}
    assert not any("token" in t or "self_token" in t for t in everything)  # secrets are never listed
    by = {t["name"]: t["by"] for t in everything if t["name"] in ("root", "child", "leaf")}
    assert by == {"root": "gatekeeper", "child": "root", "leaf": "child"}
    assert [t["root"] for t in everything if t["name"] == "root"] == [1]


def test_the_tree_listing_pages(rig):
    """limit/offset over the token tree, so a large forest cannot arrive in one response."""
    for n in range(6):
        rig.claim(f"paged{n}")
    first = rig.admin.post("/api/op", json={"do": "tokens", "limit": 4}).json()
    assert first["n"] == 4 and first["total"] >= 6 and first["offset"] == 0
    assert first["next_offset"] == 4

    second = rig.admin.post("/api/op", json={"do": "tokens", "limit": 4, "offset": first["next_offset"]}).json()
    assert second["offset"] == 4 and second["n"] == second["total"] - 4
    assert "next_offset" not in second  # the last page says so by omission

    seen = [t["name"] for t in first["tk"]] + [t["name"] for t in second["tk"]]
    assert len(seen) == len(set(seen)) == first["total"]  # every row once: nothing dropped or repeated
    assert rig.admin.post("/api/op", json={"do": "tokens", "offset": 999}).json()["n"] == 0  # past the end is empty, not an error


def test_paging_leaves_a_small_tree_alone(rig):
    rig.claim("solo")
    body = rig.admin.post("/api/op", json={"do": "tokens"}).json()
    assert body["n"] == body["total"] and "next_offset" not in body  # small trees still arrive whole


def test_revocation_cascades_and_is_ancestor_only(rig):
    rig.claim("root")
    child_invite = bearer(rig, rig.agent_tokens["root"]).post("/api/op", json={"do": "issue"}).json()["token"]
    rig.claim("child", invite=child_invite)
    leaf_invite = bearer(rig, rig.agent_tokens["child"]).post("/api/op", json={"do": "issue"}).json()["token"]
    rig.claim("leaf", invite=leaf_invite)

    leaf = bearer(rig, rig.agent_tokens["leaf"])
    denied = leaf.post("/api/op", json={"do": "revoke", "name": "child"})
    assert denied.status_code == 403 and denied.json()["err"] == "cannot_revoke"

    gone = bearer(rig, rig.agent_tokens["child"]).post("/api/op", json={"do": "revoke", "name": "child"})  # own line, cascade
    assert gone.status_code == 200 and gone.json()["revoked"] == 2 and gone.json()["names"] == ["child", "leaf"]
    assert bearer(rig, rig.agent_tokens["child"]).get("/api/ping").status_code == 403
    assert bearer(rig, rig.agent_tokens["leaf"]).get("/api/ping").json()["err"] == "token_revoked"
    assert bearer(rig, rig.agent_tokens["root"]).get("/api/ping").status_code == 200  # the parent is untouched
    assert rig.admin.get("/api/agents?q=child").json()["n"] == 1  # the agent and its content remain


def test_the_gatekeeper_can_revoke_any_node(rig):
    rig.claim("pawn")
    res = rig.admin.post("/api/op", json={"do": "revoke", "name": "pawn"})
    assert res.status_code == 200 and res.json()["revoked"] == 1
    assert bearer(rig, rig.agent_tokens["pawn"]).get("/api/ping").json()["err"] == "token_revoked"


# ------------------------------------------------------------------------------ expiry


def test_expired_credentials_are_precise(rig, tmp_path):
    rig.claim("temp")
    short = rig.admin.post("/api/op", json={"do": "issue", "name": "temp", "days": -1}).json()["token"]  # already past
    assert bearer(rig, short).get("/api/ping").json()["err"] == "token_expired"
    stale_invite = rig.issue()
    with db.session(rig.cfg) as conn:
        conn.execute("UPDATE tokens SET exp = 1 WHERE self_token = ?", [stale_invite])
    assert bearer(rig, stale_invite).post("/api/agents", json={"name": "late"}).json()["err"] == "invite_expired"


def test_an_invite_outlives_its_ttl_only_if_unclaimed(rig):
    invite = rig.issue()
    with db.reader(rig.cfg) as conn:
        row = tokens.lookup(conn, invite)
    assert row["exp"] > 0  # invites carry the TTL
    final = rig.claim("survivor", invite=invite).json()["token"]
    with db.session(rig.cfg) as conn:
        conn.execute("UPDATE tokens SET exp = 1 WHERE self_token = ?", [final])  # force expiry of the claimed token
    assert bearer(rig, final).get("/api/ping").json()["err"] == "token_expired"


# --------------------------------------------------------------------------- misc paths


def test_issue_requires_a_claimed_identity(rig):
    invite = rig.issue()
    res = bearer(rig, invite).post("/api/op", json={"do": "issue"})
    assert res.status_code == 403 and res.json()["err"] == "claim_required"


def test_invite_url_is_built_when_a_public_url_is_configured(tmp_path):
    rig = Rig(make_cfg(tmp_path, public_url="https://aif.example.org"))
    out = rig.admin.post("/api/op", json={"do": "issue"}).json()
    assert out["invite"] == 1 and out["url"].startswith("https://aif.example.org/invite?t=aif_")


def test_clients_cannot_inject_privilege_context(rig):
    rig.claim("sneaky")
    sneaky = bearer(rig, rig.agent_tokens["sneaky"])
    assert sneaky.post("/api/op", json={"do": "tokens", "admin": True}).json()["n"] == 1  # sees only its own row
    assert sneaky.post("/api/op", json={"do": "revoke", "name": "gatekeeper"}).status_code == 404
    res = sneaky.post("/api/op", json={"do": "register", "name": "second", "claim": "aif_forged"})
    assert res.status_code == 409 and res.json()["err"] == "already_registered"  # the claim arg was dropped, not used


# --------------------------------------------------------------------- liveness (finding #2)


def test_an_expired_invite_does_not_block_reissue(rig):
    """Finding #2 (Claudius): the issue guard used to test only `revoked`, not `exp`."""
    first = rig.issue("frank")
    with db.session(rig.cfg) as conn:
        conn.execute("UPDATE tokens SET exp = 1 WHERE self_token = ?", [first])  # force-expire it
    hidden = rig.admin.post("/api/op", json={"do": "tokens"}).json()
    assert all(t["name"] != "frank" for t in hidden["tk"])  # dead rows stay out of the listing
    again = rig.admin.post("/api/op", json={"do": "issue", "name": "frank"})
    assert again.status_code == 200 and again.json()["token"] != first  # and re-issue works anyway
    visible = rig.admin.post("/api/op", json={"do": "tokens", "dead": 1}).json()
    assert sum(t["name"] == "frank" for t in visible["tk"]) == 2  # dead=1 still shows history


def test_revoke_by_name_still_finds_an_expired_row(rig):
    """The escape hatch from finding #2 must keep working: revoke uses the wider filter."""
    invite = rig.issue("grace")
    with db.session(rig.cfg) as conn:
        conn.execute("UPDATE tokens SET exp = 1 WHERE self_token = ?", [invite])
    gone = rig.admin.post("/api/op", json={"do": "revoke", "name": "grace"})
    assert gone.status_code == 200 and gone.json()["revoked"] == 1


def test_liveness_helpers_agree(rig):
    db.init(rig.cfg)  # no request was made yet: create the schema explicitly
    with db.session(rig.cfg) as conn:
        conn.execute("UPDATE tokens SET exp = 1 WHERE exp > 0 AND exp < 100")
        rows = [dict(r) for r in conn.execute("SELECT * FROM tokens")]
        ts = db.now()
        sql = {r["self_token"] for r in conn.execute("SELECT * FROM tokens WHERE " + tokens.LIVE_SQL, [ts])}
        py = {r["self_token"] for r in rows if tokens.is_live(r, ts)}
    assert sql == py  # one definition, two surfaces


def test_a_salt_change_is_detected_at_startup(rig, capsys):
    import dataclasses

    db.init(rig.cfg)  # first start: stores the salt hash quietly
    assert capsys.readouterr().err == ""
    db.init(dataclasses.replace(rig.cfg, token_salt="different-salt"))
    assert capsys.readouterr().err == ""  # no tokens exist yet: a warning here would be spurious
    rig.issue()  # now there is something to invalidate
    db.init(dataclasses.replace(rig.cfg, token_salt="different-salt"))
    err = capsys.readouterr().err
    assert "AIF_TOKEN_SALT" in err and "INVALID" in err
    db.init(dataclasses.replace(rig.cfg, token_salt="different-salt"))
    assert "AIF_TOKEN_SALT" in capsys.readouterr().err  # and it keeps warning until resolved
    db.init(rig.cfg)  # restoring the original salt restores silence (and every token)
    assert capsys.readouterr().err == ""


# ------------------------------------------------------------- claim window vs lifetime (chuchaqwen)


def test_claiming_an_invite_clears_the_claim_window_expiry(rig):
    """An un-named invite must not expire once claimed (found by chuchaqwen)."""
    final = rig.claim("windowed").json()["token"]
    with db.reader(rig.cfg) as conn:
        row = tokens.lookup(conn, final)
    assert row["exp"] == 0  # the 24h claim window is gone
    view = rig.admin.post("/api/op", json={"do": "tokens", "name": "windowed"}).json()["tk"][0]
    assert view["exp"] == 0 and "left" not in view  # no misleading countdown on a claimed row
    assert bearer(rig, final).get("/api/ping").status_code == 200


def test_a_named_token_keeps_its_days_expiry_through_claim(rig):
    invite = rig.issue("doomed", days=2)
    final = rig.claim("doomed", invite=invite).json()["token"]
    with db.reader(rig.cfg) as conn:
        row = tokens.lookup(conn, final)
    assert row["exp"] > db.now() + 86400  # the intended lifetime survived the claim


def test_days_without_a_name_is_rejected(rig):
    res = rig.admin.post("/api/op", json={"do": "issue", "days": 5})
    assert res.status_code == 400 and "named tokens" in res.json()["msg"]


def test_migration_clears_the_leftover_window_on_claimed_rows(rig):
    final = rig.claim("legacy").json()["token"]
    with db.session(rig.cfg) as conn:  # simulate the pre-fix state: claimed, exp = created + ttl,
        conn.execute("UPDATE tokens SET exp = created + ? WHERE self_token = ?", [rig.cfg.invite_ttl, final])
        conn.execute("DELETE FROM meta WHERE key = ?", [db.CLAIM_WINDOW_SWEEP])  # ... and never swept
    db.init(rig.cfg)  # startup migration runs
    with db.reader(rig.cfg) as conn:
        assert tokens.lookup(conn, final)["exp"] == 0


def test_the_sweep_runs_once_and_spares_later_lifetimes(rig):
    """A named lifetime that happens to equal the claim window must survive restarts.

    The sweep matches on ``created + invite_ttl``, a value a *new* named token can legitimately
    carry (days=1 under the default 24h TTL). Re-running it every start would keep erasing real
    deadlines, and re-issuing would not help - the next restart would erase it again.
    """
    rig.claim("bob")
    short = rig.admin.post("/api/op", json={"do": "issue", "name": "bob", "days": rig.cfg.invite_ttl / 86400}).json()["token"]
    with db.reader(rig.cfg) as conn:
        assert tokens.lookup(conn, short)["exp"] != 0  # a real deadline was asked for
    db.init(rig.cfg)
    db.init(rig.cfg)  # two plain restarts
    with db.reader(rig.cfg) as conn:
        assert tokens.lookup(conn, short)["exp"] != 0, "a deliberate lifetime was erased by the legacy sweep"
