package config

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/kagent-dev/kagent/go/api/adk"
)

const valid = `{"runtime":{"control_plane_url":"http://controller:8083"},"agent":{"model":{"model":"deepseek-chat","openai":{"base_url":"https://api.deepseek.com/v1"}},"instruction":"Answer accurately.","name":"dsh-agent"}}`

func TestProtectedMCPBindingUsesAdapterApproval(t *testing.T) {
	c, err := Parse([]byte(strings.TrimSuffix(valid, "}}") + `,"http_tools":[{"params":{"url":"https://tools.example/mcp"},"require_approval":true}]}}`))
	require.NoError(t, err)
	require.True(t, c.Agent.HttpTools[0].RequireApproval)
	require.Len(t, c.MCP, 1)
}

func TestParseRejectsUnsupportedSemantics(t *testing.T) {
	for _, test := range []struct{ name, field string }{
		{"missing model", `"model":null`},
		{"responses", `"model":{"model":"x","openai":{"api_format":"responses"}}`},
		{"generation overrides", `"model":{"model":"x","openai":{"temperature":0.2}}`},
		{"credential passthrough", `"model":{"model":"x","openai":{},"api_key_passthrough":true}`},
		{"model userinfo", `"model":{"model":"x","openai":{"base_url":"https://key@example.com/v1"}}`},
		{"subagents", `"sub_agents":[{}]`},
		{"plugins", `"agent_plugins":{}`},
		{"memory", `"memory":{}`},
		{"SSE", `"sse_tools":[{}]`},
		{"non-streaming", `"stream":false`},
		{"MCP tool subset", `"http_tools":[{"params":{"url":"https://tools.example/mcp"},"tools":["read"]}]`},
		{"MCP forwarded headers", `"http_tools":[{"params":{"url":"https://tools.example/mcp"},"allowed_headers":["authorization"]}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var root map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(valid), &root))
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(root["agent"], &fields))
			var change map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte("{"+test.field+"}"), &change))
			maps.Copy(fields, change)
			root["agent"], _ = json.Marshal(fields)
			data, err := json.Marshal(root)
			require.NoError(t, err)
			_, err = Parse(data)
			require.Error(t, err)
		})
	}
}

func TestMCPReferencesUseOnlyGatewayPlaceholders(t *testing.T) {
	c, err := Parse([]byte(valid))
	require.NoError(t, err)
	c.MCP = []MCPServer{{Type: "http", Name: "tools", URL: "https://tools.example/mcp", Headers: []Header{{Name: "Authorization", Value: "__KAGENT_ENV[KAGENT_CREDENTIAL_TEST]__"}}}}
	servers, err := c.Bindings([]string{"KAGENT_CREDENTIAL_TEST=kagent-credential-injected"})
	require.NoError(t, err)
	require.Equal(t, "kagent-credential-injected", servers[0].Headers[0].Value)
	require.Equal(t, "__KAGENT_ENV[KAGENT_CREDENTIAL_TEST]__", c.MCP[0].Headers[0].Value)
	for _, env := range [][]string{nil, {"KAGENT_CREDENTIAL_TEST=actual-secret"}} {
		_, err = c.Bindings(env)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "actual-secret")
	}
}

func TestPatchPreservesInstructionAndUsesDurableState(t *testing.T) {
	c, err := Parse([]byte(valid))
	require.NoError(t, err)
	c.Agent.Instruction = "quotes: \"\n!!js doNotExecute()\n$variable"
	data, err := c.Patch("/data")
	require.NoError(t, err)
	var rows []struct {
		ID       string          `json:"id"`
		Config   json.RawMessage `json:"config"`
		Disabled bool            `json:"disabled"`
	}
	require.NoError(t, json.Unmarshal(data, &rows))
	disabled := make(map[string]bool)
	for _, row := range rows {
		disabled[row.ID] = row.Disabled
		if row.ID == "system-prompt" {
			var p persona
			require.NoError(t, json.Unmarshal(row.Config, &p))
			require.Equal(t, c.Agent.Instruction, p.Prefix)
		}
		if row.ID == "session-persistence-jsonl" {
			require.JSONEq(t, `{"root":"/data/dsh/sessions"}`, string(row.Config))
		}
	}
	for _, id := range []string{"session-log-deepseek", "plugin-package-inventory-deepseek", "otel", "otel-exporter"} {
		require.True(t, disabled[id], "%s must be explicitly disabled", id)
	}
	for _, id := range []string{"session-persistence-jsonl", "storage-json"} {
		require.Contains(t, disabled, id)
		require.False(t, disabled[id], "%s must remain enabled for recovery", id)
	}
	require.Contains(t, string(data), `"apiKeyEnv":"OPENAI_API_KEY"`)
}

func TestWholeHTTPMCPAndEnvironment(t *testing.T) {
	data := strings.TrimSuffix(valid, "}}") + `,"http_tools":[{"params":{"url":"https://tools.example/mcp","headers":{"z":"last","a":"first"}}}]}}`
	c, err := Parse([]byte(data))
	require.NoError(t, err)
	require.Equal(t, []Header{{Name: "a", Value: "first"}, {Name: "z", Value: "last"}}, c.MCP[0].Headers)
	env := Environment([]string{"NODE_EXTRA_CA_CERTS=/run/trust.pem", "OPENAI_API_KEY=placeholder", "DSH_HOME=/old", "DSH_TELEMETRY_DISABLED=0", "KAGENT_TOKEN=controller-token"}, "/tmp/dsh")
	require.Contains(t, env, "NODE_EXTRA_CA_CERTS=/run/trust.pem")
	require.Contains(t, env, "OPENAI_API_KEY=placeholder")
	require.NotContains(t, env, "DSH_HOME=/old")
	require.Contains(t, env, "DSH_TELEMETRY_DISABLED=1")
	require.NotContains(t, env, "KAGENT_TOKEN=controller-token")
}

func TestBindingsPreserveEmptyHeaderArray(t *testing.T) {
	c, err := Parse([]byte(strings.TrimSuffix(valid, "}}") + `,"http_tools":[{"params":{"url":"https://tools.example/mcp"}}]}}`))
	require.NoError(t, err)
	servers, err := c.Bindings(nil)
	require.NoError(t, err)
	data, err := json.Marshal(servers)
	require.NoError(t, err)
	require.Contains(t, string(data), `"headers":[]`)
}

func TestParseControllerProtoJSON(t *testing.T) {
	compiled := &adk.Config{
		Runtime: &adk.RuntimeConfig{ControlPlaneUrl: "http://controller:8083"},
		Agent:   &adk.AgentConfig{Name: "dsh-agent", Model: &adk.Model{Model: "deepseek-chat", Headers: map[string]string{"x-route": "test"}, Provider: &adk.Model_Openai{Openai: &adk.OpenAI{BaseUrl: "https://api.deepseek.com/v1"}}}, HttpTools: []*adk.HttpMcpServerConfig{{Params: &adk.StreamableHTTPConnectionParams{Url: "https://tools.example/mcp", Timeout: durationpb.New(250 * time.Millisecond), TerminateOnClose: new(false)}}}},
	}
	data, err := adk.Marshal(compiled)
	require.NoError(t, err)
	cfg, err := Parse(data)
	require.NoError(t, err)
	require.Equal(t, 250*time.Millisecond, cfg.Agent.HttpTools[0].Params.Timeout.AsDuration())
	require.False(t, *cfg.Agent.HttpTools[0].Params.TerminateOnClose)
	patch, err := cfg.Patch("/data")
	require.NoError(t, err)
	require.Contains(t, string(patch), `"x-route":"test"`)
	require.Equal(t, "http://controller:8083", cfg.Runtime.ControlPlaneUrl)
}

func TestParseRejectsMalformedControllerInputs(t *testing.T) {
	for _, raw := range []string{
		`{"model":{"type":"openai","model":"old"},"runtime":{"control_plane_url":"http://controller:8083"}}`,
		`{"runtime":{},"agent":null}`,
		strings.TrimSuffix(valid, "}}") + `,"http_tools":[null]}}`,
		strings.TrimSuffix(valid, "}}") + `,"http_tools":[{"params":null}]}}`,
		strings.TrimSuffix(valid, "}}") + `,"http_tools":[{"params":{"url":"https://tools.example/mcp","timeout":"9223372036.854775808s"}}]}}`,
		strings.TrimSuffix(valid, "}}") + `,"http_tools":[{"params":{"url":"https://tools.example/mcp","timeout":0.25}}]}}`,
	} {
		_, err := Parse([]byte(raw))
		require.Error(t, err)
	}
}
