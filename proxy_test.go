package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockAgent simulates an ACP agent. It reads requests from its stdin,
// responds to initialize and session/new, and echoes session/prompt
// as session/update notifications.
func mockAgent(stdin io.Reader, stdout io.Writer) {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		env, err := parseEnvelope(line)
		if err != nil {
			continue
		}

		kind := classify(env)
		if kind != KindRequest {
			continue
		}

		switch env.Method {
		case "initialize":
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(*env.ID),
				"result": map[string]interface{}{
					"protocolVersion": 1,
					"agentInfo":       map[string]string{"name": "mock-agent", "version": "0.1"},
				},
			}
			b, _ := json.Marshal(resp)
			stdout.Write(b)
			stdout.Write([]byte("\n"))

		case "session/new":
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(*env.ID),
				"result":  map[string]string{"sessionId": "test-session-1"},
			}
			b, _ := json.Marshal(resp)
			stdout.Write(b)
			stdout.Write([]byte("\n"))

		case "session/set_mode":
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(*env.ID),
				"result":  map[string]interface{}{},
			}
			b, _ := json.Marshal(resp)
			stdout.Write(b)
			stdout.Write([]byte("\n"))

		case "session/prompt":
			// Send a session/update notification
			update := map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]interface{}{
					"sessionId": "test-session-1",
					"update": map[string]interface{}{
						"kind": "agentMessageChunk",
						"content": map[string]string{
							"type": "text",
							"text": "Hello from mock agent",
						},
					},
				},
			}
			b, _ := json.Marshal(update)
			stdout.Write(b)
			stdout.Write([]byte("\n"))

			// Then send the prompt response
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(*env.ID),
				"result":  map[string]string{"stopReason": "end_turn"},
			}
			b, _ = json.Marshal(resp)
			stdout.Write(b)
			stdout.Write([]byte("\n"))
		}
	}
}

// readLine reads one ndjson line with a timeout.
func readLine(t *testing.T, scanner *bufio.Scanner, timeout time.Duration) []byte {
	t.Helper()
	done := make(chan []byte, 1)
	go func() {
		if scanner.Scan() {
			line := make([]byte, len(scanner.Bytes()))
			copy(line, scanner.Bytes())
			done <- line
		} else {
			done <- nil
		}
	}()
	select {
	case line := <-done:
		if line == nil {
			t.Fatal("unexpected EOF")
		}
		return line
	case <-time.After(timeout):
		t.Fatal("timeout reading line")
		return nil
	}
}

