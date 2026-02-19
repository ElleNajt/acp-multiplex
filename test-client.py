#!/usr/bin/env python3
"""Minimal ACP client that connects to acp-multiplex via Unix socket.

Usage:
    # First, start the proxy:
    #   ./acp-multiplex claude --acp
    # Note the socket path printed to stderr.
    #
    # Then connect:
    #   python3 test-client.py /path/to/socket.sock

Sends initialize + session/new, then enters an interactive loop
where you type prompts and see session/update notifications.
"""

import json
import socket
import sys
import threading


def read_loop(sock):
    """Read ndjson from socket and print each message."""
    buf = b""
    while True:
        data = sock.recv(4096)
        if not data:
            print("\n[disconnected]")
            break
        buf += data
        while b"\n" in buf:
            line, buf = buf.split(b"\n", 1)
            msg = json.loads(line)
            method = msg.get("method", "")
            if method == "session/update":
                update = msg["params"]["update"]
                kind = update.get("sessionUpdate", "")
                if kind in ("agent_message_chunk", "user_message_chunk"):
                    text = update.get("text", "")
                    print(text, end="", flush=True)
                elif kind == "agent_thought_chunk":
                    text = update.get("text", "")
                    print(f"[thought] {text}", end="", flush=True)
                elif kind == "tool_call":
                    print(f"\n[tool: {update.get('toolName', '?')}]")
                elif kind == "tool_call_update":
                    status = update.get("status", "")
                    print(f"[tool {status}]")
                else:
                    print(f"\n[update: {kind}]")
            elif "result" in msg:
                result = msg["result"]
                if "sessionId" in result:
                    print(f"[session: {result['sessionId']}]")
                elif "stopReason" in result:
                    print(f"\n[stop: {result['stopReason']}]")
                elif "agentInfo" in result:
                    info = result["agentInfo"]
                    print(
                        f"[connected: {info.get('name', '?')} {info.get('version', '')}]"
                    )
                else:
                    print(f"[response: {json.dumps(result)[:100]}]")
            elif "error" in msg:
                print(f"[error: {msg['error']}]")
            else:
                # Reverse call from agent — just print it
                print(f"\n[agent request: {method}]")


def send(sock, msg):
    sock.sendall((json.dumps(msg) + "\n").encode())


def main():
    if len(sys.argv) < 2:
        print("usage: test-client.py <socket-path> [--observe]")
        print("  --observe: skip init/session, just watch notifications")
        sys.exit(1)

    sock_path = sys.argv[1]
    observe = "--observe" in sys.argv

    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.connect(sock_path)
    print(f"connected to {sock_path}")

    # Start reader thread
    t = threading.Thread(target=read_loop, args=(sock,), daemon=True)
    t.start()

    if not observe:
        # Initialize
        send(
            sock,
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": 1,
                    "clientInfo": {"name": "test-client", "version": "0.1"},
                },
            },
        )

        # Wait a beat for response
        import time

        time.sleep(0.5)

        # Session new
        send(
            sock,
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "session/new",
                "params": {"cwd": "/tmp", "mcpServers": []},
            },
        )
        time.sleep(0.5)

    # Interactive prompt loop
    next_id = 10
    print("\nType a prompt (or 'quit' to exit):")
    while True:
        try:
            text = input("> ")
        except (EOFError, KeyboardInterrupt):
            break
        if text.strip().lower() == "quit":
            break
        if not text.strip():
            continue

        send(
            sock,
            {
                "jsonrpc": "2.0",
                "id": next_id,
                "method": "session/prompt",
                "params": {
                    "sessionId": "test-session-1",
                    "prompt": [{"type": "text", "text": text}],
                },
            },
        )
        next_id += 1

    sock.close()


if __name__ == "__main__":
    main()
