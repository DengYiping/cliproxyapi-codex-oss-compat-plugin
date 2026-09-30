//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func isolatedProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}

// The staged host and Codex home are isolated. Upstream requests go through the
// configured loopback CPA's Claude routes; only the fake echo MCP tool executes.
func TestNativeCodexClientSearch(t *testing.T) {
	if os.Getenv("COMPAT_CODEX_E2E") != "1" {
		t.Skip("opt-in native Codex/CPA/MCP probe; see README for required environment")
	}
	model := os.Getenv("COMPAT_CODEX_MODEL")
	if model == "" {
		model = "claude-opus-5-5"
	}
	if !searchSupported(model, "") {
		t.Fatal("E2E model must be in the Claude search allowlist")
	}
	key, err := os.ReadFile(os.Getenv("CPA_CLIENT_KEY_FILE"))
	if err != nil {
		t.Fatal("cannot read local CPA client key; details redacted")
	}
	catalogBytes, err := os.ReadFile(os.Getenv("COMPAT_MODEL_CATALOG"))
	if err != nil {
		t.Fatal("cannot read model catalog")
	}
	var catalog map[string]any
	if json.Unmarshal(catalogBytes, &catalog) != nil {
		t.Fatal("invalid model catalog")
	}
	models, _ := catalog["models"].([]any)
	found := false
	for _, raw := range models {
		if entry, ok := raw.(map[string]any); ok && entry["slug"] == model {
			entry["supports_search_tool"] = true
			found = true
		}
	}
	if !found {
		t.Fatal("probe alias missing from model catalog")
	}
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "models.json")
	if err := os.WriteFile(catalogPath, jsonBytes(t, catalog), 0600); err != nil {
		t.Fatal(err)
	}
	pluginPath, err := filepath.Abs(os.Getenv("COMPAT_PLUGIN_LIBRARY"))
	if err != nil {
		t.Fatal(err)
	}
	platform := filepath.Join(dir, "plugins", runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(platform, 0700); err != nil {
		t.Fatal(err)
	}
	pluginBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal("cannot read candidate plugin")
	}
	if err := os.WriteFile(filepath.Join(platform, filepath.Base(pluginPath)), pluginBytes, 0600); err != nil {
		t.Fatal(err)
	}
	mcp := filepath.Join(dir, "mcp-probe")
	if output, err := exec.Command("go", "build", "-o", mcp, "testdata/mcp_probe.go").CombinedOutput(); err != nil {
		t.Fatalf("build synthetic MCP fixture: %v: %s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	localKey := "compat-isolated-probe"
	config := fmt.Sprintf("host: 127.0.0.1\nport: %d\nauth-dir: %q\napi-keys: [%q]\nrequest-retry: 0\nlogging-to-file: false\nrequest-log: false\nplugins:\n  enabled: true\n  dir: %q\n  configs:\n    codex-oss-compat:\n      enabled: true\nclaude-api-key:\n  - api-key: %q\n    base-url: http://127.0.0.1:8317\n    models:\n      - name: %q\n        alias: %q\n        is-compat: true\n", port, filepath.Join(dir, "auths"), localKey, filepath.Join(dir, "plugins"), strings.TrimSpace(string(key)), model, model)
	configPath := filepath.Join(dir, "cpa.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	serverCtx, stopServer := context.WithCancel(ctx)
	defer stopServer()
	server := exec.CommandContext(serverCtx, "cliproxyapi", "--local-model", "--config", configPath)
	isolatedProcessGroup(server)
	// Do not inherit storage backends, active auth homes, or private config.
	server.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LANG=C"}
	server.Dir, server.WaitDelay = dir, 2*time.Second
	var serverLogs bytes.Buffer
	server.Stdout, server.Stderr = &serverLogs, &serverLogs
	if err := server.Start(); err != nil {
		t.Fatal("cannot start isolated CPA; details redacted")
	}
	defer func() { stopServer(); _ = server.Wait() }()
	base := fmt.Sprintf("http://127.0.0.1:%d/v1", port)
	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(readyCtx, "GET", base+"/models", nil)
		req.Header.Set("Authorization", "Bearer "+localKey)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-readyCtx.Done():
			t.Fatal("isolated CPA did not become ready; logs redacted")
		case <-ticker.C:
		}
	}
	for _, websocket := range []bool{false, true} {
		if ctx.Err() != nil {
			break
		}
		t.Run(fmt.Sprintf("websocket=%t", websocket), func(t *testing.T) {
			home := filepath.Join(dir, fmt.Sprintf("codex-%t", websocket))
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			prompt := "Read-only protocol test. Use tool_search to discover the compat_search_probe echo MCP tool; invoke it with marker=bridge-test. Repeat discovery using tool_search once more and invoke echo again with that same marker. Do not run shell commands or use any other tools. Finally reply exactly compat_probe_ok:bridge-test."
			args := []string{"exec", "--json", "--skip-git-repo-check", "-s", "read-only", "--disable", "shell_tool", "-m", model,
				"-c", "model_provider=\"compat_probe\"", "-c", "model_catalog_json=" + fmt.Sprintf("%q", catalogPath), "-c", "model_reasoning_effort=\"none\"",
				"-c", "model_providers.compat_probe.name=\"Isolated compatibility probe\"", "-c", "model_providers.compat_probe.base_url=" + fmt.Sprintf("%q", base),
				"-c", "model_providers.compat_probe.wire_api=\"responses\"", "-c", "model_providers.compat_probe.env_key=\"COMPAT_PROBE_KEY\"",
				"-c", fmt.Sprintf("model_providers.compat_probe.supports_websockets=%t", websocket), "-c", "mcp_servers.compat_search_probe.command=" + fmt.Sprintf("%q", mcp),
				"-C", dir, prompt}
			cmd := exec.CommandContext(ctx, "codex", args...)
			isolatedProcessGroup(cmd)
			defer func() {
				if cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
			}()
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + home, "COMPAT_PROBE_KEY=" + localKey, "LANG=C"}
			cmd.WaitDelay = 2 * time.Second
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				// The transcript and server logs are intentionally never emitted.
				var hints []string
				for _, hint := range []string{"400", "401", "403", "500", "tool_search", "unsupported payload", "invalid", "api key", "catalog", "not found", "unknown", "timeout", "sandbox", "permission", "connection", "error loading", "json", "schema", "requires", "plugin", "function_call", "thinking", "signature", "failed to", "approval", "is not supported", "payload field", "adaptive", "budget_tokens", "enabled", "disabled", "output_config", "extra inputs", "effort", "minimum", "must be less"} {
					if bytes.Contains(bytes.ToLower(stderr.Bytes()), []byte(hint)) || bytes.Contains(bytes.ToLower(stdout.Bytes()), []byte(hint)) {
						hints = append(hints, hint)
					}
				}
				t.Fatalf("isolated Codex failed (%v); diagnostic keywords=%v; logs redacted", err, hints)
			}
			// MCP completion events retain the local result even when persisted
			// history omits or normalizes ordinary function-output items.
			completedMCP, successfulMCP := 0, 0
			outputScanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
			outputScanner.Buffer(make([]byte, 4096), 4<<20)
			for outputScanner.Scan() {
				var event struct {
					Type string
					Item map[string]any
				}
				if json.Unmarshal(outputScanner.Bytes(), &event) != nil || event.Type != "item.completed" || event.Item["type"] != "mcp_tool_call" {
					continue
				}
				completedMCP++
				var hints []string
				for _, hint := range []string{"approval", "denied", "permission", "invalid", "timed out", "unavailable", "unknown", "not found", "compat_probe_ok:"} {
					if bytes.Contains(bytes.ToLower(jsonBytes(t, event.Item)), []byte(hint)) {
						hints = append(hints, hint)
					}
				}
				if event.Item["status"] != "completed" {
					t.Logf("synthetic MCP completion failed; diagnostic-keywords=%v", hints)
				}
				if event.Item["status"] == "completed" && bytes.Contains(jsonBytes(t, event.Item["result"]), []byte("compat_probe_ok:bridge-test")) {
					successfulMCP++
				}
			}
			searches, results, calls := 0, 0, 0
			echoCalls, echoOutputs := make(map[string]bool), make(map[string]bool)
			finalReply := ""
			walkErr := filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				scanner := bufio.NewScanner(bytes.NewReader(data))
				scanner.Buffer(make([]byte, 4096), 4<<20)
				for scanner.Scan() {
					var event struct {
						Type    string
						Payload map[string]any
					}
					if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "response_item" {
						continue
					}
					switch event.Payload["type"] {
					case "tool_search_call":
						if event.Payload["execution"] == "client" {
							searches++
						}
					case "tool_search_output":
						tools, _ := event.Payload["tools"].([]any)
						if len(tools) > 0 {
							results++
						}
					case "function_call":
						if strings.Contains(stringField(event.Payload, "name"), "echo") {
							calls++
							echoCalls[stringField(event.Payload, "call_id")] = true
						}
					case "function_call_output":
						if bytes.Contains(jsonBytes(t, event.Payload["output"]), []byte("compat_probe_ok:bridge-test")) {
							echoOutputs[stringField(event.Payload, "call_id")] = true
						}
					case "message":
						if event.Payload["role"] == "assistant" {
							finalReply = ""
							content, _ := event.Payload["content"].([]any)
							for _, raw := range content {
								if part, ok := raw.(map[string]any); ok {
									finalReply += stringField(part, "text")
								}
							}
						}
					}
				}
				return scanner.Err()
			})
			if walkErr != nil {
				t.Fatal("cannot inspect isolated session evidence; details redacted")
			}
			successfulEchoes := 0
			for id := range echoCalls {
				if echoOutputs[id] {
					successfulEchoes++
				}
			}
			if searches < 2 || results < 2 || calls < 2 || max(successfulEchoes, successfulMCP) < 2 || strings.TrimSpace(finalReply) != "compat_probe_ok:bridge-test" {
				t.Fatalf("native E2E evidence missing: searches=%d loaded-results=%d echo-calls=%d successful-echoes=%d completed-MCP=%d successful-MCP=%d final-marker=%t; transcripts redacted", searches, results, calls, successfulEchoes, completedMCP, successfulMCP, strings.TrimSpace(finalReply) == "compat_probe_ok:bridge-test")
			}
			t.Logf("PASS: native candidate + actual Codex search + synthetic MCP, search=%d result=%d invocation=%d success=%d final-marker=true", searches, results, calls, successfulEchoes)
		})
	}
}
