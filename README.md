# Codex OSS compatibility plugin for CLIProxyAPI

A small native C-ABI plugin for CLIProxyAPI's Responses path. For the
allowlisted models `kimi-k3`, `glm-5.3`, `glm-5.3-flash`, `glm-5.3-cyber`, and
`claude-opus-5-5`, it marks plaintext
`collaboration.spawn_agent`, `send_message`, and `followup_task` function calls
with `"encrypted_function_args": []`. Codex v2 then delivers their actual message
text to another agent. Except for the Claude discovery bridge below, the plugin
does not rewrite opaque/encrypted arguments, other tools, or other models.

On the inbound side, it converts plaintext Codex `agent_message` items into
ordinary user messages so an OpenAI-compatible OSS backend actually receives the
child task. Mixed or encrypted-content agent messages remain untouched.

It also maps a raw Responses request's `reasoning.effort: "ultra"` to `high`
for Kimi K3 and the GLM 5.3 models, `max` for GLM Cyber, or `xhigh` for Claude
Opus 5.5. Current Codex already resolves `ultra`
locally before the request; the request hook only helps clients that send it raw.

Build on macOS with `go build -buildmode=c-shared -o dist/codex-oss-compat-v0.1.5.dylib .`
after creating `dist/`. Place the versioned library at the configured CLIProxyAPI plugin
directory under `darwin/arm64/`. A new filename permits the running host to
hot-load this version. From v0.1.5 onward, the plugin drains its active discovery
requests through CPA's quiesce hook before a replacement becomes active.
Enable it with:

```yaml
plugins:
  enabled: true
  dir: /absolute/path/to/plugins
  configs:
    codex-oss-compat:
      enabled: true
```

The executable `go run ./cmd/e2e -model glm-5.3-flash -timeout 180s` drives a
real local Codex -> proxy -> model -> child Codex flow. It reports GREEN only if
both the child and parent return the same unique delegation marker. Its run is
read-only except for Codex's normal local session records. Run `go test ./...`
for the packet and RPC regression tests. Add `-websocket` to force Codex's
Responses WebSocket transport for that run.

The stream interceptor accepts both SSE-framed chunks and the raw JSON event
chunks that CLIProxyAPI's WebSocket-backed executors pass through the same hook
before forwarding them downstream. Split JSON events are buffered only until the
event is complete; batched events are rewritten individually. CLIProxyAPI's
separate WebSocket response observer capability remains read-only and is not used.

## Local Codex tool discovery for Claude

For `claude-opus-5-5`, `claude-opus-4-8`, and `claude-fable-5-1`, the
post-authentication request interceptor bridges Responses client-side tool
search only when the selected upstream format is `claude`. An advertised
`{"type":"tool_search","execution":"client",...}` becomes an ordinary
function named `tool_search`. A collision with an existing callable uses
`codex_tool_search_1` (or the next available suffix) instead. Ordinary functions
named `tool_search` are never treated as discovery without request activation.

Claude's function calls become `tool_search_call` items with `execution: client`
and object-valued arguments, preserving item IDs and call IDs. Codex executes
the search against its own deferred registry. Search argument delta/done events
are consumed locally; added/done items and terminal/non-streaming responses use
the special Codex protocol. Ordinary interleaved calls and collaboration markers
remain intact. Argument buffering is bounded to 1 MiB per execution and call
tracking to 512 keys; final complete arguments do not require the delta buffer.
An incomplete event larger than 1 MiB, or a malformed bare-JSON stream, emits
one `response.failed` compatibility error and suppresses the rest of that
attempt. An activated discovery bridge never silently switches to passthrough.
Non-discovery compatibility retains its existing best-effort behavior.

On later requests, search call/result history becomes ordinary function
call/result history that CPA can translate into Anthropic tool-use/result
exchanges. Completed results supply callable schemas; empty, failed, and normal
function-output errors remain model-visible. Namespace containers are retained
so CPA can restore namespaced calls. Current declarations take precedence, newer
discoveries win over older ones, and schemas are deduplicated by CPA's qualified
callable identity. Injected schemas have `defer_loading` removed: **this is not
Anthropic native tool search**, and no remote service searches Codex's registry.

Only transient response-routing state lives in the plugin. Schemas are rebuilt
from each request's history, not shared across chats. Request completion and
plugin shutdown release state. Auth retries, including already-normalized
bodies, start with fresh framing, pending bytes, failure flags, and argument
tracking while retaining activation and the collision-safe shim name.

`plugin.quiesce` closes discovery admission, waits for active bridged requests
to complete, and keeps their response hooks available during the drain. New
bridge requests receive HTTP 503 with `plugin_reloading`; existing auth retries
continue. A failed/canceled replacement re-registers the old library, resuming
admission without clearing its active requests. This requires CPA's quiesce
hook; when replacing an older discovery bridge without that hook, drain those
requests manually first. Disabling/unloading the plugin outright is not a
quiesced replacement and likewise requires a manual drain. The existing
effort/collaboration allowlists are not expanded to the other Claude aliases.

## Validation

```sh
go test ./...
go test -race ./...
go vet ./...
CPA_SOURCE=/Users/ydeng/src/cliproxyapi go test -v -run TestCPAClientSearchRoundTrip .
mkdir -p dist
go build -buildmode=c-shared -o dist/codex-oss-compat-v0.1.5.dylib .
CPA_SOURCE=/Users/ydeng/src/cliproxyapi \
COMPAT_PLUGIN_LIBRARY="$PWD/dist/codex-oss-compat-v0.1.5.dylib" \
go test -v -run TestCPANativePluginDrain .
```

