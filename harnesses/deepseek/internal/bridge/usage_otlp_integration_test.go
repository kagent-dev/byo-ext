//go:build linux

package bridge

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/kagent-dev/byo-ext/harnesses/deepseek/internal/config"
	adka2a "github.com/kagent-dev/kagent/go/adk/pkg/a2a"
	"github.com/kagent-dev/kagent/go/adk/pkg/a2a/server"
	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/pkg/telemetry"
	"github.com/kagent-dev/kagent/go/pkg/telemetry/conv"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

// Run in a separate process: telemetry.Init installs global SDK providers and
// changes OTEL environment defaults. Neither may leak into parallel bridge tests.
func TestRealDSHUsageOTLP(t *testing.T) {
	if os.Getenv("DSH_TEST_BIN") == "" {
		t.Skip("DSH_TEST_BIN required")
	}
	if os.Getenv("DSH_TEST_OTLP_CHILD") != "1" {
		binary, err := os.Executable()
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "-test.run=^TestRealDSHUsageOTLP$", "-test.v", "-test.timeout=90s")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "OTEL_") && !strings.HasPrefix(entry, "DSH_TEST_OTLP_CHILD=") {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env, "DSH_TEST_OTLP_CHILD=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Log(string(output))
		return
	}
	testUsageOTLP(t)
}

// otlpReceiver is a local collector-protocol fixture, not an in-memory SDK
// exporter. It decodes the actual HTTP/protobuf payloads from production OTLP
// exporters. Cumulative metric snapshots replace prior values; they are not sums
// of export batches (a flush must not look like additional model usage).
type otlpReceiver struct {
	mu        sync.Mutex
	spans     []*tracepb.Span
	resources [][]*commonpb.KeyValue
	metrics   map[string]*metricspb.Metric
}

