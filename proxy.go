package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PendingRequest tracks a request forwarded to the agent so we can
// route the response back to the correct frontend with the correct ID.
type PendingRequest struct {
	originalID json.RawMessage
	frontend   *Frontend
	method     string          // for cache decisions
	params     json.RawMessage // original params (for synthesizing notifications)
}

// Proxy is the core multiplexer. It reads from the agent and fans out
// to frontends, and reads from frontends and forwards to the agent.
type Proxy struct {
	agentIn  io.Writer      // agent's stdin
	agentOut *bufio.Scanner // agent's stdout

	mu        sync.Mutex
	frontends []*Frontend

	nextID         atomic.Int64
	pending        sync.Map // proxyID (int64) -> *PendingRequest
	pendingReverse sync.Map // agent request ID (string) -> struct{}
	agentDead      atomic.Bool

	cache *Cache

	// Channel for messages from all frontends
	fromFrontends chan FrontendMessage

	// Socket path for mtime updates (optional)
	sockPath  string
	lastTouch atomic.Int64 // unix timestamp of last touch
}

func NewProxy(agentIn io.Writer, agentOut io.Reader, cache *Cache) *Proxy {
	scanner := bufio.NewScanner(agentOut)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	p := &Proxy{
		agentIn:       agentIn,
		agentOut:      scanner,
		cache:         cache,
		fromFrontends: make(chan FrontendMessage, 64),
	}
	p.nextID.Store(1)
	return p
}

// touchSocket updates the socket file's mtime so external tools (e.g. acp-mobile)
// can see when a session was last active. Rate-limited to once per second.
func (p *Proxy) touchSocket() {
	if p.sockPath == "" {
		return
	}
	now := time.Now().Unix()
	if p.lastTouch.Load() == now {
		return
	}
	p.lastTouch.Store(now)
	os.Chtimes(p.sockPath, time.Now(), time.Now())
}

func (p *Proxy) AddFrontend(f *Frontend) {
	p.mu.Lock()
	p.frontends = append(p.frontends, f)
	p.mu.Unlock()

	// Start reading from this frontend
	go f.ReadLines(p.fromFrontends)

	// Remove the frontend when it disconnects.
	go func() {
		<-f.done
		p.RemoveFrontend(f)
	}()

	// Replay cached history for non-primary frontends.
	// Run in a goroutine because Send may block on synchronous writers.
	if !f.primary {
		go p.cache.Replay(f)
	}
}

func (p *Proxy) RemoveFrontend(f *Frontend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, fe := range p.frontends {
		if fe == f {
			p.frontends = append(p.frontends[:i], p.frontends[i+1:]...)
			return
		}
	}
}

// sendToAgent writes a JSON line to the agent's stdin.
// Returns an error if the agent is dead or the write fails.
func (p *Proxy) sendToAgent(line []byte) error {
	if p.agentDead.Load() {
		return errAgentDead
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.agentIn.Write(line); err != nil {
		return err
	}
	_, err := p.agentIn.Write([]byte("\n"))
	return err
}

var errAgentDead = fmt.Errorf("agent process exited")

// broadcast sends a JSON line to all connected frontends.
func (p *Proxy) broadcast(line []byte) {
	p.broadcastExcept(line, nil)
}

// broadcastExcept sends a JSON line to all frontends except the given one.
func (p *Proxy) broadcastExcept(line []byte, except *Frontend) {
	p.mu.Lock()
	fes := make([]*Frontend, len(p.frontends))
	copy(fes, p.frontends)
	p.mu.Unlock()

	if Debug {
		exceptID := -1
		if except != nil {
			exceptID = except.id
		}
		log.Printf("broadcast to %d frontends (except %d): %.100s", len(fes), exceptID, string(line))
	}

	for _, f := range fes {
		if f != except {
			f.Send(line)
		}
	}
}

// Run starts the proxy's read loops. Blocks until the agent exits.
func (p *Proxy) Run() {
	go p.readFromFrontends()
	p.readFromAgent() // blocks
}

// readFromAgent reads ndjson from the agent and routes each message.
func (p *Proxy) readFromAgent() {
	for p.agentOut.Scan() {
		line := make([]byte, len(p.agentOut.Bytes()))
		copy(line, p.agentOut.Bytes())

		env, err := parseEnvelope(line)
		if err != nil {
			log.Printf("agent: bad json: %v", err)
			continue
		}

		p.touchSocket()

		kind := classify(env)
		switch kind {
		case KindNotification:
			// session/update — fan out to all frontends and cache
			if env.Method == "session/update" {
				p.cache.AddUpdate(line)
			}
			p.broadcast(line)

		case KindResponse:
			// Response to a request we forwarded. Look up who sent it.
			p.routeResponseToFrontend(env, line)

		case KindRequest:
			// Reverse call from agent (fs/*, terminal/*, session/requestPermission).
			// fs/terminal go to primary; permissions broadcast to all.
			p.routeReverseCall(env, line)

		default:
			log.Printf("agent: unclassifiable message")
		}
	}
	if err := p.agentOut.Err(); err != nil {
		log.Printf("agent stdout error: %v", err)
	}

	// Agent exited — mark dead and drain all pending requests with errors.
	p.agentDead.Store(true)
	log.Printf("agent exited, draining pending requests")
	p.pending.Range(func(key, value interface{}) bool {
		proxyID := key.(int64)
		pr := value.(*PendingRequest)
		p.pending.Delete(proxyID)

		errResp, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      pr.originalID,
			"error": map[string]interface{}{
				"code":    -32603,
				"message": "Agent process exited",
			},
		})
		log.Printf("sending error response for pending request %d (method %s) to frontend %d",
			proxyID, pr.method, pr.frontend.id)
		pr.frontend.Send(errResp)
		return true
	})
}

