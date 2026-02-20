#!/usr/bin/env python3
"""Connect to the proxy socket in observe mode - just print everything received."""

import json
import socket
import sys
import time

sock_path = sys.argv[1]
sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.connect(sock_path)
print(f"Connected to {sock_path}, observing...\n")

buf = b""
while True:
    try:
        sock.settimeout(1)
        data = sock.recv(8192)
        if not data:
            print("[disconnected]")
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
                    text = u.get("content", {}).get("text", "") or u.get("text", "")
                    print(f"  [{kind}] {repr(text)}")
                elif kind == "agent_thought_chunk":
                    text = u.get("content", {}).get("text", "") or u.get("text", "")
                    print(f"  [thought] {repr(text)}")
                else:
                    print(f"  [{kind}]")
            elif "result" in msg:
                r = msg["result"]
                if isinstance(r, dict) and "stopReason" in r:
                    print(f"  [stop: {r['stopReason']}]")
                elif isinstance(r, dict) and "agentInfo" in r:
                    print(f"  [replay: init response]")
                elif isinstance(r, dict) and "sessionId" in r:
                    print(f"  [replay: session {r['sessionId'][:12]}...]")
                else:
                    print(f"  [response: {str(r)[:80]}]")
            else:
                print(f"  {json.dumps(msg)[:120]}")
    except socket.timeout:
        continue
    except KeyboardInterrupt:
        break

sock.close()
