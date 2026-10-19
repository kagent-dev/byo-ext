package usage

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

func logLines(t *testing.T, values ...any) string {
	t.Helper()
	var b strings.Builder
	for _, value := range values {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}
func event(kind string, seq int64, data any) map[string]any {
	return map[string]any{"type": kind, "seq": seq, "time": 1000 + seq, "data": data}
}
func header() map[string]any { return map[string]any{"type": "session", "version": 4, "id": "session"} }
func message(turn int, model, reason string, u any, interrupted bool) map[string]any {
	return map[string]any{"turn": turn, "step": 1, "interrupted": interrupted, "usage": u,
		"message": map[string]any{"source": map[string]any{"kind": "model", "provider": "kagent-byo", "model": model, "replayState": map[string]any{"response": map[string]any{"kind": "pi-ai", "responseId": "response-1"}}}},
		"stream":  []any{map[string]any{"type": "chunk", "chunk": map[string]any{"type": "finish", "reason": map[string]string{"kind": reason}}}}}
}
func sample(t *testing.T) string {
	return logLines(t, header(),
		event("request/context", 1, map[string]string{"provider": "kagent-byo", "model": "a"}),
		event("step/start", 2, map[string]int{"turn": 1, "step": 1}),
		event("assistant/message", 3, message(1, "a", "tool-calls", map[string]int{"inputTokens": 10, "cacheReadTokens": 2, "cacheWriteTokens": 3, "outputTokens": 4, "totalTokens": 19}, false)),
		event("request/context", 4, map[string]string{"provider": "kagent-byo", "model": "b"}),
		event("step/start", 5, map[string]int{"turn": 2, "step": 1}),
		event("assistant/message", 6, message(2, "b", "stop", nil, true)))
}

func TestMappingAndHistoricalModelContext(t *testing.T) {
	snapshot, err := parse(strings.NewReader(sample(t)), "session")
	require.NoError(t, err)
	require.Len(t, snapshot.Calls, 2)
	a, b := snapshot.Calls[0].Value, snapshot.Calls[1].Value
	require.Equal(t, "a", a.RequestModel)
	require.Equal(t, "b", b.RequestModel)
	require.Equal(t, int64(15), a.InputTokens)
	require.Equal(t, int64(2), a.CacheReadTokens)
	require.Equal(t, int64(3), a.CacheWriteTokens)
	require.Equal(t, int64(4), a.OutputTokens)
	require.Equal(t, "response-1", a.ResponseID)
	require.Equal(t, tracing.FinishReasonToolCall, a.StopReason)
	require.Equal(t, int64(1002), a.Start.UnixMilli())
	require.Equal(t, int64(1003), a.End.UnixMilli())
	require.False(t, a.UsagePartial)
	require.True(t, b.UsagePartial)
	require.False(t, b.Canceled, "an interrupted record alone does not establish user cancellation")
}

func TestCursorExcludesHistoryRepairAndRepeatedEmission(t *testing.T) {
	snapshot, err := parse(strings.NewReader(sample(t)), "session")
	require.NoError(t, err)
	cursor := Cursor{Seq: 3, Turn: 1}
	// A late repair of the previous turn must not be attributed to this prompt.
	repair := snapshot.Calls[0]
	repair.Seq = 4
	snapshot.Calls = append([]Call{repair}, snapshot.Calls...)
	calls, err := cursor.NewCalls(snapshot, true)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.True(t, calls[0].Canceled)
	calls, err = cursor.NewCalls(snapshot, true)
	require.NoError(t, err)
	require.Empty(t, calls)
	// A new native process/fork starts from the complete copied log.
	restored := Cursor{Seq: snapshot.Seq, Turn: snapshot.Turn}
	calls, err = restored.NewCalls(snapshot, false)
	require.NoError(t, err)
	require.Empty(t, calls)
	_, err = restored.NewCalls(Snapshot{Seq: 1, Turn: 1}, false)
	require.Error(t, err)
}

func TestCountsValidateAndPreserveUnknownUsage(t *testing.T) {
	for _, tc := range []struct {
		name         string
		usage        any
		bad, partial bool
	}{
		{"zero", map[string]int{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0}, false, false},
		{"missing", nil, false, true},
		{"input only", map[string]int{"inputTokens": 3, "totalTokens": 8}, false, true},
		{"negative", map[string]int{"inputTokens": -1, "outputTokens": 1}, true, false},
		{"overflow", map[string]int64{"inputTokens": math.MaxInt64, "cacheReadTokens": 1}, true, false},
		{"double counted cache", map[string]int{"inputTokens": 12, "cacheReadTokens": 2, "outputTokens": 3, "totalTokens": 15}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := logLines(t, header(), event("step/start", 1, map[string]int{"turn": 1, "step": 1}), event("assistant/message", 2, message(1, "a", "max-tokens", tc.usage, false)))
			s, err := parse(strings.NewReader(data), "session")
			if tc.bad {
				require.Error(t, err)
				require.Empty(t, s.Calls)
				return
			}
			require.NoError(t, err)
			require.Len(t, s.Calls, 1)
			require.Equal(t, tc.partial, s.Calls[0].Value.UsagePartial)
			require.Equal(t, tracing.FinishReasonLength, s.Calls[0].Value.StopReason)
		})
	}
}

