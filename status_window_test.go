package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// This file exists for card `3c18632a`: a suite can name a numeric boundary,
// exercise the mechanism that boundary guards on every row, and still have every
// row land on the same side, at which point the boundary is documented,
// exercised and unpinned all at once. Three windows in this repo were in exactly
// that state — `hermesClient.doJSON`'s 2xx test, `dashboardClient.doJSON`'s
// identical copy of it, and the retryable band on an API error. Every fixture
// that reached them sent 200, 429 or 500, so no edge had a fixture on both
// sides. Scored against mutation: 2 of 13 edges held before this file, 13 of 13
// after. Instruments and both runs: scripts/sabotage-status-windows.py.
//
// Every case here names BOTH sides of an edge. A row on one side alone leaves
// the edge free anywhere between the nearest fixtures, which is the defect this
// file is about.

// statusOnlyTransport answers every request with one fixed status code, without
// a server in the way.
//
// ⚠️ A net/http server cannot deliver 199 as a final status: WriteHeader with a
// 1xx code writes an interim response and leaves the handler to send a real one
// afterwards. 199 is the lower edge of the window under test, so producing it is
// the whole point, and a round tripper is the only thing here that can. Every
// row in the two doJSON tests therefore goes through this same transport — the
// status code is then the ONLY thing that differs between a passing row and a
// failing one, which is what makes the pair a comparison rather than two
// separate assertions.
type statusOnlyTransport struct {
	code int
	body string
}

func (t statusOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: t.code,
		Status:     fmt.Sprintf("%d synthetic", t.code),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Request:    req,
		Close:      true,
	}, nil
}

func fixedStatusClient(code int, body string) *http.Client {
	return &http.Client{Transport: statusOnlyTransport{code: code, body: body}}
}

// The four rows of the window `resp.StatusCode < 200 || resp.StatusCode >= 300`,
// straddling both of its edges. 199/200 pins where success starts, 299/300 pins
// where it stops.
var doJSONWindowRows = []struct {
	code    int
	success bool
	why     string
}{
	{199, false, "below the window's lower edge"},
	{200, true, "the window's lower edge itself"},
	{299, true, "the last code inside the window"},
	{300, false, "the window's upper edge, which is outside it"},
}

func TestHermesClientJSONWindowIsStraddledAtBothEdges(t *testing.T) {
	for _, row := range doJSONWindowRows {
		t.Run(fmt.Sprintf("%d", row.code), func(t *testing.T) {
			c := testClient("http://hermes.invalid")
			c.httpClient = fixedStatusClient(row.code, `{"status":"ok"}`)

			var out map[string]string
			err := c.doJSON(t.Context(), "GET", "/v1/health", &out)

			if row.success {
				if err != nil {
					t.Fatalf("doJSON on %d (%s): got error %v, want success — the "+
						"hermes success window no longer admits %d", row.code, row.why, err, row.code)
				}
				if out["status"] != "ok" {
					t.Errorf("doJSON on %d: body was not decoded, out = %v", row.code, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("doJSON on %d (%s): got success, want an error — the hermes "+
					"success window now admits %d", row.code, row.why, row.code)
			}
			apiErr, ok := err.(*apiError)
			if !ok {
				t.Fatalf("doJSON on %d: error is %T, want *apiError", row.code, err)
			}
			if apiErr.StatusCode != row.code {
				t.Errorf("doJSON on %d: apiError.StatusCode = %d, want %d",
					row.code, apiErr.StatusCode, row.code)
			}
		})
	}
}

// The same four rows against the second implementation.
//
// ⭐ `dashboardClient.doJSON` carries its own copy of the window — the two blocks
// are byte-identical for three lines and diverge only at the Message format
// string. A row that pins one says nothing about the other, and a reader who
// greps `resp.StatusCode < 200`, finds one hit near a test, and stops, will
// believe both are held. Scored separately: both copies were free at three of
// their four edges.
func TestDashboardClientJSONWindowIsStraddledAtBothEdges(t *testing.T) {
	for _, row := range doJSONWindowRows {
		t.Run(fmt.Sprintf("%d", row.code), func(t *testing.T) {
			d := newDashboardClient("http://dashboard.invalid", "")
			d.httpClient = fixedStatusClient(row.code, `[]`)

			var out []hermesSession
			err := d.doJSON(t.Context(), "GET", "/api/sessions", &out)

			if row.success {
				if err != nil {
					t.Fatalf("dashboard doJSON on %d (%s): got error %v, want success — "+
						"the dashboard success window no longer admits %d",
						row.code, row.why, err, row.code)
				}
				return
			}
			if err == nil {
				t.Fatalf("dashboard doJSON on %d (%s): got success, want an error — the "+
					"dashboard success window now admits %d", row.code, row.why, row.code)
			}
			apiErr, ok := err.(*apiError)
			if !ok {
				t.Fatalf("dashboard doJSON on %d: error is %T, want *apiError", row.code, err)
			}
			if apiErr.StatusCode != row.code {
				t.Errorf("dashboard doJSON on %d: apiError.StatusCode = %d, want %d",
					row.code, apiErr.StatusCode, row.code)
			}
		})
	}
}

// handler.go says "5xx errors and 429 are typically retryable" and computes
//
//	retryable = (statusCode >= 500 && statusCode < 600) || statusCode == 429
//
// Before this test nothing asserted Retryable for an API error at all — the only
// Retryable assertion in the suite was FORK_UNSUPPORTED, which never reaches
// this line. So the band's start, its end and the 429 special case were three
// free axes on one line, and the sentence above them was the only thing saying
// what they were meant to be.
//
// 428 and 430 are here because `>= 500` widened to `>= 429` is invisible to a
// suite that only ever sends 429 itself: the special case answers for the band.
func TestAPIErrorRetryableBandIsStraddledOnAllThreeAxes(t *testing.T) {
	rows := []struct {
		code      int
		retryable bool
		why       string
	}{
		{428, false, "one below the rate-limit special case"},
		{429, true, "the rate-limit special case itself"},
		{430, false, "one above the special case, and still below the 5xx band"},
		{499, false, "the last code below the 5xx band"},
		{500, true, "the 5xx band's lower edge"},
		{599, true, "the last code inside the 5xx band"},
		{600, false, "the 5xx band's upper edge, which is outside it"},
	}

	for _, row := range rows {
		t.Run(fmt.Sprintf("%d", row.code), func(t *testing.T) {
			h := testHarness()
			h.client.httpClient = fixedStatusClient(row.code, `{"error":"synthetic"}`)
			getEvents := captureEvents(t)

			// A non-200 makes sendResponses return an *apiError carrying the
			// status, which is the only input the retryable line reads.
			if err := h.sendMessageDirect("hello", "idem-key"); err == nil {
				t.Fatalf("sendMessageDirect on %d: expected an error", row.code)
			}

			events := getEvents()
			var errEvent *msg.ErrorEvent
			for i := range events {
				if events[i].Type == msg.EventError && events[i].Error != nil &&
					events[i].Error.Code == "API_ERROR" {
					errEvent = events[i].Error
				}
			}
			if errEvent == nil {
				t.Fatalf("sendMessageDirect on %d: no API_ERROR event in %d events",
					row.code, len(events))
			}
			if errEvent.StatusCode != row.code {
				t.Errorf("API_ERROR on %d: StatusCode = %d, want %d",
					row.code, errEvent.StatusCode, row.code)
			}
			if errEvent.Retryable != row.retryable {
				t.Errorf("API_ERROR on %d (%s): Retryable = %v, want %v",
					row.code, row.why, errEvent.Retryable, row.retryable)
			}
		})
	}
}
