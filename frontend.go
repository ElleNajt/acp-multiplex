package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"os"
	"sync"
)

// Frontend represents a connected ACP client.
type Frontend struct {
	id      int
	primary bool
	scanner *bufio.Scanner
	writer  io.Writer
	mu      sync.Mutex // protects writer
	done    chan struct{}
}

// Send writes a JSON line to this frontend. Thread-safe.
func (f *Frontend) Send(line []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Append newline
	f.writer.Write(line)
	f.writer.Write([]byte("\n"))
}

// NewStdioFrontend creates a frontend connected to stdin/stdout.
func NewStdioFrontend(id int) *Frontend {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer
	return &Frontend{
		id:      id,
		primary: true,
		scanner: scanner,
		writer:  os.Stdout,
		done:    make(chan struct{}),
	}
}

// NewSocketFrontend creates a frontend from a Unix socket connection.
func NewSocketFrontend(id int, conn net.Conn) *Frontend {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	return &Frontend{
		id:      id,
		primary: false,
		scanner: scanner,
		writer:  conn,
		done:    make(chan struct{}),
	}
}

// ReadLines reads ndjson lines from the frontend and sends them to ch.
// Closes ch and done when the connection ends.
func (f *Frontend) ReadLines(ch chan<- FrontendMessage) {
	defer close(f.done)
	for f.scanner.Scan() {
		line := make([]byte, len(f.scanner.Bytes()))
		copy(line, f.scanner.Bytes())
		ch <- FrontendMessage{Frontend: f, Line: line}
	}
	if err := f.scanner.Err(); err != nil {
		log.Printf("frontend %d read error: %v", f.id, err)
	}
}

// FrontendMessage pairs a raw JSON line with the frontend that sent it.
type FrontendMessage struct {
	Frontend *Frontend
	Line     []byte
}
