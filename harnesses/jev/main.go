// Command jev-harness runs TypeSafe's Jev, a decision model, as a Kagent
// agent. Each message is a System One request, and the reply is System One's
// response, unchanged.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	kagentconfig "github.com/kagent-dev/kagent/go/adk/pkg/config"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

const (
	defaultBaseURL = "https://api.typesafe.ai"
	defaultModel   = "jev-latest"
	// Kagent connects to a BYO harness on port 80 only.
	privatePort = "80"
)

func main() {
	logger, err := logging.New(os.Stderr, "info")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := run(logger); err != nil {
		logger.Error("jev harness stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// KAGENT_CONFIG_JSON holds the system prompt, the model from the agent's
	// ModelConfig, and the Kagent controller that stores the A2A tasks. An
	// OpenAI ModelConfig whose base URL is TypeSafe's API makes Kagent's
	// egress gateway add the API key, so the key never enters the sandbox.
	var cfg configv1.ADKConfig
	if err := kagentconfig.Unmarshal([]byte(os.Getenv("KAGENT_CONFIG_JSON")), &cfg); err != nil {
		return fmt.Errorf("decode KAGENT_CONFIG_JSON: %w", err)
	}
	agent := cfg.GetAgent()
	var card a2atype.AgentCard
	if err := json.Unmarshal([]byte(os.Getenv("KAGENT_AGENT_CARD_JSON")), &card); err != nil || card.Name == "" {
		return fmt.Errorf("KAGENT_AGENT_CARD_JSON needs an agent card with a name: %v", err)
	}
	client := &Client{
		BaseURL: firstOf(agent.GetModel().GetOpenai().GetBaseUrl(), os.Getenv("JEV_BASE_URL"), defaultBaseURL),
		Model:   firstOf(agent.GetModel().GetModel(), os.Getenv("JEV_MODEL"), defaultModel),
		APIKey:  firstOf(os.Getenv("OPENAI_API_KEY"), os.Getenv("TYPESAFE_API_KEY")),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Retries: 4, Backoff: time.Second,
	}
	executor, err := runtimea2a.New(&runner{client: client, instructions: agent.GetInstruction()}, noContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		return err
	}
	logger.Info("jev harness starting", "model", client.Model, "baseURL", client.BaseURL)
	application, err := app.New(app.AppConfig{AgentCard: card, Port: privatePort, AppName: card.Name, ControlPlaneURL: cfg.GetRuntime().GetControlPlaneUrl(), Logger: logger}, executor)
	if err != nil {
		return fmt.Errorf("construct A2A app: %w", err)
	}
	return application.Run()
}

// runner answers each message on its own. It keeps no conversation.
type runner struct {
	client       *Client
	instructions string
}

func (r *runner) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	fail := func(err error) (runtime.Outcome, error) {
		return runtime.Outcome{Failure: &runtime.Failure{Message: err.Error()}}, nil
	}
	if turn.InputResponse != nil {
		return fail(fmt.Errorf("the jev harness doesn't ask for input"))
	}
	request, err := Prepare(turn.Prompt, r.client.Model, r.instructions)
	if err != nil {
		return fail(err)
	}
	response, err := r.client.Evaluate(ctx, request)
	if err != nil {
		return fail(err)
	}
	if err := sink.TextDelta(runtime.TextDelta{Text: string(response)}); err != nil {
		return runtime.Outcome{}, err
	}
	return runtime.Outcome{}, nil
}

// noContinuation is the continuation store of a harness without a native
// conversation to continue.
type noContinuation struct{}

func (noContinuation) Load() (string, bool, error) { return "", false, nil }
func (noContinuation) Bind(string) error           { return nil }

func firstOf(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
