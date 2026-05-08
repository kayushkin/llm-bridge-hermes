// llm-bridge-hermes is a harness binary that bridges llm-bridge's subprocess
// protocol (NDJSON on stdin/stdout) to the Hermes Agent API.
//
// Hermes exposes an OpenAI-compatible HTTP API with SSE streaming.
// This bridge uses the /v1/responses endpoint for server-side conversation
// state and translates SSE events into canonical msg.Event NDJSON.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var emitMu sync.Mutex

func emitEvent(ev any) {
	emitMu.Lock()
	defer emitMu.Unlock()

	data, err := json.Marshal(ev)
	if err != nil {
		log.Printf("failed to marshal event: %v", err)
		return
	}
	data = append(data, '\n')
	os.Stdout.Write(data)
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("[llm-bridge-hermes] ")

	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println("0.2.0")
		os.Exit(0)
	}

	cfg := loadConfig()

	if len(os.Args) > 1 && os.Args[1] == "-discover" {
		// Persistent sessions live in the Hermes web dashboard, not on this
		// host. When HERMES_DASHBOARD_URL is unset the harness has no source
		// to enumerate, so the contract-correct response is an empty array
		// (matches forgecode/aider/nanoclaw shape for stateless-from-the-
		// harness's-view-of-disk).
		if cfg.DashboardURL == "" {
			fmt.Println("[]")
			os.Exit(0)
		}
		sessions, err := discoverSessions(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "discover: %v\n", err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(sessions)
		os.Exit(0)
	}

	// -import-history is part of the conformance contract but not yet
	// implemented for hermes. Exit 2 to signal "unsupported" rather than
	// silently falling through to the JSON-RPC loop, which would otherwise
	// show up as a false-positive PASS on the conformance dashboard.
	if len(os.Args) > 1 && os.Args[1] == "-import-history" {
		fmt.Fprintln(os.Stderr, "llm-bridge-hermes: -import-history not yet implemented")
		os.Exit(2)
	}

	h := newHarness(cfg)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("received %v, shutting down", sig)
		h.shutdown()
		os.Exit(0)
	}()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("invalid request: %v", err)
			continue
		}

		log.Printf("request: method=%s", req.Method)
		if err := h.handleRequest(req); err != nil {
			log.Printf("handler error: method=%s err=%v", req.Method, err)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("stdin scanner error: %v", err)
	}

	log.Printf("stdin closed, shutting down")
	h.shutdown()
}
