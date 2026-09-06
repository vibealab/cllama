# cllama

LLM API server convertor and proxy.

**cllama** is a lightweight proxy server that sits in front of one or more LLM
backends (Ollama, any OpenAI-compatible server, or any Anthropic-compatible
server) and re-exposes them through the Ollama API, the OpenAI API *and* the
Anthropic Messages API. Register a backend through any API, and clients can
consume the same model through the others — an Ollama-only server becomes
usable by OpenAI and Anthropic clients, and vice versa.

cllama can also be arranged in a **hierarchy**: an internet-facing cllama
server exposes mock models, while internal cllama servers — each connected to
local GPUs on machines behind NAT — dynamically register as backends through a
reverse tunnel. No inbound connections to the GPU machines are needed.

```
                        ┌──────────────────────────────┐
 Ollama / OpenAI ──────▶│  public cllama (exposed)     │
 clients                │  -name llama3,embeddings     │
                        └─────────────┬────────────────┘
                                      │ reverse tunnel (outbound SSE)
                    ┌─────────────────┴──────────────────┐
                    │                                    │
        ┌───────────▼──────────┐             ┌───────────▼──────────┐
        │ internal cllama #1   │             │ internal cllama #2   │
        │  (GPU box, behind    │             │  (GPU box, behind    │
        │   NAT) -parent …)    │             │   NAT) -parent …)    │
        └───────────┬──────────┘             └───────────┬──────────┘
                    │                                    │
              local ollama                       local vLLM / LM Studio
              http://127.0.0.1:11434             http://127.0.0.1:8000
```

## Features

- **API conversion**: one backend registration serves all three APIs
  (Ollama `/api/chat`, `/api/generate`, `/api/embeddings` ↔ OpenAI
  `/v1/chat/completions`, `/v1/embeddings` ↔ Anthropic `/v1/messages`),
  including streaming, tools and images.
- **Claude Code compatible**: point Claude Code at
  `ANTHROPIC_BASE_URL=http://<cllama>/api/anthropic` and it drives any
  backend, Anthropic-native or not.
- **Ollama CLI compatible**: `ollama run` / `ollama list` work against a
  cllama server (`/api/version`, `/api/show`, `/api/pull`, `/api/tags` are
  implemented).
- **Dynamic backends**: register/unregister upstream servers at runtime via
  the admin API; requests are load-balanced across healthy backends per model
  and queued (up to `-maxqueue`) when none are available. Every request is
  tracked in the queue through a
  [request lifecycle](wiki/what-is-request-lifecycle.md):
  **pending → takeaway → processing → done / fail**.
- **Model aliasing**: map any upstream model name to a mock model name
  exposed by the proxy.
- **Hierarchies / tunneling**: child cllama servers dial out to a parent and
  serve its traffic using their local backends — perfect for GPUs behind NAT.
- **Web UI**: a single-page dashboard embedded in the binary (served at
  `/ui`): a live request-queue view, a model ↔ backend map with
  register / unregister / enable / disable, and the in-memory system
  settings, updated over SSE.
- **Auth**: optional bearer token for API traffic (`-token`) and separate
  token for child tunnel connections (`-parentauth`).

## Install / build

```sh
cd cllama
go build .
```

## Quick start

### 1. Start the proxy

```sh
./cllama -listen :11434 -name llama3,embeddings
```

Every name in `-name` is a *mock model* the proxy advertises; it becomes
servable once at least one backend is bound to it.

### 2. Register a backend

Register an upstream Ollama server:

```sh
curl -X POST http://localhost:11434/admin/backends -d '{
  "type": "ollama",
  "endpoint": "http://192.168.1.101:11434",
  "model": "llama3",
  "upstream_model": "llama3:8b"
}'
```

Or an OpenAI-compatible server (vLLM, LM Studio, OpenRouter, …):

```sh
curl -X POST http://localhost:11434/admin/backends -d '{
  "type": "openai",
  "endpoint": "http://192.168.1.101:8000/v1",
  "token": "sk-…",
  "model": "llama3"
}'
```

