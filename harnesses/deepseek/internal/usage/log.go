// Package usage reads model-call evidence from pinned DSH v4 session logs.
// It deliberately ignores ACP context-occupancy updates, which are not usage.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

const (
	maxFileBytes    = 64 << 20
	maxDecodedBytes = 128 << 20
	maxLineBytes    = 16 << 20
	maxCalls        = 4096
	maxEntries      = 4096
)

// Snapshot retains only telemetry, never prompts or tool contents.
type Snapshot struct {
	Seq   int64
	Turn  int
	Calls []Call
}

type Call struct {
	Seq         int64
	Turn        int
	Interrupted bool
	Value       runtime.ModelCall
}

// Read supports the pinned native zstd format and raw JSONL fixtures. Missing,
// ambiguous, corrupt or unsupported logs are errors; callers must not invent
// metrics or fail a conversation solely because telemetry is unavailable.
func Read(rootPath, sessionID string) (Snapshot, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return Snapshot{}, err
	}
	defer root.Close()
	var candidates []string
	entries := 0
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("session directory entry limit exceeded")
		}
		if d.IsDir() {
			return nil
		}
		// Native ACP session IDs are directory names in the pinned format. Ignore
		// sibling/subagent sessions: their calls cannot be attributed to this root.
		if filepath.Base(filepath.Dir(path)) != sessionID {
			return nil
		}
		if strings.HasPrefix(d.Name(), "session.v") && strings.Contains(d.Name(), ".jsonl") {
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("session log is a symlink")
			}
			if d.Name() != "session.v4.jsonl" && d.Name() != "session.v4.jsonl.zstd" {
				return fmt.Errorf("unsupported session log generation")
			}
			candidates = append(candidates, path)
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	if len(candidates) == 0 {
		return Snapshot{}, fs.ErrNotExist
	}
	if len(candidates) != 1 {
		return Snapshot{}, fmt.Errorf("ambiguous session logs")
	}
	f, err := root.Open(candidates[0])
	if err != nil {
		return Snapshot{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Snapshot{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return Snapshot{}, fmt.Errorf("invalid session log size or type")
	}
	reader := io.LimitReader(f, maxFileBytes+1)
	if strings.HasSuffix(candidates[0], ".zstd") {
		decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxDecodedBytes))
		if err != nil {
			return Snapshot{}, fmt.Errorf("invalid compressed session log")
		}
		defer decoder.Close()
		reader = decoder
	}
	return parse(reader, sessionID)
}

// completeLines ignores an unterminated tail, including a complete JSON value
// lacking its newline. A truncated/corrupt zstd frame still fails the snapshot.
func completeLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return 0, nil, nil
	}
	return 0, nil, nil
}

type record struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
}

type counts struct {
	Input      *int64 `json:"inputTokens"`
	Output     *int64 `json:"outputTokens"`
	CacheRead  *int64 `json:"cacheReadTokens"`
	CacheWrite *int64 `json:"cacheWriteTokens"`
	Total      *int64 `json:"totalTokens"`
}

type assistant struct {
	Turn        int     `json:"turn"`
	Step        int     `json:"step"`
	Interrupted bool    `json:"interrupted"`
	Usage       *counts `json:"usage"`
	Message     struct {
		Source struct {
			Kind        string `json:"kind"`
			Provider    string `json:"provider"`
			Model       string `json:"model"`
			ReplayState struct {
				Response struct {
					Kind       string `json:"kind"`
					ResponseID string `json:"responseId"`
				} `json:"response"`
			} `json:"replayState"`
		} `json:"source"`
	} `json:"message"`
	Stream []struct {
		Type  string `json:"type"`
		Chunk struct {
			Type   string `json:"type"`
			Reason struct {
				Kind string `json:"kind"`
			} `json:"reason"`
		} `json:"chunk"`
	} `json:"stream"`
}