The dependency-free tests cover SSE/CRLF and bare JSON, single-byte fragments,
batched/interleaved calls, namespaces, current/newest schema precedence,
collisions, malformed arguments, empty/error results, scope, fresh retry state,
fail-closed stream bounds, quiesced draining/rollback, and concurrent lifecycle
cleanup. The opt-in CPA fixture tests its **current
checkout's** real request, SSE, and non-streaming response translators in a
temporary module without changing that checkout.

`TestCPANativePluginDrain` uses the real native loader with two temporary copies
of the candidate library. It verifies admission closes during reload, a buffered
stream and a non-streaming request finish on the old instance, and the
replacement accepts new discovery requests. It starts no server, performs no
upstream generation, and does not touch the running CPA or installed plugins.

Bounded live route probes (180 seconds overall, two small generations per alias):

```sh
COMPAT_LIVE_PROBE=1 \
CPA_CLIENT_KEY_FILE=/Users/ydeng/.cli-proxy-api/client-key \
go test -v -run TestLiveClientToolSearch .
```

This applies the candidate request/response bridge in the test process around
the running CPA's routes, then simulates a local discovered tool. It does not
install the plugin or call real MCP tools. Fable uses automatic tool choice
because the configured route rejects forced choice. Set `COMPAT_LIVE_MODELS`
to a comma-separated subset if desired. Only loopback endpoints are accepted;
credentials, account identifiers, response bodies, and request logs are not
printed.

Native end-to-end probe using actual Codex local search and a read-only fake MCP
server (requires `codex` and `cliproxyapi` on PATH):

```sh
COMPAT_CODEX_E2E=1 \
COMPAT_PLUGIN_LIBRARY=/Users/ydeng/src/cliproxyapi-codex-oss-compat-plugin/dist/codex-oss-compat-v0.1.5.dylib \
COMPAT_MODEL_CATALOG=/Users/ydeng/.codex/models.cliproxy.json \
CPA_CLIENT_KEY_FILE=/Users/ydeng/.cli-proxy-api/client-key \
go test -v -run TestNativeCodexClientSearch .
```

The test copies the candidate library into an isolated temporary CPA plugin
directory, starts a separate loopback CPA instance forwarding to the configured
CPA on port 8317, and uses a temporary Codex home and catalog with search enabled
only for the probe alias. It requires two searches, two nonempty local results,
two fake MCP invocations, and the expected marker over **both HTTP/SSE and
WebSocket**. No active catalog/config/plugin is changed and the running CPA is
not restarted. `COMPAT_CODEX_MODEL` chooses another allowlisted alias. Reasoning
is disabled in this double-proxy probe because that staged path rejects adaptive
thinking; reasoning-enabled live behavior is not certified by this test. Temporary
hosts, credentials, catalogs, and session records are cleaned up afterwards.

Live validation record for v0.1.4, September 30, 2026 (not rerun for v0.1.5):

- `go test ./...`, `go test -race ./...`, `go vet ./...`, the current CPA
  translator fixture under `-race`, and the macOS arm64 shared-library build
  passed. Root-package statement coverage was 86.0%. Source checkouts inspected:
  CPA `673131f5` and Codex
  `f53f5a6fed`; the live Codex CLI was `0.159.1`.
- All three aliases passed live SSE search followed by non-streaming namespaced
  invocation through the configured CPA routes. Fable required automatic choice.
- Opus 5.5 and Fable 5.1 passed the actual native candidate plugin + Codex local
  registry + fake MCP flow, under `-race`, over HTTP/SSE and WebSocket: two client
  searches, two nonempty results, two **successful** MCP invocations, and the
  exact final marker for each transport.
- Opus 4.8's full staged Codex probe did not yield response/tool items within its
  bounded window. This native scenario is inconclusive, not a certified pass;
  the shorter route-level protocol probe passed. Adaptive-thinking replay through
  the artificial double-proxy topology is also not certified.
- The active catalog remained byte-identical with all three flags `false`;
  installed plugin version `0.1.3` and the running CPA were not changed.

## Local rollout (requires approval; not performed by the tests)

1. Build the versioned library above. Keep the prior installed version available.
2. Copy the **regular file**, not a symlink, to
   `/Users/ydeng/.cli-proxy-api/plugins/darwin/arm64/codex-oss-compat-v0.1.5.dylib`.
   Do not overwrite a loaded library. The current CPA loader selects the highest
   version unless pinned; confirm its Plugins UI/management API reports version
   `0.1.5`, with request, response, stream, and lifecycle interceptors enabled.
   If `plugins.configs.codex-oss-compat.store.version` is pinned, update that pin
   explicitly. A versioned hot reload does not require restarting CPA. Drain
   bridged requests manually if the outgoing version predates v0.1.5; subsequent
   replacements use automatic quiescing on a CPA host supporting that hook.
3. Back up `/Users/ydeng/.codex/models.cliproxy.json`. Set
   `supports_search_tool` to `true` **only** on the existing entries whose exact
   slugs are `claude-opus-5-5`, `claude-opus-4-8`, and `claude-fable-5-1`.
   Leave every other catalog field and model unchanged. No Anthropic native
   search/defer-loading setting or route change is required.
   Gate each alias on its native probe: enable Opus 5.5/Fable 5.1 first; keep
   Opus 4.8's flag `false` until its full Codex probe completes successfully.
4. Start a fresh Codex process/chat with the reloaded catalog (restart Codex if
   the app caches it). Perform a read-only MCP discovery/invocation check and
   confirm the session contains client `tool_search_call`, `tool_search_output`,
   and the discovered tool invocation. Schemas must be absent before discovery
   and available afterwards.
5. To roll back, restore the three flags first, then pin the prior plugin version
   or move only the new versioned library out of the watched directory. Retain
   both libraries for recovery; do not delete session history.
