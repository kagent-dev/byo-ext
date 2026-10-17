# DeepSeek Harness

This image runs DeepSeek Harness (DSH) `0.2.0-rc.2` behind kagent's existing
`spec.byo` contract. It adds no CRD fields or native runtime type. The bridge
reads `KAGENT_CONFIG_JSON` and `KAGENT_AGENT_CARD_JSON`, uses the shared A2A
executor, and serves private gRPC on port 80 and readiness on port 8081.

This is an experimental community integration maintained by **@Jacob953**.
kagent maintainers do not own its maintenance or support. Source provenance and
locally maintained upstream helpers are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
Implementation and validation used AI assistance and require human review.

## Compatibility

The Go dependency is pinned to kagent commit
`0a57368aca336ea5523c6e7ac3d551aa7e9f37f1` (the merge of
[#3165](https://github.com/kagent-dev/kagent/pull/3165)), with DSH `0.2.0-rc.2`.
Production process supervision requires Linux. Use the
[kagent BYO guide](https://github.com/kagent-dev/website/blob/main/docs-site/content/kagent/1.x/agents/bring-your-own-agent.md)
for the surrounding installation. The verification report below separates the
current migration checks from earlier cluster checks on an older control plane.

## Supported configuration

- OpenAI-compatible Chat Completions ModelConfig, including a custom base URL
  and headers. Provider credentials remain gateway-injected Secret references.
- The compiled system prompt and one root conversation per Actor.
- Whole Streamable HTTP MCP servers with compiled headers, HTTP timeouts and
  session termination policy, enforced by a loopback Go MCP proxy.
- Text replies and text tool results, including tool errors.
- ACP cancellation and session resumption after the DSH process exits.
- DSH one-shot permission requests mapped to kagent's approval messages.
- `requireApproval` on HTTP MCP servers, enforced by the adapter before dispatch
  and answered through the same kagent HITL flow.

ACP emits committed semantic updates, rather than raw provider token deltas.
Context occupancy is not per-call token usage, so the bridge does not report
model token totals from ACP usage updates. DSH's own telemetry exporter,
session-log upload and plugin-inventory upload plugins, and auxiliary web tools
are explicitly disabled; the kagent wrapper retains its telemetry path.
Local session persistence remains enabled for conversation recovery.

Unsupported configuration is rejected at startup: Responses API, model
generation overrides, API-key passthrough, custom TLS, SSE/stdio MCP bindings,
tool subsets, SSE read-timeout overrides, and forwarded caller headers.
DSH sandbox permission requests and adapter-owned MCP approvals are separate
channels; both use kagent's approval messages.

The proxy discovers the remote tool catalog at each turn. It preserves the
CRD's default 30-second timeout and `terminateOnClose: true`; an explicit false
closes the local client without terminating the remote session. It does not
forward mid-turn catalog notifications, MCP resources, prompts or elicitation.

kagent skills, Agent Plugins, subagent bindings, memory, structured output and
ADK-specific context settings are not translated. DSH keeps its own built-in
tools and plugin composition; those are not equivalent to kagent Agent Plugins.
The first version accepts text input and output only.

Non-object tool input is retained under `args.rawInput` in tool-call events.
DSH validates the original input and returns tool errors to the model, allowing
recovery from arrays, scalars, null and malformed argument strings.

## State and process lifetime

Each turn starts a DSH ACP process and creates or resumes the Actor's private
DSH session. A pending permission request retains that process until the
decision arrives. Successful completion waits for `session/close`, which drains
activity and flushes persistence in the pinned DSH version. A close/flush error
fails the task even if assistant text has already been emitted. This barrier
does not guarantee recovery before an external DATA snapshot is published.
Two nested Linux child subreapers own each process tree, including
orphaned tools and tools that create a separate session or process group.

Cancellation interrupts blocked ACP writes by closing stdin. After five seconds
of graceful shutdown, the adapter closes a private lifetime pipe to request forced
cleanup. Adapter death also closes that pipe. If the inner supervisor dies, the
outer guard adopts and reaps its descendants. If the guard dies, its lifetime
pipe closes and the inner supervisor kills DSH and reaps its descendants.
The adapter waits for an explicit cleanup confirmation before releasing the turn;
process exit or closed output pipes alone are insufficient.

During payload execution, this tolerates the loss of either supervisor. If neither
can confirm cleanup, for example after losing both, the adapter logs the failure
and leaves the task unsettled. Replacing the Actor restores process containment;
it does not reconcile the controller's outstanding task. Recovery also needs
controller-owned fencing and task reconciliation, tracked in
[kagent #3010](https://github.com/kagent-dev/kagent/issues/3010).
As a temporary DSH-only mitigation, cancellation with no live execution owner
returns an explicit `TASK_NOT_CANCELABLE` error instead of waiting indefinitely.
It leaves the stored task unchanged; it does not certify cleanup or unblock the
session's lifecycle operations.
[kagent #3011](https://github.com/kagent-dev/kagent/pull/3011) addresses the shared
executor's empty cancel response. Its missing-owner cancellation does not confirm
DSH descendant cleanup, so merging it alone is not sufficient to remove this guard.
Uninterruptible kernel I/O can also delay cleanup. This is
not a guarantee against simultaneous process kills or malicious same-UID tools.

Process supervision requires Linux with readable procfs child lists and permission
to set `PR_SET_CHILD_SUBREAPER`. It runs as the image's unprivileged user without
additional Linux capabilities. Unsupported hosts fail validation; run runtime
tests in Linux rather than launching this adapter natively on macOS. This manages
process lifetime; sandbox and container isolation remain platform responsibilities.

MCP HTTP redirects must retain the configured scheme, hostname and effective
port. Same-origin path redirects are supported, with a ten-redirect limit per
request. Cross-origin redirects, including HTTPS-to-HTTP redirects, are rejected
before sending a request or injecting binding headers.

`/data/adapter/state.json` binds the Actor to one native session. DSH session
logs and storage live under `/data/dsh`, and the workspace is `/data/workspace`.
The CLI profile and compiled overlay use a disposable home outside `/data`.
Do not delete the native session logs while retaining the continuation binding.
Public tasks and history remain in kagent's controller-backed TaskStore.

Native ACP permission requests need the live process. They cannot recover
from a DATA-only snapshot or process replacement.
In DSH 0.2.0-rc.2, native session resume closes the interrupted call with an
unknown-outcome tool result. The approval audit log does not reconstruct the
execution point or the ACP permission response. Saving its request ID and
reissuing the original prompt would therefore not implement approval recovery.
The DSH adapter accepts a send referencing an existing task only
when that task owns a pending approval. It rejects both approval decisions
and plain replies to a lost task before the SDK appends history or changes task
state. Continue completed work with a new A2A task in the same conversation.
These guards do not replay prompts or apply approvals after ownership is lost;
they provide explicit failure for lost native ACP ownership.

### Protected MCP approvals

Set `requireApproval: true` on an HTTP MCP binding to use the adapter-owned
path. The proxy holds the call before dispatch, correlates its name and arguments
with the ACP tool event, and stops DSH before publishing `input-required`.
`/data/adapter/dsh-approval.json` retains the exact call and its public task IDs.
The conversation binding comes from the A2A executor and is recorded in
`dsh-approval-owner.json`; the model supplies neither routing nor scope. Each
Actor has its own DATA. Do not share one Actor or DATA directory between
conversations.

Approval executes the saved call once and persists its result. Denial records a
declined result without dispatch; canceling a waiting request does not execute
it. DSH then resumes its session in a new native turn with the saved result as
context, while kagent continues the original A2A task. This does not reconstruct
the original native stack. Request protected tools sequentially; approval batches
are not supported. Other native or unprotected tools retain their own policies.

The shared `PendingRestorer` contract from merged
[kagent #3165](https://github.com/kagent-dev/kagent/pull/3165) restores public
task ownership after replacing the adapter. This module uses that contract
directly, including persisted trace origin and deferred execution telemetry.
The MCP journal still belongs to this adapter: the shared runtime cannot infer
whether an interrupted remote call committed. Container reconstruction tests use
a controller TaskStore fixture. Separate cluster acceptance verifies protected
MCP restoration after an external DATA snapshot and replacement of the worker
Pod; see [VERIFICATION.md](VERIFICATION.md) for its scope and failures.

Transport discovery or native setup failures that prove non-execution preserve
the approval for retry. A persisted result is reused. A crash during dispatch or
native continuation leaves an uncertain record and blocks automatic replay.
HTTP cancellation cannot certify that a remote side effect stopped; cancellation
of an uncertain operation returns an error instead of publishing `CANCELED`.
Inspect the upstream outcome before recovering the conversation. Generic
exactly-once execution and remote reconciliation are not provided. Target
configuration changes, malformed records, wrong-context decisions and late
decisions fail closed. The existing #3010 shared recovery boundary still applies.

For business actions that must wait across process replacement, the optional
[external approval service](approval-mcp/README.md) offers a different contract:
DSH submits a durable operation through MCP and completes its current turn; an
external worker executes the saved action after approval, then starts a new A2A
task in the same conversation. Use the protected-binding path above when the
approval must continue the original public task. The business service does not
resume a native ACP permission request. It runs with its own database,
credentials and image, so it can move with the BYO integration to another repo.

If only the DSH subprocess exits while awaiting approval, the surviving adapter
still owns the pending task and its cleanup confirmation. A later approval or
rejection ends that task as failed; cancellation ends it as canceled. Once the
terminal state is published, send a new message without the old task ID in the
same conversation. The adapter resumes the native session for the new task.
This does not apply the lost approval or replay the interrupted tool. It requires
a subsequent request; there is no background task-recovery worker. Losing the
adapter or the cleanup confirmation still requires the recovery work above.

The local tests verify recovery after a DSH process restart. Cluster acceptance
also covers conversation and workspace continuity across automatic suspension
and explicit suspend/resume, protected MCP approvals after worker replacement,
and MCP forks after completion, denial, cancellation or a turn that did not
request approval. A fork can approve a new operation without repeating the old
one, and the source conversation remains usable. The adapter rebinds settled
approval DATA only when a trusted new turn is admitted; unresolved operations
retain their original task and context ownership. The shared terminal-only
checkpoint rule still excludes tasks awaiting input.

## Shared dependencies and remaining limits

Status checked on 2026-10-16. These upstream changes are not bundled into this
community harness; their closure alone is not proof that a local guard is obsolete.

| Upstream work | Boundary and replacement condition |
| --- | --- |
| [#3165](https://github.com/kagent-dev/kagent/pull/3165), merged | Consumed through the pinned Go module. Owns restored public task admission and execution telemetry. DSH still owns its MCP operation journal. |
| [#3010](https://github.com/kagent-dev/kagent/issues/3010), open | Orphaned tasks and unpublished settlement boundaries require shared fencing/reconciliation. Keep local lost-owner guards until cleanup can be certified. |
| [#3011](https://github.com/kagent-dev/kagent/pull/3011), open | A missing-owner cancel response does not prove DSH descendants stopped. Adopt only with the required cleanup guarantee. |
| [#3131](https://github.com/kagent-dev/kagent/pull/3131) / [#3006](https://github.com/kagent-dev/kagent/pull/3006), open | ADK process cleanup work. Replace the DSH supervisor only when a reusable implementation also handles detached descendants, adapter death and cleanup confirmation. |
| [#3177](https://github.com/kagent-dev/kagent/pull/3177), open | Shared egress policies do not replace the MCP client's same-origin redirect guard unless they enforce the same credential boundary. |
| [#3213](https://github.com/kagent-dev/kagent/pull/3213), open | Explicit deletion after failed settlement is an escape hatch, not task or native-call recovery. |

Native ACP permission recovery, approval batches, generic exactly-once remote
execution and per-call model usage reporting remain unsupported. The pinned
telemetry vocabulary has no DSH runtime value; the adapter leaves that identity
tuple unset rather than reporting another runtime. Protected-MCP DATA restoration
and worker replacement after suspension passed cluster acceptance, as did
continuation from settled protected-MCP forks. Arbitrary Actor loss before a
settled snapshot is not covered by those successful checks.
The latest migration checks are recorded in [VERIFICATION.md](VERIFICATION.md).

## Build and deploy

From the repository root:

```sh
docker build -t YOUR_REGISTRY/kagent-dsh-byo:dev harnesses/deepseek
docker push YOUR_REGISTRY/kagent-dsh-byo:dev
```

Record the pushed image's manifest digest. The example requires the full
`YOUR_REGISTRY/kagent-dsh-byo@sha256:...` reference, not its tag.

Use a clean kagent installation whose Agent CRD supports `spec.template.inline`.
This example targets the pinned kagent commit above. `1.0.0-alpha9` uses
the earlier `templateRef` shape and rejects this Agent manifest. Earlier cluster
acceptance used kagent `b42a6c53` and Substrate `0.5.0-alpha2`; it must not be
read as acceptance of the newly pinned control plane.
Configure a `kagent-default`
WorkerPool, snapshot storage, and Substrate credential gateway. In namespace
`kagent`, create the Secret `deepseek-api-key` with an `OPENAI_API_KEY` entry
using your normal secret-management workflow. Grant the Substrate atespace
access to that namespace as described in
[credential injection](https://github.com/kagent-dev/kagent/blob/0a57368aca336ea5523c6e7ac3d551aa7e9f37f1/docs/architecture/credential-injection.md).
The bridge honors the platform's Node CA and proxy environment.

Set `DSH_MODEL` to a Chat Completions model available to your DeepSeek account,
`DSH_BYO_IMAGE` to the image digest, and `SNAPSHOT_LOCATION` to your installation's
snapshot storage URI. Render only those variables:

```sh
envsubst '${DSH_MODEL} ${DSH_BYO_IMAGE} ${SNAPSHOT_LOCATION}' \
  < harnesses/deepseek/examples/agent.yaml.tmpl > /tmp/dsh-agent.yaml
kubectl apply --dry-run=server -f /tmp/dsh-agent.yaml
kubectl apply -f /tmp/dsh-agent.yaml
kubectl get harness,agent -n kagent
```

After preparation succeeds, open `dsh-agent` in the kagent UI and create a
conversation. To bind an MCP server, add a normal `tools[].mcp` binding to the
inline AgentTemplate and omit the `tools` subset. Set `requireApproval: true`
when that whole MCP server requires human approval.
The server must use Streamable HTTP and the platform trust bundle.

## Verification

From `harnesses/deepseek/` in a Linux development environment with Go 1.27:

```sh
go test -race ./...
```

The Linux subprocess tests exercise permission decisions, cancellation, identity
validation, malformed protocol handling, detached and orphaned tool cleanup,
unrelated-process survival, and adapter death without a model API. Configuration,
event conversion and wire tests also run on non-Linux hosts.
For the published runtime tests, install the locked npm dependencies and run:

```sh
npm ci --ignore-scripts --no-audit --no-fund
DSH_TEST_BIN="$PWD/node_modules/.bin/dsh" \
  go test -race ./internal/bridge -run TestRealDSH -count=1
```

These tests use a real DSH process, a local model fixture, and a local MCP
server. A2A gRPC calls use the production TaskStore client and settlement wrapper
against a local controller RPC fixture. They check MCP execution, resumed turns,
runtime identity headers and versioned settlement without provider credentials.
Separate process tests require DSH and detached tools to exit before settlement
on normal completion, forced cleanup and cancellation.

A real-DSH fault test kills the process with SIGKILL while a tool awaits approval.
It checks that allow/reject/cancel can settle the old task after cleanup, a new
task completes in the same native session and A2A context, and the interrupted
tool's marker file is never created. The adapter remains alive throughout this
test; it does not establish recovery after Actor or adapter replacement.

`TestRealDSHRestartClosesPendingCallWithoutReplay` reconstructs the runner from
the durable directory after terminating DSH without a permission response. It
checks that native resume records an unknown outcome for the original call and
does not execute its command. This is a regression check for safe interruption;
it is not a passing test for durable approval continuation.

The durable-workspace test creates, reads and edits a file with DSH's built-in
tools across three fresh adapter instances. It verifies that the native session
identity, conversation history and workspace contents survive reconstruction
from the same durable directory. These checks do not exercise the real controller
database or Substrate snapshots and are not a substitute for cluster acceptance.

Manual cluster acceptance on Linux arm64 with gVisor, Kubernetes `1.37.0` and
Substrate `0.5.0-alpha2` exercised the public A2A API and real PostgreSQL-backed
TaskStore. Three turns completed and automatically suspended; public `GetTask`
returned the completed task after each suspension. After explicit suspend/resume,
the model fixture received the earlier conversation history. Gateway credential
injection and workspace write/read/edit/read across suspensions also passed.
The model was a local Chat Completions fixture, not a live provider.

Migration cluster acceptance against the pinned controller additionally passed
MCP success and error responses, approval/denial/cancellation followed by another
turn, approved tool errors, worker Pod replacement after a DATA snapshot, and
unprotected and settled protected-MCP forks with retained history. The original
settled-owner fork defect was fixed in the adapter. The rerun covered completed,
denied, canceled and unused approval state, a new approval in the fork, and
continued use of the source conversation without replaying the original effect.
Pending approvals are correctly excluded from checkpoint creation by the shared
terminal-only rule.
See [VERIFICATION.md](VERIFICATION.md) for the tested boundaries.

### Reproducible container checks

From the repository root, the default image build needs no credentials:

```sh
docker build -t byo-ext/deepseek:local harnesses/deepseek
```

On an ARM host, add `--platform linux/amd64` to check the CI target architecture.
The optional test image contains race-instrumented tests and the pinned DSH:

```sh
docker build --target test -t byo-ext/deepseek:test harnesses/deepseek
docker run --rm byo-ext/deepseek:test -test.v -test.timeout=10m
```

The bridge suite uses local model/MCP fixtures. Its external approval-service
integration additionally needs `DSH_APPROVAL_TEST_DATABASE` pointing to a
**disposable** PostgreSQL database. Without that variable it explicitly skips
that case. The operations package tests create their own PostgreSQL containers;
run `go test -race ./internal/operations` on a development host with Docker.
No live model API is called by these tests. Repository CI only builds the default
image; it does not run these tests or publish an image.

## Try it

After applying the manifest, open `dsh-agent` in the kagent UI and ask it to
write `hello.txt` containing `hello`, then read it back in the next message.
Expect the file contents to survive the turn boundary in the same conversation.
This invokes your configured provider and may incur usage charges.

## Cleanup

Delete only the resources rendered for this example:

```sh
kubectl delete -f /tmp/dsh-agent.yaml
```

Remove the `deepseek-api-key` Secret only if it was created solely for this
example. Use the platform's normal session and snapshot cleanup procedure for
persisted conversations. The optional approval service has its own database
and credentials; retain its audit records according to your retention policy.
