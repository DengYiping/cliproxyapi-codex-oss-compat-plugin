package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func searchDefinition() map[string]any {
	return map[string]any{"type": "tool_search", "execution": "client", "description": "Search local deferred tools.", "parameters": map[string]any{
		"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "limit": map[string]any{"type": "number"}}, "required": []any{"query"}, "additionalProperties": false,
	}}
}

func functionDefinition(name string) map[string]any {
	return map[string]any{"type": "function", "name": name, "description": "A test tool", "defer_loading": true, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
}

func searchHistory(callID string, tools []any) []any {
	return []any{
		map[string]any{"type": "tool_search_call", "execution": "client", "call_id": callID, "arguments": map[string]any{"query": "test", "limit": 2}},
		map[string]any{"type": "tool_search_output", "execution": "client", "call_id": callID, "status": "completed", "tools": tools},
	}
}

func jsonBytes(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hookedBody(t *testing.T, method string, request any) []byte {
	t.Helper()
	result, err := handleMethod(method, jsonBytes(t, request))
	if err != nil {
		t.Fatal(err)
	}
	var response struct{ Body []byte }
	if err := json.Unmarshal(jsonBytes(t, result), &response); err != nil {
		t.Fatal(err)
	}
	return response.Body
}

func activateSearch(t *testing.T, id, model string, body []byte) []byte {
	t.Helper()
	t.Cleanup(func() { releaseStream(id) })
	return hookedBody(t, "request.intercept_after", requestIntercept{RequestID: id, SourceFormat: "openai-response", ToFormat: "claude", Model: model, Body: body})
}

func TestSearchRequestHistoryAndNamespaces(t *testing.T) {
	namespace := map[string]any{"type": "namespace", "name": "docs", "description": "Docs", "tools": []any{functionDefinition("read"), functionDefinition("write")}}
	input := searchHistory("c1", []any{functionDefinition("mcp__test__read"), namespace})
	input = append(input, searchHistory("c2", []any{functionDefinition("mcp__test__read"), namespace})...)
	current := functionDefinition("read")
	current["description"] = "Current schema wins"
	request := map[string]any{"tools": []any{searchDefinition(), map[string]any{"type": "namespace", "name": "docs", "tools": []any{current}}}, "input": input, "tool_choice": map[string]any{"type": "tool_search"}}
	updated, bridge := rewriteSearchRequest(jsonBytes(t, request), nil)
	if bridge == nil || bridge.name != "tool_search" {
		t.Fatal("missing client bridge")
	}
	want := []any{
		map[string]any{"type": "function", "name": "tool_search", "description": "Search local deferred tools.", "parameters": searchDefinition()["parameters"]},
		map[string]any{"type": "namespace", "name": "docs", "tools": []any{current, map[string]any{"type": "function", "name": "write", "description": "A test tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}},
		map[string]any{"type": "function", "name": "mcp__test__read", "description": "A test tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
	}
	if got := marker(t, updated, "tools"); !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded tools = %#v; want %#v", got, want)
	}
	for _, index := range []string{"0", "2"} {
		if marker(t, updated, "input", index, "type") != "function_call" || marker(t, updated, "input", index, "arguments") != `{"limit":2,"query":"test"}` {
			t.Fatal("search call did not become a valid function history item")
		}
	}
	if marker(t, updated, "input", "1", "call_id") != "c1" || marker(t, updated, "input", "3", "call_id") != "c2" || marker(t, updated, "tool_choice", "type") != "function" {
		t.Fatal("call identity or forced choice lost")
	}
	if again, reused := rewriteSearchRequest(updated, bridge); again != nil || reused != bridge {
		t.Fatal("auth retry is not idempotent")
	}
}

func TestSearchNewestSchemaAndCollision(t *testing.T) {
	older, newer := functionDefinition("read"), functionDefinition("read")
	older["description"], newer["description"] = "old", "new"
	input := append(searchHistory("c1", []any{older}), searchHistory("c2", []any{newer})...)
	request := map[string]any{"tools": []any{searchDefinition(), functionDefinition("tool_search")}, "input": input}
	updated, bridge := rewriteSearchRequest(jsonBytes(t, request), nil)
	if bridge.name != "codex_tool_search_1" || marker(t, updated, "tools", "2", "description") != "new" {
		t.Fatal("collision or newest-schema precedence failed")
	}
	ordinary := jsonBytes(t, map[string]any{"output": []any{map[string]any{"type": "function_call", "name": "tool_search", "arguments": `{}`, "call_id": "ordinary"}}})
	if out, drop := bridge.rewritePayload(ordinary); out != nil || drop {
		t.Fatal("ordinary function was hijacked")
	}
}

func TestSearchScopeAndErrorHistory(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-opus-4-8", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			input := searchHistory("empty", []any{})
			failure := searchHistory("error", []any{functionDefinition("must_not_load")})
			failure[1].(map[string]any)["status"] = "failed"
			input = append(input, failure...)
			input = append(input, map[string]any{"type": "function_call_output", "call_id": "error2", "output": "query must not be empty"})
			body := jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}, "input": input})
			updated := activateSearch(t, model, model, body)
			if len(marker(t, updated, "tools").([]any)) != 1 || marker(t, updated, "input", "1", "type") != "function_call_output" || marker(t, updated, "input", "4", "output") != "query must not be empty" {
				t.Fatal("empty or failed search changed available tools or lost error")
			}
		})
	}
	body := jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}})
	for i, req := range []requestIntercept{
		{SourceFormat: "openai-response", ToFormat: "claude", Model: "gpt-6-sol"},
		{SourceFormat: "openai-response", ToFormat: "openai", Model: "claude-opus-5-5"},
		{SourceFormat: "claude", ToFormat: "claude", Model: "claude-opus-5-5"},
		{SourceFormat: "openai-response", ToFormat: "claude", Model: "claude-opus-5-5", RequestedModel: "unrelated-alias"},
	} {
		req.Body = body
		if updated := hookedBody(t, "request.intercept_after", req); updated != nil {
			t.Fatalf("scope case %d changed", i)
		}
	}
	server := searchDefinition()
	server["execution"] = "server"
	if updated, bridge := rewriteSearchRequest(jsonBytes(t, map[string]any{"tools": []any{server}}), nil); updated != nil || bridge != nil {
		t.Fatal("server search was bridged")
	}
}

