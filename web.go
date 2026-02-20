package main

import (
	_ "embed"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	"golang.org/x/net/websocket"
)

//go:embed index.html
var indexHTML string

// serveWeb starts an HTTP server with a WebSocket endpoint that bridges
// to the proxy's Unix socket, plus a minimal chat UI.
func serveWeb(sockPath string, port string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(indexHTML))
	})

	mux.Handle("/ws", &websocket.Server{
		Handler: func(ws *websocket.Conn) {
			handleWebSocket(ws, sockPath)
		},
		Handshake: func(config *websocket.Config, r *http.Request) error {
			config.Origin, _ = websocket.Origin(config, r)
			return nil // accept any origin (localhost only)
		},
	})

	addr := fmt.Sprintf("127.0.0.1:%s", port)
	log.Printf("web UI: http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// handleWebSocket bridges a WebSocket connection to the proxy's Unix socket.
// Messages from WebSocket are sent as ndjson to the socket.
// Lines from the socket are sent as WebSocket text frames.
func handleWebSocket(ws *websocket.Conn, sockPath string) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Printf("ws: connect to %s: %v", sockPath, err)
		ws.Close()
		return
	}
	defer conn.Close()

	var once sync.Once
	closeAll := func() {
		ws.Close()
		conn.Close()
	}

	// WebSocket -> Unix socket (ndjson lines)
	go func() {
		defer once.Do(closeAll)
		for {
			var msg string
			if err := websocket.Message.Receive(ws, &msg); err != nil {
				if err != io.EOF {
					log.Printf("ws recv: %v", err)
				}
				return
			}
			// Write as ndjson line
			conn.Write([]byte(msg))
			conn.Write([]byte("\n"))
		}
	}()

	// Unix socket -> WebSocket (line by line)
	func() {
		defer once.Do(closeAll)
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, err := conn.Read(tmp)
			if err != nil {
				if err != io.EOF {
					log.Printf("ws sock read: %v", err)
				}
				return
			}
			buf = append(buf, tmp[:n]...)
			// Split on newlines and send each complete line
			for {
				nlIdx := -1
				for i, b := range buf {
					if b == '\n' {
						nlIdx = i
						break
					}
				}
				if nlIdx < 0 {
					break
				}
				line := buf[:nlIdx]
				if len(line) > 0 {
					websocket.Message.Send(ws, string(line))
				}
				buf = buf[nlIdx+1:]
			}
		}
	}()
}
