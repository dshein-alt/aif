"""Injection resistance: text sanitisation at ingest, plus a static audit of every SQL string.

Two independent lines of defence, both tested here:

1. **Transport** - no statement is ever built from request text. The AST audit walks every module
   and rejects interpolated SQL unless the interpolation is a module CONSTANT or one of the
   ``db.where`` / ``db.marks`` / ``db.desc`` / ``db.sort_expr`` helpers, which only ever join
   constant fragments. Values travel as bound parameters.
2. **Content** - control, bidi and zero-width characters are dropped at ingest so stored text
   renders to agents exactly as it is stored, and ``bo\\u200bt`` cannot impersonate ``bot``.
   SQL-looking *content* is stored verbatim: a forum that rejects ``DROP TABLE`` could not discuss
   databases.
"""

from __future__ import annotations

import ast
import pathlib

import pytest
from fastapi.testclient import TestClient

from aif import sanitize
from aif.app import create_app
from aif.config import Config

TOKEN = "t0ken"
ALLOWED_CALLS = {"where", "marks", "desc", "sort_expr", "like_arg", "now"}
SRC = pathlib.Path(__file__).resolve().parent.parent / "aif"

PROBES = [
    "'); DROP TABLE messages;--",
    "'); DELETE FROM agents;--",
    "1' OR '1'='1",
    'x" OR "1"="1',
    "'; INSERT INTO agents (name,low) VALUES ('pwn','pwn');--",
    " UNION SELECT name, descr FROM agents--",
    "/* comment */ SELECT 1",
    "admin'--",
    "1; UPDATE threads SET subject='hax' WHERE id=1",
    "xp_cmdshell('id')",
    "' || (SELECT group_concat(name) FROM agents) || '",
]


# --------------------------------------------------------------------------- static audit


def _sql_nodes():
    """Yield (path, lineno, first-argument-node) for every execute()/executescript() call."""
    for path in sorted(SRC.glob("*.py")):
        tree = ast.parse(path.read_text(), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) and node.func.attr in ("execute", "executescript", "executemany"):
                if node.args:
                    yield path, node.lineno, node.args[0]


def _interpolations(node: ast.AST) -> list[str]:
    """Names/expressions interpolated into a SQL string, excluding the whitelisted builders."""
    offenders: list[str] = []
    for sub in ast.walk(node):
        if isinstance(sub, ast.FormattedValue):
            expr = sub.value
            is_constant = isinstance(expr, ast.Name) and expr.id.isupper() and len(expr.id) > 2
            is_helper = isinstance(expr, ast.Call) and isinstance(expr.func, ast.Attribute) and expr.func.attr in ALLOWED_CALLS
            if not (is_constant or is_helper):
                offenders.append(ast.unparse(expr))
        if isinstance(sub, ast.BinOp) and isinstance(sub.op, ast.Mod):
            offenders.append("%-format: " + ast.unparse(sub))
        if isinstance(sub, ast.Call) and isinstance(sub.func, ast.Attribute) and sub.func.attr == "format":
            offenders.append(".format: " + ast.unparse(sub))
    return offenders


def test_no_statement_is_built_from_request_text():
    bad = []
    for path, lineno, arg in _sql_nodes():
        for offence in _interpolations(arg):
            bad.append(f"{path.name}:{lineno}: {offence}")
    assert not bad, "SQL must not interpolate anything but CONSTANTS / db.where / db.marks / db.desc / db.sort_expr:\n" + "\n".join(bad)


def _sum_leaves(node: ast.AST):
    """Flatten ``a + b + c`` into its operands."""
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        yield from _sum_leaves(node.left)
        yield from _sum_leaves(node.right)
    else:
        yield node


def test_statements_only_concatenate_constants():
    """SQL may be assembled from literal fragments and UPPER_CASE module constants, never from values."""
    bad = []
    for path, lineno, arg in _sql_nodes():
        for sub in ast.walk(arg):
            if isinstance(sub, ast.BinOp) and isinstance(sub.op, ast.Add):
                for leaf in _sum_leaves(sub):
                    ok = isinstance(leaf, (ast.Constant, ast.JoinedStr)) or (isinstance(leaf, ast.Name) and leaf.id.isupper() and len(leaf.id) > 2)
                    if not ok:
                        bad.append(f"{path.name}:{lineno}: {ast.unparse(leaf)}")
    assert not bad, "SQL fragments must be literals or CONSTANTS:\n" + "\n".join(bad)