Or an Anthropic-compatible server (api.anthropic.com, a NewAPI gateway, …);
`endpoint` is the server root, `token` is sent as `x-api-key`:

```sh
curl -X POST http://localhost:11434/admin/backends -d '{
  "type": "anthropic",
  "endpoint": "https://api.anthropic.com",
  "token": "sk-ant-…",
  "model": "claude-3-5-sonnet-20241022"
}'
```

Inspect / manage backends:

```sh
curl http://localhost:11434/admin/backends          # list backends
curl http://localhost:11434/admin/models            # models and their bindings
curl -X DELETE http://localhost:11434/admin/backends/<id>   # unregister
```

### Chat vs embedding models: capabilities

Each backend registration can declare what its model can actually do, via the
`capabilities` field. This is what the proxy advertises for the model through
`/api/show`, and the ollama CLI consults it before enabling features like
vision, tools, thinking, or embedding:

| Capability | Meaning |
| --- | --- |
| `completion` | interactive chat / text completion |
| `vision` | accepts images in messages |
| `tools` | supports tool/function calling |
| `thinking` | supports extended-thinking modes |
| `embedding` | embedding model — **the ollama CLI then treats the model as embedding-only and refuses interactive chat** |

The default (when omitted) is `["completion", "vision", "tools", "thinking"]`.

So a **chat model** needs no extra configuration — the default already serves
chat through both APIs:

```sh
curl -X POST http://localhost:11434/admin/backends -d '{
  "type": "ollama", "endpoint": "http://192.168.1.101:11434",
  "model": "llama3", "upstream_model": "llama3:8b"
}'
```

An **embedding model** should be registered with `"capabilities":
["embedding"]` so clients (and `ollama run`) treat it as an embedding model
rather than offering it for chat:

```sh
curl -X POST http://localhost:11434/admin/backends -d '{
  "type": "ollama", "endpoint": "http://192.168.1.101:11434",
  "model": "embeddings", "upstream_model": "nomic-embed-text",
  "capabilities": ["embedding"]
}'
```

Chat and embedding models can coexist in one proxy — just list them both in
`-name` (e.g. `-name llama3,embeddings`) and bind each to the right backend.
Embeddings can then be requested from `/api/embeddings` (Ollama style) or
`/api/openai/v1/embeddings` (OpenAI style) regardless of the backend type.

Notes:

- Trim the list to match reality — e.g. a backend without tool calling should
  be registered with `"capabilities": ["completion"]`.
- When several backends serve the same mock model, the advertised capabilities
  are the **intersection** of all their capability lists, so a feature is only
  promised when every round-robin peer can serve it.
- Capabilities are per-backend and set at registration time; the
  `/admin/backends/<id>/bindings` endpoint adds model bindings but does not
  change them.

### 3. Use the APIs

Both APIs serve the same backends:

```sh
# Ollama style
curl http://localhost:11434/api/chat -d '{
  "model": "llama3", "stream": false,
  "messages": [{"role": "user", "content": "hi"}]
}'

# …or point the ollama CLI at it
OLLAMA_HOST=http://localhost:11434 ollama run llama3

# OpenAI style
curl http://localhost:11434/api/openai/v1/chat/completions -d '{
  "model": "llama3",
  "messages": [{"role": "user", "content": "hi"}]
}'

# …or point Claude Code at the Anthropic-format endpoint
ANTHROPIC_BASE_URL=http://localhost:11434/api/anthropic \
ANTHROPIC_API_KEY=<cllama -token, if set> \
claude
```

