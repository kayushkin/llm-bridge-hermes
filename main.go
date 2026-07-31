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
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/kayushkin/llm-bridge/ndjson"
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

	// SIGINT is the whole interrupt contract. POST /sessions/{id}/interrupt is
	// not a JSON-RPC method — bridge-server's Manager.Stop calls
	// Process.Interrupt, which signals SIGINT, and Manager.SendJSONRPC (the only
	// path that could deliver an `interrupt` method) has no caller in that repo.
	// Having sent it the server marks the session idle and KEEPS the process
	// registered, so the contract is "cancel the in-flight turn and stay alive".
	// Shutting down here ended the whole session instead: pressing Stop on a
	// hermes session killed the harness and every later message met a dead
	// process. SIGTERM still means shut down.
	//
	// Range, not a single read: a second Stop in the same session used to sit
	// unread in this channel and do nothing at all.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go watchSignals(h, sigs, func() { os.Exit(0) })

	// ndjson.ReadLine carries no practical line cap and reports an oversized
	// line as its own error, so a single large request (a pasted image, a large
	// tool result) no longer looks like a closed stdin and kills the session —
	// the failure mode of the old bufio.Scanner, whose over-cap line ended the
	// scan indistinguishably from EOF.
	reader := bufio.NewReader(os.Stdin)

	for {
		line, readErr := ndjson.ReadLine(reader, ndjson.MaxLineBytes)
		if errors.Is(readErr, ndjson.ErrLineTooLong) {
			log.Printf("dropping request line above %d bytes; session continues", ndjson.MaxLineBytes)
			continue
		}
		if len(line) == 0 {
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					log.Printf("stdin read error: %v", readErr)
				}
				break
			}
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

	log.Printf("stdin closed, shutting down")
	h.shutdown()
}

// watchSignals runs the interrupt contract until the signal channel closes.
// terminate is what ends the process on SIGTERM; main passes os.Exit and the
// test passes a recorder, because a handler that can only be observed by dying
// cannot be pinned.
func watchSignals(h *harness, sigs <-chan os.Signal, terminate func()) {
	for sig := range sigs {
		if sig == syscall.SIGINT {
			log.Printf("received %v, cancelling the in-flight turn", sig)
			if err := h.handleInterrupt(); err != nil {
				log.Printf("interrupt: %v", err)
			}
			continue
		}
		log.Printf("received %v, shutting down", sig)
		h.shutdown()
		terminate()
		return
	}
}
