#!/usr/bin/env python3
"""End-to-end test: two frontends connected to acp-multiplex, verifying fan-out.

Starts the proxy with claude-code-acp, connects two clients,
sends prompts from each, and verifies both see all notifications.
"""

import json
import os
import socket
import subprocess
import sys
import time


def send_line(f, msg):
    """Send a JSON-RPC message as ndjson."""
    line = json.dumps(msg) + "\n"
    f.write(line.encode())
    f.flush()


def read_line(f, timeout=30):
    """Read one ndjson line. Returns parsed JSON or None on timeout."""
    import select

    if hasattr(f, "fileno"):
        ready, _, _ = select.select([f], [], [], timeout)
        if not ready:
            return None
    line = f.readline()
    if not line:
        return None
    return json.loads(line)


def read_socket_line(sock, buf, timeout=30):
    """Read one ndjson line from a socket. Returns (parsed_msg, remaining_buf)."""
    deadline = time.time() + timeout
    while b"\n" not in buf:
        remaining = deadline - time.time()
        if remaining <= 0:
            return None, buf
        sock.settimeout(remaining)
        try:
            data = sock.recv(4096)
        except socket.timeout:
            return None, buf
        if not data:
            return None, buf
        buf += data
    line, buf = buf.split(b"\n", 1)
    return json.loads(line), buf


def collect_socket_messages(sock, buf, timeout=5, max_msgs=50):
    """Collect all available messages from socket within timeout."""
    msgs = []
    while len(msgs) < max_msgs:
        msg, buf = read_socket_line(sock, buf, timeout=timeout)
        if msg is None:
            break
        msgs.append(msg)
        timeout = 2  # shorter timeout for subsequent messages
    return msgs, buf