func TestProxyFanOut(t *testing.T) {
	// Create pipes for mock agent
	agentInR, agentInW := io.Pipe()
	agentOutR, agentOutW := io.Pipe()

	go mockAgent(agentInR, agentOutW)

	cache := NewCache()
	proxy := NewProxy(agentInW, agentOutR, cache)

	// Create pipe-based "frontends" instead of stdio
	fe1R, fe1W := io.Pipe()
	fe2R, _ := io.Pipe()
	pr1R, pr1W := io.Pipe()
	pr2R, pr2W := io.Pipe()

	fe1Scanner := bufio.NewScanner(pr1R)
	fe1Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	fe2Scanner := bufio.NewScanner(pr2R)
	fe2Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Frontend 1 (primary): writes to fe1W, reads from pr1R
	f1 := &Frontend{
		id:      1,
		primary: true,
		scanner: bufio.NewScanner(fe1R),
		writer:  pr1W,
		done:    make(chan struct{}),
	}
	f1.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	proxy.AddFrontend(f1)
	go proxy.Run()

	// Send initialize from frontend 1
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`
	fe1W.Write([]byte(initReq + "\n"))

	// Read initialize response on frontend 1
	line := readLine(t, fe1Scanner, 2*time.Second)
	var initResp map[string]interface{}
	if err := json.Unmarshal(line, &initResp); err != nil {
		t.Fatalf("bad init response: %v", err)
	}
	if initResp["result"] == nil {
		t.Fatalf("expected result in init response, got: %s", line)
	}

	// Send session/new
	newReq := `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`
	fe1W.Write([]byte(newReq + "\n"))

	line = readLine(t, fe1Scanner, 2*time.Second)
	var newResp map[string]interface{}
	if err := json.Unmarshal(line, &newResp); err != nil {
		t.Fatalf("bad new response: %v", err)
	}

	// Now connect frontend 2 (should get replay)
	f2 := &Frontend{
		id:      2,
		primary: false,
		scanner: bufio.NewScanner(fe2R),
		writer:  pr2W,
		done:    make(chan struct{}),
	}
	f2.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	proxy.AddFrontend(f2)

	// Frontend 2 should get replayed init and session/new responses
	replayLine1 := readLine(t, fe2Scanner, 2*time.Second)
	replayLine2 := readLine(t, fe2Scanner, 2*time.Second)
	t.Logf("replay 1: %s", replayLine1)
	t.Logf("replay 2: %s", replayLine2)

	// Send a prompt from frontend 1
	promptReq := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"test-session-1","prompt":[{"type":"text","text":"hello"}]}}`
	fe1W.Write([]byte(promptReq + "\n"))

	// Frontend 2 (non-sender) should get the synthesized user_message_chunk.
	// Frontend 1 (sender) should NOT — its UI already shows the input.
	userUpdate2 := readLine(t, fe2Scanner, 2*time.Second)

	var uu2 map[string]interface{}
	json.Unmarshal(userUpdate2, &uu2)
	if uu2["method"] != "session/update" {
		t.Errorf("frontend 2: expected user_message_chunk session/update, got %s", userUpdate2)
	}

	// Both frontends should get the agent's session/update notification
	update1 := readLine(t, fe1Scanner, 2*time.Second)
	update2 := readLine(t, fe2Scanner, 2*time.Second)

	var u1, u2 map[string]interface{}
	json.Unmarshal(update1, &u1)
	json.Unmarshal(update2, &u2)

	if u1["method"] != "session/update" {
		t.Errorf("frontend 1: expected agent session/update, got %s", update1)
	}
	if u2["method"] != "session/update" {
		t.Errorf("frontend 2: expected agent session/update, got %s", update2)
	}

	// Frontend 1 should get the prompt response
	resp1 := readLine(t, fe1Scanner, 2*time.Second)
	var r1 map[string]interface{}
	json.Unmarshal(resp1, &r1)
	// The response should have the original ID (3), not the proxy's ID
	if r1["id"] == nil {
		t.Errorf("expected id in prompt response")
	}
	idFloat, ok := r1["id"].(float64)
	if !ok || int(idFloat) != 3 {
		t.Errorf("expected id=3 in prompt response, got %v", r1["id"])
	}

	// Frontend 2 should get a synthetic turn_complete notification
	turnComplete := readLine(t, fe2Scanner, 2*time.Second)
	var tc map[string]interface{}
	json.Unmarshal(turnComplete, &tc)
	if tc["method"] != "session/update" {
		t.Errorf("expected session/update for turn_complete, got %s", turnComplete)
	}
	params := tc["params"].(map[string]interface{})
	update := params["update"].(map[string]interface{})
	if update["sessionUpdate"] != "turn_complete" {
		t.Errorf("expected turn_complete, got %v", update["sessionUpdate"])
	}
	if update["stopReason"] != "end_turn" {
		t.Errorf("expected stopReason end_turn, got %v", update["stopReason"])
	}
}

