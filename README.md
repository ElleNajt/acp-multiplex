# acp-multiplex

Multiplexing proxy for [ACP](https://github.com/agentclientprotocol/agent-client-protocol) agents. Allows multiple frontends to share a single agent session.

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
              agent-shell  Toad  web   attach
              (Emacs)            app   client
```

The proxy rewrites JSON-RPC message IDs so multiple frontends can use overlapping ID spaces. Notifications (`session/update`) are broadcast to all connected frontends. Responses are routed back to the specific frontend that sent the request.

Late-joining clients receive a replay of the full session history (initialize response, session/new response, and all session/update notifications).

When a frontend sends `session/prompt`, the proxy synthesizes `user_message_chunk` notifications so other frontends see what was typed.

## Socket directory

All sockets live in `$TMPDIR/acp-multiplex/` (or `$XDG_RUNTIME_DIR/acp-multiplex/`), named by PID:

```
$TMPDIR/acp-multiplex/
  12345.sock
  67890.sock
```

Stale sockets from dead processes are cleaned up automatically on proxy startup. External apps can discover sessions by listing this directory and checking liveness with `kill -0 <pid>`.

## Usage

### Start the proxy with an agent

```bash
acp-multiplex claude-code-acp
```

Spawns the agent, creates a primary frontend on stdin/stdout, and listens on a Unix socket for additional frontends. The socket path is printed to stderr.

### Connect a second frontend (attach mode)

```bash
acp-multiplex attach $TMPDIR/acp-multiplex/12345.sock
```

Bridges stdin/stdout to the proxy's Unix socket. Use this to connect any stdio-based ACP client as a secondary frontend.

### agent-shell (Emacs)

Set the command in your Emacs config:

```elisp
(setq agent-shell-anthropic-claude-command '("acp-multiplex" "claude-code-acp"))
```

agent-shell talks ACP on stdin/stdout as usual — it doesn't know the multiplexer is there. The socket is available for other frontends to attach.

### Toad

```bash
# Primary frontend
toad acp acp-multiplex claude-code-acp

# Secondary frontend
toad acp "acp-multiplex attach $TMPDIR/acp-multiplex/12345.sock" .
```

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
| `main.go` | CLI entry point — proxy and attach modes |
| `proxy.go` | Core multiplexer: ID rewriting, fan-out, user message synthesis |
| `frontend.go` | Frontend abstraction for stdio and socket connections |
| `message.go` | JSON-RPC 2.0 envelope parsing and classification |
| `cache.go` | Session replay cache for late-joining clients |
