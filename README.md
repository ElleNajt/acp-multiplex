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
               (primary)       (secondary)
                  │               │
              agent-shell     acp-mobile
              or Toad
```

A primary frontend ([agent-shell](https://github.com/xenodium/agent-shell), Toad, etc.) spawns the proxy and talks to it on stdin/stdout. Secondary frontends like [acp-mobile](https://github.com/ElleNajt/acp-mobile) connect via Unix socket and get a full replay of the session history.

The proxy rewrites JSON-RPC message IDs so multiple frontends can use overlapping ID spaces. Notifications are broadcast to all frontends. Responses are routed back to the specific frontend that sent the request.

When a frontend sends `session/prompt`, the proxy synthesizes `user_message_chunk` notifications so other frontends see what was typed.

## Socket directory

All sockets live in `$TMPDIR/acp-multiplex/`, named by PID:

```
$TMPDIR/acp-multiplex/
  12345.sock
  67890.sock
```

Stale sockets from dead processes are cleaned up on proxy startup. Secondary frontends discover sessions by listing this directory and checking liveness with `kill -0 <pid>`.

## Usage

### Primary frontends

#### agent-shell (Emacs)

```elisp
(setq agent-shell-anthropic-claude-command '("acp-multiplex" "claude-code-acp"))
```

agent-shell talks ACP on stdin/stdout as usual — it doesn't know the multiplexer is there.

#### Toad

```bash
toad acp acp-multiplex claude-code-acp
```

#### Standalone

```bash
acp-multiplex claude-code-acp
```

### Secondary frontends

#### acp-mobile

[acp-mobile](https://github.com/ElleNajt/acp-mobile) discovers sockets in the socket directory and connects via WebSocket bridge.

#### Attach (any stdio ACP client)

```bash
acp-multiplex attach $TMPDIR/acp-multiplex/12345.sock
```

Bridges stdin/stdout to an existing proxy's Unix socket.

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
```

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | CLI entry point — proxy and attach modes |
| `proxy.go` | Core multiplexer: ID rewriting, fan-out, user message synthesis |
| `frontend.go` | Frontend abstraction for stdio and socket connections |
| `message.go` | JSON-RPC 2.0 envelope parsing and classification |
| `cache.go` | Session replay cache (coalesces streaming chunks) |
