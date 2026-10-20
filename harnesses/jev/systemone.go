package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls TypeSafe's System One API.
type Client struct {
	BaseURL string
	Model   string
	// APIKey is a placeholder in a Kagent sandbox: the egress gateway
	// replaces the Authorization header with the real key.
	APIKey string
	HTTP   *http.Client
	// Retries bounds retries of rate-limited or overloaded requests.
	Retries int
	Backoff time.Duration
}

// Prepare completes a System One request from a message: it sets the model
// when the request has none, and gives each question without instructions
// the agent's system prompt.
func Prepare(message, model, instructions string) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal([]byte(message), &request); err != nil {
		return nil, fmt.Errorf("the message must be a System One request, a JSON object with state and questions: %w", err)
	}
	if _, ok := request["state"]; !ok {
		return nil, fmt.Errorf("the System One request has no state")
	}
	var questions map[string]map[string]json.RawMessage
	if err := json.Unmarshal(request["questions"], &questions); err != nil || len(questions) == 0 {
		return nil, fmt.Errorf("the System One request needs questions, a map of question IDs to questions")
	}
	instructions = strings.TrimSpace(instructions)
	for id, question := range questions {
		if _, ok := question["instructions"]; ok {
			continue
		}
		if instructions == "" {
			return nil, fmt.Errorf("question %q has no instructions, and the agent has no system prompt", id)
		}
		question["instructions"], _ = json.Marshal(instructions)
	}
	request["questions"], _ = json.Marshal(questions)
	if _, ok := request["model"]; !ok {
		request["model"], _ = json.Marshal(model)
	}
	return json.Marshal(request)
}

// Evaluate posts a System One request and returns the response body.
func (c *Client) Evaluate(ctx context.Context, body []byte) ([]byte, error) {
	backoff := c.Backoff
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+"/v1/systemone", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("call System One: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read System One response: %w", err)
		}
		// 429 is a rate limit and 529 an overloaded service; both ask for a retry.
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529) && attempt < c.Retries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("System One returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		}
		return data, nil
	}
}
