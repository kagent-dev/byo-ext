package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func TestPrepare(t *testing.T) {
	body, err := Prepare(`{"state":{"title":"Crash"},"questions":{"kind":{"type":"choice","criteria":{"bug":"A defect."}},"ready":{"type":"noul","instructions":"Ready?"}}}`, "jev-latest", "Classify this issue.")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model     string
		State     map[string]any
		Questions map[string]map[string]any
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "jev-latest" || got.State["title"] != "Crash" ||
		got.Questions["kind"]["instructions"] != "Classify this issue." || got.Questions["ready"]["instructions"] != "Ready?" ||
		got.Questions["kind"]["criteria"].(map[string]any)["bug"] != "A defect." {
		t.Fatalf("request = %s", body)
	}
	// The request's own model wins.
	body, _ = Prepare(`{"model":"jev-1.13","state":"x","questions":{"q":{"type":"noul","instructions":"?"}}}`, "jev-latest", "")
	if !strings.Contains(string(body), `"model":"jev-1.13"`) {
		t.Fatalf("request = %s", body)
	}
}

func TestPrepareRejects(t *testing.T) {
	for _, message := range []string{
		`not json`,
		`{"questions":{"q":{"type":"noul","instructions":"?"}}}`,
		`{"state":"x"}`,
		`{"state":"x","questions":{}}`,
		`{"state":"x","questions":{"q":{"type":"noul"}}}`, // no instructions and no system prompt
	} {
		if _, err := Prepare(message, "jev-latest", ""); err == nil {
			t.Errorf("Prepare(%s) accepted", message)
		}
	}
}

func TestEvaluateRetriesAndFails(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("request to %s with %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			_, _ = w.Write([]byte(`{"answers":{}}`))
		default:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"detail":"bad question"}`))
		}
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL, APIKey: "key", HTTP: server.Client(), Retries: 2, Backoff: time.Millisecond}
	if body, err := c.Evaluate(context.Background(), []byte(`{}`)); err != nil || string(body) != `{"answers":{}}` || calls.Load() != 2 {
		t.Fatalf("body = %s, err = %v, calls = %d", body, err, calls.Load())
	}
	if _, err := c.Evaluate(context.Background(), []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "bad question") {
		t.Fatalf("err = %v", err)
	}
}

type textSink struct {
	runtime.EventSink
	text strings.Builder
}

func (s *textSink) TextDelta(d runtime.TextDelta) error { s.text.WriteString(d.Text); return nil }

func TestRunnerRepliesWithTheResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.9}}}`))
	}))
	defer server.Close()
	r := &runner{client: &Client{BaseURL: server.URL, Model: "jev-latest", HTTP: server.Client()}, instructions: "Ready?"}
	sink := &textSink{}
	outcome, err := r.Run(context.Background(), runtime.Turn{Prompt: `{"state":"x","questions":{"q":{"type":"noul"}}}`}, sink)
	if err != nil || outcome.Failure != nil || sink.text.String() != `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.9}}}` {
		t.Fatalf("outcome = %+v, err = %v, reply = %s", outcome, err, sink.text.String())
	}
	outcome, _ = r.Run(context.Background(), runtime.Turn{Prompt: "hello"}, sink)
	if outcome.Failure == nil {
		t.Fatal("want a failure for a message that isn't a System One request")
	}
}

// TestLive calls System One when TYPESAFE_API_KEY is set.
func TestLive(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Skip("TYPESAFE_API_KEY is not set")
	}
	c := &Client{BaseURL: defaultBaseURL, APIKey: key, HTTP: &http.Client{Timeout: 30 * time.Second}, Retries: 2, Backoff: time.Second}
	body, err := Prepare(`{"state":"Since v2.3 the app crashes when I press Save.","questions":{"kind":{"type":"choice","criteria":{"bug":"A defect.","feature":"A request.","question":"A question."}}}}`, defaultModel, "Classify this issue.")
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.Evaluate(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", response)
	if !strings.Contains(string(response), `"choice":"bug"`) {
		t.Errorf("response = %s", response)
	}
}
