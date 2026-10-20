package main

import (
	"testing"

	kagentconfig "github.com/kagent-dev/kagent/go/adk/pkg/config"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
)

// Checks that the harness finds its settings in the JSON Kagent writes for a
// BYO harness with an OpenAI ModelConfig.
func TestConfigReadsKagentBYOConfig(t *testing.T) {
	data, err := kagentconfig.Marshal(&configv1.ADKConfig{
		Runtime: &configv1.ADKRuntimeConfig{ControlPlaneUrl: "http://kagent-controller.kagent.svc.cluster.local:8083"},
		Agent: &configv1.ADKAgentConfig{
			Name:        "judge",
			Instruction: "Classify this issue.",
			Model: &configv1.ADKModelConfig{
				Model:    "jev-latest",
				Provider: &configv1.ADKModelConfig_Openai{Openai: &configv1.ADKOpenAI{BaseUrl: "http://llm-gateway/typesafe"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg configv1.ADKConfig
	if err := kagentconfig.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	agent := cfg.GetAgent()
	if got := agent.GetInstruction(); got != "Classify this issue." {
		t.Errorf("instruction = %q", got)
	}
	if got := agent.GetModel().GetModel(); got != "jev-latest" {
		t.Errorf("model = %q", got)
	}
	if got := agent.GetModel().GetOpenai().GetBaseUrl(); got != "http://llm-gateway/typesafe" {
		t.Errorf("base URL = %q", got)
	}
	if got := cfg.GetRuntime().GetControlPlaneUrl(); got != "http://kagent-controller.kagent.svc.cluster.local:8083" {
		t.Errorf("control plane URL = %q", got)
	}
}
