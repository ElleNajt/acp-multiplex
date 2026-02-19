package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestIntegrationWithAgent tests the proxy against the real claude-code-acp agent.
// Skip if claude-code-acp is not in PATH.
func TestIntegrationWithAgent(t *testing.T) {
	agentPath, err := exec.LookPath("claude-code-acp")
	if err != nil {
		t.Skip("claude-code-acp not found in PATH, skipping integration test")
	}
	t.Logf("using agent: %s", agentPath)

	// Start agent subprocess
	cmd := exec.Command(agentPath)
	agentIn, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	agentOut, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	cache := NewCache()
	proxy := NewProxy(agentIn, agentOut, cache)

	// Create pipe-based primary frontend
	feR, feW := io.Pipe()
	prR, prW := io.Pipe()

	f := &Frontend{
		id:      0,
		primary: true,
		scanner: bufio.NewScanner(feR),
		writer:  prW,
		done:    make(chan struct{}),
	}
	f.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	proxy.AddFrontend(f)
	go proxy.Run()

	scanner := bufio.NewScanner(prR)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Helper: send a JSON-RPC message
	send := func(msg map[string]interface{}) {
		b, _ := json.Marshal(msg)
		feW.Write(b)
		feW.Write([]byte("\n"))
	}

	// Helper: read a line with timeout
	read := func(timeout time.Duration) (map[string]interface{}, error) {
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
				return nil, fmt.Errorf("EOF")
			}
			var msg map[string]interface{}
			if err := json.Unmarshal(line, &msg); err != nil {
				return nil, err
			}
			return msg, nil
		case <-time.After(timeout):
			return nil, fmt.Errorf("timeout")
		}
	}

	// 1. Initialize
	t.Log("sending initialize...")
	send(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": 1,
			"clientCapabilities": map[string]interface{}{
				"fs": map[string]bool{"readTextFile": true, "writeTextFile": true},
			},
			"clientInfo": map[string]string{"name": "test", "version": "0.1"},
		},
	})

	initResp, err := read(10 * time.Second)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Logf("init response: %v", initResp)

	if initResp["error"] != nil {
		t.Fatalf("initialize error: %v", initResp["error"])
	}
	result, ok := initResp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected result map, got %T", initResp["result"])
	}
	agentInfo := result["agentInfo"]
	t.Logf("agent: %v", agentInfo)

	// 2. Session new
	t.Log("sending session/new...")
	send(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/new",
		"params":  map[string]interface{}{"cwd": "/tmp", "mcpServers": []interface{}{}},
	})

	newResp, err := read(10 * time.Second)
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	t.Logf("session/new response: %v", newResp)

	if newResp["error"] != nil {
		t.Fatalf("session/new error: %v", newResp["error"])
	}
	newResult, ok := newResp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected result map, got %T", newResp["result"])
	}
	sessionID, ok := newResult["sessionId"].(string)
	if !ok || sessionID == "" {
		t.Fatalf("expected sessionId, got %v", newResult)
	}
	t.Logf("sessionId: %s", sessionID)

	// 3. Now set up a Unix socket frontend to test multiplexing
	sockPath := fmt.Sprintf("/tmp/acp-multiplex-test-%d.sock", os.Getpid())
	os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer os.Remove(sockPath)
	defer ln.Close()

	// Accept connections and add as frontends
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			sf := NewSocketFrontend(99, conn)
			proxy.AddFrontend(sf)
		}
	}()

	// Connect secondary frontend
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	secondaryScanner := bufio.NewScanner(conn)
	secondaryScanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Secondary should receive replayed init and session/new
	time.Sleep(500 * time.Millisecond) // let replay happen

	readSecondary := func(timeout time.Duration) (map[string]interface{}, error) {
		done := make(chan []byte, 1)
		go func() {
			if secondaryScanner.Scan() {
				line := make([]byte, len(secondaryScanner.Bytes()))
				copy(line, secondaryScanner.Bytes())
				done <- line
			} else {
				done <- nil
			}
		}()
		select {
		case line := <-done:
			if line == nil {
				return nil, fmt.Errorf("EOF")
			}
			var msg map[string]interface{}
			if err := json.Unmarshal(line, &msg); err != nil {
				return nil, err
			}
			return msg, nil
		case <-time.After(timeout):
			return nil, fmt.Errorf("timeout")
		}
	}

	replay1, err := readSecondary(5 * time.Second)
	if err != nil {
		t.Fatalf("replay init: %v", err)
	}
	t.Logf("secondary replay 1: %v", replay1)

	replay2, err := readSecondary(5 * time.Second)
	if err != nil {
		t.Fatalf("replay session/new: %v", err)
	}
	t.Logf("secondary replay 2: %v", replay2)

	// 4. Send a simple prompt
	t.Log("sending prompt...")
	send(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "session/prompt",
		"params": map[string]interface{}{
			"sessionId": sessionID,
			"prompt":    []map[string]string{{"type": "text", "text": "Say exactly: hello world"}},
		},
	})

	// Read messages from both frontends until we get the prompt response
	var gotUpdateOnPrimary, gotUpdateOnSecondary bool
	var promptResponse map[string]interface{}

	primaryDone := make(chan struct{})
	secondaryUpdates := make(chan map[string]interface{}, 100)

	// Read secondary in background
	go func() {
		for {
			msg, err := readSecondary(30 * time.Second)
			if err != nil {
				return
			}
			secondaryUpdates <- msg
		}
	}()

	// Read primary until prompt response
	go func() {
		defer close(primaryDone)
		for {
			msg, err := read(30 * time.Second)
			if err != nil {
				t.Logf("primary read error: %v", err)
				return
			}
			method, _ := msg["method"].(string)
			if method == "session/update" {
				gotUpdateOnPrimary = true
				params, _ := msg["params"].(map[string]interface{})
				update, _ := params["update"].(map[string]interface{})
				kind, _ := update["sessionUpdate"].(string)
				t.Logf("primary update: %s", kind)
			} else if msg["result"] != nil && msg["id"] != nil {
				// Check if this is the prompt response (id: 3)
				if id, ok := msg["id"].(float64); ok && int(id) == 3 {
					promptResponse = msg
					return
				}
			} else if method != "" {
				// Reverse call — just respond with an error for now
				t.Logf("reverse call: %s", method)
				if id, ok := msg["id"]; ok {
					// Check if this is a request we need to respond to
					if strings.HasPrefix(method, "fs/") || strings.HasPrefix(method, "terminal/") || method == "session/request_permission" {
						resp := map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      id,
							"error":   map[string]interface{}{"code": -32001, "message": "not implemented in test"},
						}
						b, _ := json.Marshal(resp)
						feW.Write(b)
						feW.Write([]byte("\n"))
					}
				}
			}
		}
	}()

	// Wait for prompt to complete (with timeout)
	select {
	case <-primaryDone:
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for prompt response")
	}

	// Check secondary got at least one update
	select {
	case msg := <-secondaryUpdates:
		method, _ := msg["method"].(string)
		if method == "session/update" {
			gotUpdateOnSecondary = true
		}
		t.Logf("secondary got: %v", msg)
	case <-time.After(2 * time.Second):
		t.Log("no secondary updates received (may have been fast)")
	}

	t.Logf("gotUpdateOnPrimary: %v, gotUpdateOnSecondary: %v", gotUpdateOnPrimary, gotUpdateOnSecondary)
	if promptResponse == nil {
		t.Error("never got prompt response")
	} else {
		t.Logf("prompt response: %v", promptResponse)
	}
	if gotUpdateOnPrimary {
		t.Log("PRIMARY: received session/update notifications")
	}
	if gotUpdateOnSecondary {
		t.Log("SECONDARY: received session/update notifications (fan-out works!)")
	}
}
