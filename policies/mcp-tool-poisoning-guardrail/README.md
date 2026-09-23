# MCP Tool Poisoning Guardrail

Inspects MCP `tools/list` responses for tool poisoning — instructions hidden
inside tool metadata that target the agent rather than describing the tool —
before the metadata reaches the client.

The gateway policy is implemented in **Go** and compiled into the gateway like
every other policy in this repository. The model does **not** run in the
gateway: the policy calls a separately deployed classifier service, which is a
**Python** service that serves the SetFit model with PyTorch.

```
MCP client ──tools/list──▶ Gateway ── Go policy: correlate, extract, static detectors,
                                      batch, filter / block / flag
                              │  POST {endpoint}/classify  (Authorization: Bearer <apiKey>)
                              ▼
                          Internal classifier service (Python · FastAPI · SetFit · PyTorch)
                              │
                          wso2/tool-poisoning-detection model files
                              │
                          poisoning scores ──▶ Go policy enforcement
```

| Path | What it is |
|------|------------|
| `*.go`, `go.mod`, `policy-definition.yaml` | The gateway policy (Go). Owns correlation, extraction, static detection, classifier calls and enforcement. Depends only on the Go standard library and the gateway policy SDK. |
| [`wso2/samples-apim`](https://github.com/wso2/samples-apim/tree/master/apim-ai-deployments/mcp-tool-poisoning-classifier) | The **separately deployed** classifier sample that serves the [wso2/tool-poisoning-detection](https://huggingface.co/wso2/tool-poisoning-detection) SetFit model. It is not compiled into the gateway, and its ML dependencies are not policy dependencies. |

This is the same split the `nvidia-nemoguard-content-safety` policy uses for its
inference endpoint. Hugging Face supplies the model artifacts only; live tool
metadata is classified inside your deployment and never leaves it. Keep the
classifier endpoint private — reachable from the gateway only, never publicly
exposed. The API key authenticates gateway-to-classifier requests; it is not a
Hugging Face token.

## Quick start

1. Deploy the classifier service and wait until it is ready — see the
   [MCP tool poisoning classifier deployment sample](https://github.com/wso2/samples-apim/tree/master/apim-ai-deployments/mcp-tool-poisoning-classifier)
   for Docker Compose and Kubernetes manifests.
2. Include the policy in the gateway build (`build.yaml`):

   ```yaml
   policies:
     - name: mcp-tool-poisoning-guardrail
       gomodule: github.com/wso2/gateway-controllers/policies/mcp-tool-poisoning-guardrail@v0.9
   ```

3. Point the gateway at the classifier in `config.toml`:

   ```toml
   mcp_tool_poisoning_classifier_endpoint = "http://mcp-tool-poisoning-classifier:8080"
   mcp_tool_poisoning_classifier_api_key  = "<the service's bearer token>"
   ```

4. Attach the policy to an MCP proxy:

   ```yaml
   policies:
     - name: mcp-tool-poisoning-guardrail
       version: v0.9
       params:
         action: filter
         classifierAction: flag
         classifierThreshold: 0.9
   ```

`classifierThreshold` defaults to `0.9`. Treat that as a starting point: run
the classifier against your own honest tool catalogue and tune the value before
letting model findings enforce.

`classifierAction` defaults to `flag`: the model's score is recorded but does
not remove or block a tool. Static detector findings and incomplete inspection
still enforce `action`. Switch to `enforce` only after calibrating against your
own honest tool catalogue — measured against the bundled model, honest
credential-requirement wording scored *above* poisoned metadata, so no single
threshold separates them.

`filter` and `block` fail closed: a `tools/list` response that cannot be
inspected completely (classifier unavailable, timed out or returning an unusable
result, malformed or oversized body, exhausted limits) is refused with a
JSON-RPC error rather than delivered uninspected.

## Deadlines

Every classifier call for one `tools/list` response runs under a single Go
`context.Context` deadline: `classificationDeadlineMillis` from the start of
classification, and never later than the gateway's own deadline for the request
less 250 ms, so the policy answers with its own JSON-RPC error before the
gateway gives up. Cancelling the request cancels in-flight classifier calls and
retry waits immediately. Each HTTP call is additionally bounded by
`requestTimeoutMillis`.

## Documentation

Full documentation, including every parameter, the enforcement semantics and
worked examples:
[`docs/mcp-tool-poisoning-guardrail/v0.9/docs/mcp-tool-poisoning-guardrail.md`](../../docs/mcp-tool-poisoning-guardrail/v0.9/docs/mcp-tool-poisoning-guardrail.md).

## Discovery filtering is not authorization

Removing a tool from a `tools/list` response hides it from the client. It does
not stop a client that already knows the tool name from calling it — this policy
does not inspect `tools/call`. Enforce tool execution permissions with
`mcp-acl-list` or `mcp-authz`, and layer this guardrail on top.

## Tests

```bash
go test ./...                  # the gateway policy (mocked classifier)
go test -race ./...            # the same, under the race detector
# In a separate wso2/samples-apim checkout:
cd apim-ai-deployments/mcp-tool-poisoning-classifier && python -m pytest tests
```

Neither suite needs Docker, a model download or network access beyond loopback.
To check compatibility with a running classifier service:

```bash
MCP_TOOL_POISONING_ENDPOINT=http://localhost:8101 \
MCP_TOOL_POISONING_API_KEY="$TOOL_POISONING_API_KEY" \
go test -count=1 -run 'Live|AgainstRealClassifierService' -v ./...
```