func TestSearchResponsesAndExistingRewrites(t *testing.T) {
	id := "nonstream-search"
	request := jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}, "reasoning": map[string]any{"effort": "ultra"}, "input": []any{map[string]any{"type": "agent_message", "content": []any{map[string]any{"type": "input_text", "text": "task"}}}}})
	before := hookedBody(t, "request.intercept_before", requestIntercept{SourceFormat: "openai-response", Model: "claude-opus-5-5", Body: request})
	if marker(t, before, "reasoning", "effort") != "xhigh" || marker(t, before, "input", "0", "role") != "user" {
		t.Fatal("existing request rewrites regressed")
	}
	activateSearch(t, id, "claude-opus-5-5", before)
	for _, typ := range []string{"", "response.completed", "response.incomplete", "response.failed"} {
		response := map[string]any{"output": []any{
			map[string]any{"type": "function_call", "id": "fc_x", "name": "tool_search", "call_id": "call_x", "arguments": `{"query":"read","limit":1}`},
			call("spawn_agent", `{"message":"hello"}`),
		}}
		payload := response
		if typ != "" {
			payload = map[string]any{"type": typ, "response": response}
		}
		body := jsonBytes(t, payload)
		updated := hookedBody(t, "response.intercept_after", responseIntercept{RequestID: id, SourceFormat: "openai-response", Model: "claude-opus-5-5", Body: body})
		path := []string{"output"}
		if typ != "" {
			path = append([]string{"response"}, path...)
		}
		if marker(t, updated, append(path, "0", "type")...) != "tool_search_call" || marker(t, updated, append(path, "0", "execution")...) != "client" || marker(t, updated, append(path, "0", "arguments", "query")...) != "read" || marker(t, updated, append(path, "0", "id")...) != "fc_x" {
			t.Fatal("nonstream search protocol or identity lost")
		}
		if got := marker(t, updated, append(path, "1", "encrypted_function_args")...); len(got.([]any)) != 0 {
			t.Fatal("collaboration marker lost")
		}
	}
	// No search activation must preserve even a literally named function.
	ordinary := jsonBytes(t, map[string]any{"output": []any{map[string]any{"type": "function_call", "name": "tool_search", "arguments": `{}`}}})
	if updated := hookedBody(t, "response.intercept_after", responseIntercept{SourceFormat: "responses", Model: "claude-opus-4-8", Body: ordinary}); updated != nil {
		t.Fatal("unadvertised search was rewritten")
	}
}