func TestModeChangeSynthesis(t *testing.T) {
	agentInR, agentInW := io.Pipe()
	agentOutR, agentOutW := io.Pipe()

	go mockAgent(agentInR, agentOutW)

	cache := NewCache()
	proxy := NewProxy(agentInW, agentOutR, cache)

	// Frontend 1 (primary)
	fe1R, fe1W := io.Pipe()
	pr1R, pr1W := io.Pipe()
	fe1Scanner := bufio.NewScanner(pr1R)
	fe1Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f1 := &Frontend{
		id: 1, primary: true,
		scanner: bufio.NewScanner(fe1R),
		writer:  pr1W,
		done:    make(chan struct{}),
	}
	f1.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Frontend 2 (secondary, simulating acp-mobile)
	fe2R, fe2W := io.Pipe()
	pr2R, pr2W := io.Pipe()
	fe2Scanner := bufio.NewScanner(pr2R)
	fe2Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f2 := &Frontend{
		id: 2, primary: false,
		scanner: bufio.NewScanner(fe2R),
		writer:  pr2W,
		done:    make(chan struct{}),
	}
	f2.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	proxy.AddFrontend(f1)
	go proxy.Run()

	// Initialize via frontend 1
	fe1W.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n"))
	readLine(t, fe1Scanner, 2*time.Second) // init response

	// session/new via frontend 1
	fe1W.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}` + "\n"))
	readLine(t, fe1Scanner, 2*time.Second) // new response

	// Now add frontend 2 — it gets replayed init + session/new
	proxy.AddFrontend(f2)
	readLine(t, fe2Scanner, 2*time.Second) // replayed init
	readLine(t, fe2Scanner, 2*time.Second) // replayed session/new

	// Frontend 2 sends session/set_mode (like acp-mobile changing mode)
	fe2W.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/set_mode","params":{"sessionId":"test-session-1","modeId":"plan"}}` + "\n"))

	// Frontend 2 should get the response back
	resp2 := readLine(t, fe2Scanner, 2*time.Second)
	var r2 map[string]interface{}
	json.Unmarshal(resp2, &r2)
	if r2["result"] == nil {
		t.Fatalf("frontend 2: expected result in set_mode response, got: %s", resp2)
	}

	// Frontend 1 (primary) should get a synthetic current_mode_update notification
	modeUpdate := readLine(t, fe1Scanner, 2*time.Second)
	var mu map[string]interface{}
	json.Unmarshal(modeUpdate, &mu)
	if mu["method"] != "session/update" {
		t.Fatalf("frontend 1: expected session/update, got: %s", modeUpdate)
	}
	params := mu["params"].(map[string]interface{})
	update := params["update"].(map[string]interface{})
	if update["sessionUpdate"] != "current_mode_update" {
		t.Errorf("expected current_mode_update, got %v", update["sessionUpdate"])
	}
	if update["currentModeId"] != "plan" {
		t.Errorf("expected modeId 'plan', got %v", update["currentModeId"])
	}
}