func parse(r io.Reader, sessionID string) (Snapshot, error) {
	limited := &io.LimitedReader{R: r, N: maxDecodedBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	scanner.Split(completeLines)
	if !scanner.Scan() {
		return Snapshot{}, fmt.Errorf("missing session header")
	}
	var header struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
		ID      string `json:"id"`
	}
	if json.Unmarshal(scanner.Bytes(), &header) != nil || header.Type != "session" || header.Version != 4 || header.ID != sessionID {
		return Snapshot{}, fmt.Errorf("session header identity or version mismatch")
	}
	var snapshot Snapshot
	type modelContext struct{ Provider, Model string }
	var request modelContext
	starts := make(map[[2]int]int64)
	previousSeq := int64(-1)
	records := 0
	for scanner.Scan() {
		records++
		if records > 100000 {
			return Snapshot{}, fmt.Errorf("session record limit exceeded")
		}
		var rec record
		if json.Unmarshal(scanner.Bytes(), &rec) != nil || rec.Seq <= previousSeq {
			return Snapshot{}, fmt.Errorf("invalid session record or sequence")
		}
		previousSeq, snapshot.Seq = rec.Seq, rec.Seq
		switch rec.Type {
		case "request/context":
			request = modelContext{}
			if json.Unmarshal(rec.Data, &request) != nil {
				return Snapshot{}, fmt.Errorf("invalid model context")
			}
		case "turn/start", "step/start":
			var step struct{ Turn, Step int }
			if json.Unmarshal(rec.Data, &step) != nil || step.Turn < 1 {
				return Snapshot{}, fmt.Errorf("invalid turn identity")
			}
			snapshot.Turn = max(snapshot.Turn, step.Turn)
			if rec.Type == "step/start" {
				if step.Step < 1 || rec.Time <= 0 {
					return Snapshot{}, fmt.Errorf("invalid model step")
				}
				starts[[2]int{step.Turn, step.Step}] = rec.Time
			}
		case "assistant/message":
			var data assistant
			if json.Unmarshal(rec.Data, &data) != nil {
				return Snapshot{}, fmt.Errorf("invalid assistant record")
			}
			snapshot.Turn = max(snapshot.Turn, data.Turn)
			if data.Message.Source.Kind != "model" {
				continue
			}
			call, err := mapCall(rec, data, request.Provider, request.Model, starts[[2]int{data.Turn, data.Step}])
			if err != nil {
				return Snapshot{}, err
			}
			if len(snapshot.Calls) >= maxCalls {
				return Snapshot{}, fmt.Errorf("model call limit exceeded")
			}
			snapshot.Calls = append(snapshot.Calls, call)
		}
	}
	if scanner.Err() != nil || limited.N <= 0 {
		return Snapshot{}, fmt.Errorf("unreadable or oversized session log")
	}
	return snapshot, nil
}

func mapCall(rec record, data assistant, provider, requestModel string, start int64) (Call, error) {
	if data.Turn < 1 || data.Step < 1 || start <= 0 || rec.Time < start {
		return Call{}, fmt.Errorf("invalid model call timing or identity")
	}
	source := data.Message.Source
	if source.Provider != "" {
		provider = source.Provider
	}
	value := runtime.ModelCall{Provider: provider, RequestModel: requestModel, ResponseModel: source.Model, Start: time.UnixMilli(start), End: time.UnixMilli(rec.Time)}
	if source.ReplayState.Response.Kind == "pi-ai" {
		value.ResponseID = source.ReplayState.Response.ResponseID
	}
	for _, item := range data.Stream {
		if item.Type != "chunk" || item.Chunk.Type != "finish" {
			continue
		}
		switch item.Chunk.Reason.Kind {
		case "stop":
			value.StopReason = tracing.FinishReasonStop
		case "tool-calls":
			value.StopReason = tracing.FinishReasonToolCall
		case "max-tokens":
			value.StopReason = tracing.FinishReasonLength
		case "error":
			value.StopReason = tracing.FinishReasonError
		}
	}
	if data.Usage == nil {
		value.UsagePartial = true
	} else {
		u := data.Usage
		// Pinned llm-pi-ai/openai-completions excludes cache tokens from input.
		// ModelCall includes them. Reasoning is already included in output and is
		// not independently available on this path; do not synthesize a count.
		input, err := sum(u.Input, u.CacheRead, u.CacheWrite)
		if err != nil {
			return Call{}, err
		}
		output, err := sum(u.Output)
		if err != nil {
			return Call{}, err
		}
		total, err := sum(&input, &output)
		if err != nil {
			return Call{}, err
		}
		if u.Total != nil && (*u.Total < total || (u.Input != nil && u.Output != nil && *u.Total != total)) {
			return Call{}, fmt.Errorf("inconsistent model usage total")
		}
		value.InputTokens, value.OutputTokens = input, output
		value.CacheReadTokens, _ = sum(u.CacheRead)
		value.CacheWriteTokens, _ = sum(u.CacheWrite)
		value.UsagePartial = u.Input == nil || u.Output == nil
	}
	// Interrupted does not necessarily mean canceled (for example, process loss).
	if data.Interrupted {
		value.UsagePartial = true
		value.StopReason = tracing.FinishReasonError
	}
	return Call{Seq: rec.Seq, Turn: data.Turn, Interrupted: data.Interrupted, Value: value}, nil
}

func sum(values ...*int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value == nil {
			continue
		}
		if *value < 0 || *value > math.MaxInt64-total {
			return 0, fmt.Errorf("invalid model usage count")
		}
		total += *value
	}
	return total, nil
}

// Cursor is scoped to one native prompt. Starting from an existing log also
// excludes later repair records for earlier turns. No telemetry is replayed
// from a previous process or copied conversation after restore/fork.
type Cursor struct {
	Seq  int64
	Turn int
}

func (c *Cursor) NewCalls(snapshot Snapshot, canceled bool) ([]runtime.ModelCall, error) {
	if snapshot.Seq < c.Seq || snapshot.Turn < c.Turn {
		return nil, errors.New("session usage log moved backwards")
	}
	var calls []runtime.ModelCall
	for _, call := range snapshot.Calls {
		if call.Seq <= c.Seq || call.Turn <= c.Turn {
			continue
		}
		value := call.Value
		value.Canceled = call.Interrupted && canceled
		calls = append(calls, value)
	}
	c.Seq = snapshot.Seq
	return calls, nil
}
