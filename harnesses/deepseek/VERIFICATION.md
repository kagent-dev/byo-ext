# Migration verification

Migration acceptance: 2026-10-16; configuration and persistence follow-up:
2026-10-17; model usage follow-up: 2026-10-19. The protected-MCP fork defect
found during migration acceptance is fixed in the adapter. Container regression checks and all 15 real-cluster
acceptance scenarios passed. The remaining boundaries are listed below.

## Versions and scope

- Original implementation: kagent PR #3195 at
  `20ed48c1dfa711b08004c2346eb131bbaa36acb6`.
- kagent dependency: `0a57368aca336ea5523c6e7ac3d551aa7e9f37f1`, the merge of #3165.
- DSH: `0.2.0-rc.2`; Go: `1.27.0`; PostgreSQL test service: `16-alpine`.
- Initial clean exported source tree: `c5030ee35ddd5e7d4d7936101161c721c7c6ad3b`.
- Source tree after the fork fix and regression tests:
  `9c9c5d7beceea556a279884f63e239ee74f6d58b`. This identifies the October 16
  migration verification; the October 17 follow-up is recorded separately below.
- Production arm64 image used for the final cluster run:
  `sha256:358fb784deb5cdf4d2d3e9852eb7e387d2cbb07a8a2635a43d3c372654410783`.

The adapter is an independent Go module. Public dependencies are pinned by
`go.mod`, `go.sum` and `package-lock.json`; builds do not need another checkout.
Three upstream internal filesystem/process helpers are maintained locally with
source attribution. Shared runtime, controller and DSH sources are unchanged.

Migration changes include protobuf JSON configuration, the current TaskStore
RPC namespace, typed PendingRestorer and ToolExecutionSink contracts, persisted
trace origin, protobuf configuration cloning before selecting an MCP binding,
and cancellation after confirmed native-process cleanup.

## October 19 model usage follow-up

The adapter maps local root-session DSH v4 records to the existing kagent
`ModelCall` sink after native-process cleanup. It supports raw JSONL and the
native concatenated zstd format, associates each call with its historical model
context, includes cache reads/writes in input tokens, and preserves native timing,
finish reason and available response ID. It does not use ACP context occupancy
as model usage. Shared kagent and DSH sources remain unchanged.

Synthetic parser tests cover cache accounting, missing and invalid counts,
integer overflow, historical model changes, partial calls, repeated emission,
restore/fork baselines, late repairs, truncated logs, ambiguous paths and format
rejection. Real DSH tests verify nonzero cached usage across a new Runner using
persisted DATA, protected-MCP approval restoration without duplicate calls,
completed tool-call usage during native approval cancellation, and partial usage
when a model stream is canceled. These initial tests assert the adapter's
`ModelCall` output; the OTLP follow-up below checks its export as well.

The default AMD64 production image was built from clean exported source tree
`41eac873758b4f7f52d362a3897dc692bba3dc72`, using only the harness directory,
without credentials or extra build arguments. Subsequent changes only update
this report. Configuration/version/supervisor validation and non-root `/readyz`
HTTP 200 passed. Host short race tests and host/Linux lint passed.

The complete Linux arm64 bridge race suite passed: 51 top-level tests and
143 passing test/subtest entries, with no failures, skips or race reports.
The Linux usage package added 6 top-level tests and 22 passing test/subtest
entries. Real DSH and disposable PostgreSQL ran with all capabilities dropped
and `no-new-privileges`. Temporary containers and the test network were removed.

Usage remains best effort: subagent calls, provider retries, separate reasoning
counts and request IDs are unavailable on this path. Crash-time telemetry is
not backfilled into new tasks. Invalid or oversized logs disable usage reporting
without failing the conversation. Native permissions defer emission while the
process is alive. See the README for parser limits and reporting semantics.
This follow-up does not repeat cluster acceptance or call a live provider.

### OTLP export follow-up

`TestRealDSHUsageOTLP` exercises real DSH, the production A2A server, shared
TaskStore/settlement wrapper and SDK OTLP exporters. A local HTTP/protobuf
receiver decodes the exported gzip-compressed traces and metrics. The controller
TaskStore and model/MCP services remain local fixtures. This is not a Kubernetes
rerun, a deployed collector/backend test, or a live provider/billing check.

