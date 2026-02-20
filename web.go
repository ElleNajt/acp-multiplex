package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/websocket"
)

// serveWeb starts an HTTP server with a WebSocket endpoint that bridges
// to the proxy's Unix socket, plus a minimal chat UI.
// If sockPath is empty, it runs in "discover" mode: scanning the socket
// directory for all live proxies.
func serveWeb(sockPath string, port string, uiFile string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if uiFile != "" {
			http.ServeFile(w, r, uiFile)
		} else {
			w.Write([]byte("<html><body>acp-multiplex: use --ui to serve a UI</body></html>"))
		}
	})

	mux.Handle("/ws", &websocket.Server{
		Handler: func(ws *websocket.Conn) {
			target := sockPath
			// In discover mode, allow ?sock=<pid> to pick which socket
			if target == "" {
				pid := ws.Request().URL.Query().Get("sock")
				if pid == "" {
					log.Printf("ws: no sock param in discover mode")
					ws.Close()
					return
				}
				// Try new location first, then legacy
				target = filepath.Join(socketDir(), pid+".sock")
				if _, err := os.Stat(target); err != nil {
					target = filepath.Join(os.TempDir(), "acp-multiplex-"+pid+".sock")
				}
			}
			handleWebSocket(ws, target)
		},
		Handshake: func(config *websocket.Config, r *http.Request) error {
			config.Origin, _ = websocket.Origin(config, r)
			return nil
		},
	})

	mux.HandleFunc("/api/sessions", handleSessionList)
	mux.HandleFunc("/files/list", handleFileList)
	mux.HandleFunc("/files/read", handleFileRead)

	addr := fmt.Sprintf("127.0.0.1:%s", port)
	log.Printf("web UI: http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// sessionInfo is returned by /api/sessions for each live socket.
type sessionInfo struct {
	Pid       int    `json:"pid"`
	SessionID string `json:"sessionId,omitempty"`
	Title     string `json:"title,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

// discoverSockets finds all acp-multiplex sockets, checking both the new
// location ($TMPDIR/acp-multiplex/<pid>.sock) and legacy format
// ($TMPDIR/acp-multiplex-<pid>.sock).
func discoverSockets() []struct {
	pid  int
	path string
} {
	var socks []struct {
		pid  int
		path string
	}
	seen := map[int]bool{}

	// New location: $TMPDIR/acp-multiplex/<pid>.sock
	dir := socketDir()
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".sock") {
				continue
			}
			pidStr := strings.TrimSuffix(name, ".sock")
			pid, err := strconv.Atoi(pidStr)
			if err != nil {
				continue
			}
			if syscall.Kill(pid, 0) != nil {
				os.Remove(filepath.Join(dir, name))
				continue
			}
			seen[pid] = true
			socks = append(socks, struct {
				pid  int
				path string
			}{pid, filepath.Join(dir, name)})
		}
	}

	// Legacy location: $TMPDIR/acp-multiplex-<pid>.sock
	tmpdir := os.TempDir()
	if entries, err := os.ReadDir(tmpdir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "acp-multiplex-") || !strings.HasSuffix(name, ".sock") {
				continue
			}
			pidStr := strings.TrimPrefix(name, "acp-multiplex-")
			pidStr = strings.TrimSuffix(pidStr, ".sock")
			pid, err := strconv.Atoi(pidStr)
			if err != nil {
				continue
			}
			if seen[pid] {
				continue
			}
			if syscall.Kill(pid, 0) != nil {
				os.Remove(filepath.Join(tmpdir, name))
				continue
			}
			socks = append(socks, struct {
				pid  int
				path string
			}{pid, filepath.Join(tmpdir, name)})
		}
	}

	return socks
}

// handleSessionList scans for live proxies and probes each for session info.
func handleSessionList(w http.ResponseWriter, r *http.Request) {
	socks := discoverSockets()

	type result struct {
		info sessionInfo
		ok   bool
	}

	var wg sync.WaitGroup
	results := make([]result, len(socks))

	for i, s := range socks {
		idx := i
		sock := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := probeSocket(sock.path, sock.pid)
			if err != nil {
				log.Printf("probe %s: %v", sock.path, err)
				return
			}
			results[idx] = result{info: info, ok: true}
		}()
	}
	wg.Wait()

	var sessions []sessionInfo
	for _, r := range results {
		if r.ok {
			sessions = append(sessions, r.info)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"sessions": sessions})
}

// probeSocket connects to a proxy socket, reads the replay messages,
// and extracts session info (sessionId, title/agentInfo, cwd).
func probeSocket(sockPath string, pid int) (sessionInfo, error) {
	info := sessionInfo{Pid: pid}

	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return info, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg struct {
			Result json.RawMessage `json:"result"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		// Check responses for agentInfo and sessionId
		if msg.Result != nil {
			var res struct {
				AgentInfo *struct {
					Title string `json:"title"`
					Name  string `json:"name"`
				} `json:"agentInfo"`
				SessionID string `json:"sessionId"`
				Cwd       string `json:"cwd"`
			}
			if err := json.Unmarshal(msg.Result, &res); err == nil {
				if res.AgentInfo != nil {
					info.Title = res.AgentInfo.Title
					if info.Title == "" {
						info.Title = res.AgentInfo.Name
					}
				}
				if res.SessionID != "" {
					info.SessionID = res.SessionID
				}
				if res.Cwd != "" {
					info.Cwd = res.Cwd
				}
			}
		}

		// Check for title updates in session notifications
		if msg.Method == "session/update" && msg.Params != nil {
			var params struct {
				Update struct {
					Kind  string `json:"sessionUpdate"`
					Title string `json:"title"`
				} `json:"update"`
			}
			if err := json.Unmarshal(msg.Params, &params); err == nil {
				if params.Update.Kind == "title_update" && params.Update.Title != "" {
					info.Title = params.Update.Title
				}
			}
		}
	}

	return info, nil
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

// handleWebSocket bridges a WebSocket connection to a proxy's Unix socket.
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
