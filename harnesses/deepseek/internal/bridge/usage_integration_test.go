//go:build linux

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/byo-ext/harnesses/deepseek/internal/config"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func TestRealDSHUsageAcrossProtectedApprovalRestore(t *testing.T) {
	executable := os.Getenv("DSH_TEST_BIN")
	if executable == "" {
		t.Skip("DSH_TEST_BIN required")
	}
	remote := mcp.NewServer(&mcp.Implementation{Name: "usage", Version: "1"}, nil)
	mcp.AddTool(remote, &mcp.Tool{Name: "lookup"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]string, error) {
		return nil, map[string]string{"result": "found"}, nil
	})
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer upstream.Close()
	var requests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if requests.Add(1) == 1 {
			name := ""
			for _, tool := range body.Tools {
				if strings.HasSuffix(tool.Function.Name, "__lookup") {
					name = tool.Function.Name
				}
			}
			writeCompletion(w, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "usage-call", "type": "function", "function": map[string]string{"name": name, "arguments": "{}"}}}}, "tool_calls")
		} else {
			writeCompletion(w, map[string]string{"role": "assistant", "content": "found"}, "stop")
		}
	}))
	defer model.Close()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{"runtime":{"control_plane_url":"http://controller:8083"},"agent":{"model":{"model":"mock","openai":{"base_url":%q}},"http_tools":[{"params":{"url":%q},"require_approval":true}],"name":"dsh-agent"}}`, model.URL+"/v1", upstream.URL)))
	require.NoError(t, err)
	data := t.TempDir()
	makeRunner := func() *Runner {
		r, err := New(cfg, executable, data, append(os.Environ(), "OPENAI_API_KEY=mock"))
		require.NoError(t, err)
		return r
	}
	runner := makeRunner()
	first := &recordingSink{}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	out, err := runner.Run(ctx, runtime.Turn{Prompt: "Look up the value.", TaskID: "task", ContextID: "context"}, first)
	require.NoError(t, err)
	require.NotNil(t, out.Pending)
	require.Len(t, first.modelCalls, 1)
	require.False(t, first.modelCalls[0].Canceled, "parking after a completed model call is not model cancellation")
	runner = makeRunner()
	_, _, pending, err := runner.RestorePending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	second := &recordingSink{}
	out, err = pending.Resume(ctx, &runtime.ApprovalDecision{ID: pending.Request().(*runtime.ApprovalRequest).ID, Approved: true}, second)
	require.NoError(t, err)
	require.Nil(t, out.Pending)
	require.Nil(t, out.Failure)
	require.Len(t, second.modelCalls, 1, "old model call must not be replayed into the resumed segment")
	require.Equal(t, int32(2), requests.Load())
}

func TestRealDSHUsageOnStreamCancellation(t *testing.T) {
	executable := os.Getenv("DSH_TEST_BIN")
	if executable == "" {
		t.Skip("DSH_TEST_BIN required")
	}
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"partial\",\"object\":\"chat.completion.chunk\",\"model\":\"mock\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial answer\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		close(ready)
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{"runtime":{"control_plane_url":"http://controller:8083"},"agent":{"model":{"model":"mock","openai":{"base_url":%q}},"name":"dsh-agent"}}`, server.URL+"/v1")))
	require.NoError(t, err)
	runner, err := New(cfg, executable, t.TempDir(), append(os.Environ(), "OPENAI_API_KEY=mock"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink := &recordingSink{}
	done := make(chan error, 1)
	go func() { _, err := runner.Run(ctx, runtime.Turn{Prompt: "Stream a long response."}, sink); done <- err }()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("model request did not start")
	}
	// Let DSH consume the first model chunk; ACP emits it only when interrupted.
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(15 * time.Second):
		t.Fatal("cancel did not finish")
	}
	require.Len(t, sink.modelCalls, 1)
	require.True(t, sink.modelCalls[0].Canceled)
	require.True(t, sink.modelCalls[0].UsagePartial)
}