func TestBufferNameReplay(t *testing.T) {
	// When ACP_MULTIPLEX_NAME is set, a secondary frontend should receive
	// the acp-multiplex/meta notification with the buffer name on connect.
	agentInR, agentInW := io.Pipe()
	agentOutR, agentOutW := io.Pipe()

	go mockAgent(agentInR, agentOutW)

	cache := NewCache()

	// Simulate what main.go does with ACP_MULTIPLEX_NAME
	bufferName := "Claude Code Agent @ myproject"
	meta, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "acp-multiplex/meta",
		"params":  map[string]string{"name": bufferName},
	})
	cache.SetMeta(meta)

	proxy := NewProxy(agentInW, agentOutR, cache)

	// Primary frontend
	fe1R, fe1W := io.Pipe()
	pr1R, pr1W := io.Pipe()
	fe1Scanner := bufio.NewScanner(pr1R)
	fe1Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f1 := &Frontend{
		id: 1, primary: true,
		scanner: bufio.NewScanner(fe1R),
		writer:  pr1W,
		done:    make(chan struct{}),
	}
	f1.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	proxy.AddFrontend(f1)
	go proxy.Run()

	// Initialize + session/new via primary
	fe1W.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n"))
	readLine(t, fe1Scanner, 2*time.Second)
	fe1W.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}` + "\n"))
	readLine(t, fe1Scanner, 2*time.Second)

	// Secondary frontend connects (like acp-mobile probing the socket)
	_, _ = io.Pipe() // fe2R not needed — secondary won't send anything
	pr2R, pr2W := io.Pipe()
	fe2Scanner := bufio.NewScanner(pr2R)
	fe2Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	fe2ReadR, _ := io.Pipe()
	f2 := &Frontend{
		id: 2, primary: false,
		scanner: bufio.NewScanner(fe2ReadR),
		writer:  pr2W,
		done:    make(chan struct{}),
	}
	f2.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	proxy.AddFrontend(f2)

	// First replay message should be the meta notification
	metaLine := readLine(t, fe2Scanner, 2*time.Second)
	var m map[string]interface{}
	json.Unmarshal(metaLine, &m)
	if m["method"] != "acp-multiplex/meta" {
		t.Fatalf("expected acp-multiplex/meta, got: %s", metaLine)
	}
	params := m["params"].(map[string]interface{})
	if params["name"] != bufferName {
		t.Errorf("expected buffer name %q, got %v", bufferName, params["name"])
	}

	// Then init response
	initLine := readLine(t, fe2Scanner, 2*time.Second)
	var ir map[string]interface{}
	json.Unmarshal(initLine, &ir)
	if ir["result"] == nil {
		t.Errorf("expected init response, got: %s", initLine)
	}

	// Then session/new response
	newLine := readLine(t, fe2Scanner, 2*time.Second)
	var nr map[string]interface{}
	json.Unmarshal(newLine, &nr)
	if nr["result"] == nil {
		t.Errorf("expected session/new response, got: %s", newLine)
	}
}

func TestModeChangeReplayedToLateJoiner(t *testing.T) {
	// After a mode change, a frontend that connects later should see
	// the current_mode_update in its replay stream.
	agentInR, agentInW := io.Pipe()
	agentOutR, agentOutW := io.Pipe()

	go mockAgent(agentInR, agentOutW)

	cache := NewCache()
	proxy := NewProxy(agentInW, agentOutR, cache)

	// Primary frontend
	fe1R, fe1W := io.Pipe()
	pr1R, pr1W := io.Pipe()
	fe1Scanner := bufio.NewScanner(pr1R)
	fe1Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f1 := &Frontend{
		id: 1, primary: true,
		scanner: bufio.NewScanner(fe1R),
		writer:  pr1W,
		done:    make(chan struct{}),
	}
	f1.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Secondary frontend that triggers the mode change
	fe2R, fe2W := io.Pipe()
	pr2R, pr2W := io.Pipe()
	fe2Scanner := bufio.NewScanner(pr2R)
	fe2Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f2 := &Frontend{
		id: 2, primary: false,
		scanner: bufio.NewScanner(fe2R),
		writer:  pr2W,
		done:    make(chan struct{}),
	}
	f2.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	proxy.AddFrontend(f1)
	go proxy.Run()

	// Initialize + session/new
	fe1W.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n"))
	readLine(t, fe1Scanner, 2*time.Second)
	fe1W.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}` + "\n"))
	readLine(t, fe1Scanner, 2*time.Second)

	// Add frontend 2, drain replay
	proxy.AddFrontend(f2)
	readLine(t, fe2Scanner, 2*time.Second) // init
	readLine(t, fe2Scanner, 2*time.Second) // session/new

	// Frontend 2 changes mode
	fe2W.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/set_mode","params":{"sessionId":"test-session-1","modeId":"plan"}}` + "\n"))
	readLine(t, fe2Scanner, 2*time.Second) // set_mode response
	readLine(t, fe1Scanner, 2*time.Second) // mode update on primary

	// Now a third frontend connects — it should see the mode change in replay
	fe3ReadR, _ := io.Pipe()
	pr3R, pr3W := io.Pipe()
	fe3Scanner := bufio.NewScanner(pr3R)
	fe3Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f3 := &Frontend{
		id: 3, primary: false,
		scanner: bufio.NewScanner(fe3ReadR),
		writer:  pr3W,
		done:    make(chan struct{}),
	}
	f3.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	proxy.AddFrontend(f3)

	// Drain replay: init, session/new, then the mode update
	readLine(t, fe3Scanner, 2*time.Second) // init
	readLine(t, fe3Scanner, 2*time.Second) // session/new

	modeLine := readLine(t, fe3Scanner, 2*time.Second)
	var mu map[string]interface{}
	json.Unmarshal(modeLine, &mu)
	if mu["method"] != "session/update" {
		t.Fatalf("expected session/update, got: %s", modeLine)
	}
	params := mu["params"].(map[string]interface{})
	update := params["update"].(map[string]interface{})
	if update["sessionUpdate"] != "current_mode_update" {
		t.Errorf("expected current_mode_update in replay, got %v", update["sessionUpdate"])
	}
	if update["currentModeId"] != "plan" {
		t.Errorf("expected modeId 'plan' in replay, got %v", update["currentModeId"])
	}
}

