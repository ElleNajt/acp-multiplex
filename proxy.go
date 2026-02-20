package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"sync"
	"sync/atomic"
)

// PendingRequest tracks a request forwarded to the agent so we can
// route the response back to the correct frontend with the correct ID.
type PendingRequest struct {
	originalID json.RawMessage
	frontend   *Frontend
	method     string // for cache decisions
}

// Proxy is the core multiplexer. It reads from the agent and fans out
// to frontends, and reads from frontends and forwards to the agent.
type Proxy struct {
	agentIn  io.Writer      // agent's stdin
	agentOut *bufio.Scanner // agent's stdout

	mu        sync.Mutex
	frontends []*Frontend

	nextID  atomic.Int64
	pending sync.Map // proxyID (int64) -> *PendingRequest

	cache *Cache

	// Channel for messages from all frontends
	fromFrontends chan FrontendMessage
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
func (p *Proxy) sendToAgent(line []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agentIn.Write(line)
	p.agentIn.Write([]byte("\n"))
}

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
			// Route to primary frontend.
			p.routeReverseCall(line)

		default:
			log.Printf("agent: unclassifiable message")
		}
	}
	if err := p.agentOut.Err(); err != nil {
		log.Printf("agent stdout error: %v", err)
	}
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

	// Rewrite ID back to the frontend's original
	rewritten, err := restoreID(line, pr.originalID)
	if err != nil {
		log.Printf("failed to restore id: %v", err)
		return
	}
	pr.frontend.Send(rewritten)
}

// routeReverseCall sends an agent-initiated request to the primary frontend.
func (p *Proxy) routeReverseCall(line []byte) {
	p.sendToPrimary(line)
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
			p.sendToAgent(msg.Line)

		case KindResponse:
			// Response to a reverse call — forward directly to agent
			p.sendToAgent(msg.Line)

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

	p.sendToAgent(rewritten)
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

// restoreID replaces the "id" field with the original raw JSON value.
func restoreID(line []byte, origID json.RawMessage) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	raw["id"] = origID
	return json.Marshal(raw)
}
