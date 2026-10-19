// Package config translates the existing BYO payload into DSH launch inputs.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/kagent-dev/kagent/go/api/adk"
)

const (
	Version  = "0.2.0-rc.2"
	Provider = "kagent-byo"
)

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type MCPServer struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Headers []Header `json:"headers"`
}

// Config retains the controller-owned payload without inventing another public API.
type Config struct {
	Agent   *adk.AgentConfig
	Runtime *adk.RuntimeConfig
	Model   *adk.OpenAI
	MCP     []MCPServer
}

func Parse(data []byte) (*Config, error) {
	var compiled adk.Config
	if err := adk.Unmarshal(data, &compiled); err != nil {
		return nil, fmt.Errorf("failed to decode BYO configuration: %w", err)
	}
	agent := compiled.GetAgent()
	if agent == nil || agent.GetModel() == nil || agent.Model.GetOpenai() == nil || agent.Model.Model == "" {
		return nil, fmt.Errorf("DSH BYO requires an OpenAI-compatible ModelConfig")
	}
	model := agent.Model.GetOpenai()
	if model.ApiFormat != "" && model.ApiFormat != "chatCompletions" {
		return nil, fmt.Errorf("DSH BYO supports only chatCompletions")
	}
	if agent.Model.ApiKeyPassthrough || model.TokenExchange != nil || agent.Model.TlsCaCertPath != nil || agent.Model.TlsDisableSystemCas != nil || (agent.Model.TlsInsecureSkipVerify != nil && *agent.Model.TlsInsecureSkipVerify) {
		return nil, fmt.Errorf("DSH BYO requires gateway-injected credentials and platform trust")
	}
	if model.FrequencyPenalty != nil || model.MaxTokens != nil || model.MaxCompletionTokens != nil || model.N != nil || model.PresencePenalty != nil || model.ReasoningEffort != nil || model.Seed != nil || model.Temperature != nil || model.Timeout != nil || model.TopP != nil {
		return nil, fmt.Errorf("DSH BYO does not yet map OpenAI generation overrides")
	}
	if compiled.Runtime == nil || compiled.Runtime.ControlPlaneUrl == "" {
		return nil, fmt.Errorf("runtime.control_plane_url is required")
	}
	if compiled.Runtime.GetTokenPropagation().GetEnabled() {
		return nil, fmt.Errorf("DSH BYO does not support token propagation")
	}
	if agent.Stream != nil && !*agent.Stream {
		return nil, fmt.Errorf("DSH BYO requires streaming model requests")
	}
	if len(agent.SubAgents) != 0 || len(agent.RemoteAgents) != 0 || agent.AgentPlugins != nil || agent.Memory != nil || agent.ContextConfig != nil || agent.Output != nil || agent.SkillsDirectory != "" || len(agent.SseTools) != 0 || len(agent.StdioTools) != 0 || agent.Network != nil {
		return nil, fmt.Errorf("DSH BYO supports a root agent and whole Streamable HTTP MCP servers; kagent skills, plugins, subagents and ADK-specific settings are unsupported")
	}
	baseURL := model.BaseUrl
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if err := httpURL(baseURL); err != nil {
		return nil, err
	}
	model.BaseUrl = baseURL
	c := &Config{Agent: agent, Runtime: compiled.Runtime, Model: model}
	for i, server := range agent.HttpTools {
		if len(server.Tools) != 0 || len(server.AllowedHeaders) != 0 {
			return nil, fmt.Errorf("MCP binding %d: tool selection and forwarded headers are unsupported by DSH ACP", i)
		}
		if server.Params.SseReadTimeout != nil || server.Params.TlsCaCertPath != nil || server.Params.TlsDisableSystemCas != nil || (server.Params.TlsInsecureSkipVerify != nil && *server.Params.TlsInsecureSkipVerify) {
			return nil, fmt.Errorf("MCP binding %d: connection overrides are unsupported by DSH ACP", i)
		}
		if server.Params.Timeout != nil && (server.Params.Timeout.AsDuration() <= 0 || !proto.Equal(server.Params.Timeout, durationpb.New(server.Params.Timeout.AsDuration()))) {
			return nil, fmt.Errorf("MCP binding %d: timeout must be positive and fit a Go duration", i)
		}
		if err := httpURL(server.Params.Url); err != nil {
			return nil, err
		}
		headers := make([]Header, 0, len(server.Params.Headers))
		for name, value := range server.Params.Headers {
			headers = append(headers, Header{Name: name, Value: value})
		}
		slices.SortFunc(headers, func(a, b Header) int { return strings.Compare(a.Name, b.Name) })
		c.MCP = append(c.MCP, MCPServer{Type: "http", Name: fmt.Sprintf("kagent_%d", i), URL: server.Params.Url, Headers: headers})
	}
	return c, nil
}

func httpURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("DSH endpoint must be an absolute HTTP(S) URL without userinfo")
	}
	return nil
}

type patch struct {
	ID       string `json:"id"`
	Config   any    `json:"config,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

type route struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type persona struct {
	Prefix string `json:"personaPrefix"`
}
type directory struct {
	Root string `json:"root"`
}
type providerConfig struct {
	Providers map[string]provider `json:"providers"`
}
type provider struct {
	API       string            `json:"api"`
	BaseURL   string            `json:"baseURL"`
	APIKeyEnv string            `json:"apiKeyEnv"`
	Headers   map[string]string `json:"headers,omitempty"`
	Models    []modelEntry      `json:"models"`
}
type modelEntry struct {
	ID    string   `json:"id"`
	Input []string `json:"input"`
}

// Patch generates JSON (valid YAML) without interpolating user text into code.
// Only runtime-private session data is durable; the launcher profile is disposable.
func (c *Config) Patch(dataDir string) ([]byte, error) {
	rows := []patch{
		{ID: "llm-pi-ai", Config: providerConfig{Providers: map[string]provider{Provider: {
			API: "openai-completions", BaseURL: c.Model.BaseUrl, APIKeyEnv: "OPENAI_API_KEY", Headers: c.Agent.Model.Headers,
			Models: []modelEntry{{ID: c.Agent.Model.Model, Input: []string{"text"}}},
		}}}},
		{ID: "acp", Config: route{Provider: Provider, Model: c.Agent.Model.Model}},
		{ID: "agent-default-model", Config: route{Provider: Provider, Model: c.Agent.Model.Model}},
		{ID: "system-prompt", Config: persona{Prefix: c.Agent.Instruction}},
		{ID: "session-persistence-jsonl", Config: directory{Root: filepath.Join(dataDir, "dsh", "sessions")}},
		{ID: "storage-json", Config: directory{Root: filepath.Join(dataDir, "dsh", "storages")}},
		// Disable request attachments independently of the selected provider.
		// Local conversation recovery still uses session-persistence-jsonl above.
		{ID: "session-log-deepseek", Disabled: true},
		{ID: "plugin-package-inventory-deepseek", Disabled: true},
		// These optional network consumers are outside the compiled ModelConfig/MCP egress contract.
		{ID: "tool-web", Disabled: true},
		{ID: "otel", Disabled: true},
		{ID: "otel-exporter", Disabled: true},
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, fmt.Errorf("failed to encode DSH patch: %w", err)
	}
	return data, nil
}

// Environment preserves platform CA/proxy settings and gateway placeholders.
// Per-process homes prevent persisted profile edits from changing composition.
func Environment(environment []string, home string) []string {
	result := make([]string, 0, len(environment)+3)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if name == "DSH_HOME" || name == "DSH_AGENTS_HOME" || name == "DSH_TELEMETRY_DISABLED" || name == "KAGENT_TOKEN" {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "DSH_HOME="+home, "DSH_AGENTS_HOME="+filepath.Join(home, "agents"), "DSH_TELEMETRY_DISABLED=1")
}

// Bindings resolves the ADK compiler's MCP header references to inert gateway
// placeholders. Actual Secret values must never enter DSH's persisted session.
func (c *Config) Bindings(environment []string) ([]MCPServer, error) {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	servers := append([]MCPServer(nil), c.MCP...)
	for i := range servers {
		servers[i].Headers = slices.Clone(servers[i].Headers)
		for j, header := range servers[i].Headers {
			const prefix, suffix = "__KAGENT_ENV[", "]__"
			if !strings.HasPrefix(header.Value, prefix) || !strings.HasSuffix(header.Value, suffix) {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(header.Value, prefix), suffix)
			value := values[name]
			if !strings.HasPrefix(name, "KAGENT_CREDENTIAL_") || value != "kagent-credential-injected" {
				return nil, fmt.Errorf("MCP credential reference requires a gateway-injected placeholder")
			}
			servers[i].Headers[j].Value = value
		}
	}
	return servers, nil
}