func TestPermissionReplayedToLateJoiner(t *testing.T) {
	// When the agent sends a permission request and a frontend connects later,
	// the pending permission should be replayed so the new frontend can respond.
	agentInR, agentInW := io.Pipe()
	agentOutR, agentOutW := io.Pipe()

	// Custom mock: responds to initialize, then sends a permission request
	go func() {
		scanner := bufio.NewScanner(agentInR)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			env, err := parseEnvelope(scanner.Bytes())
			if err != nil {
				continue
			}
			if env.Method == "initialize" {
				resp, _ := json.Marshal(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      json.RawMessage(*env.ID),
					"result":  map[string]interface{}{"protocolVersion": 1, "agentInfo": map[string]string{"name": "mock"}},
				})
				agentOutW.Write(resp)
				agentOutW.Write([]byte("\n"))

				// Then send a permission request (reverse call)
				perm, _ := json.Marshal(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      99,
					"method":  "session/request_permission",
					"params": map[string]interface{}{
						"sessionId": "s1",
						"options": []map[string]string{
							{"kind": "allow_once", "name": "Yes", "optionId": "yes"},
							{"kind": "reject_once", "name": "No", "optionId": "no"},
						},
						"toolCall": map[string]string{"title": "Ready to code?"},
					},
				})
				agentOutW.Write(perm)
				agentOutW.Write([]byte("\n"))
			}
		}
	}()

	cache := NewCache()
	proxy := NewProxy(agentInW, agentOutR, cache)

	// Primary frontend
	fe1R, fe1W := io.Pipe()
	pr1R, pr1W := io.Pipe()
	fe1Scanner := bufio.NewScanner(pr1R)
	fe1Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f1 := &Frontend{
		id: 1, primary: true,
		scanner: bufio.NewScanner(fe1R),
		writer:  pr1W,
		done:    make(chan struct{}),
	}
	f1.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	proxy.AddFrontend(f1)
	go proxy.Run()

	// Send init request — mock agent responds then sends permission request
	fe1W.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n"))

	// Primary frontend receives init response + permission
	readLine(t, fe1Scanner, 2*time.Second) // init response
	permLine := readLine(t, fe1Scanner, 2*time.Second)
	var perm map[string]interface{}
	json.Unmarshal(permLine, &perm)
	if perm["method"] != "session/request_permission" {
		t.Fatalf("expected session/request_permission, got: %s", permLine)
	}

	// Now a second frontend connects — it should get the pending permission in replay
	fe2ReadR, _ := io.Pipe()
	pr2R, pr2W := io.Pipe()
	fe2Scanner := bufio.NewScanner(pr2R)
	fe2Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f2 := &Frontend{
		id: 2, primary: false,
		scanner: bufio.NewScanner(fe2ReadR),
		writer:  pr2W,
		done:    make(chan struct{}),
	}
	f2.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	proxy.AddFrontend(f2)

	// Drain replay: init response, then the pending permission
	readLine(t, fe2Scanner, 2*time.Second) // init response

	permReplay := readLine(t, fe2Scanner, 2*time.Second)
	var pr map[string]interface{}
	json.Unmarshal(permReplay, &pr)
	if pr["method"] != "session/request_permission" {
		t.Fatalf("expected session/request_permission in replay, got: %s", permReplay)
	}
	prParams := pr["params"].(map[string]interface{})
	tc := prParams["toolCall"].(map[string]interface{})
	if tc["title"] != "Ready to code?" {
		t.Errorf("expected title 'Ready to code?', got %v", tc["title"])
	}
}

