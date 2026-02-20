package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/net/websocket"
)

//go:embed index.html
var indexHTML string

// serveWeb starts an HTTP server with a WebSocket endpoint that bridges
// to the proxy's Unix socket, plus a minimal chat UI.
func serveWeb(sockPath string, port string, uiFile string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if uiFile != "" {
			http.ServeFile(w, r, uiFile)
		} else {
			w.Write([]byte(indexHTML))
		}
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

	mux.HandleFunc("/files/list", handleFileList)
	mux.HandleFunc("/files/read", handleFileRead)

	addr := fmt.Sprintf("127.0.0.1:%s", port)
	log.Printf("web UI: http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

type fileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size,omitempty"`
}

func handleFileList(w http.ResponseWriter, r *http.Request) {
	dirPath := r.URL.Query().Get("path")
	if dirPath == "" {
		dirPath = "."
	}
	showHidden := r.URL.Query().Get("show_hidden") == "true"

	// Resolve to absolute, clean path
	absPath, err := filepath.Abs(dirPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	var files []fileEntry
	for _, e := range entries {
		name := e.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fe := fileEntry{Name: name, IsDir: e.IsDir()}
		if !e.IsDir() {
			fe.Size = info.Size()
		}
		files = append(files, fe)
	}

	// Sort: dirs first, then alphabetical
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"path":  absPath,
		"files": files,
	})
}

func handleFileRead(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}

	// 1MB limit for text files
	const maxSize = 1024 * 1024
	if info.Size() > maxSize {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"path":    absPath,
		"content": string(data),
	})
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
			if len(buf) > 1024*1024 {
				log.Printf("ws: buffer exceeded 1MB, disconnecting")
				return
			}
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
