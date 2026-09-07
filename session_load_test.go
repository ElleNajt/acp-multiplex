package main

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func TestLoadedSessionAvailableToLateJoiner(t *testing.T) {
	for _, result := range []string{`{}`, `null`, `{"modes":{"currentModeId":"plan"}}`} {
		t.Run(result, func(t *testing.T) {
			cache := NewCache()
			proxy := NewProxy(io.Discard, bytes.NewReader(nil), cache)
			var primary bytes.Buffer
			frontend := &Frontend{writer: &primary}
			proxy.pending.Store(int64(5), &PendingRequest{
				frontend: frontend, originalID: json.RawMessage(`42`), method: "session/load",
				params: json.RawMessage(`{"sessionId":"existing-session","cwd":"/Users/test/project"}`),
			})
			// ACP replays history before returning the load response.
			cache.AddUpdate([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"existing-session","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Earlier prompt"}}}}`))
			line := []byte(`{"jsonrpc":"2.0","id":5,"result":` + result + `}`)
			env, err := parseEnvelope(line)
			if err != nil {
				t.Fatal(err)
			}
			proxy.routeResponseToFrontend(env, line)
			var reply Envelope
			if err := json.Unmarshal(primary.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			if string(reply.Result) != result || string(*reply.ID) != "42" {
				t.Fatalf("primary response changed: %s", primary.Bytes())
			}
			var replay bytes.Buffer
			cache.Replay(&Frontend{writer: &replay})
			decoder := json.NewDecoder(&replay)
			var session struct {
				Result map[string]json.RawMessage `json:"result"`
			}
			if err := decoder.Decode(&session); err != nil {
				t.Fatal(err)
			}
			if string(session.Result["sessionId"]) != `"existing-session"` || string(session.Result["cwd"]) != `"/Users/test/project"` {
				t.Fatalf("loaded session missing from replay: %+v", session)
			}
			if result != `{}` && result != `null` && session.Result["modes"] == nil {
				t.Fatal("lost load response modes")
			}
			var update Envelope
			if err := decoder.Decode(&update); err != nil {
				t.Fatal(err)
			}
			if update.Method != "session/update" {
				t.Fatalf("lost replayed history: %+v", update)
			}
		})
	}
}

func TestFailedLoadDoesNotReplaceCachedSession(t *testing.T) {
	cache := NewCache()
	before := []byte(`{"jsonrpc":"2.0","id":0,"result":{"sessionId":"original"}}`)
	cache.SetNewResponse(before)
	proxy := NewProxy(io.Discard, bytes.NewReader(nil), cache)
	proxy.pending.Store(int64(5), &PendingRequest{
		frontend: &Frontend{writer: io.Discard}, originalID: json.RawMessage(`42`), method: "session/load",
		params: json.RawMessage(`{"sessionId":"missing"}`),
	})
	line := []byte(`{"jsonrpc":"2.0","id":5,"error":{"code":-32000,"message":"not found"}}`)
	env, _ := parseEnvelope(line)
	proxy.routeResponseToFrontend(env, line)
	var replay bytes.Buffer
	cache.Replay(&Frontend{writer: &replay})
	if !bytes.Equal(replay.Bytes(), append(before, '\n')) {
		t.Fatal("failed load replaced cached session")
	}
}
