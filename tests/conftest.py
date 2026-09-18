"""Shared test harness: an admin client plus helpers that claim real per-agent tokens.

Everything goes through the public API (issue invite -> register -> final token), so the tests
exercise exactly what an agent's client would do.
"""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from aif.app import create_app
from aif.config import Config

ADMIN = "admin-secret"
SALT = "unit-test-salt-0001"


def make_cfg(tmp_path, **overrides) -> Config:
    return Config(
        **{
            "admin_tokens": [ADMIN],
            "token_salt": SALT,
            "seed": False,
            "data_dir": str(tmp_path),
            "db_path": str(tmp_path / "aif.db"),
            "attachments_dir": str(tmp_path / "attachments"),
            **overrides,
        }
    )


class Rig:
    """One server plus a registry of claimed agent tokens."""

    def __init__(self, cfg: Config, ui: bool = False):
        self.cfg = cfg
        self.admin = TestClient(create_app(cfg, mount_ui=ui), headers={"authorization": f"Bearer {ADMIN}"})
        self.agent_tokens: dict[str, str] = {}

    def client(self, bearer: str | None = None, **headers) -> TestClient:
        return TestClient(create_app(self.cfg, mount_ui=False), headers={"authorization": f"Bearer {bearer or ADMIN}", **headers})

    def issue(self, name: str | None = None, **kw) -> str:
        """Have the gatekeeper mint an invite (no name) or a named token."""
        body = {"do": "issue", **({"name": name} if name else {}), **kw}
        res = self.admin.post("/api/op", json=body)
        assert res.status_code == 200, res.text
        return res.json()["token"]

    def claim(self, name: str, invite: str | None = None, **kw):
        """Full join flow: issue (unless an invite is given) then register; returns the response."""
        invite = invite or self.issue()
        with TestClient(create_app(self.cfg, mount_ui=False), headers={"authorization": f"Bearer {invite}"}) as client:
            res = client.post("/api/agents", json={"name": name, **kw})
        if res.status_code == 200:
            self.agent_tokens[name] = res.json()["token"]
        return res

    def headers(self, name: str) -> dict[str, str]:
        return {"authorization": f"Bearer {self.agent_tokens[name]}", "x-agent": name}

    def agent(self, name: str, **kw) -> TestClient:
        """A registered agent as its own client (its token in the Authorization header)."""
        self.claim(name, **kw)
        return TestClient(create_app(self.cfg, mount_ui=False), headers=self.headers(name))


@pytest.fixture()
def rig(tmp_path) -> Rig:
    return Rig(make_cfg(tmp_path))
