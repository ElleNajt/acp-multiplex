# acp-multiplex

Multiplexing proxy for [ACP](https://github.com/agentclientprotocol/agent-client-protocol) agents. Allows multiple frontends (Toad, chrome-acp, custom UIs) to share a single agent session.

## How it works

```
                    ACP Agent (e.g. claude-code-acp)
                         stdin/stdout
                              |
                     ┌────────┴────────┐
                     │  acp-multiplex  │
                     │                 │
                     │  - ID rewriting │
                     │  - Fan-out      │
                     │  - Replay cache │
                     └──┬─────────┬────┘
                        │         │
               stdin/stdout    Unix socket
               (primary)       (secondary frontends)
                  │               │
                  │         ┌─────┼─────┐
                  │         │     │     │
                Toad      Toad  chrome  attach
                (TUI)     (web)  -acp   client
```

The proxy rewrites JSON-RPC message IDs so multiple frontends can use overlapping ID spaces. Notifications (`session/update`) are broadcast to all connected frontends. Responses are routed back to the specific frontend that sent the request.

Late-joining clients receive a replay of the full session history (initialize response, session/new response, and all session/update notifications).

When a frontend sends `session/prompt`, the proxy synthesizes `user_message_chunk` notifications so other frontends see what was typed.

## Usage

### Start the proxy with an agent

```bash
acp-multiplex claude-code-acp
```

This spawns the agent, creates a primary frontend on stdin/stdout, and listens on a Unix socket for additional frontends. The socket path is printed to stderr.

### Connect a second frontend (attach mode)

```bash
acp-multiplex attach /path/to/socket.sock
```

Bridges stdin/stdout to the proxy's Unix socket. Use this to connect any stdio-based ACP client as a secondary frontend.

### Connect Toad

```bash
# Primary frontend
toad acp acp-multiplex claude-code-acp

# Secondary frontend (via attach)
toad acp /path/to/attach-script.sh /tmp
```

### Connect chrome-acp

```bash
acp-proxy --no-auth /path/to/attach-script.sh
# Open http://localhost:9315/app
```

Where the attach script is:
```bash
#!/bin/bash
exec acp-multiplex attach /path/to/socket.sock
```

### Web UI

```bash
acp-multiplex web /path/to/socket.sock [port]
```

Serves a minimal chat UI with WebSocket bridge on the given port (default 8080).

## Building

```bash
go build -o acp-multiplex .
```

## Testing

```bash
# Unit tests (mock agent)
go test -v -run TestProxy

# End-to-end test (requires claude-code-acp in PATH)
python3 test_e2e.py

# Interactive demo with two clients
python3 demo_multiplex.py
```

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | CLI entry point — proxy, attach, and web modes |
| `proxy.go` | Core multiplexer: ID rewriting, fan-out, user message synthesis |
| `frontend.go` | Frontend abstraction for stdio and socket connections |
| `message.go` | JSON-RPC 2.0 envelope parsing and classification |
| `cache.go` | Session replay cache for late-joining clients |
| `web.go` | HTTP/WebSocket bridge and embedded web UI |