# ---------------------------------------------------------------- sanitiser unit tests


def test_fold_drops_junk_and_folds_newlines():
    assert sanitize.fold("a\r\nb\rc") == "a\nb\nc"
    assert sanitize.fold("zero\u200bwidth\ufefftext") == "zerowidthtext"
    assert sanitize.fold("bi\u202ed") == "bid"  # RLO removed, not interpreted
    assert sanitize.fold("tab\there\x00\x1b[31m\x07") == "tab\there[31m"
    assert sanitize.fold(None) == "" and sanitize.fold("") == ""
    assert sanitize.fold("emoji \U0001f600 and é") == "emoji \U0001f600 and é"  # not ASCII-only


def test_text_and_oneline_shape_and_cap():
    assert sanitize.text("  hi \n there  \n\n") == "hi\n there"
    assert sanitize.text("line   \nnext\t", 4) == "line"
    assert sanitize.oneline("a\n b\tc", 10) == "a b c"
    assert sanitize.oneline("  spaced  out  ", 200) == "spaced out"


def test_sanitisation_is_idempotent():
    for value in ["a\u200bb\r\nc\x00", "'); DROP TABLE x;--", "\r\n\r\n\nx", "  pad  "]:
        once = sanitize.text(value)
        assert sanitize.text(once) == once
        assert sanitize.oneline(sanitize.oneline(value)) == sanitize.oneline(value)


def test_sql_looking_content_is_not_rejected():
    for probe in PROBES:
        assert sanitize.text(probe) == probe.strip()  # verbatim apart from edge trimming
    assert sanitize.sqlish("'); DROP TABLE messages;--") is True
    assert sanitize.sqlish("the quarterly budget is 5k") is False


# ------------------------------------------------------------------- ingest through the API


@pytest.fixture()
def cli(tmp_path):
    cfg = Config(tokens=[TOKEN], data_dir=str(tmp_path), attachments_dir=str(tmp_path / "att"))
    client = TestClient(create_app(cfg, mount_ui=False))
    client.headers["authorization"] = f"Bearer {TOKEN}"
    client.post("/api/agents", json={"name": "alice"})
    client.post("/api/agents", json={"name": "bob"})
    return client


def post(cli, agent, **payload):
    return cli.post("/api/threads", json=payload, headers={"x-agent": agent})


def body_of(cli, mid: int) -> str:
    return cli.get(f"/api/messages/{mid}").json()["b"]


def test_bodies_are_stored_without_invisible_junk(cli):
    made = post(cli, "alice", subject="clean", b="hello\r\nworld\x00\u200b!\x1b[0m tail")
    assert body_of(cli, made.json()["i"]) == "hello\nworld!\x1b[0m tail".replace("\x1b", "")


def test_dangerous_looking_content_survives_verbatim(cli):
    for probe in PROBES:
        made = post(cli, "alice", subject="probe", b=probe, at=["bob"])
        assert made.status_code == 200, made.text
        assert body_of(cli, made.json()["i"]) == probe.strip()  # content survives, edges trimmed
    # ... and the schema is untouched afterwards
    cli.post("/api/agents", json={"name": "carol"})
    assert cli.get("/api/threads?limit=100").json()["n"] == len(PROBES)
    assert {a["n"] for a in cli.get("/api/agents?limit=100").json()["a"]} == {"alice", "bob", "carol"}


def test_invisible_characters_cannot_bypass_a_taken_name(cli):
    for candidate in ["ali\u200bce", "ALICE", "alic\u200ce"]:
        res = cli.post("/api/agents", json={"name": candidate})
        assert res.status_code == 409 and res.json()["err"] == "name_taken", candidate


def test_invisible_characters_are_canonicalised_not_stored(cli):
    """ev\u202eil *is* "evil" once the override is folded away - so it collides or registers, never hides."""
    assert cli.post("/api/agents", json={"name": "ev\u202eil"}).json()["name"] == "evil"
    assert cli.post("/api/agents", json={"name": "evil"}).status_code == 409  # the folded form is taken
    assert cli.post("/api/agents", json={"name": "\u200bhidden"}).json()["name"] == "hidden"
    for candidate in ["sp ace", "café", "a\nb", "-", "x" * 65, "no$dollar", "ev\u202e il"]:
        res = cli.post("/api/agents", json={"name": candidate})
        assert res.status_code == 400 and res.json()["err"] == "bad_request", candidate
    assert {a["n"] for a in cli.get("/api/agents?limit=100").json()["a"]} == {"alice", "bob", "evil", "hidden"}


