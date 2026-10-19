package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/kagent-dev/byo-ext/harnesses/deepseek/internal/config"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func savedApprovalFixture(t *testing.T, endpoint string) (*Runner, *approvalRecord, context.Context) {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{"runtime":{"control_plane_url":"http://controller:8083"},"agent":{"model":{"model":"mock","openai":{}},"http_tools":[{"params":{"url":%q},"require_approval":true}],"name":"dsh-agent"}}`, endpoint)))
	require.NoError(t, err)
	r, err := New(cfg, "/missing-test-dsh", t.TempDir(), nil)
	require.NoError(t, err)
	owner := approvalOwner{TaskID: "task-1", ContextID: "context-1"}
	ctx := context.WithValue(t.Context(), approvalOwnerKey{}, owner)
	require.NoError(t, r.admitApprovalTurn(ctx, ""))
	binding, err := r.approvalBinding(0)
	require.NoError(t, err)
	name := publicToolName(r.servers[0].Name, "deploy")
	record := &approvalRecord{Version: 1, Owner: owner, SessionID: "session-1", Binding: binding, Phase: "waiting", Call: heldTool{Server: 0, Name: "deploy", NativeName: name, Arguments: json.RawMessage(`{"version":"7"}`)}, Request: runtime.ApprovalRequest{ID: "approval-1", CallID: "call-1", Name: name, Args: map[string]any{"version": "7"}}}
	require.NoError(t, r.saveApproval(record))
	return r, record, ctx
}

func TestDeferredApprovalReusesSavedResultAfterRestart(t *testing.T) {
	var effects atomic.Int32
	remote := mcp.NewServer(&mcp.Implementation{Name: "tools", Version: "1"}, nil)
	mcp.AddTool(remote, &mcp.Tool{Name: "deploy"}, func(_ context.Context, _ *mcp.CallToolRequest, args map[string]string) (*mcp.CallToolResult, map[string]string, error) {
		effects.Add(1)
		require.Equal(t, "7", args["version"])
		return nil, map[string]string{"receipt": "ok\x00exact"}, nil
	})
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer server.Close()
	r, saved, ctx := savedApprovalFixture(t, server.URL)
	r.config.Agent.HttpTools = append(r.config.Agent.HttpTools, proto.CloneOf(r.config.Agent.HttpTools[0]))
	r.servers = append(r.servers, r.servers[0])
	decision := &runtime.ApprovalDecision{ID: saved.Request.ID, Approved: true}
	for range 2 {
		// Missing native executable is a proven setup failure. Retry from DATA
		// must reuse the result, never dispatch the approved effect again.
		reconstructed, err := New(r.config, r.executable, r.dataDir, nil)
		require.NoError(t, err)
		taskID, contextID, pending, err := reconstructed.RestorePending()
		require.NoError(t, err)
		require.Equal(t, saved.Owner.TaskID, taskID)
		require.Equal(t, saved.Owner.ContextID, contextID)
		sink := &recordingSink{}
		out, err := pending.Resume(ctx, decision, sink)
		require.NoError(t, err)
		require.NotNil(t, out.Pending)
		require.Empty(t, sink.results, "do not publish duplicate results during a setup retry")
		require.Equal(t, int32(1), effects.Load())
		require.Len(t, r.config.Agent.HttpTools, 2, "resolving one binding must not mutate the runner configuration")
		restored, err := reconstructed.loadApproval()
		require.NoError(t, err)
		require.Equal(t, "result", restored.Phase)
		encoded, err := json.Marshal(restored.Result)
		require.NoError(t, err)
		require.Contains(t, string(encoded), `\u0000`)
		_, err = out.Pending.Resume(ctx, &runtime.ApprovalDecision{ID: decision.ID, Approved: false}, sink)
		require.ErrorContains(t, err, "decision changed")
	}
}

