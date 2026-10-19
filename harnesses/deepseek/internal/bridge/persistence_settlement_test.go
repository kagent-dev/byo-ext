//go:build linux

package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

// The subprocess has already emitted assistant output and the prompt response
// when it reaches the gate. This exercises the real A2A executor and TaskStore
// wrapper, while simulating the pinned DSH session/close flush contract.
func TestSettlementWaitsForSessionPersistence(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("flush_failure=%t", fail), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var flushed atomic.Bool
			gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				if fail {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				flushed.Store(true)
				w.WriteHeader(http.StatusOK)
			}))
			defer gate.Close()
			defer unblock()
			runner := testRunner(t, "")
			runner.environment = append(runner.environment, "KAGENT_DSH_TEST_CLOSE_GATE="+gate.URL)
			store, err := continuation.New(filepath.Join(runner.dataDir, "adapter"), "dsh-byo", ValidateSessionID)
			require.NoError(t, err)
			executor, err := NewExecutor(runner, store, tracing.RuntimeTelemetry{})
			require.NoError(t, err)
			client, api := newSettlementClient(t, executor, func(task *a2apb.Task) error {
				if task.Status.State == a2apb.TaskState_TASK_STATE_COMPLETED && !flushed.Load() {
					return fmt.Errorf("successful settlement preceded persistence flush")
				}
				return nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			msg := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("work"))
			msg.ContextID = uuid.NewString()
			req, err := pbconv.ToProtoSendMessageRequest(&a2atype.SendMessageRequest{Message: msg})
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { _, err := client.SendMessage(ctx, req); done <- err }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("task returned before session/close: %v", err)
			case <-ctx.Done():
				t.Fatal("session/close did not reach the persistence gate")
			}
			select {
			case task := <-api.settled:
				t.Fatalf("task settled before flush was released: %s", task.Status.State)
			case err := <-done:
				t.Fatalf("task returned before flush was released: %v", err)
			case <-time.After(200 * time.Millisecond):
			}
			unblock()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("task did not return after session/close")
			}
			select {
			case task := <-api.settled:
				want := a2apb.TaskState_TASK_STATE_COMPLETED
				if fail {
					want = a2apb.TaskState_TASK_STATE_FAILED
				}
				require.Equal(t, want, task.Status.State)
			case <-ctx.Done():
				t.Fatal("task never settled")
			}
		})
	}
}
