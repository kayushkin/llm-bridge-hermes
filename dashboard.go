package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// dashboardClient talks to the Hermes web dashboard API (port 9119 by default).
// This is separate from the /v1 OpenAI-compatible API and provides session
// browsing, search, and analytics.
type dashboardClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func newDashboardClient(baseURL, apiKey string) *dashboardClient {
	return &dashboardClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (d *dashboardClient) doJSON(ctx context.Context, method, path string, out any) error {
	httpReq, err := http.NewRequestWithContext(ctx, method, d.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if d.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+d.apiKey)
	}

	resp, err := d.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return &apiError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("dashboard API error: %s %s -> %d %s", method, path, resp.StatusCode, string(b)),
		}
	}

	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// hermesSession is the shape returned by GET /api/sessions and /api/sessions/{id}.
type hermesSession struct {
	ID        string    `json:"id"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  int       `json:"messages,omitempty"`
	Title     string    `json:"title,omitempty"`
}

// listSessions returns all Hermes sessions from the dashboard API.
func (d *dashboardClient) listSessions(ctx context.Context) ([]hermesSession, error) {
	var sessions []hermesSession
	if err := d.doJSON(ctx, "GET", "/api/sessions", &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// toStoredSessions converts dashboard sessions to the canonical StoredSession type.
func toStoredSessions(sessions []hermesSession) []msg.StoredSession {
	out := make([]msg.StoredSession, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, msg.StoredSession{
			ID:        s.ID,
			Harness:   msg.HarnessHermes,
			Prompt:    s.Title,
			CreatedAt: s.CreatedAt,
			UpdatedAt: s.UpdatedAt,
			TurnCount: s.Messages,
		})
	}
	return out
}

// discoverSessions connects to the Hermes dashboard API and returns stored sessions.
func discoverSessions(cfg Config) ([]msg.StoredSession, error) {
	if cfg.DashboardURL == "" {
		return nil, fmt.Errorf("HERMES_DASHBOARD_URL not set; session discovery requires the dashboard API")
	}

	client := newDashboardClient(cfg.DashboardURL, cfg.DashboardKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sessions, err := client.listSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return toStoredSessions(sessions), nil
}