def test_mentions_resolve_through_invisible_characters(cli):
    made = post(cli, "alice", subject="tag", b="ping @bo\u200bb")
    assert made.json()["at"] == ["bob"]
    inbox = cli.get("/api/unread", headers={"x-agent": "bob"}).json()
    assert [m["i"] for m in inbox["ms"]] == [made.json()["i"]]


def test_like_wildcards_in_search_are_matched_literally(cli):
    post(cli, "alice", subject="50% done", b="x")
    post(cli, "alice", subject="50d", b="x")
    post(cli, "alice", subject="under_score", b="x")
    assert [t["s"] for t in cli.get("/api/threads", params={"q": "50%"}).json()["th"]] == ["50% done"]
    assert cli.get("/api/threads", params={"q": "5_d"}).json()["n"] == 0  # _ is not a wildcard here
    post(cli, "alice", subject="5_d", b="x")
    assert [t["s"] for t in cli.get("/api/threads", params={"q": "5_d"}).json()["th"]] == ["5_d"]
    assert [t["s"] for t in cli.get("/api/threads", params={"q": "under_score"}).json()["th"]] == ["under_score"]
    assert cli.get("/api/threads", params={"q": "%"}).json()["n"] == 1  # only the literal percent row


def test_numeric_parameters_are_coerced_never_interpolated(cli):
    tid = post(cli, "alice", subject="probe", b="x").json()["t"]
    for value in ["1 OR 1=1", "1; DROP TABLE messages", "1 UNION SELECT 1", "-1 OR 2>1", "1.5;--"]:
        for key in ("since", "limit", "offset", "before", "max_body"):
            res = cli.get(f"/api/threads/{tid}", params={key: value})
            assert res.status_code == 400 and res.json()["err"] == "bad_request", (key, value, res.text)
    cli.post("/api/agents", json={"name": "dave"})  # every table still usable
    assert cli.get(f"/api/threads/{tid}").json()["s"] == "probe"
    assert cli.get("/api/messages/1").status_code == 200


def test_path_traversal_in_file_names_only_affects_the_display_name(cli):
    made = cli.post(
        "/api/threads",
        json={"subject": "files", "b": "x", "files": [{"n": "../../../etc/passwd", "text": "pwned"}, {"n": "a\u200bb\tc.txt", "text": "ok"}]},
        headers={"x-agent": "alice"},
    )
    assert made.status_code == 200, made.text
    names = {f["n"] for f in made.json()["fl"]}
    assert names == {"passwd", "ab c.txt"}  # tab collapses to one space, zero width disappears
    blobs = list(pathlib.Path(cli.app.state.cfg.attachments_dir).rglob("*"))
    stored = [p.name for p in blobs if p.is_file()]
    assert len(stored) == 2 and all(len(name) == 32 and "/" not in name for name in stored)


def test_subject_derived_from_body_is_sanitised(cli):
    made = post(cli, "alice", b="first\u200b line\x00\nsecond line")
    assert cli.get(f"/api/threads/{made.json()['t']}").json()["s"] == "first line"


def test_description_is_sanitised(cli):
    cli.post("/api/agents", json={"name": "eve", "descr": " role\r\nwith\x00 junk\u200b "})
    assert cli.get("/api/agents?q=role with junk").json()["n"] == 1  # matched after normalisation


def test_ui_output_is_escaped_not_rewritten(cli):
    """The API stores raw text; only the HTML view escapes it on output."""
    cfg = Config(tokens=[TOKEN], data_dir=str(pathlib.Path(cli.app.state.cfg.data_dir)), ui=True)
    ui = TestClient(create_app(cfg, mount_ui=True))
    made = ui.post("/api/threads", json={"subject": "<script>x</script>", "b": "<b>bold</b> & <img src=x>"}, headers={"authorization": f"Bearer {TOKEN}", "x-agent": "alice"})
    page = ui.get(f"/ui/thread/{made.json()['t']}?token={TOKEN}").text
    assert "&lt;script&gt;" in page and "<script>" not in page
    assert "&lt;b&gt;bold&lt;/b&gt;" in page
    assert ui.get(f"/api/messages/{made.json()['i']}", headers={"authorization": f"Bearer {TOKEN}"}).json()["b"] == "<b>bold</b> & <img src=x>"
