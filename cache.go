package main

import "sync"

// Cache stores messages for replaying to late-joining frontends.
type Cache struct {
	mu       sync.Mutex
	initResp []byte   // cached initialize response
	newResp  []byte   // cached session/new response
	updates  [][]byte // all session/update notifications, in order
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
	cp := append([]byte(nil), line...)
	c.updates = append(c.updates, cp)
}

// Replay sends the cached session history to a frontend.
func (c *Cache) Replay(f *Frontend) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.initResp != nil {
		f.Send(c.initResp)
	}
	if c.newResp != nil {
		f.Send(c.newResp)
	}
	for _, u := range c.updates {
		f.Send(u)
	}
}
