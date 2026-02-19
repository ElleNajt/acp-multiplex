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

	promptMu sync.Mutex // serialize session/prompt turns

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
	p.mu.Lock()
	fes := make([]*Frontend, len(p.frontends))
	copy(fes, p.frontends)
	p.mu.Unlock()

	for _, f := range fes {
		f.Send(line)
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

	// Serialize prompts — only one at a time
	if env.Method == "session/prompt" {
		p.promptMu.Lock()
		p.sendToAgent(rewritten)
		// We release promptMu when we get the response — but that's
		// complex to wire up. For now, release immediately and rely
		// on frontends behaving. TODO: proper prompt serialization.
		p.promptMu.Unlock()
	} else {
		p.sendToAgent(rewritten)
	}
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
