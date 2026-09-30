package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in, synthetic read-only probe: the candidate bridge runs in this test
// process around the configured CPA route. It does not install/reload a plugin,
// enable the active catalog, or execute any real MCP tools.
func TestLiveClientToolSearch(t *testing.T) {
	if os.Getenv("COMPAT_LIVE_PROBE") != "1" {
		t.Skip("set COMPAT_LIVE_PROBE=1 and CPA_CLIENT_KEY_FILE for a bounded live probe")
	}
	base := os.Getenv("CPA_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:8317"
	}
	endpoint, err := url.Parse(base)
	if err != nil || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost" && endpoint.Hostname() != "::1") || endpoint.User != nil || endpoint.RawQuery != "" {
		t.Fatal("live probes require a credential-free loopback CPA URL")
	}
	key, err := os.ReadFile(os.Getenv("CPA_CLIENT_KEY_FILE"))
	if err != nil {
		t.Fatal("cannot read CPA_CLIENT_KEY_FILE; no credential details logged")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	models := strings.Split(os.Getenv("COMPAT_LIVE_MODELS"), ",")
	if len(models) == 1 && models[0] == "" {
		models = []string{"claude-opus-5-5", "claude-opus-4-8", "claude-fable-5-1"}
	}
	for _, model := range models {
		if !searchSupported(model, "") {
			t.Fatal("live probe model is outside the Claude search allowlist")
		}
		t.Run(model, func(t *testing.T) {
			markerValue := "compat-probe-read-only"
			input := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "This is a read-only compatibility test. Discover the deferred echo tool using tool_search with query 'compat probe echo'. Then invoke the discovered echo tool with marker '" + markerValue + "'. Do not use any other tools."}}}}
			makeRequest := func(input []any, choice any, stream bool) []byte {
				return jsonBytes(t, map[string]any{"model": model, "input": input, "tools": []any{searchDefinition()}, "tool_choice": choice, "max_output_tokens": 384, "reasoning": map[string]any{"effort": "none"}, "stream": stream})
			}
			requestID := "live-search-" + model
			var searchChoice any = map[string]any{"type": "tool_search"}
			var invokeChoice any = map[string]any{"type": "function", "namespace": "compat_probe", "name": "echo"}
			if model == "claude-fable-5-1" {
				// This route rejects forced tool_choice; use its supported auto
				// selection while still requiring both boundary calls below.
				searchChoice, invokeChoice = "auto", "auto"
			}
			first := activateSearch(t, requestID, model, makeRequest(input, searchChoice, true))
			response := liveCPAResponse(t, ctx, base, key, model, requestID, first, true)
			var search map[string]any
			for _, raw := range response {
				if item, ok := raw.(map[string]any); ok && item["type"] == "tool_search_call" {
					search = item
					break
				}
			}
			if search == nil || search["execution"] != "client" || stringField(search, "call_id") == "" {
				t.Fatal("live route did not produce a client search call")
			}
			arguments, ok := search["arguments"].(map[string]any)
			if !ok || strings.TrimSpace(stringField(arguments, "query")) == "" {
				t.Fatal("live route did not return structured search arguments")
			}
			// Simulate the local registry's output; do not access real deferred tools.
			echo := functionDefinition("echo")
			echo["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"marker": map[string]any{"type": "string"}}, "required": []any{"marker"}, "additionalProperties": false}
			namespace := map[string]any{"type": "namespace", "name": "compat_probe", "description": "Synthetic read-only test tools", "tools": []any{echo}}
			input = append(input, response...)
			input = append(input, map[string]any{"type": "tool_search_output", "execution": "client", "status": "completed", "call_id": search["call_id"], "tools": []any{namespace}})
			nextID := "live-invoke-" + model
			second := activateSearch(t, nextID, model, makeRequest(input, invokeChoice, false))
			invoked := liveCPAResponse(t, ctx, base, key, model, nextID, second, false)
			found := false
			for _, raw := range invoked {
				item, ok := raw.(map[string]any)
				if !ok || item["type"] != "function_call" || item["namespace"] != "compat_probe" || item["name"] != "echo" {
					continue
				}
				var args map[string]any
				if json.Unmarshal([]byte(stringField(item, "arguments")), &args) == nil && args["marker"] == markerValue && stringField(item, "call_id") != "" {
					found = true
				}
			}
			if !found {
				t.Fatal("live route did not invoke the discovered namespaced schema")
			}
			t.Log("PASS: live SSE client-search -> local synthetic result -> nonstream namespaced invocation; no real MCP execution")
		})
	}
}

func liveCPAResponse(t *testing.T, ctx context.Context, base string, key []byte, model, requestID string, body []byte, stream bool) []any {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal("cannot create loopback live request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	// Do not follow redirects with the local client credential.
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("live request failed or hit the bounded deadline; details redacted")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var hints []string
		for _, hint := range []string{"thinking", "tool_choice", "max_tokens", "model", "speed", "effort", "disabled", "temperature", "beta", "unsupported", "invalid", "authentication", "token", "out of date"} {
			if bytes.Contains(bytes.ToLower(errorBody), []byte(hint)) {
				hints = append(hints, hint)
			}
		}
		t.Fatalf("configured CPA route returned HTTP %d; diagnostic keyword matches=%v; upstream body redacted", resp.StatusCode, hints)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		t.Fatal("cannot read live response; details redacted")
	}
	if !stream {
		updated := hookedBody(t, "response.intercept_after", responseIntercept{RequestID: requestID, SourceFormat: "responses", Model: model, Body: data})
		if len(updated) > 0 {
			data = updated
		}
		var response struct{ Output []any }
		if json.Unmarshal(data, &response) != nil {
			t.Fatal("invalid live nonstream response; body redacted")
		}
		return response.Output
	}
	var output []any
	// HTTP reader chunk boundaries are deliberately smaller than SSE events.
	var rewritten []byte
	for index, offset := 0, 0; offset < len(data); index++ {
		end := min(offset+37, len(data))
		chunk, drop := rewriteChunk(chunkIntercept{RequestID: requestID, SourceFormat: "responses", Model: model, Body: data[offset:end], ChunkIndex: index})
		if !drop {
			rewritten = append(rewritten, chunk...)
		}
		offset = end
	}
	for _, line := range bytes.Split(rewritten, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var event map[string]any
		if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
			continue
		}
		if event["type"] == "response.output_item.done" {
			output = append(output, event["item"])
		}
	}
	if len(output) == 0 {
		t.Fatal(fmt.Sprintf("no completed output items for %s; body redacted", model))
	}
	return output
}