func (c *otlpReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "invalid compression", 400)
			return
		}
		defer reader.Close()
		body = reader
	}
	data, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil || r.Header.Get("Content-Type") != "application/x-protobuf" {
		http.Error(w, "invalid OTLP request", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var response proto.Message
	switch r.URL.Path {
	case "/v1/traces":
		var request collectortrace.ExportTraceServiceRequest
		if proto.Unmarshal(data, &request) != nil {
			http.Error(w, "invalid traces", 400)
			return
		}
		for _, resource := range request.ResourceSpans {
			c.resources = append(c.resources, resource.GetResource().GetAttributes())
			for _, scope := range resource.ScopeSpans {
				c.spans = append(c.spans, scope.Spans...)
			}
		}
		response = &collectortrace.ExportTraceServiceResponse{}
	case "/v1/metrics":
		var request collectormetrics.ExportMetricsServiceRequest
		if proto.Unmarshal(data, &request) != nil {
			http.Error(w, "invalid metrics", 400)
			return
		}
		for _, resource := range request.ResourceMetrics {
			c.resources = append(c.resources, resource.GetResource().GetAttributes())
			for _, scope := range resource.ScopeMetrics {
				for _, metric := range scope.Metrics {
					c.metrics[metric.Name] = metric
				}
			}
		}
		response = &collectormetrics.ExportMetricsServiceResponse{}
	default:
		http.NotFound(w, r)
		return
	}
	encoded, _ := proto.Marshal(response)
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(encoded)
}

type usageMetricsStarter struct{ start func() }

func (s usageMetricsStarter) Intercept(ctx context.Context, _ *a2asrv.ExecutorContext) (context.Context, error) {
	s.start()
	return ctx, nil
}

func testUsageOTLP(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	receiver := &otlpReceiver{metrics: make(map[string]*metricspb.Metric)}
	collector := httptest.NewServer(receiver)
	defer collector.Close()
	// Keep periodic export out of the assertions; production quiescent flushes
	// and explicit repeat flushes below drive both real exporters.
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "cumulative")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	identity := tracing.RuntimeTelemetry{Provider: "openai", Model: "mock"}

	var effects, requests atomic.Int32
	remote := mcp.NewServer(&mcp.Implementation{Name: "usage", Version: "1"}, nil)
	mcp.AddTool(remote, &mcp.Tool{Name: "lookup"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]string, error) {
		effects.Add(1)
		return nil, map[string]string{"result": "found"}, nil
	})
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer upstream.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		index := requests.Add(1)
		var delta any = map[string]string{"role": "assistant", "content": "found"}
		finish := "stop"
		if index == 1 {
			name := ""
			for _, tool := range input.Tools {
				if strings.HasSuffix(tool.Function.Name, "__lookup") {
					name = tool.Function.Name
				}
			}
			if name == "" {
				http.Error(w, "missing tool", 500)
				return
			}
			delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "lookup-1", "type": "function", "function": map[string]string{"name": name, "arguments": "{}"}}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, choice := range []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}, map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
			chunk, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("response-%d", index), "object": "chat.completion.chunk", "model": "mock", "choices": []any{choice}})
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		fmt.Fprintf(w, "data: {\"id\":\"response-%d\",\"object\":\"chat.completion.chunk\",\"model\":\"mock\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15,\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\ndata: [DONE]\n\n", index)
	}))
	defer model.Close()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{"runtime":{"control_plane_url":"http://controller:8083"},"agent":{"name":"dsh-agent","model":{"model":"mock","openai":{"base_url":%q}},"http_tools":[{"params":{"url":%q},"require_approval":true}]}}`, model.URL+"/v1", upstream.URL)))
	require.NoError(t, err)
	directory := t.TempDir()
	newExecutor := func() *Executor {
		runner, err := New(cfg, os.Getenv("DSH_TEST_BIN"), directory, append(os.Environ(), "OPENAI_API_KEY=fixture"))
		require.NoError(t, err)
		store, err := continuation.New(filepath.Join(directory, "adapter"), "dsh-byo", ValidateSessionID)
		require.NoError(t, err)
		executor, err := NewExecutor(runner, store, identity)
		require.NoError(t, err)
		return executor
	}
	switcher := &replaceableDSHExecutor{}
	switcher.current.Store(newExecutor()) // Production constructs instruments before SDK initialization.
	traces := &telemetry.OTLPConfig{Endpoint: collector.URL + "/v1/traces", Protocol: "http/protobuf"}
	metrics := &telemetry.OTLPConfig{Endpoint: collector.URL + "/v1/metrics", Protocol: "http/protobuf"}
	providers, err := telemetry.Init(ctx, telemetry.Options{Config: telemetry.Config{Traces: traces, Metrics: metrics}, Identity: identity.ResourceIdentity(), Defaults: identity.ResourceDefaults("dsh-usage-test"), DeferMetrics: true})
	require.NoError(t, err)
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, providers.Shutdown(stop))
	}()
	tasks, api := newSettlementStore(t, nil)
	wrapped := tasks.WrapExecutor(switcher, identity.Runtime, providers.ForceFlush)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	svc, err := server.NewA2AServer(a2atype.AgentCard{Name: "dsh-usage-test"}, wrapped, slog.Default(), server.ServerConfig{Host: "127.0.0.1", Port: port, Telemetry: identity, Flush: providers.ForceFlush},
		a2asrv.WithTaskStore(tasks), a2asrv.WithCallInterceptors(adka2a.HITLActivationInterceptor(), adka2a.UserIDCallInterceptor(), wrapped, switcher), a2asrv.WithExecutorContextInterceptor(usageMetricsStarter{providers.StartMetrics}))
	require.NoError(t, err)
	require.NoError(t, svc.Start())
	// The enclosing child process owns the server listeners and all SDK globals.
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer connection.Close()
	client := a2apb.NewA2AServiceClient(connection)
	send := func(message *a2atype.Message) *a2apb.Task {
		req, err := pbconv.ToProtoSendMessageRequest(&a2atype.SendMessageRequest{Message: message})
		require.NoError(t, err)
		result, err := client.SendMessage(ctx, req)
		require.NoError(t, err)
		task := result.GetTask()
		require.NotNil(t, task)
		select {
		case <-api.settled:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		require.NoError(t, providers.ForceFlush(ctx))
		return task
	}
	first := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("Look up a value."))
	first.ContextID = uuid.NewString()
	task := send(first)
	require.Equal(t, a2apb.TaskState_TASK_STATE_INPUT_REQUIRED, task.Status.State)
	require.Zero(t, effects.Load())
	receiver.checkUsage(t, 1)
	native, err := pbconv.FromProtoTask(task)
	require.NoError(t, err)
	approval, err := apia2a.ParseToolApprovalRequest(native.Status.Message)
	require.NoError(t, err)
	require.NotNil(t, approval)
	switcher.current.Store(newExecutor()) // Retain only DATA and the controller fixture, no pending handles.
	decision := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("Approve"))
	decision.TaskID, decision.ContextID = native.ID, native.ContextID
	require.NoError(t, apia2a.AttachHITL(decision, apia2a.ToolApprovalResponse{Type: apia2a.HITLTypeToolApprovalResponse, Approvals: []apia2a.ToolApproval{{ID: approval.Tools[0].ID, Approved: true}}}))
	completed := send(decision)
	require.Equal(t, a2apb.TaskState_TASK_STATE_COMPLETED, completed.Status.State)
	require.Equal(t, task.Id, completed.Id)
	require.Equal(t, task.ContextId, completed.ContextId)
	require.Equal(t, int32(1), effects.Load())
	receiver.checkUsage(t, 2)
	receiver.checkOwnership(t, task.Id, task.ContextId)
	// Neither a duplicate export nor a rejected late approval adds calls/tokens.
	req, err := pbconv.ToProtoSendMessageRequest(&a2atype.SendMessageRequest{Message: decision})
	require.NoError(t, err)
	_, err = client.SendMessage(ctx, req)
	require.Error(t, err)
	require.NoError(t, providers.ForceFlush(ctx))
	require.NoError(t, providers.ForceFlush(ctx))
	require.Equal(t, int32(2), requests.Load())
	require.Equal(t, int32(1), effects.Load())
	receiver.checkUsage(t, 2)
}

func otlpAttribute(attrs []*commonpb.KeyValue, key string) *commonpb.AnyValue {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value
		}
	}
	return nil
}

func (c *otlpReceiver) checkUsage(t *testing.T, calls int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	chats := 0
	for _, span := range c.spans {
		if otlpAttribute(span.Attributes, "gen_ai.operation.name").GetStringValue() != "chat" {
			continue
		}
		chats++
		require.Equal(t, "mock", otlpAttribute(span.Attributes, "gen_ai.request.model").GetStringValue())
		require.Equal(t, "mock", otlpAttribute(span.Attributes, "gen_ai.response.model").GetStringValue())
		require.Equal(t, "kagent-byo", otlpAttribute(span.Attributes, "gen_ai.provider.name").GetStringValue())
		require.Equal(t, int64(12), otlpAttribute(span.Attributes, "gen_ai.usage.input_tokens").GetIntValue())
		require.Equal(t, int64(3), otlpAttribute(span.Attributes, "gen_ai.usage.output_tokens").GetIntValue())
		require.Equal(t, int64(2), otlpAttribute(span.Attributes, "gen_ai.usage.cache_read.input_tokens").GetIntValue())
		require.NotEqual(t, tracepb.Status_STATUS_CODE_ERROR, span.GetStatus().GetCode())
		require.GreaterOrEqual(t, span.EndTimeUnixNano, span.StartTimeUnixNano)
	}
	require.Equal(t, calls, chats)
	for name, perCall := range map[string]int64{conv.GenAIClientInferenceUsageInputTokens: 12, conv.GenAIClientInferenceUsageOutputTokens: 3, conv.GenAIClientInferenceUsageCacheReadInputTokens: 2} {
		metric := c.metrics[name]
		require.NotNil(t, metric, name)
		sum := metric.GetSum()
		require.NotNil(t, sum, name)
		require.Equal(t, metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, sum.AggregationTemporality)
		require.Len(t, sum.DataPoints, 1, name)
		point := sum.DataPoints[0]
		require.Equal(t, int64(calls)*perCall, point.GetAsInt(), name)
		require.Equal(t, "mock", otlpAttribute(point.Attributes, "gen_ai.request.model").GetStringValue())
		require.Equal(t, "kagent-byo", otlpAttribute(point.Attributes, "gen_ai.provider.name").GetStringValue())
	}
	require.NotEmpty(t, c.resources)
	for _, resource := range c.resources {
		require.Equal(t, "dsh-usage-test", otlpAttribute(resource, "service.name").GetStringValue())
		require.Equal(t, "openai", otlpAttribute(resource, "gen_ai.provider.name").GetStringValue())
		require.Equal(t, "mock", otlpAttribute(resource, "gen_ai.request.model").GetStringValue())
		require.Nil(t, otlpAttribute(resource, "kagent.runtime"), "DSH must not impersonate another registered runtime")
	}
}

func (c *otlpReceiver) checkOwnership(t *testing.T, taskID, contextID string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	byID := make(map[string]*tracepb.Span)
	for _, span := range c.spans {
		byID[hex.EncodeToString(span.SpanId)] = span
	}
	parents := make(map[string]*tracepb.Span)
	for _, chat := range c.spans {
		if otlpAttribute(chat.Attributes, "gen_ai.operation.name").GetStringValue() != "chat" {
			continue
		}
		parent := byID[hex.EncodeToString(chat.ParentSpanId)]
		require.NotNil(t, parent)
		require.Equal(t, parent.TraceId, chat.TraceId)
		require.Equal(t, taskID, otlpAttribute(parent.Attributes, tracing.AttributeTaskID).GetStringValue())
		require.Equal(t, contextID, otlpAttribute(parent.Attributes, tracing.AttributeConversationID).GetStringValue())
		require.Equal(t, contextID, otlpAttribute(chat.Attributes, tracing.AttributeConversationID).GetStringValue())
		segment := otlpAttribute(parent.Attributes, tracing.AttributeSegment).GetStringValue()
		response := otlpAttribute(chat.Attributes, "gen_ai.response.id").GetStringValue()
		require.Contains(t, map[string]string{"response-1": tracing.SegmentInitial, "response-2": tracing.SegmentResumed}, response)
		require.Equal(t, map[string]string{"response-1": tracing.SegmentInitial, "response-2": tracing.SegmentResumed}[response], segment)
		parents[segment] = parent
	}
	require.Len(t, parents, 2)
	initial, resumed := parents[tracing.SegmentInitial], parents[tracing.SegmentResumed]
	require.NotNil(t, initial)
	require.NotNil(t, resumed)
	linked := false
	for _, link := range resumed.Links {
		if hex.EncodeToString(link.SpanId) == hex.EncodeToString(initial.SpanId) && hex.EncodeToString(link.TraceId) == hex.EncodeToString(initial.TraceId) {
			linked = otlpAttribute(link.Attributes, tracing.AttributeLinkRelationship).GetStringValue() == tracing.RelationshipResumeOrigin
		}
	}
	require.True(t, linked, "restored approval segment must link to its persisted origin")
}
