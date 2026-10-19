package bridge

import (
	"context"
	"path/filepath"

	"github.com/kagent-dev/byo-ext/harnesses/deepseek/internal/usage"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

func (p *process) beginUsage(ctx context.Context, resumed bool) {
	if !resumed {
		p.usage = &usage.Cursor{}
		return
	}
	snapshot, err := usage.Read(filepath.Join(p.runner.dataDir, "dsh", "sessions"), p.events.sessionID)
	if err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "DSH model usage baseline unavailable", "error", err)
		return
	}
	p.usage = &usage.Cursor{Seq: snapshot.Seq, Turn: snapshot.Turn}
}

// Read only once native writers have stopped. Native approvals keep their
// cursor until resume/cancel; protected MCP parking has already stopped DSH.
// A restarted invocation establishes a new baseline instead of backfilling old
// calls into a different task/trace. Crash-time telemetry loss remains possible:
// there is no atomic transaction between the native log and the telemetry sink.
func (p *process) reportUsage(ctx context.Context, sink runtime.ModelCallSink) {
	select {
	case <-p.done:
	default:
		return
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	if p.usage == nil {
		return
	}
	snapshot, err := usage.Read(filepath.Join(p.runner.dataDir, "dsh", "sessions"), p.events.sessionID)
	if err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "DSH model usage unavailable", "error", err)
		return
	}
	calls, err := p.usage.NewCalls(snapshot, p.usageCanceled.Load())
	if err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "DSH model usage unavailable", "error", err)
		return
	}
	for _, call := range calls {
		sink.ModelCall(call)
	}
}
