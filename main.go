package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage:\n")
		fmt.Fprintf(os.Stderr, "  acp-multiplex <agent-command> [args...]   Start proxy with agent\n")
		fmt.Fprintf(os.Stderr, "  acp-multiplex attach <socket-path>        Connect stdio to existing proxy\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "attach":
		runAttach()
	default:
		runProxy()
	}
}

// runProxy starts the agent subprocess and multiplexing proxy.
func runProxy() {
	cleanStaleSockets()

	agentArgs := os.Args[1:]

	cmd := exec.Command(agentArgs[0], agentArgs[1:]...)
	agentIn, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("agent stdin pipe: %v", err)
	}
	agentOut, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("agent stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Fatalf("start agent: %v", err)
	}

	cache := NewCache()

	// If a session name is provided, cache it as metadata for secondary frontends.
	if name := os.Getenv("ACP_MULTIPLEX_NAME"); name != "" {
		meta, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "acp-multiplex/meta",
			"params":  map[string]string{"name": name},
		})
		cache.SetMeta(meta)
	}

	proxy := NewProxy(agentIn, agentOut, cache)
	proxy.sockPath = socketPath()

	// Primary frontend on stdin/stdout
	primary := NewStdioFrontend(0)
	proxy.AddFrontend(primary)

	// Unix socket for additional frontends
	sockPath := socketPath()
	ln, err := listenUnix(sockPath)
	if err != nil {
		log.Fatalf("listen unix: %v", err)
	}
	defer os.Remove(sockPath)
	fmt.Fprintf(os.Stderr, "acp-multiplex: socket %s\n", sockPath)

	nextID := 1
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("accept: %v", err)
				return
			}
			nextID++
			f := NewSocketFrontend(nextID, conn)
			proxy.AddFrontend(f)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Remove(sockPath)
		cmd.Process.Kill()
		os.Exit(0)
	}()

	go proxy.Run()
	cmd.Wait()
}

// runAttach bridges stdin/stdout to an existing proxy's Unix socket.
// This lets stdio-only ACP clients (Toad, acp-ui) connect as secondary frontends.
func runAttach() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: acp-multiplex attach <socket-path>\n")
		os.Exit(1)
	}
	sockPath := os.Args[2]

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Fatalf("connect to %s: %v", sockPath, err)
	}
	defer conn.Close()

	// Bidirectional pipe: stdin -> socket, socket -> stdout
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, os.Stdin); done <- struct{}{} }()
	go func() { io.Copy(os.Stdout, conn); done <- struct{}{} }()
	<-done
}

// socketDir returns the directory for acp-multiplex sockets, creating it if needed.
func socketDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "acp-multiplex")
	os.MkdirAll(dir, 0700)
	return dir
}

func socketPath() string {
	return filepath.Join(socketDir(), fmt.Sprintf("%d.sock", os.Getpid()))
}

// cleanStaleSockets removes sockets whose owning process is dead.
func cleanStaleSockets() {
	dir := socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
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
		if err := syscall.Kill(pid, 0); err != nil {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

func listenUnix(path string) (net.Listener, error) {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Owner-only so other users on the machine can't connect.
	os.Chmod(path, 0600)
	return ln, nil
}