The test constructs the initial executor before SDK initialization and starts
metrics on first execution, matching production initialization order. It parks a
protected MCP call, reconstructs the Runner and Executor from DATA while retaining
the controller fixture, then approves the original task. It checks:

- Exactly one model span per segment, with request/response model, native provider
  alias, timing and cache counts preserved in the exported payload.
- The original task/context identity on both parent spans, the correct initial
  or resumed parent for each model response, and a persisted resume-origin link.
- Cumulative input/output/cache-read totals of 12/3/2 before approval and 24/6/4
  afterward. Cache reads are included in input, not added to it a second time.
- Repeated flushes and a rejected late approval add no calls or tokens; the MCP
  operation executes once. Resources retain the service name without claiming
  another registered kagent runtime.

The test runs in an isolated child process so SDK globals do not affect parallel
bridge tests. It retains one SDK provider across executor reconstruction; it does
not assert cross-process metric aggregation after Actor replacement. The local
receiver required the signal-specific OTLP HTTP paths and gzip decoding; those
fixture corrections did not require changes to the adapter or shared runtime.

The full Linux arm64 race suite passed with 52 top-level tests and 144 unique
passing test/subtest cases, with zero failures, skips or race reports. The usage
parser suite passed 22 cases. After adding production resource identity to the
OTLP fixture, its focused race test and Linux lint passed again. Host short race
tests and host lint also passed. No production logic changed in this follow-up.

The final default AMD64 image built from clean exported source tree
`2c6fc907e331a5f00790fc7b39a5b34211345e2c`; subsequent changes only update this
report. The production image also passed non-root configuration/version/
supervisor validation and readiness HTTP 200. Temporary PostgreSQL and the
verification network were removed after the suite completed.

## October 17 follow-up

