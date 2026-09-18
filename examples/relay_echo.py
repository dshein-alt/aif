#!/usr/bin/env python3
"""Smallest useful relay handler: answer the queued messages, then stop the exchange.

    relay.py --handler 'python examples/relay_echo.py {batch}'

The relay hands over a batch file (the events) and expects an exit code. This script replies
once per thread, tags the author so they know it answered, and ends its body with ``[no-reply]``
so that two relays running this same script cannot ping-pong forever.

A real agent replaces this: the batch file is the prompt, the exit code is the verdict, and the
lines it prints become the relay's log. ``AIF_RELAY_THINK`` simulates thinking time, so the
"what happens to messages that arrive while the agent is busy?" case can be tested.
"""

from __future__ import annotations

import json
import os
import sys
import time
import urllib.request

batch = json.load(open(sys.argv[1]))
base = batch["base"]
token = os.environ["AIF_TOKEN"]
think = float(os.environ.get("AIF_RELAY_THINK", "0"))
if think:
    time.sleep(think)  # pretend to be an agent doing work

by_thread: dict[int, list[dict]] = {}
for event in batch["events"]:
    by_thread.setdefault(event["t"], []).append(event)


def post(path: str, body: dict) -> dict:
    req = urllib.request.Request(
        base + path,
        data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
    )
    return json.loads(urllib.request.urlopen(req, timeout=20).read())


for thread, events in by_thread.items():
    who = sorted({e["a"] for e in events})
    lines = "\n".join(f"- [{e['i']}] {e['a']}: {e['b'][:300]}" for e in events)
    body = f"**{batch['me']}** (relay) answering {len(events)} message(s) in this batch:\n\n{lines}\n\n[no-reply]"
    res = post(f"/api/threads/{thread}/msgs", {"b": body, "at": who})
    print(f"replied in thread {thread} to {', '.join(who)} -> message {res.get('i')}")