func TestDeferredApprovalRejectsUnsafeRestoration(t *testing.T) {
	for _, scenario := range []string{"executing", "continuing", "changed-target", "changed-arguments", "changed-decision", "trailing-data", "symlink", "owner-binding", "foreign-context", "wrong-decision", "unresolved-new-turn"} {
		t.Run(scenario, func(t *testing.T) {
			r, saved, ctx := savedApprovalFixture(t, "http://127.0.0.1:1/mcp")
			switch scenario {
			case "executing", "continuing":
				saved.Phase = scenario
				saved.Decision = &runtime.ApprovalDecision{ID: saved.Request.ID, Approved: true}
				if scenario == "continuing" {
					saved.Result = &mcp.CallToolResult{}
				}
				require.NoError(t, r.saveApproval(saved))
			case "changed-target":
				r.config.Agent.HttpTools[0].Params.Url = "http://127.0.0.1:2/mcp"
			case "changed-arguments":
				saved.Call.Arguments = json.RawMessage(`{"version":"evil"}`)
			case "changed-decision":
				saved.Decision = &runtime.ApprovalDecision{ID: "foreign", Approved: true}
			case "trailing-data":
				data, err := os.ReadFile(r.approvalPath())
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(r.approvalPath(), append(data, []byte(` {}`)...), 0600))
			case "symlink":
				other := filepath.Join(t.TempDir(), "record")
				require.NoError(t, os.Rename(r.approvalPath(), other))
				require.NoError(t, os.Symlink(other, r.approvalPath()))
			case "owner-binding":
				require.NoError(t, os.WriteFile(filepath.Join(r.dataDir, "adapter", "dsh-approval-owner.json"), []byte(`{"context_id":"foreign"}`), 0600))
			case "foreign-context", "wrong-decision":
				decision := &runtime.ApprovalDecision{ID: saved.Request.ID, Approved: true}
				if scenario == "foreign-context" {
					ctx = context.WithValue(ctx, approvalOwnerKey{}, approvalOwner{TaskID: saved.Owner.TaskID, ContextID: "foreign"})
				} else {
					decision.ID = "foreign"
				}
				_, err := (&deferredTool{runner: r, record: *saved}).Resume(ctx, decision, &recordingSink{})
				require.Error(t, err)
				return
			case "unresolved-new-turn":
				require.ErrorContains(t, r.admitApprovalTurn(ctx, ""), "unresolved")
				return
			}
			if scenario == "changed-arguments" || scenario == "changed-decision" {
				data, err := json.Marshal(saved)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(r.approvalPath(), data, 0600))
			}
			_, _, pending, err := r.RestorePending()
			require.Error(t, err)
			require.Nil(t, pending)
		})
	}
}

func TestDeferredApprovalPreflightRetryAndCancel(t *testing.T) {
	r, saved, ctx := savedApprovalFixture(t, "http://127.0.0.1:1/mcp")
	p := &deferredTool{runner: r, record: *saved}
	out, err := p.Resume(ctx, &runtime.ApprovalDecision{ID: saved.Request.ID, Approved: true}, &recordingSink{})
	require.NoError(t, err)
	require.NotNil(t, out.Pending)
	loaded, err := r.loadApproval()
	require.NoError(t, err)
	require.Equal(t, "waiting", loaded.Phase)
	require.Nil(t, loaded.Decision)
	require.NoError(t, out.Pending.Cancel(ctx, &recordingSink{}))
	r.config.Agent.HttpTools = nil
	_, _, pending, err := r.RestorePending()
	require.NoError(t, err, "completed/canceled records do not pin obsolete configuration")
	require.Nil(t, pending)
	_, err = p.Resume(ctx, &runtime.ApprovalDecision{ID: saved.Request.ID, Approved: true}, &recordingSink{})
	require.Error(t, err)
}

func TestDeferredApprovalCanceledWrongDecisionDoesNotChangeOwnership(t *testing.T) {
	r, saved, ctx := savedApprovalFixture(t, "http://127.0.0.1:1/mcp")
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	p := &deferredTool{runner: r, record: *saved}
	_, err := p.Resume(ctx, &runtime.ApprovalDecision{ID: "foreign", Approved: true}, &recordingSink{})
	require.Error(t, err)
	loaded, err := r.loadApproval()
	require.NoError(t, err)
	require.Equal(t, "waiting", loaded.Phase)
	_, err = p.Resume(ctx, &runtime.ApprovalDecision{ID: saved.Request.ID, Approved: true}, &recordingSink{})
	require.ErrorIs(t, err, context.Canceled)
	loaded, err = r.loadApproval()
	require.NoError(t, err)
	require.Equal(t, "canceled", loaded.Phase)
}

func TestApprovalConversationBinding(t *testing.T) {
	r, _, _ := savedApprovalFixture(t, "http://127.0.0.1:1/mcp")
	require.NoError(t, r.checkApprovalContext("context-1"))
	require.Error(t, r.checkApprovalContext("foreign"))
	r.config.Agent.HttpTools = nil
	require.Error(t, r.admitApprovalTurn(t.Context(), ""), "removing protected tools must not bypass pending ownership")
	other, _, _ := savedApprovalFixture(t, "http://127.0.0.1:1/mcp")
	require.NotEqual(t, r.dataDir, other.dataDir)
	require.NoError(t, os.WriteFile(filepath.Join(r.dataDir, "adapter", "dsh-approval-owner.json"), []byte(`{} {}`), 0600))
	require.Error(t, r.checkApprovalContext("context-1"))
}

func TestHeldCallCorrelationRejectsIdenticalParallelCalls(t *testing.T) {
	held := &heldTool{NativeName: "mcp__tools__deploy", Arguments: json.RawMessage(`{"version":"7"}`)}
	e := events{tools: map[string]tool{"first": {name: held.NativeName, args: map[string]any{"version": "7"}}}}
	id, err := e.heldCallID(held)
	require.NoError(t, err)
	require.Equal(t, "first", id)
	e.tools["second"] = e.tools["first"]
	_, err = e.heldCallID(held)
	require.ErrorContains(t, err, "parallel identical")
}