Following the independent findings in
[kagent #3194](https://github.com/kagent-dev/kagent/issues/3194#issuecomment-6231088406),
the adapter explicitly disables `session-log-deepseek` and
`plugin-package-inventory-deepseek`. Local `session-persistence-jsonl` and
`storage-json` remain enabled. This is configuration hardening: the current
`llm-pi-ai` provider path had not exhibited the upload fields in model requests.

The existing completion barrier is unchanged. In pinned DSH `0.2.0-rc.2`,
`session/close` drains activity and awaits persistence flush. A new A2A/TaskStore
regression test uses a controlled ACP subprocess to delay that response after
assistant output and the prompt response. Neither the call nor settlement may
complete while the gate is held. Releasing it succeeds; returning a close error
instead settles the task as failed. This simulates the ACP persistence contract;
it does not reproduce the reported 1 ms crash window or inject a real disk failure.

The real-DSH two-turn recovery test now checks both model request bodies for
absence of `dsh_session_log` and `dsh_plugin_packages`, while verifying that the
second process retains the first turn's history. Configuration tests also require
both upload plugins to be disabled and local persistence to remain enabled.

The AMD64 production image was built without extra build arguments or credentials
from clean exported source tree `9e83660b2f1518934f3a8e61e29ea54cad426162`.
Subsequent changes only update this report. The non-root AMD64 smoke test passed
configuration/version/supervisor validation and returned `/readyz` HTTP 200.
Host short race tests, Linux configuration race tests, and host/Linux lint passed.
The complete Linux arm64 bridge race suite passed with 49 top-level tests and
141 passing test/subtest entries, with zero failures, skips or race reports.
It used real DSH and a disposable PostgreSQL service; test containers dropped all
capabilities and enabled `no-new-privileges`. Temporary containers and the test
network were removed after verification.

At that checkpoint, per-call usage reporting, incremental ACP output and
compatibility with newer DSH versions remained separate work. The October 19
follow-up above adds per-call usage for the pinned provider path. The October 17
changes did not modify kagent core or DSH sources, or repeat the October 16
real-cluster acceptance below.

## October 16 passed checks

The fork-fix rerun rebuilt both harness image architectures and repeated AMD64
startup, the full Linux bridge race suite, host short race tests, both lint
targets and real-cluster acceptance. Approval-service image/startup checks and
standalone SQL/configuration/helper checks below are retained from the initial
migration acceptance; those components were unchanged by this fix.

| Check | Result |
| --- | --- |
| Default production image, Linux AMD64 | Rebuilt after the fork fix, harness directory as the complete context; no extra build arguments or credentials. |
| Optional approval-service image, Linux AMD64 | Built from the initial clean export; the fork fix does not change this service. |
| Production harness startup, AMD64 emulated on arm64 | Configuration, pinned DSH version and supervisor validation passed; `/readyz` returned 200 as UID/GID 65532. |
| Approval-service startup, AMD64 emulated on arm64 | Started with fixture secrets and disposable PostgreSQL; `/readyz` returned 204 as UID/GID 65532. |
| Full Linux arm64 bridge suite with race instrumentation | 48 top-level tests; 138 passing test/subtest entries; zero failures, skips or race reports. Used real DSH, HTTP MCP, private A2A gRPC and the production TaskStore/settlement wrapper with a controller RPC fixture. |
| Protected MCP approval/recovery | All eleven concurrent live/reconstructed scenarios passed, including approval, denial, waiting cancellation, uncertain dispatch, continuation cancellation and saved-result reuse without redispatch. Reconstructed settled DATA admitted a new context after allow/deny/cancel, while late source approvals were rejected without new effects. |
| Native process death and cleanup | Allow/reject/cancel after native process death passed, followed by a new task. Detached/orphaned tools, adapter/supervisor death and settlement ordering passed. Missing cleanup confirmation still blocks completion. |
| External approval service | Real DSH + MCP + PostgreSQL restart workflow passed in the full Linux run. Host `go test -race -count=1 ./internal/operations` also passed, including migrations and SQL checks. |
| Configuration and local filesystem helpers | Race-instrumented Linux tests passed separately. |
| Static and host checks | Host and Linux-targeted golangci-lint 2.13.0 reported zero issues. Host short race suite and Git whitespace checks passed. |
| Migrated control plane, Linux arm64 / gVisor | Three public A2A turns, automatic external DATA snapshots, persisted GetTask, explicit suspend/resume, conversation history and workspace write/read/edit/read passed against the pinned controller and real PostgreSQL. |
| Cluster MCP execution and recovery | Ordinary success, tool error, approval, denial, waiting cancellation, approved tool error, and subsequent turns passed. Approved calls retained the original task/context and executed once; denial/cancellation executed zero remote calls. |
| Worker replacement after a DATA snapshot | Deleted the dedicated WorkerPool Pod while approval was suspended, observed a different ready Pod UID and confirmed the resumed Actor's assignment used that new UID. Approval completed on the original public task/context with one remote execution; the next turn passed. |
| Cluster fork boundaries | Unprotected forks and protected forks after completion, denial, cancellation or no approval retained history and continued without repeating tools. A completed protected fork approved a new operation once; its source conversation remained usable. A pending approval correctly failed the shared terminal-only checkpoint precondition, and its source approval still completed afterward. |

Linux test containers and both image smoke checks ran with all capabilities
dropped and `no-new-privileges`. The complete runtime regression suite ran on
Linux arm64; AMD64 coverage consists of builds and startup smoke checks, not a
second full runtime regression run.

The first full run exposed cancellation after native process death returning an
ACP error despite confirmed descendant cleanup. The corrected path passed the
complete rerun. A separate environment failure exhausted host disk and caused
Docker metadata/filesystem I/O errors; interrupted results were discarded.
After freeing regenerable cache and restarting Docker with user approval, all
checks above were completed. The readiness probe for the approval service was
corrected to expect its documented 204 response, rather than 200.

## Real cluster acceptance

The migration was tested in an independently owned Kind cluster using Kubernetes
`1.37.0`, Substrate `0.5.0-alpha2`, a controller compiled from the exact kagent
dependency commit, the production arm64 DSH image, and local model/MCP fixtures.
No real provider credentials were used. Snapshot assertions checked both the
external snapshot URI and its `DATA` content scope through the public system API.

The final run passed two baseline tests and all thirteen MCP scenarios:
ordinary success/error, approval, denial, cancellation, approved tool error,
worker replacement, unprotected fork, completed/denied/canceled/unused protected
forks, and the pending-checkpoint rejection boundary. The completed-fork case
also approved a new operation in the fork, verified exactly one new remote call,
and continued the source conversation without replaying the original operation.

Earlier fixture errors (forwarded MCP Host validation and an approved decision
carrying a rejection reason) were corrected before the original defect was
confirmed. In the final run, a chart-fetch interruption and a dependency image
pull timeout required installation retries; acceptance started only after the
cluster was ready. None of these fixture or installation errors was counted as
a passing application test.

### Fixed adapter defect: protected-MCP fork

The original reproduction approved a protected MCP call, waited for task
completion and an external DATA snapshot, then created a checkpoint and fork.
Public history was copied with new task/context identities, but the next message
failed with `FailedPrecondition: DSH approval DATA belongs to another conversation`.
The copied `adapter/dsh-approval-owner.json` still contained the source context.

The adapter now permits a trusted, admitted runtime turn to rebind that file
when the saved operation is complete, canceled, or absent. Denied operations use
the complete phase. The pre-admission check does not mutate DATA, and the settled
operation journal remains intact as a replay tombstone. Waiting, executing,
result and continuing records retain their original task/context ownership;
malformed or unknown records remain errors. Controller authorization and the
shared terminal-only checkpoint contract are unchanged.

Regression tests cover all ten state cases, untrusted admission, a foreign owner
presenting the saved continuation ID, unchanged journal bytes, rejected old
approvals, and ownership
of a new pending operation after rebinding. Real DSH reconstruction and the
cluster checkpoint/fork run passed. No shared core or DSH source change is needed
for this fix.

## Commands

From the repository root:

```sh
docker build --platform linux/amd64 -t byo-ext/deepseek:amd64 harnesses/deepseek
docker build --platform linux/amd64 -t byo-ext/deepseek-approval:amd64 \
  -f harnesses/deepseek/approval-mcp/Dockerfile harnesses/deepseek
docker build --target test -t byo-ext/deepseek:test harnesses/deepseek
```

For the full bridge run, provide a disposable PostgreSQL database reachable from
the container and export `DSH_APPROVAL_TEST_DATABASE`. This fixture must not be
pointed at production data. Set `TEST_NETWORK` to the Docker network containing
that database:

```sh
docker run --rm --network "$TEST_NETWORK" --cap-drop=ALL \
  --security-opt=no-new-privileges -e DSH_APPROVAL_TEST_DATABASE \
  byo-ext/deepseek:test -test.v -test.timeout=10m
```

The usage parser and the OTLP export test can also run independently, without
PostgreSQL or provider credentials:

```sh
docker run --rm --cap-drop=ALL --security-opt=no-new-privileges \
  --entrypoint /tests/usage.test byo-ext/deepseek:test -test.v
docker run --rm --cap-drop=ALL --security-opt=no-new-privileges \
  byo-ext/deepseek:test -test.v -test.run '^TestRealDSHUsageOTLP$' -test.timeout=3m
```

The OTLP test runs the production A2A server and SDK exporters in an isolated
child process. Its local receiver decodes HTTP/protobuf traces and metrics;
it is a collector-protocol fixture, not a deployed collector/backend. The test
uses the server's readiness port 8081, so run it in the dedicated container.

Without the database variable, the external approval integration test skips;
that is not equivalent to the complete acceptance above. Repository CI checks
only the default image build.

## Remaining boundaries

- Pending approvals cannot be checkpointed under the shared terminal-only
  contract; that rejection is expected. The settled-owner fix does not allow
  unresolved operations to move between conversations.
- Recovery from a completed external DATA snapshot does not establish recovery
  from arbitrary Actor deletion, crashes before settlement/snapshot publication,
  or worker loss during an in-flight remote effect. These remain separate from
  the tested suspension/reconstruction paths. No live model-provider requests
  were made in this acceptance.
- Native ACP permission requests still need a live native process. External MCP
  composition does not reconstruct the original native stack. Unknown remote
  outcomes are not replayed automatically; generic exactly-once execution and
  remote reconciliation remain outside scope.
- Shared orphan-task/fencing and unpublished-settlement issues remain tracked in
  the README. This migration does not bundle their core fixes.

Temporary test containers, database volume and Docker network were removed.
The pre-existing `kagent-poc-control-plane` was confirmed running with its
Kubernetes node Ready after Docker recovery and after cluster acceptance.
The dedicated acceptance cluster, its local registry and temporary core checkout
were removed after preserving test sources and logs. Built images remain local
for review. Nothing was pushed to a public registry or repository, and no issue,
PR or comment was published by this acceptance run.