func searchEvent(typ, id, name, arguments string, index int) map[string]any {
	return map[string]any{"type": typ, "output_index": index, "sequence_number": index + 1, "item": map[string]any{"type": "function_call", "id": id, "name": name, "call_id": "call_" + id, "arguments": arguments, "status": "in_progress"}}
}

func TestSearchStreamsFragmentedAndInterleaved(t *testing.T) {
	for _, framing := range []string{"sse", "json"} {
		t.Run(framing, func(t *testing.T) {
			id := "interleaved-" + framing
			activateSearch(t, id, "claude-opus-4-8", jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}}))
			events := []map[string]any{
				searchEvent("response.output_item.added", "a", "tool_search", "", 0),
				searchEvent("response.output_item.added", "b", "ordinary", "", 1),
				searchEvent("response.output_item.added", "c", "tool_search", "", 2),
				{"type": "response.function_call_arguments.delta", "item_id": "a", "output_index": 0, "delta": `{"query":`},
				{"type": "response.function_call_arguments.delta", "item_id": "b", "output_index": 1, "delta": `{"keep":true}`},
				{"type": "response.function_call_arguments.delta", "item_id": "c", "output_index": 2, "delta": `{"query":"three"}`},
				{"type": "response.function_call_arguments.delta", "item_id": "a", "output_index": 0, "delta": `"one"}`},
				{"type": "response.function_call_arguments.done", "item_id": "c", "output_index": 2, "arguments": `{"query":"three"}`},
				searchEvent("response.output_item.done", "c", "tool_search", "", 2),
				searchEvent("response.output_item.done", "b", "ordinary", `{"keep":true}`, 1),
				searchEvent("response.output_item.done", "a", "tool_search", "", 0),
			}
			var wire []byte
			for _, event := range events {
				if framing == "sse" {
					wire = append(wire, []byte("event: "+event["type"].(string)+"\r\ndata: ")...)
				}
				wire = append(wire, jsonBytes(t, event)...)
				if framing == "sse" {
					wire = append(wire, []byte("\r\n\r\n")...)
				}
			}
			var output []byte
			// Single-byte fragments exercise every JSON token and CRLF boundary.
			for i, value := range wire {
				body, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-4-8", Body: []byte{value}, ChunkIndex: i})
				if !drop {
					output = append(output, body...)
				}
			}
			if bytes.Count(output, []byte(`"type":"tool_search_call"`)) != 4 || !bytes.Contains(output, []byte(`"arguments":{"query":"one"}`)) || !bytes.Contains(output, []byte(`"arguments":{"query":"three"}`)) || bytes.Count(output, []byte(`"type":"function_call"`)) != 2 || bytes.Count(output, []byte(`"type":"response.function_call_arguments.delta"`)) != 1 {
				t.Fatalf("incorrect interleaved bridge: %s", output)
			}
		})
	}
}

func TestSearchAdditionalToolsAndMalformedArguments(t *testing.T) {
	input := []any{map[string]any{"type": "additional_tools", "tools": []any{searchDefinition(), functionDefinition("tool_search")}}}
	updated, bridge := rewriteSearchRequest(jsonBytes(t, map[string]any{"input": input}), nil)
	if bridge == nil || marker(t, updated, "input", "0", "tools", "0", "name") != "codex_tool_search_1" {
		t.Fatal("Responses Lite tools were not bridged")
	}
	for _, arguments := range []string{"not JSON", "null", "[]", `{"query":""}`} {
		response := jsonBytes(t, map[string]any{"output": []any{map[string]any{"type": "function_call", "name": bridge.name, "arguments": arguments, "call_id": "error"}}})
		out, drop := bridge.rewritePayload(response)
		if drop || marker(t, out, "output", "0", "type") != "tool_search_call" {
			t.Fatal("malformed arguments must still use the client search error path")
		}
	}
}

func TestSearchStateIsolationAndConcurrentLifecycle(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-%d", i)
			body := jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}})
			activateSearch(t, id, "claude-fable-5-1", body)
			event := jsonBytes(t, searchEvent("response.output_item.done", "same-id", "tool_search", `{"query":"local"}`, 0))
			out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-fable-5-1", Body: event})
			if drop || marker(t, out, "item", "type") != "tool_search_call" {
				t.Error("request state crossed or was lost")
			}
			hookedBody(t, "request.complete", map[string]any{"RequestID": id})
			streams.Lock()
			_, exists := streams.items[id]
			streams.Unlock()
			if exists {
				t.Error("request state leaked")
			}
		}(i)
	}
	wg.Wait()
}