func TestDeferredApprovalRestoresTraceOrigin(t *testing.T) {
	for _, traced := range []bool{false, true} {
		t.Run(fmt.Sprint(traced), func(t *testing.T) {
			r, saved, _ := savedApprovalFixture(t, "http://127.0.0.1:1/mcp")
			if traced {
				saved.Origin = runtime.NewTraceOrigin(trace.NewSpanContext(trace.SpanContextConfig{
					TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled,
				}))
			}
			require.NoError(t, r.saveApproval(saved))
			reconstructed, err := New(r.config, r.executable, r.dataDir, nil)
			require.NoError(t, err)
			task, contextID, pending, err := reconstructed.RestorePending()
			require.NoError(t, err)
			require.Equal(t, saved.Owner.TaskID, task)
			require.Equal(t, saved.Owner.ContextID, contextID)
			origin := pending.(*deferredTool).TraceOrigin()
			require.Equal(t, saved.Origin.IsZero(), origin.IsZero())
			require.Equal(t, saved.Origin.SpanContext().TraceID(), origin.SpanContext().TraceID())
			require.Equal(t, saved.Origin.SpanContext().SpanID(), origin.SpanContext().SpanID())
			require.Equal(t, saved.Origin.SpanContext().TraceFlags(), origin.SpanContext().TraceFlags())
		})
	}
}

func TestApprovalForkAdmission(t *testing.T) {
	for _, phase := range []string{"unused", "complete", "denied", "canceled", "waiting", "executing", "result", "continuing", "unknown", "malformed"} {
		t.Run(phase, func(t *testing.T) {
			r, saved, _ := savedApprovalFixture(t, "http://127.0.0.1:1/mcp")
			allowed := phase == "unused" || phase == "complete" || phase == "denied" || phase == "canceled"
			saved.Phase = phase
			switch phase {
			case "unused":
				require.NoError(t, os.Remove(r.approvalPath()))
			case "unknown", "malformed":
				data, err := json.Marshal(saved)
				require.NoError(t, err)
				if phase == "malformed" {
					data = []byte(`{"phase":"complete"}`)
				}
				require.NoError(t, os.WriteFile(r.approvalPath(), data, 0600))
			default:
				if phase == "denied" {
					saved.Phase = "complete"
				}
				if phase != "waiting" && phase != "canceled" {
					saved.Decision = &runtime.ApprovalDecision{ID: saved.Request.ID, Approved: phase != "denied"}
					if phase != "executing" {
						saved.Result = &mcp.CallToolResult{IsError: phase == "denied"}
					}
				}
				require.NoError(t, r.saveApproval(saved))
			}
			original, readErr := os.ReadFile(r.approvalPath())
			if phase != "unused" {
				require.NoError(t, readErr)
			}
			forkOwner := approvalOwner{TaskID: "fork-task", ContextID: "fork-context"}
			forkCtx := context.WithValue(t.Context(), approvalOwnerKey{}, forkOwner)
			checkErr := r.checkApprovalContext(forkOwner.ContextID)
			bound, err := r.approvalContext()
			require.NoError(t, err)
			require.Equal(t, "context-1", bound, "pre-admission inspection must not rebind DATA")
			if allowed {
				require.NoError(t, checkErr)
				require.Error(t, r.admitApprovalTurn(t.Context(), ""), "rebinding requires trusted runtime identity")
				require.NoError(t, r.admitApprovalTurn(forkCtx, ""))
			} else {
				require.Error(t, checkErr)
				require.Error(t, r.admitApprovalTurn(forkCtx, ""))
				// Knowing the continuation ID cannot transfer an unresolved operation.
				require.Error(t, r.admitApprovalTurn(forkCtx, saved.Request.ID))
			}
			bound, err = r.approvalContext()
			require.NoError(t, err)
			if allowed {
				require.Equal(t, forkOwner.ContextID, bound)
			} else {
				require.Equal(t, "context-1", bound)
			}
			if phase != "unused" {
				after, err := os.ReadFile(r.approvalPath())
				require.NoError(t, err)
				require.Equal(t, original, after, "rebinding must preserve the original operation and replay tombstone")
			}
			if !allowed {
				return
			}
			if phase != "unused" {
				_, err = (&deferredTool{runner: r, record: *saved}).Resume(forkCtx,
					&runtime.ApprovalDecision{ID: saved.Request.ID, Approved: true}, &recordingSink{})
				require.Error(t, err, "fork must not replay the source operation")
			}
			// A new approval on the fork must bind to its new public task/context,
			// and must reject both the old conversation and a third conversation.
			saved.Owner, saved.Phase, saved.Decision, saved.Result = forkOwner, "waiting", nil, nil
			saved.Request.ID = "fork-approval"
			require.NoError(t, r.saveApproval(saved))
			require.NoError(t, r.checkApprovalContext(forkOwner.ContextID))
			require.Error(t, r.checkApprovalContext("context-1"))
			require.Error(t, r.checkApprovalContext("another-fork"))
			task, contextID, pending, err := r.RestorePending()
			require.NoError(t, err)
			require.Equal(t, forkOwner.TaskID, task)
			require.Equal(t, forkOwner.ContextID, contextID)
			require.NotNil(t, pending)
		})
	}
}
