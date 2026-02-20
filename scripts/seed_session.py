#!/usr/bin/env python3
"""Connect to multiplexer socket, initialize session, send a prompt."""

import json
import socket
import sys
import time


def send(sock, msg):
    sock.sendall((json.dumps(msg) + "\n").encode())


def read_until(sock, predicate, timeout=15):
    """Read messages until predicate returns truthy, or timeout."""
    buf = b""
    deadline = time.time() + timeout
    while time.time() < deadline:
        sock.settimeout(max(0.1, deadline - time.time()))
        try:
            data = sock.recv(8192)
            if not data:
                break
            buf += data
            while b"\n" in buf:
                line, buf = buf.split(b"\n", 1)
                msg = json.loads(line)
                result = predicate(msg)
                if result:
                    return result, msg
        except socket.timeout:
            continue
    return None, None


sock_path = sys.argv[1] if len(sys.argv) > 1 else None
if not sock_path:
    # Find the socket from proxy log
    with open("/tmp/acp-proxy.log") as f:
        for line in f:
            if ".sock" in line:
                sock_path = line.strip().split("socket ")[-1]
    if not sock_path:
        print("No socket found")
        sys.exit(1)

print(f"Connecting to {sock_path}")
sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.connect(sock_path)

# Initialize
print("Sending initialize...")
send(
    sock,
    {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "initialize",
        "params": {
            "protocolVersion": 1,
            "clientInfo": {"name": "seed", "version": "0.1"},
        },
    },
)


def has_agent_info(msg):
    r = msg.get("result", {})
    if isinstance(r, dict) and "agentInfo" in r:
        return r["agentInfo"]["name"]
    return None


name, _ = read_until(sock, has_agent_info)
print(f"  -> {name}")

# Session new
print("Sending session/new...")
send(
    sock,
    {
        "jsonrpc": "2.0",
        "id": 2,
        "method": "session/new",
        "params": {"cwd": "/tmp", "mcpServers": []},
    },
)


def has_session_id(msg):
    r = msg.get("result", {})
    if isinstance(r, dict) and "sessionId" in r:
        return r["sessionId"]
    return None


sid, _ = read_until(sock, has_session_id)
print(f"  -> session {sid}")

if not sid:
    print("FAIL: no session ID")
    sys.exit(1)

# Drain initial notifications
time.sleep(1)
read_until(sock, lambda m: None, timeout=1)

# Send prompt
prompt_text = "Hello! What is the capital of Japan? One word answer."
print(f"Sending prompt: {prompt_text}")
send(
    sock,
    {
        "jsonrpc": "2.0",
        "id": 3,
        "method": "session/prompt",
        "params": {"sessionId": sid, "prompt": [{"type": "text", "text": prompt_text}]},
    },
)


def has_stop(msg):
    r = msg.get("result", {})
    if isinstance(r, dict) and "stopReason" in r:
        return r["stopReason"]
    return None


# Collect until stop
buf = b""
deadline = time.time() + 30
got_stop = False
while time.time() < deadline and not got_stop:
    sock.settimeout(max(0.1, deadline - time.time()))
    try:
        data = sock.recv(8192)
        if not data:
            break
        buf += data
        while b"\n" in buf:
            line, buf = buf.split(b"\n", 1)
            msg = json.loads(line)
            method = msg.get("method", "")
            if method == "session/update":
                u = msg["params"]["update"]
                kind = u.get("sessionUpdate", "?")
                if kind in ("agent_message_chunk", "user_message_chunk"):
                    text = u.get("content", {}).get("text", "")
                    print(f"  [{kind}] {repr(text)}")
            elif "result" in msg:
                r = msg.get("result", {})
                if isinstance(r, dict) and r.get("stopReason"):
                    print(f"  [stop: {r['stopReason']}]")
                    got_stop = True
    except socket.timeout:
        continue

print("\nSession is live. Open http://localhost:9315/app in your browser.")
print("chrome-acp should show this conversation.")
sock.close()
