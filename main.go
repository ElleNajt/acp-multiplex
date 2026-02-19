package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: acp-multiplex <agent-command> [args...]\n")
		fmt.Fprintf(os.Stderr, "\nSpawns the agent and multiplexes ACP sessions.\n")
		fmt.Fprintf(os.Stderr, "Primary frontend: stdin/stdout of this process.\n")
		fmt.Fprintf(os.Stderr, "Additional frontends: connect to the Unix socket printed below.\n")
		os.Exit(1)
	}

	agentArgs := os.Args[1:]

	// Start agent subprocess
	cmd := exec.Command(agentArgs[0], agentArgs[1:]...)
	agentIn, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("agent stdin pipe: %v", err)
	}
	agentOut, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("agent stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr // forward agent stderr

	if err := cmd.Start(); err != nil {
		log.Fatalf("start agent: %v", err)
	}

	cache := NewCache()
	proxy := NewProxy(agentIn, agentOut, cache)

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
			log.Printf("frontend %d connected", nextID)
		}
	}()

	// Clean up on signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Remove(sockPath)
		cmd.Process.Kill()
		os.Exit(0)
	}()

	// Run proxy (blocks until agent exits)
	go proxy.Run()
	cmd.Wait()
}

func socketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, fmt.Sprintf("acp-multiplex-%d.sock", os.Getpid()))
}

func listenUnix(path string) (net.Listener, error) {
	// Remove stale socket if it exists
	os.Remove(path)
	return net.Listen("unix", path)
}