Embeddings are available at `/api/embeddings` (Ollama style) and
`/api/openai/v1/embeddings` (OpenAI style). The full endpoint reference —
LLM, admin, tunnel and UI — is in
[What are cllama's APIs?](wiki/what-are-cllama-apis.md).

## Hierarchy: chaining cllama servers

A child cllama server can connect *outbound* to a parent cllama server and
serve one of the parent's models with its own local backends. The connection
is a long-lived SSE tunnel that reconnects automatically; the child never
needs a public address.

**Parent** (exposed to the outside world):

```sh
./cllama -listen 0.0.0.0:11434 -name llama3 -parentauth s3cret -token pubToken
```

**Child** (on a GPU machine behind NAT):

```sh
./cllama -listen :11435 -name local-llama3 \
  -parent 'http://s3cret@public.example.com:11434/llama3'
```

then register the local GPU as its backend:

```sh
curl -X POST http://localhost:11435/admin/backends -d '{
  "type": "ollama", "endpoint": "http://127.0.0.1:11434", "model": "local-llama3"
}'
```

Now requests for `llama3` on the parent are forwarded through the tunnel and
executed on the child's local Ollama. Details:

- The `-parent` URL format is `http://[token@]host:port/<parent-model>`; the
  token is presented as a bearer token and must match the parent's
  `-parentauth`.
- Add `?to=<local-model>` to rewrite incoming requests to a different local
  model, e.g. `-parent 'http://s3cret@parent:11434/llama3?to=local-llama3'`.
- `-parent` is repeatable — a child can serve several parent models or connect
  to several parents.
- Manage parent connections at runtime via `GET /admin/parents` (and
  `DELETE /admin/parents/<id>` to disconnect one).

## Configuration

| Flag | Default | Description |
| --- | --- | --- |
| `-listen` | `:11434` | host:port to listen on |
| `-name` | — | comma-separated mock model names exposed by this proxy |
| `-maxqueue` | `100` | max requests queued while no backend is available (seeds the runtime config) |
| `-parent` | — | parent cllama URL `http://[token@]host:port/<model>` (repeatable; `?to=<local-model>` to remap) |
| `-parentauth` | — | token child cllama servers must present to tunnel into this server (seeds the runtime config) |
| `-token` | — | bearer token required on all LLM API and model-list requests (seeds the runtime config) |
| `-gen-timeout` | `0` | seconds before aborting an upstream generation request (0 = none; seeds the runtime config) |
| `-debug-ui` | — | serve the web UI from this directory instead of the embedded assets (development) |

## Endpoints

cllama's full API surface — the Ollama-, OpenAI- and Anthropic-format LLM
APIs, the admin API, the tunnel endpoints and the web UI — is documented in
[What are cllama's APIs?](wiki/what-are-cllama-apis.md).

## Web UI

Every cllama server embeds a small dashboard, served at `http://<listen>/ui/`:

- **Request queue** tab — live view of every in-flight request (model,
  [lifecycle state](wiki/what-is-request-lifecycle.md), assigned backend,
  wait time, queue depth), with manual removal, from `GET /admin/queue`.
  Pending requests can be taken away (✋) for manual handling: a dialog
  lets you type a response and 📤 send it to the waiting client, optionally
  drafting it by 📡 proxying the original request through any chat backend;
  cancelling marks the request `pending` again.
- **Models & backends** tab — which mock model maps to which upstream server
  or connected child cllama tunnel, with register / unregister and
  enable / disable controls, plus the child side's parent connections.
- **Settings** tab — the server's in-memory system configuration (max queue
  size, done / failed request retention, generation timeout, API and
  parent-auth tokens with masked / 👁 reveal display), from
  `GET /admin/config`.

The page subscribes to `GET /admin/events` (Server-Sent Events); the server
pushes a `queue`, `backends`, `parents` or `config` topic whenever state
changes, so the UI updates in real time without polling. Like the rest of
`/admin`, the UI and these endpoints are unauthenticated — keep them off
untrusted networks.

While developing the dashboard, `-debug-ui` serves it straight from the
source tree instead of the compiled-in copy, so edits are visible on a
plain browser refresh:

```sh
go run . -debug-ui ./ui
```

## Security notes

- The `/admin` endpoints are unauthenticated — keep them off untrusted
  networks (bind to localhost or firewall them).
- Set `-token` on any internet-facing server, and `-parentauth` whenever
  children tunnel into it.

## License

See [LICENSE](LICENSE).
