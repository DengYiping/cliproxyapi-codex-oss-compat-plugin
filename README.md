# Codex OSS compatibility plugin for CLIProxyAPI

A small native C-ABI plugin for CLIProxyAPI's Responses-over-HTTP path. For the
allowlisted models `glm-5.3-flash` and `glm-5.3-cyber`, it marks plaintext
`collaboration.spawn_agent`, `send_message`, and `followup_task` function calls
with `"encrypted_function_args": []`. Codex v2 then delivers their actual message
text to another agent. The plugin does not rewrite opaque/encrypted arguments,
other tools, other models, or WebSocket frames.

It also maps a raw Responses request's `reasoning.effort: "ultra"` to `high`
for GLM Flash or `max` for GLM Cyber. Current Codex already resolves `ultra`
locally before the request; the request hook only helps clients that send it raw.

Build on macOS with `go build -buildmode=c-shared -o dist/codex-oss-compat.dylib .`
after creating `dist/`. Place the library at the configured CLIProxyAPI plugin
directory under `darwin/arm64/`, using the filename `codex-oss-compat.dylib`.
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
for the packet and RPC regression tests.

This plugin deliberately does not attempt to mutate WebSocket responses because
CLIProxyAPI's public WebSocket plugin capability is observational. Models using
WebSockets need an HTTP/SSE route or a proxy core change to support a mutable hook.
