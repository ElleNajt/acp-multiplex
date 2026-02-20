package main

import (
	"encoding/json"
	"sync"
)

// Cache stores messages for replaying to late-joining frontends.
// Streaming chunks (agent_message_chunk, agent_thought_chunk) are
// coalesced into single messages so replay is compact and fast.
type Cache struct {
	mu       sync.Mutex
	initResp []byte   // cached initialize response
	newResp  []byte   // cached session/new response
	updates  [][]byte // coalesced session/update notifications

	// Accumulator for the current run of chunks
	chunkType string // "agent_message_chunk" or "agent_thought_chunk", or ""
	chunkText string
	chunkMeta updateMeta // sessionId etc from first chunk in run
}

// updateMeta captures the envelope fields needed to reconstruct a coalesced notification.
type updateMeta struct {
	SessionID string `json:"sessionId"`
}

func NewCache() *Cache {
	return &Cache{}
}

func (c *Cache) SetInitResponse(line []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initResp = append([]byte(nil), line...)
}

func (c *Cache) SetNewResponse(line []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.newResp = append([]byte(nil), line...)
}

func (c *Cache) AddUpdate(line []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Parse the update type and content
	kind, text, sessionID := parseUpdateType(line)

	switch kind {
	case "agent_message_chunk", "agent_thought_chunk":
		if c.chunkType == kind {
			// Same chunk type — accumulate
			c.chunkText += text
		} else {
			// Different type — flush previous, start new
			c.flushChunks()
			c.chunkType = kind
			c.chunkText = text
			c.chunkMeta = updateMeta{SessionID: sessionID}
		}
	default:
		// Non-chunk update — flush any pending chunks, then store
		c.flushChunks()
		c.updates = append(c.updates, append([]byte(nil), line...))
	}
}

// flushChunks coalesces accumulated chunks into a single notification.
// Must be called with c.mu held.
func (c *Cache) flushChunks() {
	if c.chunkType == "" {
		return
	}

	notif := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]interface{}{
			"sessionId": c.chunkMeta.SessionID,
			"update": map[string]interface{}{
				"sessionUpdate": c.chunkType,
				"content": map[string]string{
					"type": "text",
					"text": c.chunkText,
				},
			},
		},
	}

	if line, err := json.Marshal(notif); err == nil {
		c.updates = append(c.updates, line)
	}

	c.chunkType = ""
	c.chunkText = ""
}

// Replay sends the cached session history to a frontend.
func (c *Cache) Replay(f *Frontend) {
	c.mu.Lock()
	// Flush any in-progress chunks so replay is complete
	c.flushChunks()

	// Snapshot under lock
	initResp := c.initResp
	newResp := c.newResp
	updates := make([][]byte, len(c.updates))
	copy(updates, c.updates)
	c.mu.Unlock()

	if initResp != nil {
		f.Send(initResp)
	}
	if newResp != nil {
		f.Send(newResp)
	}
	for _, u := range updates {
		f.Send(u)
	}
}

// parseUpdateType extracts the sessionUpdate type and text content from a notification.
func parseUpdateType(line []byte) (kind, text, sessionID string) {
	var msg struct {
		Params struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return "", "", ""
	}
	return msg.Params.Update.SessionUpdate, msg.Params.Update.Content.Text, msg.Params.SessionID
}
