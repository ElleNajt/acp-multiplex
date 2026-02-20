#!/usr/bin/env python3
"""Live demo: two ACP clients multiplexed through acp-multiplex."""

import json
import os
import select
import socket
import subprocess
import sys
import time


def send(f, msg):
    f.write((json.dumps(msg) + "\n").encode())
    f.flush()


def read_stdout(f, timeout=30):
    ready, _, _ = select.select([f], [], [], timeout)
    if not ready:
        return None
    line = f.readline()
    return json.loads(line) if line else None


def sock_read(s, buf, timeout=10):
    deadline = time.time() + timeout
    while b"\n" not in buf:
        rem = deadline - time.time()
        if rem <= 0:
            return None, buf
        s.settimeout(rem)
        try:
            data = s.recv(8192)
        except socket.timeout:
            return None, buf
        if not data:
            return None, buf
        buf += data
    line, buf = buf.split(b"\n", 1)
    return json.loads(line), buf


def drain_stdout(f, timeout=2):
    msgs = []
    while True:
        ready, _, _ = select.select([f], [], [], timeout)
        if not ready:
            break
        line = f.readline()
        if not line:
            break
        msgs.append(json.loads(line))
        timeout = 0.5
    return msgs


def drain_socket(s, buf, timeout=2):
    msgs = []
    while True:
        msg, buf = sock_read(s, buf, timeout=timeout)
        if msg is None:
            break
        msgs.append(msg)
        timeout = 0.5
    return msgs, buf


def describe(msg):
    mid = msg.get("id", "")
    id_str = f" [id={mid}]" if mid != "" else ""

    if "method" in msg:
        m = msg["method"]
        if m == "session/update":
            u = msg.get("params", {}).get("update", {})
            kind = u.get("sessionUpdate", "?")
            if kind in ("agent_message_chunk", "user_message_chunk"):
                text = u.get("content", {}).get("text", "") or u.get("text", "")
                return f"{kind}: {repr(text[:100])}"
            return kind
        return f"notification: {m}"
    elif "result" in msg:
        r = msg["result"]
        if isinstance(r, dict):
            if "stopReason" in r:
                return f"response{id_str}: stopReason={r['stopReason']}"
            if "sessionId" in r:
                return f"response{id_str}: sessionId={r['sessionId'][:12]}..."
            if "agentInfo" in r:
                return f"response{id_str}: agent={r['agentInfo'].get('name', '?')}"
        return f"response{id_str}: {str(r)[:60]}"
    elif "error" in msg:
        return f"error: {msg['error']}"
    return json.dumps(msg)[:80]