func TestProxyIDRewriting(t *testing.T) {
	// Two frontends send requests with the same ID — proxy must handle this.
	agentInR, agentInW := io.Pipe()
	agentOutR, agentOutW := io.Pipe()

	go mockAgent(agentInR, agentOutW)

	cache := NewCache()
	proxy := NewProxy(agentInW, agentOutR, cache)

	fe1R, fe1W := io.Pipe()
	fe2R, fe2W := io.Pipe()
	pr1R, pr1W := io.Pipe()
	pr2R, pr2W := io.Pipe()

	fe1Scanner := bufio.NewScanner(pr1R)
	fe1Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	fe2Scanner := bufio.NewScanner(pr2R)
	fe2Scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f1 := &Frontend{
		id: 1, primary: true,
		scanner: bufio.NewScanner(fe1R),
		writer:  pr1W,
		done:    make(chan struct{}),
	}
	f1.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f2 := &Frontend{
		id: 2, primary: false,
		scanner: bufio.NewScanner(fe2R),
		writer:  pr2W,
		done:    make(chan struct{}),
	}
	f2.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	proxy.AddFrontend(f1)
	proxy.AddFrontend(f2)
	go proxy.Run()

	// Frontend 1 sends initialize with id:1
	fe1W.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n"))
	resp1 := readLine(t, fe1Scanner, 2*time.Second)

	var r1 map[string]interface{}
	json.Unmarshal(resp1, &r1)
	if id, ok := r1["id"].(float64); !ok || int(id) != 1 {
		t.Errorf("frontend 1: expected id=1, got %v", r1["id"])
	}

	// Frontend 2 sends session/new also with id:1 (same ID space!)
	fe2W.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/tmp"}}` + "\n"))

	resp2 := readLine(t, fe2Scanner, 2*time.Second)

	var r2 map[string]interface{}
	json.Unmarshal(resp2, &r2)
	// Should have id:1 (frontend 2's original ID), not the proxy's rewritten ID
	if id, ok := r2["id"].(float64); !ok || int(id) != 1 {
		t.Errorf("frontend 2: expected id=1, got %v", r2["id"])
	}
}

func TestMetadataReplay(t *testing.T) {
	cache := NewCache()

	// Set metadata
	meta, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "acp-multiplex/meta",
		"params":  map[string]string{"name": "Claude Code Agent @ myproject"},
	})
	cache.SetMeta(meta)

	// Set an init response too
	initResp, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      0,
		"result":  map[string]string{"protocolVersion": "1"},
	})
	cache.SetInitResponse(initResp)

	// Replay to a frontend and check order: meta first, then init
	pr, pw := io.Pipe()
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	f := &Frontend{
		id:     99,
		writer: pw,
		done:   make(chan struct{}),
	}

	go cache.Replay(f)

	// First line should be metadata
	line1 := readLine(t, scanner, 2*time.Second)
	var m map[string]interface{}
	json.Unmarshal(line1, &m)
	if m["method"] != "acp-multiplex/meta" {
		t.Errorf("expected acp-multiplex/meta, got %v", m["method"])
	}
	params := m["params"].(map[string]interface{})
	if params["name"] != "Claude Code Agent @ myproject" {
		t.Errorf("expected name in meta, got %v", params["name"])
	}

	// Second line should be init response
	line2 := readLine(t, scanner, 2*time.Second)
	var ir map[string]interface{}
	json.Unmarshal(line2, &ir)
	if ir["result"] == nil {
		t.Errorf("expected init response with result, got %s", line2)
	}
}

