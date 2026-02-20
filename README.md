# acp-multiplex

Multiplexing proxy for [ACP](https://github.com/agentclientprotocol/agent-client-protocol) agents.

ACP is the protocol between a frontend and an AI agent, using JSON-RPC over stdin/stdout. It's 1:1 — one frontend, one agent. acp-multiplex makes it 1:N, so multiple frontends can share a single agent session.

The main use case: you run Claude agents via [agent-shell](https://github.com/xenodium/agent-shell) in Emacs, and you also want to see and interact with those sessions from your phone via [acp-mobile](https://github.com/ElleNajt/acp-mobile).

## How it works

```
              Emacs (agent-shell)
                  stdin/stdout
                       |
                 acp-multiplex  ←→  claude-code-acp
                       |
                   Unix socket
                       |
                   acp-mobile (phone)
```

1. Emacs starts `acp-multiplex claude-code-acp`. The proxy spawns the agent as a subprocess and creates a Unix socket at `$TMPDIR/acp-multiplex/<pid>.sock`.

2. Emacs talks to the proxy on stdin/stdout. It has no idea the proxy is there — it thinks it's talking directly to the agent.

3. When Emacs sends a request (say `session/prompt` with id 1), the proxy rewrites the id to a unique internal id (say 7), remembers "id 7 came from Emacs, originally id 1", and forwards it to the agent.

4. When the agent responds with id 7, the proxy looks up the mapping, rewrites the id back to 1, and sends it only to Emacs.

5. When the agent sends notifications (streaming text, tool calls, etc.), the proxy broadcasts them to all connected frontends and stores them in a cache.

6. When acp-mobile connects to the Unix socket, it gets a replay of the cached history — the initialize response, session/new response, and all notifications (with streaming chunks coalesced into complete messages so replay is fast). Then it receives live updates.

7. acp-mobile can also send prompts. The proxy rewrites its ids the same way, and synthesizes `user_message_chunk` notifications so Emacs sees what was typed from the phone.

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
