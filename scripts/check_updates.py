#!/usr/bin/env python3
"""Check what session/update types claude-code-acp sends."""

import json
import select
import subprocess
import time


def send(proc, msg):
    proc.stdin.write((json.dumps(msg) + "\n").encode())
    proc.stdin.flush()


def read(proc, timeout=10):
    ready, _, _ = select.select([proc.stdout], [], [], timeout)
    if not ready:
        return None
    line = proc.stdout.readline()
    return json.loads(line) if line else None


proc = subprocess.Popen(
    ["claude-code-acp"],
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
)

send(
    proc,
    {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "initialize",
        "params": {
            "protocolVersion": 1,
            "clientInfo": {"name": "test", "version": "0.1"},
        },
    },
)
read(proc)

send(
    proc,
    {
        "jsonrpc": "2.0",
        "id": 2,
        "method": "session/new",
        "params": {"cwd": "/tmp", "mcpServers": []},
    },
)
resp = read(proc)
sid = resp["result"]["sessionId"]
time.sleep(0.5)
while read(proc, timeout=0.5):
    pass

send(
    proc,
    {
        "jsonrpc": "2.0",
        "id": 3,
        "method": "session/prompt",
        "params": {"sessionId": sid, "prompt": [{"type": "text", "text": "Say hi"}]},
    },
)

deadline = time.time() + 30
while time.time() < deadline:
    msg = read(proc, timeout=3)
    if msg is None:
        break
    kind = msg.get("method", "")
    if kind == "session/update":
        u = msg["params"]["update"]
        stype = u.get("sessionUpdate", "?")
        print(f"  {stype}: {json.dumps(u)[:200]}")
    elif "result" in msg:
        print(f"  response: {json.dumps(msg)[:200]}")
        if isinstance(msg.get("result"), dict) and msg["result"].get("stopReason"):
            break
    else:
        print(f"  other: {json.dumps(msg)[:200]}")

proc.stdin.close()
proc.terminate()
proc.wait(timeout=5)