def main():
    print("=== ACP Multiplex E2E Test ===\n")

    # Build first
    print("Building...")
    subprocess.check_call(["go", "build", "-o", "acp-multiplex", "."])

    # Start proxy
    print("Starting proxy with claude-code-acp...")
    proc = subprocess.Popen(
        ["./acp-multiplex", "claude-code-acp"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    # Wait for socket path from stderr
    sock_path = None
    deadline = time.time() + 10
    stderr_lines = []
    while time.time() < deadline:
        line = proc.stderr.readline().decode().strip()
        stderr_lines.append(line)
        if "socket" in line:
            sock_path = line.split("socket ")[-1]
            break

    if not sock_path:
        print(f"FAIL: no socket path in stderr: {stderr_lines}")
        proc.kill()
        sys.exit(1)

    print(f"Socket: {sock_path}")

    # --- Client A (primary, via stdin/stdout) ---
    print("\n--- Client A (primary) ---")

    # Initialize
    send_line(
        proc.stdin,
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": 1,
                "clientInfo": {"name": "client-a", "version": "0.1"},
            },
        },
    )
    resp = read_line(proc.stdout)
    if resp is None or "result" not in resp:
        print(f"FAIL: bad initialize response: {resp}")
        proc.kill()
        sys.exit(1)
    agent_name = resp["result"].get("agentInfo", {}).get("name", "?")
    print(f"  initialize OK: {agent_name}")
    assert resp["id"] == 1, f"expected id=1, got {resp['id']}"

    # Session new
    send_line(
        proc.stdin,
        {
            "jsonrpc": "2.0",
            "id": 2,
            "method": "session/new",
            "params": {"cwd": "/tmp", "mcpServers": []},
        },
    )
    resp = read_line(proc.stdout)
    if resp is None or "result" not in resp:
        print(f"FAIL: bad session/new response: {resp}")
        proc.kill()
        sys.exit(1)
    session_id = resp["result"].get("sessionId")
    print(f"  session/new OK: {session_id}")
    assert resp["id"] == 2, f"expected id=2, got {resp['id']}"
    assert session_id, "no sessionId"

    # There may be initial session/update notifications (available_commands, etc.)
    # Drain them before connecting client B
    time.sleep(1)

    # --- Client B (secondary, via Unix socket) ---
    print("\n--- Client B (secondary, via socket) ---")
    sock_b = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock_b.connect(sock_path)
    buf_b = b""

    # Client B should get replayed messages (init response + session/new response + any updates)
    print("  Waiting for replay...")
    time.sleep(1)
    replay_msgs, buf_b = collect_socket_messages(sock_b, buf_b, timeout=3)
    print(f"  Got {len(replay_msgs)} replayed messages:")
    for i, m in enumerate(replay_msgs):
        method = m.get("method", "")
        if "result" in m:
            result_keys = (
                list(m["result"].keys())
                if isinstance(m["result"], dict)
                else str(m["result"])
            )
            print(f"    [{i}] response: {result_keys}")
        elif method:
            update_kind = ""
            if method == "session/update":
                update_kind = (
                    m.get("params", {}).get("update", {}).get("sessionUpdate", "?")
                )
            print(f"    [{i}] {method}: {update_kind}")
        else:
            print(f"    [{i}] {json.dumps(m)[:80]}")

    assert len(replay_msgs) >= 2, (
        f"expected at least 2 replay messages (init + session/new), got {len(replay_msgs)}"
    )
    print("  Replay OK")

    # --- Send prompt from Client A, verify both see updates ---
    print("\n--- Prompt from Client A ---")
    send_line(
        proc.stdin,
        {
            "jsonrpc": "2.0",
            "id": 3,
            "method": "session/prompt",
            "params": {
                "sessionId": session_id,
                "prompt": [
                    {"type": "text", "text": "Say exactly: HELLO_FROM_TEST_123"}
                ],
            },
        },
    )

    # Collect messages on both sides
    print("  Waiting for agent response...")

    # Client A messages (from stdout)
    a_msgs = []
    a_got_stop = False
    deadline = time.time() + 60
    while time.time() < deadline and not a_got_stop:
        msg = read_line(proc.stdout, timeout=5)
        if msg is None:
            continue
        a_msgs.append(msg)
        # Check for stop
        if (
            "result" in msg
            and isinstance(msg.get("result"), dict)
            and msg["result"].get("stopReason")
        ):
            a_got_stop = True

    # Client B messages (from socket)
    b_msgs, buf_b = collect_socket_messages(sock_b, buf_b, timeout=5)

    a_updates = [m for m in a_msgs if m.get("method") == "session/update"]
    b_updates = [m for m in b_msgs if m.get("method") == "session/update"]

    print(
        f"  Client A: {len(a_msgs)} msgs total, {len(a_updates)} session/updates, stop={a_got_stop}"
    )
    print(f"  Client B: {len(b_msgs)} msgs total, {len(b_updates)} session/updates")

    # Both should have gotten session/update notifications
    assert len(a_updates) > 0, "Client A got no session/update notifications"
    assert len(b_updates) > 0, "Client B got no session/update notifications"

    # Client A should have gotten the prompt response (with stop reason)
    assert a_got_stop, "Client A never got stopReason response"

    # Verify the prompt response kept Client A's original ID
    stop_msgs = [
        m
        for m in a_msgs
        if "result" in m
        and isinstance(m.get("result"), dict)
        and m["result"].get("stopReason")
    ]
    if stop_msgs:
        assert stop_msgs[0]["id"] == 3, (
            f"expected id=3 on prompt response, got {stop_msgs[0].get('id')}"
        )
        print("  ID rewriting OK: prompt response has id=3")

    print(
        f"\n  Fan-out verified: both clients received {len(a_updates)}/{len(b_updates)} update notifications"
    )

    # --- Send prompt from Client B, verify both see updates ---
    print("\n--- Prompt from Client B (secondary) ---")
    b_prompt = {
        "jsonrpc": "2.0",
        "id": 100,
        "method": "session/prompt",
        "params": {
            "sessionId": session_id,
            "prompt": [{"type": "text", "text": "Say exactly: HELLO_FROM_CLIENT_B"}],
        },
    }
    sock_b.sendall((json.dumps(b_prompt) + "\n").encode())

    # Collect on both sides
    print("  Waiting for agent response...")

    # Client B: collect until we see a response with stopReason
    b_got_stop = False
    b_msgs2 = []
    deadline = time.time() + 60
    while time.time() < deadline and not b_got_stop:
        msg, buf_b = read_socket_line(sock_b, buf_b, timeout=5)
        if msg is None:
            continue
        b_msgs2.append(msg)
        if (
            "result" in msg
            and isinstance(msg.get("result"), dict)
            and msg["result"].get("stopReason")
        ):
            b_got_stop = True

    # Client A: drain what it got
    a_msgs2 = []
    a_deadline = time.time() + 3
    while time.time() < a_deadline:
        msg = read_line(proc.stdout, timeout=2)
        if msg is None:
            break
        a_msgs2.append(msg)

    a_updates2 = [m for m in a_msgs2 if m.get("method") == "session/update"]
    b_updates2 = [m for m in b_msgs2 if m.get("method") == "session/update"]

    print(f"  Client A: {len(a_msgs2)} msgs, {len(a_updates2)} session/updates")
    print(
        f"  Client B: {len(b_msgs2)} msgs, {len(b_updates2)} session/updates, stop={b_got_stop}"
    )

    assert len(b_updates2) > 0, (
        "Client B got no session/update notifications for its own prompt"
    )
    assert b_got_stop, "Client B never got stopReason for its prompt"
    assert len(a_updates2) > 0, "Client A got no session/updates from Client B's prompt"

    # Verify ID rewriting for Client B
    b_stop = [
        m
        for m in b_msgs2
        if "result" in m
        and isinstance(m.get("result"), dict)
        and m["result"].get("stopReason")
    ]
    if b_stop:
        assert b_stop[0]["id"] == 100, (
            f"expected id=100 on B's prompt response, got {b_stop[0].get('id')}"
        )
        print("  ID rewriting OK: Client B's prompt response has id=100")

    print(
        f"\n  Fan-out verified: A got {len(a_updates2)} updates, B got {len(b_updates2)} updates"
    )

    # --- Cleanup ---
    print("\n=== ALL TESTS PASSED ===")
    sock_b.close()
    proc.stdin.close()
    proc.terminate()
    proc.wait(timeout=5)


if __name__ == "__main__":
    main()
