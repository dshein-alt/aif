#!/usr/bin/env python3
"""End-to-end check of the relay against a real server process.

Starts a scratch AIF server, registers two agents, runs the relay for one of them, and measures
what a human would actually feel: how long after a message is posted until the agent's answer
appears, and what happens to messages posted while the agent is busy.
"""

from __future__ import annotations

import json
import os
import pathlib
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = str(pathlib.Path(__file__).resolve().parent.parent)  # repository root (this file lives in examples/)
BASE = "http://127.0.0.1:18099"
DATA = "/tmp/aifrelay-e2e"
ADMIN = "e2e-secret"


def call(path, token, method="GET", body=None):
    req = urllib.request.Request(
        BASE + path, data=json.dumps(body).encode() if body is not None else None,
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"}, method=method)
    try:
        return json.loads(urllib.request.urlopen(req, timeout=30).read() or b"{}")
    except urllib.error.HTTPError as exc:
        return json.loads(exc.read() or b"{}")
    except urllib.error.URLError:
        return {}  # not up yet


def wait_for(fn, timeout, what):
    deadline = time.time() + timeout
    while time.time() < deadline:
        got = fn()
        if got:
            return got
        time.sleep(0.1)
    raise SystemExit(f"timeout waiting for {what}")


def main() -> int:
    shutil.rmtree(DATA, ignore_errors=True)
    os.makedirs(DATA, exist_ok=True)
    server = subprocess.Popen(
        ["uv", "run", "aif", "serve", "--host", "127.0.0.1", "--port", "18099", "--data-dir", DATA, "--token", ADMIN, "--ui", "off"],
        cwd=ROOT, env={**os.environ, "AIF_TOKEN_SALT": "e2e-salt-0123456789"}, stdout=open(f"{DATA}/server.log", "w"), stderr=subprocess.STDOUT)
    stop_children = [server]
    try:
        wait_for(lambda: (call("/healthz", ADMIN) or {}).get("ok"), 30, "server")
        def register(name):
            invite = call("/api/op", ADMIN, "POST", {"do": "issue"})["token"]
            res = call("/api/agents", invite, "POST", {"name": name})
            return res["token"]

        watcher, talker = register("watcher"), register("talker")
        call("/api/op?do=unread&advance=1", watcher)  # clear the seeded backlog before measuring
        thread = call("/api/threads", talker, "POST", {"subject": "relay e2e", "b": "starting the relay test"})["i"]
        print(f"server up: thread {thread}, agents registered")

        # --- run 1: an idle agent answers within the long poll -------------------
        relay = subprocess.Popen(
            [sys.executable, "examples/relay.py", "--base", BASE, "--state", f"{DATA}/relay", "--wait", "25",
             "--handler", f"{sys.executable} {ROOT}/examples/relay_echo.py {{batch}}"],
            cwd=ROOT, env={**os.environ, "AIF_TOKEN": watcher}, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        stop_children.append(relay)
        log = f"{DATA}/relay/relay.log"
        wait_for(lambda: os.path.exists(log) and "relay" in open(log).read(), 20, "relay startup")

        posted = time.time()
        call(f"/api/threads/{thread}/msgs", talker, "POST", {"b": "@watcher are you there?", "at": ["watcher"]})
        reply = wait_for(lambda: next((m for m in call(f"/api/threads/{thread}?limit=20", talker)["ms"] if m["a"] == "watcher"), None), 30, "first reply")
        latency = time.time() - posted
        print(f"\nRUN 1 (idle agent): answer {reply['i']} in {latency:.2f}s after the mention")

        # --- run 2: messages that arrive while the agent is busy ----------------
        relay.send_signal(signal.SIGINT)
        relay.wait(timeout=10)
        call("/api/op?do=unread&advance=1", watcher)
        busy_relay = subprocess.Popen(
            [sys.executable, "examples/relay.py", "--base", BASE, "--state", f"{DATA}/relay2", "--wait", "25",
             "--handler", f"{sys.executable} {ROOT}/examples/relay_echo.py {{batch}}"],
            cwd=ROOT, env={**os.environ, "AIF_TOKEN": watcher, "AIF_RELAY_THINK": "6"}, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        stop_children.append(busy_relay)
        log2 = f"{DATA}/relay2/relay.log"
        wait_for(lambda: os.path.exists(log2) and "relay" in open(log2).read(), 20, "busy relay startup")

        t0 = time.time()
        call(f"/api/threads/{thread}/msgs", talker, "POST", {"b": "@watcher message A (starts a slow turn)", "at": ["watcher"]})
        time.sleep(1.5)  # the agent is now inside its handler, "thinking"
        call(f"/api/threads/{thread}/msgs", talker, "POST", {"b": "@watcher message B (arrives mid-turn)", "at": ["watcher"]})
        time.sleep(1.5)
        call(f"/api/threads/{thread}/msgs", talker, "POST", {"b": "@watcher message C (still mid-turn)", "at": ["watcher"]})

        def watcher_replies(minimum):
            ms = [m for m in call(f"/api/threads/{thread}?limit=50", talker)["ms"] if m["a"] == "watcher"]
            return ms if len(ms) >= minimum else None

        first = wait_for(lambda: watcher_replies(2), 40, "reply to A")
        t_first = time.time()
        second = wait_for(lambda: watcher_replies(3), 60, "reply to B and C")
        t_second = time.time()
        body1 = first[1]["b"]
        body2 = second[2]["b"]
        print("RUN 2 (agent busy 6s per turn):")
        print(f"  reply to A appeared {t_first - t0:.2f}s after A was posted")
        print(f"  B and C answered in the next turn, {t_second - t_first:.2f}s later")
        print(f"  first reply covered: {'A' if 'message A' in body1 else '?'}   second reply covered: B={'message B' in body2} C={'message C' in body2}")
        print(f"  no message lost: B and C both in one batch = {('message B' in body2) and ('message C' in body2)}")

        # --- what the relay itself logged ---------------------------------------
        print("\nrelay log (run 2):")
        print("\n".join("  " + line for line in open(log2).read().splitlines()[-6:]))
        return 0
    finally:
        for proc in stop_children:
            if proc.poll() is None:
                proc.send_signal(signal.SIGINT)
        for proc in stop_children:
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()


if __name__ == "__main__":
    sys.exit(main())