func TestRawAndConcatenatedZstdLogs(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "zstd"}[compressed], func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "project", "session")
			require.NoError(t, os.MkdirAll(dir, 0700))
			data := []byte(sample(t))
			suffix := ""
			if compressed {
				encoder, err := zstd.NewWriter(nil)
				require.NoError(t, err)
				defer encoder.Close()
				var frames []byte
				for _, line := range strings.SplitAfter(string(data), "\n") {
					if line != "" {
						frames = append(frames, encoder.EncodeAll([]byte(line), nil)...)
					}
				}
				data = frames
				suffix = ".zstd"
			} else {
				data = append(data, []byte(`{"type":"assistant/message"`)...)
			}
			path := filepath.Join(dir, "session.v4.jsonl"+suffix)
			require.NoError(t, os.WriteFile(path, data, 0600))
			s, err := Read(root, "session")
			require.NoError(t, err)
			require.Len(t, s.Calls, 2)
			if compressed {
				require.NoError(t, os.WriteFile(path, data[:len(data)-3], 0600))
				_, err = Read(root, "session")
				require.Error(t, err)
			}
		})
	}
}

func TestRejectsInvalidLogs(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"wrong header", strings.Replace(sample(t), `"id":"session"`, `"id":"other"`, 1)},
		{"future version", strings.Replace(sample(t), `"version":4`, `"version":5`, 1)},
		{"sequence regression", sample(t) + logLines(t, event("turn/end", 1, map[string]int{"turn": 2}))},
		{"malformed complete record", sample(t) + "{invalid}\n"},
		{"oversized record", logLines(t, header()) + strings.Repeat("x", maxLineBytes) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parse(strings.NewReader(tc.data), "session")
			require.Error(t, err)
			require.Empty(t, s.Calls)
		})
	}
}

func TestRejectsAmbiguousUnsupportedAndSymlinkLogs(t *testing.T) {
	for _, mode := range []string{"ambiguous", "unsupported", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "project", "session")
			require.NoError(t, os.MkdirAll(dir, 0700))
			path := filepath.Join(dir, "session.v4.jsonl")
			require.NoError(t, os.WriteFile(path, []byte(sample(t)), 0600))
			switch mode {
			case "ambiguous":
				require.NoError(t, os.WriteFile(path+".zstd", nil, 0600))
			case "unsupported":
				require.NoError(t, os.WriteFile(filepath.Join(dir, "session.v5.jsonl"), nil, 0600))
			case "symlink":
				require.NoError(t, os.Remove(path))
				outside := filepath.Join(t.TempDir(), "log")
				require.NoError(t, os.WriteFile(outside, []byte(sample(t)), 0600))
				require.NoError(t, os.Symlink(outside, path))
			}
			_, err := Read(root, "session")
			require.Error(t, err)
		})
	}
}
