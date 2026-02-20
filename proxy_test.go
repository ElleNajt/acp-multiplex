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
				"result":  map[string]string{"stopReason": "endTurn"},
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
	if update["stopReason"] != "endTurn" {
		t.Errorf("expected stopReason endTurn, got %v", update["stopReason"])
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
