# Debugging Notes

## Hanging Sessions with Multiple Frontends (2026-02-20)

### Problem

When multiple acp-multiplex processes were running and acp-mobile was discovering/connecting to their sockets, new agent sessions would hang and become unresponsive.

### Symptoms

- New agent-shell or Toad sessions hang on startup
- Log files show "broken pipe" errors for disconnected frontends
- Sessions only work after killing all old acp-multiplex processes

### Root Cause (Theory)

1. **Old sessions accumulate**: Multiple acp-multiplex processes running from previous agent-shell sessions
2. **acp-mobile connects**: Web UI discovers sockets, connects to them (creating secondary frontends like "frontend 16")
3. **Frontends disconnect**: Browser tabs close, network issues, etc. → broken pipe errors
4. **Dead frontends linger**: Disconnected frontends remain in proxy's frontend list
5. **Reverse calls broadcast to dead frontends**: When agent sends reverse call (fs/readFile, etc.), it broadcasts to ALL frontends including dead ones
6. **No response from dead frontend**: The "first response wins" logic may wait for responses that never come
7. **Session hangs**: Everything blocks waiting for the missing response

### Evidence

- Log showed `frontend 16 write error: broken pipe` (repeated)
- Frontend 16 is a secondary frontend (primary is always 0)
- Killing all processes fixed the issue immediately
- acp-mobile was running and discovering sockets

### Proposed Fixes

1. **Auto-cleanup broken frontends**: When a frontend gets broken pipe, remove it from the proxy's frontend list
2. **Timeout reverse calls**: Add 30s timeout to reverse call responses, fail gracefully instead of hanging forever
3. **Frontend health tracking**: Mark frontends as healthy/broken, skip broken ones in broadcasts

### Related Code

- `proxy.go`: `routeReverseCall()` - broadcasts to all frontends
- `frontend.go`: `Frontend.Send()` - logs errors but doesn't remove frontend
- `proxy.go`: `pendingReverse` map - tracks pending reverse call responses
