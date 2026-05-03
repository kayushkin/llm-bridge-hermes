package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestDashboardListSessions(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	sessions := []hermesSession{
		{ID: "sess-1", Title: "First session", Messages: 5, CreatedAt: now, UpdatedAt: now},
		{ID: "sess-2", Title: "Second session", Messages: 10, CreatedAt: now, UpdatedAt: now},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions" {
			t.Errorf("path = %q, want /api/sessions", r.URL.Path)
		}
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		json.NewEncoder(w).Encode(sessions)
	}))
	defer server.Close()

	client := newDashboardClient(server.URL, "")
	got, err := client.listSessions(t.Context())
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	if got[0].ID != "sess-1" || got[1].ID != "sess-2" {
		t.Errorf("session IDs = [%s, %s], want [sess-1, sess-2]", got[0].ID, got[1].ID)
	}
}

func TestDashboardListSessions_AuthHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Errorf("Authorization = %q, want 'Bearer secret-key'", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode([]hermesSession{})
	}))
	defer server.Close()

	client := newDashboardClient(server.URL, "secret-key")
	_, err := client.listSessions(t.Context())
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
}

func TestDashboardListSessions_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("dashboard down"))
	}))
	defer server.Close()

	client := newDashboardClient(server.URL, "")
	_, err := client.listSessions(t.Context())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestToStoredSessions(t *testing.T) {
	now := time.Now()
	input := []hermesSession{
		{ID: "s1", Title: "hello world", Messages: 3, CreatedAt: now, UpdatedAt: now},
	}

	got := toStoredSessions(input)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	s := got[0]
	if s.HarnessSessionID != "s1" {
		t.Errorf("HarnessSessionID = %q, want s1", s.HarnessSessionID)
	}
	if s.BridgeSessionID != "" {
		t.Errorf("BridgeSessionID = %q, want empty (hermes has no chain head)", s.BridgeSessionID)
	}
	if s.Harness != msg.HarnessHermes {
		t.Errorf("Harness = %q, want hermes", s.Harness)
	}
	if s.Prompt != "hello world" {
		t.Errorf("Prompt = %q, want 'hello world'", s.Prompt)
	}
	if s.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", s.TurnCount)
	}
}

func TestToStoredSessions_Empty(t *testing.T) {
	got := toStoredSessions(nil)
	if len(got) != 0 {
		t.Errorf("got %d sessions for nil input, want 0", len(got))
	}
}

func TestDiscoverSessions_NoDashboardURL(t *testing.T) {
	cfg := Config{}
	_, err := discoverSessions(cfg)
	if err == nil {
		t.Fatal("expected error when DashboardURL is empty")
	}
}

func TestDiscoverSessions_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]hermesSession{
			{ID: "discovered-1", Title: "test", Messages: 1},
		})
	}))
	defer server.Close()

	cfg := Config{DashboardURL: server.URL}
	sessions, err := discoverSessions(cfg)
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].HarnessSessionID != "discovered-1" {
		t.Errorf("sessions = %+v, want 1 session with HarnessSessionID discovered-1", sessions)
	}
}
