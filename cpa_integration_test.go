package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func cpaProbe(t *testing.T, operations []map[string]any) []json.RawMessage {
	t.Helper()
	return cpaFixture(t, "testdata/cpa_probe.go", operations)
}

func cpaFixture(t *testing.T, fixturePath string, operations []map[string]any) []json.RawMessage {
	t.Helper()
	source := os.Getenv("CPA_SOURCE")
	if source == "" {
		t.Skip("set CPA_SOURCE to a current CLIProxyAPI checkout to test its translators")
	}
	dir := t.TempDir()
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	module := fmt.Sprintf("module github.com/router-for-me/CLIProxyAPI/v7/compat-probe\n\ngo 1.26\n\nrequire github.com/router-for-me/CLIProxyAPI/v7 v7.0.0\n\nreplace github.com/router-for-me/CLIProxyAPI/v7 => %q\n", source)
	for name, data := range map[string][]byte{"main.go": fixture, "go.mod": []byte(module)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "-mod=mod", ".")
	cmd.Dir, cmd.Stdin = dir, bytes.NewReader(jsonBytes(t, operations))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		// All requests in this test are synthetic, with no auth or private data.
		t.Fatalf("CPA fixture failed: %v\n%s", err, stderr.String())
	}
	var results []json.RawMessage
	if err := json.Unmarshal(stdout, &results); err != nil {
		t.Fatal(err)
	}
	return results
}

func TestCPANativePluginDrain(t *testing.T) {
	library := os.Getenv("COMPAT_PLUGIN_LIBRARY")
	if library == "" {
		t.Skip("set COMPAT_PLUGIN_LIBRARY and CPA_SOURCE to test the native drain lifecycle")
	}
	library, err := filepath.Abs(library)
	if err != nil {
		t.Fatal(err)
	}
	results := cpaFixture(t, "testdata/cpa_host_probe.go", []map[string]any{{"Library": library}})
	for _, name := range []string{"new_requests_rejected", "stream_drained", "nonstream_drained", "replacement_loaded", "replacement_bridge_active"} {
		if marker(t, results[0], name) != true {
			t.Fatalf("native host lifecycle failed: %s", name)
		}
	}
	t.Log("native CPA loader: quiesce, rejection, fragmented-stream/nonstream drain, and replacement activation passed; no upstream requests")
}

func TestCPAClientSearchRoundTrip(t *testing.T) {
	id := "cpa-roundtrip"
	definition := map[string]any{"type": "namespace", "name": "docs", "description": "Test docs", "tools": []any{functionDefinition("read")}}
	input := searchHistory("search_1", []any{definition})
	input = append(input, map[string]any{"type": "function_call", "namespace": "docs", "name": "read", "call_id": "read_1", "arguments": `{}`}, map[string]any{"type": "function_call_output", "call_id": "read_1", "output": "test document"})
	input = append(input, searchHistory("empty_1", []any{})...)
	input = append(input, map[string]any{"type": "tool_search_call", "execution": "client", "call_id": "error_1", "arguments": map[string]any{"query": ""}}, map[string]any{"type": "function_call_output", "call_id": "error_1", "output": "query must not be empty"})
	request := activateSearch(t, id, "claude-opus-5-5", jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}, "input": input}))
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_stream","model":"claude-opus-5-5","usage":{"input_tokens":1}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"next_search","name":"tool_search","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"docs\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"next_read","name":"docs__read","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}
	results := cpaProbe(t, []map[string]any{{"Action": "request", "Request": json.RawMessage(request)}, {"Action": "response", "Request": json.RawMessage(request), "Chunks": chunks}, {"Action": "stream", "Request": json.RawMessage(request), "Chunks": chunks}})
	if marker(t, results[0], "tools", "0", "name") != "tool_search" || marker(t, results[0], "tools", "1", "name") != "docs__read" {
		t.Fatal("CPA did not receive bridge and discovered namespace schema")
	}
	if schema := jsonBytes(t, marker(t, results[0], "tools")); bytes.Contains(schema, []byte(`"defer_loading"`)) {
		t.Fatal("native deferred loading leaked into the upstream schema")
	}
	messages := marker(t, results[0], "messages").([]any)
	if len(messages) != 8 {
		t.Fatalf("expected four valid tool-use/result pairs, got %d messages", len(messages))
	}
	for i, callID := range []string{"search_1", "read_1", "empty_1", "error_1"} {
		use := messages[i*2].(map[string]any)["content"].([]any)[0].(map[string]any)
		result := messages[i*2+1].(map[string]any)["content"].([]any)[0].(map[string]any)
		if use["type"] != "tool_use" || use["id"] != callID || result["type"] != "tool_result" || result["tool_use_id"] != callID {
			t.Fatal("CPA dropped or orphaned a history exchange")
		}
	}
	converted := hookedBody(t, "response.intercept_after", responseIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-5-5", Body: results[1]})
	if marker(t, converted, "output", "0", "type") != "tool_search_call" || marker(t, converted, "output", "0", "call_id") != "next_search" || marker(t, converted, "output", "1", "namespace") != "docs" || marker(t, converted, "output", "1", "name") != "read" {
		t.Fatal("translated response lost search payload or deferred namespace identity")
	}
	var stream [][]byte
	if err := json.Unmarshal(results[2], &stream); err != nil {
		t.Fatal(err)
	}
	var output []byte
	for i, chunk := range stream {
		body, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-5-5", Body: chunk, ChunkIndex: i})
		if !drop {
			output = append(output, body...)
		}
	}
	if !bytes.Contains(output, []byte(`"type":"tool_search_call"`)) || !bytes.Contains(output, []byte(`"arguments":{"query":"docs"}`)) || bytes.Count(output, []byte(`"type":"response.function_call_arguments.delta"`)) != 1 {
		t.Fatal("CPA SSE search response was not fully bridged")
	}
	t.Log("current CPA translator: paired history, schema loading, namespace invocation, nonstream and SSE search passed")
}
