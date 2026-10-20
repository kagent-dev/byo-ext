# Jev harness

Runs TypeSafe's [Jev](https://docs.typesafe.ai), a decision model, as a
kagent agent on the `byo` runtime. It is an experimental community
integration.

Each A2A message is a [System One request](https://docs.typesafe.ai/api), and
the reply is System One's response, unchanged. The harness is a thin adapter,
like kagent's own Claude and Codex harnesses: callers decide what to ask and
how to read the answer.

The harness adds two things to the request:

- `model`, from the agent's ModelConfig, unless the request sets one.
- `instructions`, the agent's system prompt, for each question without its
  own. So an agent's system prompt can hold the question, and messages carry
  only the state and the answers.

A message that isn't a System One request, or an error from the API, fails
the task with the reason. Rate-limited (429) and overloaded (529) calls are
retried with backoff. The harness keeps no conversation: each message is
evaluated on its own.

## Maintainers

- @ernoaapa

## Compatibility

Verified with kagent 1.0.0-alpha16 and Agent Substrate 0.6.0-alpha3. The image
builds for Linux AMD64 and ARM64. The Go module pins kagent's `go` module at that release and
needs Go 1.27.

The harness implements the BYO contract with kagent's own Go packages:

- `github.com/kagent-dev/kagent/go/adk/pkg/app` serves A2A and readiness on
  port 80, the only port kagent connects to for a BYO harness.
- `github.com/kagent-dev/kagent/go/harness/runtime/a2a` turns each A2A message
  into a turn and sends the reply back as text.
- `KAGENT_CONFIG_JSON` gives the system prompt, the model and base URL from the
  agent's ModelConfig, and the control-plane URL. `KAGENT_AGENT_CARD_JSON`
  gives the agent card.

kagent's harness API is still alpha. Rebuild the harness against each new
kagent release.

## Prerequisites and configuration

- A kagent installation on Agent Substrate.
- A TypeSafe API key. Calls are billed by TypeSafe.
- A container registry the cluster can pull from.

The harness holds no credentials. An `OpenAI` ModelConfig whose base URL is
TypeSafe's API gives the harness its model and URL, and makes kagent's egress
gateway add the API key to calls to that host as `Authorization: Bearer`. The
sandbox only holds a placeholder.

Outside kagent, these environment variables set the key, URL and model:

| Variable | Default |
| --- | --- |
| `TYPESAFE_API_KEY` | none |
| `JEV_BASE_URL` | `https://api.typesafe.ai` |
| `JEV_MODEL` | `jev-latest` |

## Build

```sh
docker build --tag byo-ext/jev:local .
```

Substrate runs only digest-pinned images, so push the image to your registry
and note its digest:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  --tag <registry>/jev:<tag> --push .
```

Run the tests with a fake API, or against System One:

```sh
go test ./...
TYPESAFE_API_KEY=... go test -run TestLive ./...
```

## Run with kagent

Replace `<registry>`, `<digest>` and `<snapshot location>`, and the Secret
value:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: typesafe-jev
  namespace: kagent
stringData:
  TYPESAFE_API_KEY: <your TypeSafe API key>
---
apiVersion: api.kagent.dev/v1alpha3
kind: ModelConfig
metadata:
  name: jev
  namespace: kagent
spec:
  provider: OpenAI
  model: jev-latest
  apiKeySecret: typesafe-jev
  apiKeySecretKey: TYPESAFE_API_KEY
  openAI:
    baseUrl: https://api.typesafe.ai
---
apiVersion: api.kagent.dev/v1alpha3
kind: Harness
metadata:
  name: jev
  namespace: kagent
spec:
  byo: {}
  workload:
    image: <registry>/jev@sha256:<digest>
    command: ["/jev-harness"]
  substrate:
    workerPoolRef: {name: kagent-default}
    snapshotPolicy: {location: <snapshot location>}
---
apiVersion: api.kagent.dev/v1alpha3
kind: AgentTemplate
metadata:
  name: issue-kind
  namespace: kagent
spec:
  description: Tells bugs from feature requests.
  modelConfig:
    name: jev
  systemPrompt: Is this issue a bug or a feature request?
---
apiVersion: api.kagent.dev/v1alpha3
kind: Agent
metadata:
  name: issue-kind
  namespace: kagent
spec:
  harnessRef:
    name: jev
  template:
    ref:
      name: issue-kind
```

## Try it

Send the `issue-kind` agent this message:

```json
{"state": {"title": "App crashes when I press Save"}, "questions": {"kind": {"type": "choice", "criteria": {"bug": "A defect.", "feature": "A request."}}}}
```

The reply is System One's response:

```json
{"model": "jev-1.13.0", "answers": {"kind": {"type": "choice", "choice": "bug", "confidence": 1.0, "probabilities": {"bug": 1.0, "feature": 0.0}}}, "usage": {"input_tokens": 337, "output_tokens": 38}}
```

The question has no `instructions`, so the harness uses the agent's system
prompt.

The harness runs judge agents in a kagent 1.0.0-alpha16 cluster, which
send System One requests like the one above.

## Limitations

- Each message is evaluated on its own. The harness keeps no conversation and
  never asks for input.
- It doesn't stream: the reply arrives when System One answers.
- Tools, MCP servers and skills on the agent are ignored. System One has no
  tools.

## Cleanup

```sh
kubectl delete -n kagent agent/issue-kind agenttemplate/issue-kind \
  harness/jev modelconfig/jev secret/typesafe-jev
```