func TestUnixSocket(t *testing.T) {
	sockPath := filepath.Join(os.TempDir(), "acp-multiplex-test.sock")
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	defer os.Remove(sockPath)

	// Connect a client
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Write([]byte("hello\n"))
		conn.Close()
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("expected to read a line")
	}
	if scanner.Text() != "hello" {
		t.Errorf("expected 'hello', got %q", scanner.Text())
	}
}

// TestPermissionReplayAfterTurnComplete verifies that when a previous turn
// completed (turn_complete cached) and then a new turn sends a permission
// request, the late joiner receives the permission as the very last message
// in replay — after the turn_complete. This is the plan mode scenario:
// previous conversation turn completes, then agent enters plan mode and
// sends ExitPlanMode which triggers a permission request.
func TestPermissionReplayAfterTurnComplete(t *testing.T) {
	cache := NewCache()

	// Simulate a completed previous turn
	cache.SetInitResponse([]byte(`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"agentInfo":{"name":"mock"}}}`))
	cache.SetNewResponse([]byte(`{"jsonrpc":"2.0","id":0,"result":{"sessionId":"s1"}}`))

	// User message
	cache.AddUpdate([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"help me plan"}}}}`))
	// Agent response
	cache.AddUpdate([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Sure, entering plan mode."}}}}`))
	// Turn complete from previous turn
	cache.AddUpdate([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_complete"}}}`))
	// New turn: agent writes plan
	cache.AddUpdate([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Here is my plan."}}}}`))
	cache.AddUpdate([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"tc1","toolName":"Write"}}}`))
	cache.AddUpdate([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"tc1","status":"completed"}}}`))

	// Pending permission (ExitPlanMode)
	cache.SetPendingPermission([]byte(`{"jsonrpc":"2.0","id":99,"method":"session/request_permission","params":{"sessionId":"s1","options":[{"kind":"allow_once","name":"Yes","optionId":"yes"},{"kind":"reject_once","name":"No","optionId":"no"}],"toolCall":{"title":"Ready to code?"}}}`))

	// Replay to a late joiner
	r, w := io.Pipe()
	fe := &Frontend{
		id:      99,
		primary: false,
		scanner: bufio.NewScanner(r), // unused for replay
		writer:  w,
		done:    make(chan struct{}),
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	go cache.Replay(fe)

	// Collect all replayed messages
	var msgs []map[string]interface{}
	for scanner.Scan() {
		var msg map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			t.Fatalf("bad json in replay: %v", err)
		}
		msgs = append(msgs, msg)

		// Permission is the last thing sent; after it the pipe stays open
		// so we need to stop reading. Check if this is the permission.
		if method, _ := msg["method"].(string); method == "session/request_permission" {
			break
		}
	}

	if len(msgs) == 0 {
		t.Fatal("no messages received in replay")
	}

	// The last message must be the permission request
	last := msgs[len(msgs)-1]
	if method, _ := last["method"].(string); method != "session/request_permission" {
		t.Fatalf("expected last replay message to be session/request_permission, got method=%v", last["method"])
	}

	// Verify permission has the right content
	params, _ := last["params"].(map[string]interface{})
	tc, _ := params["toolCall"].(map[string]interface{})
	if tc["title"] != "Ready to code?" {
		t.Errorf("expected title 'Ready to code?', got %v", tc["title"])
	}

	// Verify turn_complete appears before the permission
	foundTurnComplete := false
	foundPermission := false
	for _, msg := range msgs {
		method, _ := msg["method"].(string)
		if method == "session/update" {
			params, _ := msg["params"].(map[string]interface{})
			update, _ := params["update"].(map[string]interface{})
			if su, _ := update["sessionUpdate"].(string); su == "turn_complete" {
				foundTurnComplete = true
			}
		}
		if method == "session/request_permission" {
			foundPermission = true
			if !foundTurnComplete {
				t.Error("permission appeared before turn_complete in replay")
			}
		}
	}
	if !foundTurnComplete {
		t.Error("turn_complete not found in replay")
	}
	if !foundPermission {
		t.Error("permission not found in replay")
	}

	w.Close()
}