// routeResponseToFrontend routes an agent response back to the
// frontend that sent the original request, rewriting the ID.
func (p *Proxy) routeResponseToFrontend(env *Envelope, line []byte) {
	if env.ID == nil {
		return
	}

	var proxyID int64
	if err := json.Unmarshal(*env.ID, &proxyID); err != nil {
		// ID might not be a number — forward to primary as fallback
		log.Printf("agent response with non-numeric id, forwarding to primary")
		p.sendToPrimary(line)
		return
	}

	val, ok := p.pending.LoadAndDelete(proxyID)
	if !ok {
		log.Printf("agent response for unknown id %d", proxyID)
		return
	}
	pr := val.(*PendingRequest)

	// Cache initialize and session/new responses
	switch pr.method {
	case "initialize":
		rewritten, err := rewriteID(line, 0) // doesn't matter, we'll rewrite per-frontend
		if err == nil {
			p.cache.SetInitResponse(rewritten)
		}
	case "session/new":
		rewritten, err := rewriteID(line, 0)
		if err == nil {
			p.cache.SetNewResponse(rewritten)
		}
	}

	// For session/prompt responses, synthesize a turn-complete notification
	// so other frontends know the agent finished (they don't get the response).
	if pr.method == "session/prompt" {
		p.synthesizeTurnComplete(line, pr.frontend)
	}

	// For session/set_mode responses, synthesize a current_mode_update
	// notification so all other frontends (and the primary) learn about the change.
	if pr.method == "session/set_mode" {
		p.synthesizeModeChange(pr)
	}

	// Rewrite ID back to the frontend's original
	rewritten, err := restoreID(line, pr.originalID)
	if err != nil {
		log.Printf("failed to restore id: %v", err)
		return
	}
	if Debug {
		log.Printf("send to frontend %d: %.100s", pr.frontend.id, string(rewritten))
	}
	pr.frontend.Send(rewritten)
}

// routeReverseCall routes an agent-initiated request to the appropriate frontend(s).
// fs/* and terminal/* go to primary only (they need real filesystem/terminal access).
// Everything else (e.g. session/requestPermission) is broadcast to all frontends;
// the first response wins and subsequent responses are dropped.
func (p *Proxy) routeReverseCall(env *Envelope, line []byte) {
	if env.ID != nil {
		p.pendingReverse.Store(string(*env.ID), struct{}{})
	}
	if strings.HasPrefix(env.Method, "fs/") || strings.HasPrefix(env.Method, "terminal/") {
		p.sendToPrimary(line)
	} else {
		p.broadcast(line)
	}
}

func (p *Proxy) sendToPrimary(line []byte) {
	p.mu.Lock()
	var primary *Frontend
	for _, f := range p.frontends {
		if f.primary {
			primary = f
			break
		}
	}
	p.mu.Unlock()

	if primary != nil {
		primary.Send(line)
	} else {
		log.Printf("no primary frontend for reverse call")
	}
}

// readFromFrontends reads messages from all frontends and routes to agent.
func (p *Proxy) readFromFrontends() {
	for msg := range p.fromFrontends {
		env, err := parseEnvelope(msg.Line)
		if err != nil {
			log.Printf("frontend %d: bad json: %v", msg.Frontend.id, err)
			continue
		}

		kind := classify(env)
		switch kind {
		case KindRequest:
			p.handleFrontendRequest(msg.Frontend, env, msg.Line)

		case KindNotification:
			// session/cancel etc — forward directly
			if err := p.sendToAgent(msg.Line); err != nil {
				log.Printf("frontend %d: send notification to agent failed: %v", msg.Frontend.id, err)
			}

		case KindResponse:
			// Response to a reverse call — first response wins, drop duplicates.
			if env.ID != nil {
				idKey := string(*env.ID)
				if _, loaded := p.pendingReverse.LoadAndDelete(idKey); loaded {
					if err := p.sendToAgent(msg.Line); err != nil {
						log.Printf("frontend %d: send response to agent failed: %v", msg.Frontend.id, err)
					}
				}
				// else: duplicate response from another frontend, drop it
			}

		default:
			log.Printf("frontend %d: unclassifiable message", msg.Frontend.id)
		}
	}
}