def main():
    # Start proxy
    proc = subprocess.Popen(
        ["./acp-multiplex", "claude-code-acp"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    # Get socket path
    sock_path = None
    for _ in range(20):
        line = proc.stderr.readline().decode().strip()
        if "socket" in line:
            sock_path = line.split("socket ")[-1]
            break
    assert sock_path, "no socket"

    print("=" * 60)
    print("ACP MULTIPLEX LIVE DEMO")
    print("=" * 60)
    print(f"Socket: {sock_path}")

    # --- Client A: initialize + session/new ---
    print("\n[CLIENT A] Sending initialize...")
    send(
        proc.stdin,
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": 1,
                "clientInfo": {"name": "client-A", "version": "0.1"},
            },
        },
    )
    resp = read_stdout(proc.stdout)
    print(f"[CLIENT A]   <- {describe(resp)}")

    print("[CLIENT A] Sending session/new...")
    send(
        proc.stdin,
        {
            "jsonrpc": "2.0",
            "id": 2,
            "method": "session/new",
            "params": {"cwd": "/tmp", "mcpServers": []},
        },
    )
    resp = read_stdout(proc.stdout)
    print(f"[CLIENT A]   <- {describe(resp)}")
    session_id = resp["result"]["sessionId"]

    # drain initial notifications
    time.sleep(1)
    initial = drain_stdout(proc.stdout, timeout=1)
    if initial:
        print(f"[CLIENT A]   (drained {len(initial)} initial notifications)")

    # --- Connect Client B ---
    print("\n" + "-" * 60)
    print("[CLIENT B] Connecting to Unix socket...")
    sock_b = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock_b.connect(sock_path)
    time.sleep(1.5)

    buf_b = b""
    replay, buf_b = drain_socket(sock_b, buf_b, timeout=3)
    print(f"[CLIENT B]   <- {len(replay)} replayed messages on connect:")
    for m in replay:
        print(f"[CLIENT B]      {describe(m)}")

    # === Prompt from Client A ===
    print("\n" + "=" * 60)
    prompt_a = "What is 2+2? Reply in exactly one word."
    print(f'[CLIENT A] Sending prompt: "{prompt_a}"')
    print("=" * 60)
    send(
        proc.stdin,
        {
            "jsonrpc": "2.0",
            "id": 3,
            "method": "session/prompt",
            "params": {
                "sessionId": session_id,
                "prompt": [{"type": "text", "text": prompt_a}],
            },
        },
    )

    a_msgs = []
    got_stop = False
    deadline = time.time() + 60
    while time.time() < deadline and not got_stop:
        msg = read_stdout(proc.stdout, timeout=5)
        if msg is None:
            continue
        a_msgs.append(msg)
        if (
            "result" in msg
            and isinstance(msg.get("result"), dict)
            and msg["result"].get("stopReason")
        ):
            got_stop = True

    time.sleep(1)
    b_msgs, buf_b = drain_socket(sock_b, buf_b, timeout=3)

    print(f"\n[CLIENT A] received {len(a_msgs)} messages:")
    for m in a_msgs:
        print(f"[CLIENT A]   <- {describe(m)}")

    print(f"\n[CLIENT B] received {len(b_msgs)} messages (fan-out):")
    for m in b_msgs:
        print(f"[CLIENT B]   <- {describe(m)}")

    # === Prompt from Client B ===
    print("\n" + "=" * 60)
    prompt_b = "What is 3*7? Reply in exactly one word."
    print(f'[CLIENT B] Sending prompt: "{prompt_b}"')
    print("=" * 60)
    sock_b.sendall(
        (
            json.dumps(
                {
                    "jsonrpc": "2.0",
                    "id": 50,
                    "method": "session/prompt",
                    "params": {
                        "sessionId": session_id,
                        "prompt": [{"type": "text", "text": prompt_b}],
                    },
                }
            )
            + "\n"
        ).encode()
    )

    b_msgs2 = []
    got_stop_b = False
    deadline = time.time() + 60
    while time.time() < deadline and not got_stop_b:
        msg, buf_b = sock_read(sock_b, buf_b, timeout=5)
        if msg is None:
            continue
        b_msgs2.append(msg)
        if (
            "result" in msg
            and isinstance(msg.get("result"), dict)
            and msg["result"].get("stopReason")
        ):
            got_stop_b = True

    time.sleep(1)
    a_msgs2 = drain_stdout(proc.stdout, timeout=3)

    print(f"\n[CLIENT B] received {len(b_msgs2)} messages:")
    for m in b_msgs2:
        print(f"[CLIENT B]   <- {describe(m)}")

    print(f"\n[CLIENT A] received {len(a_msgs2)} messages (fan-out):")
    for m in a_msgs2:
        print(f"[CLIENT A]   <- {describe(m)}")

    # Summary
    print("\n" + "=" * 60)
    print("RESULTS")
    print("=" * 60)
    print(
        f"  Replay:       Client B got {len(replay)} msgs on connect (init + session + notifications)"
    )
    print(
        f"  Prompt 1 (A): Client A got {len(a_msgs)} msgs, Client B got {len(b_msgs)} msgs"
    )
    print(
        f"  Prompt 2 (B): Client B got {len(b_msgs2)} msgs, Client A got {len(a_msgs2)} msgs"
    )
    a_stop_id = next(
        (
            m["id"]
            for m in a_msgs
            if isinstance(m.get("result"), dict) and m["result"].get("stopReason")
        ),
        "?",
    )
    b_stop_id = next(
        (
            m["id"]
            for m in b_msgs2
            if isinstance(m.get("result"), dict) and m["result"].get("stopReason")
        ),
        "?",
    )
    print(
        f"  ID rewrite:   A's response id={a_stop_id} (expected 3), B's response id={b_stop_id} (expected 50)"
    )
    ok = len(b_msgs) > 0 and len(a_msgs2) > 0
    print(f"  Fan-out:      {'WORKING' if ok else 'FAILED'}")

    proc.stdin.close()
    proc.terminate()
    proc.wait(timeout=5)


if __name__ == "__main__":
    main()