func TestSearchBufferBoundsAndAuthoritativeFinalArguments(t *testing.T) {
	_, bridge := rewriteSearchRequest(jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}}), nil)
	for i, id := range []string{"large", "overflow", "final"} {
		bridge.rewritePayload(jsonBytes(t, searchEvent("response.output_item.added", id, "tool_search", "", i)))
	}
	large := `{"query":"` + strings.Repeat("x", 900000) + `"}`
	bridge.rewritePayload(jsonBytes(t, map[string]any{"type": "response.function_call_arguments.delta", "item_id": "large", "delta": large}))
	for _, id := range []string{"overflow", "final"} {
		bridge.rewritePayload(jsonBytes(t, map[string]any{"type": "response.function_call_arguments.delta", "item_id": id, "delta": strings.Repeat("x", 200000)}))
	}
	if bridge.buffered > maxPendingFrame {
		t.Fatal("aggregate argument buffer exceeded the execution bound")
	}
	invalid, _ := bridge.rewritePayload(jsonBytes(t, searchEvent("response.output_item.done", "overflow", "tool_search", "", 1)))
	if _, ok := marker(t, invalid, "item", "arguments").(string); !ok {
		t.Fatal("overflow must report an argument error, not execute truncated input")
	}
	valid, _ := bridge.rewritePayload(jsonBytes(t, searchEvent("response.output_item.done", "final", "tool_search", `{"query":"authoritative"}`, 2)))
	if marker(t, valid, "item", "arguments", "query") != "authoritative" {
		t.Fatal("final complete arguments must override an exhausted delta buffer")
	}
	bridge.rewritePayload([]byte(`{"type":"response.created"}`))
	if bridge.buffered != 0 || len(bridge.calls) != 0 {
		t.Fatal("new response did not reset argument tracking")
	}
}

func TestSearchUnactivatedAndAlreadyBridgedStreams(t *testing.T) {
	body := jsonBytes(t, searchEvent("response.output_item.done", "ordinary", "tool_search", `{}`, 0))
	if out, drop := rewriteChunk(chunkIntercept{RequestID: "unactivated", SourceFormat: "responses", Model: "claude-opus-4-8", Body: body}); out != nil || drop {
		t.Fatal("unactivated Claude stream was changed")
	}
	_, bridge := rewriteSearchRequest(jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}}), nil)
	for _, item := range []map[string]any{
		{"type": "tool_search_call", "execution": "client", "call_id": "already", "arguments": map[string]any{"query": "test"}},
		{"type": "function_call", "namespace": "unrelated", "name": "tool_search", "arguments": `{}`},
	} {
		if out, drop := bridge.rewritePayload(jsonBytes(t, map[string]any{"output": []any{item}})); out != nil || drop {
			t.Fatal("already bridged or namespaced ordinary call was changed")
		}
	}
}

func TestSearchPreservesNumericSchemasAndUnrelatedPayloads(t *testing.T) {
	schema := functionDefinition("large_enum")
	schema["parameters"].(map[string]any)["enum"] = []any{json.Number("9007199254740993")}
	body := jsonBytes(t, map[string]any{"tools": []any{searchDefinition(), schema}, "metadata": map[string]any{"large": json.Number("9007199254740993")}})
	updated, bridge := rewriteSearchRequest(body, nil)
	if bytes.Count(updated, []byte("9007199254740993")) != 2 {
		t.Fatal("bridge rounded an existing schema or metadata number")
	}
	response := jsonBytes(t, searchEvent("response.output_item.done", "large-limit", "tool_search", `{"query":"test","limit":9007199254740993}`, 0))
	out, _ := bridge.rewritePayload(response)
	if !bytes.Contains(out, []byte(`"limit":9007199254740993`)) {
		t.Fatal("bridge rounded structured search arguments")
	}
	if updated, bridge := rewriteSearchRequest(append(body, []byte(`{}`)...), nil); updated != nil || bridge != nil {
		t.Fatal("request bridge accepted trailing JSON values")
	}
}
