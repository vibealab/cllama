# What are cllama's APIs?

A cllama server speaks three API families on its single `-listen` port:

1. **LLM APIs** — the proxied model traffic, in both the Ollama and the
   OpenAI wire formats (converted on the fly; see
   [README](../README.md)).
2. **Admin API** — runtime management of backends, models, settings and the
   request queue, also used by the embedded web UI.
3. **Tunnel endpoints** — internal endpoints used by child cllama servers
   that dial in over the reverse tunnel.

Authentication: when `-token` is set, every LLM API and model-list request
must present `Authorization: Bearer <token>` (the Anthropic endpoints also
accept `x-api-key`). When `-parentauth` is set, it guards the tunnel endpoints
instead. The `/admin` endpoints and the UI are **unauthenticated** — keep them
off untrusted networks.

## LLM APIs (Ollama format)

| Endpoint | Description |
| --- | --- |
| `GET /` | health check and exposed model list |
| `GET /api/tags` | model list (Ollama format) |
| `POST /api/chat`, `POST /api/generate` | chat / completion (Ollama format) |
| `POST /api/embed`, `POST /api/embeddings` | embeddings (Ollama format) |

Ollama CLI compatibility is faked where needed so `ollama run` / `ollama
list` work against a cllama server: `GET /api/version`, `GET /api/show`
(advertises the model's [capabilities](../README.md#chat-vs-embedding-models-capabilities)),
`POST /api/pull` and `GET /api/tags` are implemented.

## LLM APIs (OpenAI format)

| Endpoint | Description |
| --- | --- |
| `GET /api/openai/v1/models` | model list (OpenAI format) |
| `POST /api/openai/v1/chat/completions` | chat (OpenAI format, incl. streaming) |
| `POST /api/openai/v1/embeddings` | embeddings (OpenAI format) |

Both formats serve the same backends, so a backend registered once can be
consumed through either. Streaming works in both directions (Ollama NDJSON
↔ OpenAI SSE conversion included).

## LLM APIs (Anthropic Messages format)

| Endpoint | Description |
| --- | --- |
| `POST /api/anthropic/v1/messages` | chat (Anthropic Messages format, incl. SSE streaming, tools / tool results, images, thinking blocks) |
| `POST /api/anthropic/v1/messages/count_tokens` | token count (size-based estimate, enough for Claude Code's auto-compaction heuristics) |

This format exists so Anthropic-native clients — most notably **Claude
Code** — can consume any cllama backend:

```sh
ANTHROPIC_BASE_URL=http://localhost:11434/api/anthropic \
ANTHROPIC_API_KEY=<cllama -token, if set> \
claude
```

Requests are translated to the internal OpenAI format, routed to any backend
type (Ollama, OpenAI, Anthropic, child tunnels) and translated back, so
Claude Code works against non-Anthropic upstreams too.

## Admin API

| Endpoint | Description |
| --- | --- |
| `GET/POST /admin/backends` | list / register backends |
| `DELETE /admin/backends/<id>` | unregister a backend |
| `POST /admin/backends/<id>/bindings` | bind another model to a backend |
| `POST /admin/backends/<id>/enabled` | enable/disable a backend (incl. child tunnels), body `{"enabled": true\|false}` |
| `GET /admin/models` | mock models with their backend bindings |
| `GET/PUT /admin/config` | show / update the in-memory system settings (queue depth, done/fail retention, generation timeout, API token, parent auth) |
| `GET /admin/config/secret/<name>` | reveal one token (`api_token` / `parent_auth`) in plain text — used by the web UI eye toggle |
| `GET/DELETE /admin/parents` | inspect / disconnect tunnel parent connections |
| `GET /admin/queue` | requests tracked in the lifecycle queue (pending / takeaway / processing / done / fail) |
| `DELETE /admin/queue/<id>` | remove a queued request by hand (e.g. a retained done or failed one) |
| `POST /admin/queue/<id>/takeover` | pin a `pending` request as `takeaway` so the selector never routes it and it can be operated on by hand |
| `POST /admin/queue/<id>/pending` | release a manually taken-over request back to `pending` |
| `POST /admin/queue/<id>/resolve` | answer a taken-over request by hand, body `{"content": "…"}`; the blocked client call completes with this text as the assistant response |
| `POST /admin/queue/<id>/proxy` | replay the queued request's original payload through one backend, body `{"backend_id": "…"}`; returns `{"content": "…"}` for the admin to review before resolving |
| `GET /admin/events` | Server-Sent Events stream of admin state changes |

`GET /admin/events` pushes one topic line per change — `queue`, `backends`,
`parents` or `config` (plus `hello` on connect) — so the web UI updates in
real time without polling; consumers refetch the matching admin endpoint.

`PUT /admin/config` takes whole-second values, e.g.
`{"done_retention_sec":300,"failed_retention_sec":-1,"gen_timeout_sec":120}`:
retentions of `-1` keep entries in the request queue forever (see the
[request lifecycle](what-is-request-lifecycle.md)), and `gen_timeout_sec`
of `0` means no upstream timeout. The tokens travel only when explicitly
managed: `GET /admin/config` reduces them to `has_api_token` /
`has_parent_auth` flags, `PUT` changes one by including `"api_token":
"…"` or `"parent_auth": "…"` (empty string disables the respective auth),
and `GET /admin/config/secret/<name>` reveals the plain text. Tokens are
stored in plain memory — anyone who can reach `/admin` can read and change
them, so keep it off untrusted networks.

## Tunnel endpoints (internal)

Used by child cllama servers connected with `-parent`; guarded by
`-parentauth`:

| Endpoint | Description |
| --- | --- |
| `GET /admin/parent/stream?model=<m>` | long-lived SSE stream on which the parent pushes requests for model `m` |
| `POST /admin/parent/response/<id>` | child posts the response back (single-shot JSON, or NDJSON with `?stream=true`) |

## Web UI

`GET /ui` serves the embedded single-page dashboard (request queue, model ↔
backend map, settings), which is a client of the admin API and the events
stream above. See the README's [Web UI section](../README.md#web-ui).
