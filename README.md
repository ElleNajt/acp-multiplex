# acp-multiplex

> **Work in Progress** — Functional and in daily use (multiplexing [agent-shell](https://github.com/xenodium/agent-shell) and [acp-mobile](https://github.com/ElleNajt/acp-mobile)), but still encountering edge cases and actively debugging.

Multiplexing proxy for [ACP](https://github.com/agentclientprotocol/agent-client-protocol) agents.

ACP is the protocol between a frontend and an AI agent, using JSON-RPC over stdin/stdout. It's 1:1 — one frontend, one agent. acp-multiplex makes it 1:N, so multiple frontends can share a single agent session.

## How it works

```
            Primary frontend
               stdin/stdout
                    |
              acp-multiplex  ←→  agent (e.g. claude-code-acp)
                    |
                Unix socket
                    |
            Secondary frontend(s)
```

1. The primary frontend starts `acp-multiplex <agent-command>`. The proxy spawns the agent as a subprocess and creates a Unix socket at `$TMPDIR/acp-multiplex/<pid>.sock`.

2. The primary frontend talks to the proxy on stdin/stdout. It has no idea the proxy is there — it thinks it's talking directly to the agent.

3. When the primary sends a request (say `session/prompt` with id 1), the proxy rewrites the id to a unique internal id (say 7), remembers "id 7 came from the primary, originally id 1", and forwards it to the agent.

4. When the agent responds with id 7, the proxy looks up the mapping, rewrites the id back to 1, and sends it only to the primary.

   **Example:**
   ```
   Primary sends:     {"jsonrpc":"2.0","id":1,"method":"session/prompt",...}
   Proxy forwards:    {"jsonrpc":"2.0","id":7,"method":"session/prompt",...}
   Agent responds:    {"jsonrpc":"2.0","id":7,"result":{"message_id":"msg_123"}}
   Proxy sends back:  {"jsonrpc":"2.0","id":1,"result":{"message_id":"msg_123"}}
   ```
   
   The proxy maintains a mapping: `{7: {frontend: primary, originalID: 1}}`. When multiple frontends send requests, each gets a unique internal ID so the agent never sees conflicting IDs.

5. When the agent sends notifications (streaming text, tool calls, etc.), the proxy broadcasts them to all connected frontends and stores them in a cache.

6. When a secondary frontend connects to the Unix socket, it gets a replay of the cached history — the initialize response, session/new response, and all notifications (with streaming chunks coalesced into complete messages so replay is fast). Then it receives live updates.

7. Secondary frontends can also send prompts. The proxy rewrites their ids the same way, and synthesizes `user_message_chunk` notifications so the primary sees what was typed from the secondary.

## Socket directory

All sockets live in `$TMPDIR/acp-multiplex/`, named by PID:

```
$TMPDIR/acp-multiplex/
  12345.sock
  67890.sock
```

Stale sockets from dead processes are cleaned up on proxy startup. Secondary frontends discover sessions by listing this directory and checking liveness with `kill -0 <pid>`.

## Usage

### Primary frontend

Any ACP client that talks stdio can be a primary frontend — just prefix the agent command with `acp-multiplex`:

```bash
acp-multiplex claude-code-acp
```

The primary frontend talks to the proxy on stdin/stdout as if it were the agent directly. Examples: [agent-shell](https://github.com/xenodium/agent-shell) (Emacs), [Zed](https://zed.dev/), [Toad](https://github.com/lukesmurray/toad).

### Secondary frontends

Secondary frontends connect to the Unix socket. Any program that speaks ndjson over a Unix socket can connect.

**Attach mode** bridges stdin/stdout to an existing proxy's socket, so any stdio ACP client can join as a secondary:

```bash
acp-multiplex attach $TMPDIR/acp-multiplex/12345.sock
```

[acp-mobile](https://github.com/ElleNajt/acp-mobile) is a web-based secondary frontend that discovers sockets and bridges them to WebSocket for the browser.

### Connecting other ACP clients

Any ACP client that can spawn an agent command can attach to an existing session. Instead of spawning the agent directly, configure the client to spawn `acp-multiplex attach <socket>`.

#### Discovering sessions

List active sessions by scanning the socket directory:

```bash
ls $TMPDIR/acp-multiplex/*.sock 2>/dev/null
# or with XDG_RUNTIME_DIR:
ls ${XDG_RUNTIME_DIR:-$TMPDIR}/acp-multiplex/*.sock
```

Check if a session is still alive:

```bash
# Extract PID from socket name and check liveness
for sock in $TMPDIR/acp-multiplex/*.sock; do
  pid=$(basename "$sock" .sock)
  kill -0 "$pid" 2>/dev/null && echo "$sock (alive)" || echo "$sock (stale)"
done
```

#### Mitto

Configure an agent entry that attaches to an existing session:

```bash
mitto --agent-command "acp-multiplex attach $TMPDIR/acp-multiplex/12345.sock"
```

#### ACP UI / VS Code ACP Client

In the agent configuration, set the command to attach mode:

```json
{
  "command": "acp-multiplex",
  "args": ["attach", "/tmp/acp-multiplex/12345.sock"]
}
```

#### Neovim (CodeCompanion / avante.nvim / agentic.nvim)

Configure the agent command in your neovim plugin config to use attach mode. For example with CodeCompanion:

```lua
require("codecompanion").setup({
  adapters = {
    acp = {
      command = "acp-multiplex",
      args = { "attach", vim.fn.expand("$TMPDIR/acp-multiplex/12345.sock") },
    },
  },
})
```

#### Any stdio ACP client

The general pattern: wherever the client expects an agent command, use:

```bash
acp-multiplex attach /path/to/socket.sock
```

The attach process speaks standard ACP (JSON-RPC over ndjson on stdin/stdout) and exits when the socket closes. It receives the full session replay on connect, then live updates.

## Building

```bash
go build -o acp-multiplex .
```

## Testing

```bash
# Unit tests (mock agent)
go test -v -run TestProxy

# End-to-end test (requires claude-code-acp in PATH)
python3 scripts/test_e2e.py
```

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | CLI entry point — proxy and attach modes |
| `proxy.go` | Core multiplexer: ID rewriting, fan-out, user message synthesis |
| `frontend.go` | Frontend abstraction for stdio and socket connections |
| `message.go` | JSON-RPC 2.0 envelope parsing and classification |
| `cache.go` | Session replay cache (coalesces streaming chunks) |