// handleFrontendRequest forwards a frontend request to the agent with ID rewriting.
func (p *Proxy) handleFrontendRequest(f *Frontend, env *Envelope, line []byte) {
	// Allocate a proxy ID
	proxyID := p.nextID.Add(1) - 1

	// Save the mapping
	var origID json.RawMessage
	if env.ID != nil {
		origID = append(json.RawMessage(nil), *env.ID...)
	}
	p.pending.Store(proxyID, &PendingRequest{
		originalID: origID,
		frontend:   f,
		method:     env.Method,
		params:     env.Params,
	})

	// Rewrite ID and forward
	rewritten, err := rewriteID(line, proxyID)
	if err != nil {
		log.Printf("failed to rewrite id: %v", err)
		return
	}

	// Synthesize user_message_chunk notifications for session/prompt
	// so all frontends (and the replay cache) see what was typed.
	// Skip the sender — their UI already shows the user's input.
	if env.Method == "session/prompt" {
		p.synthesizeUserMessage(env, f)
	}

	if err := p.sendToAgent(rewritten); err != nil {
		log.Printf("frontend %d: send to agent failed: %v", f.id, err)
		p.pending.Delete(proxyID)
		errResp, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      origID,
			"error": map[string]interface{}{
				"code":    -32603,
				"message": "Agent process exited",
			},
		})
		f.Send(errResp)
		return
	}
}

// synthesizeUserMessage extracts prompt content blocks from a session/prompt
// request and broadcasts them as user_message_chunk notifications to all
// frontends except the sender (whose UI already shows the input), and into
// the cache for late-joining clients.
func (p *Proxy) synthesizeUserMessage(env *Envelope, sender *Frontend) {
	var params struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(env.Params, &params); err != nil {
		log.Printf("synthesize user msg: parse params: %v", err)
		return
	}

	for _, block := range params.Prompt {
		notif := map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]interface{}{
				"sessionId": params.SessionID,
				"update": map[string]interface{}{
					"sessionUpdate": "user_message_chunk",
					"content":       json.RawMessage(block),
				},
			},
		}

		line, err := json.Marshal(notif)
		if err != nil {
			log.Printf("synthesize user msg: marshal: %v", err)
			continue
		}

		p.cache.AddUpdate(line)
		p.broadcastExcept(line, sender)
	}
}

// synthesizeTurnComplete extracts the stopReason from a session/prompt response
// and broadcasts a synthetic session/update notification to all frontends except
// the one that sent the prompt. This lets other frontends know the turn is over
// (they only see streaming notifications, not the response).
func (p *Proxy) synthesizeTurnComplete(responseLine []byte, sender *Frontend) {
	var resp struct {
		Result struct {
			StopReason string `json:"stopReason"`
			SessionID  string `json:"sessionId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(responseLine, &resp); err != nil {
		return
	}

	stopReason := resp.Result.StopReason
	if stopReason == "" {
		return
	}

	// Try to get sessionId from the response; fall back to empty
	sessionID := resp.Result.SessionID

	notif := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]interface{}{
			"sessionId": sessionID,
			"update": map[string]interface{}{
				"sessionUpdate": "turn_complete",
				"stopReason":    stopReason,
			},
		},
	}

	line, err := json.Marshal(notif)
	if err != nil {
		log.Printf("synthesize turn complete: marshal: %v", err)
		return
	}

	p.cache.AddUpdate(line)
	go p.broadcastExcept(line, sender)
}

// synthesizeModeChange broadcasts a current_mode_update notification to all
// frontends when a session/set_mode request succeeds. The agent doesn't emit
// this notification itself, so the proxy must synthesize it from the original
// request params.
func (p *Proxy) synthesizeModeChange(pr *PendingRequest) {
	var params struct {
		SessionID string `json:"sessionId"`
		ModeID    string `json:"modeId"`
	}
	if err := json.Unmarshal(pr.params, &params); err != nil {
		log.Printf("synthesize mode change: parse params: %v", err)
		return
	}

	notif := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]interface{}{
			"sessionId": params.SessionID,
			"update": map[string]interface{}{
				"sessionUpdate": "current_mode_update",
				"currentModeId": params.ModeID,
			},
		},
	}

	line, err := json.Marshal(notif)
	if err != nil {
		log.Printf("synthesize mode change: marshal: %v", err)
		return
	}

	p.cache.AddUpdate(line)
	p.broadcastExcept(line, pr.frontend)
}

// restoreID replaces the "id" field with the original raw JSON value.
func restoreID(line []byte, origID json.RawMessage) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	raw["id"] = origID
	return json.Marshal(raw)
}
