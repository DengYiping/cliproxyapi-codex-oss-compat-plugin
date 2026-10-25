package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const deepSeekModel = "deepseek-v4.1-flash-cyber"

func deepSeekRequest(t *testing.T, id string, input []any, extraTools ...any) []byte {
	t.Helper()
	tools := append([]any{functionDefinition("resident"), searchDefinition()}, extraTools...)
	return activateSearch(t, id, deepSeekModel, jsonBytes(t, map[string]any{
		"model": deepSeekModel, "instructions": "Stable system prefix.", "tools": tools, "input": input,
		"prompt_cache_key": "stable-key", "reasoning": map[string]any{"effort": "none"},
	}))
}

func discoveredNamespace(name string) map[string]any {
	return map[string]any{"type": "namespace", "name": "docs", "description": "Synthetic docs.", "tools": []any{functionDefinition(name)}}
}

func TestDeepSeekStableToolAndMessagePrefix(t *testing.T) {
	initial := []any{map[string]any{"role": "user", "content": "Find docs."}}
	first := deepSeekRequest(t, "prefix-first", initial)
	one := append(append([]any{}, initial...), searchHistory("search_1", []any{discoveredNamespace("read")})...)
	second := deepSeekRequest(t, "prefix-second", one)
	two := append(append([]any{}, one...), map[string]any{"type": "function_call", "namespace": "docs", "name": "read", "call_id": "read_1", "arguments": `{"id":1}`}, map[string]any{"type": "function_call_output", "call_id": "read_1", "output": "read result"})
	third := deepSeekRequest(t, "prefix-third", two)
	three := append(append([]any{}, two...), searchHistory("search_2", []any{discoveredNamespace("write")})...)
	// Codex can resend loaded declarations at top level or in additional_tools.
	three = append(three, map[string]any{"type": "additional_tools", "tools": []any{discoveredNamespace("write")}})
	fourth := deepSeekRequest(t, "prefix-fourth", three, discoveredNamespace("read"))
	for _, body := range [][]byte{second, third, fourth} {
		if !bytes.Equal(jsonBytes(t, marker(t, first, "tools")), jsonBytes(t, marker(t, body, "tools"))) {
			t.Fatal("discoveries changed the static tool prefix")
		}
		if marker(t, body, "instructions") != "Stable system prefix." || marker(t, body, "prompt_cache_key") != "stable-key" {
			t.Fatal("cache controls/system changed")
		}
	}
	for _, pair := range [][2][]byte{{first, second}, {second, third}, {third, fourth}} {
		before := marker(t, pair[0], "input").([]any)
		after := marker(t, pair[1], "input").([]any)
		if !reflect.DeepEqual(before, after[:len(before)]) {
			t.Fatal("a discovery rewrote the existing message prefix")
		}
	}
	if marker(t, third, "input", "3", "name") != "codex_deferred_tool" {
		t.Fatal("deferred history did not use stable dispatch")
	}
	if bytes.Contains(jsonBytes(t, marker(t, fourth, "tools")), []byte(`"docs"`)) {
		t.Fatal("discovered schemas leaked into tools")
	}
	if !bytes.Contains(jsonBytes(t, marker(t, fourth, "input")), []byte(`write`)) {
		t.Fatal("discovery schemas missing from result messages")
	}
	// Preserve unrelated residents declared in Responses Lite input.
	lite := activateSearch(t, "lite", deepSeekModel, jsonBytes(t, map[string]any{"input": []any{
		map[string]any{"type": "additional_tools", "tools": []any{functionDefinition("resident"), searchDefinition()}},
	}}))
	if marker(t, lite, "tools", "0", "name") != "resident" || marker(t, lite, "tools", "1", "name") != "tool_search" {
		t.Fatal("Responses Lite resident/search missing")
	}
}

func TestDeepSeekDispatchResponseAndCustomHistory(t *testing.T) {
	custom := map[string]any{"type": "namespace", "name": "docs", "tools": []any{map[string]any{"type": "custom", "name": "patch", "format": map[string]any{"type": "text"}}}}
	input := searchHistory("s", []any{discoveredNamespace("read"), custom})
	deepSeekRequest(t, t.Name(), input)
	for _, tc := range []struct{ name, args, kind string }{
		{"read", `{"namespace":"docs","name":"read","arguments":{"id":9007199254740993}}`, "function_call"},
		{"patch", `{"namespace":"docs","name":"patch","input":"patch text"}`, "custom_tool_call"},
	} {
		item := map[string]any{"type": "function_call", "name": "codex_deferred_tool", "id": "item", "call_id": "call", "arguments": tc.args}
		response := hookedBody(t, "response.intercept_after", responseIntercept{RequestID: t.Name(), SourceFormat: "responses", Model: deepSeekModel, Body: jsonBytes(t, map[string]any{"output": []any{item}})})
		if marker(t, response, "output", "0", "type") != tc.kind || marker(t, response, "output", "0", "namespace") != "docs" || marker(t, response, "output", "0", "name") != tc.name || marker(t, response, "output", "0", "call_id") != "call" {
			t.Fatal("dispatch lost callable identity")
		}
		if tc.kind == "function_call" && !strings.Contains(marker(t, response, "output", "0", "arguments").(string), "9007199254740993") {
			t.Fatal("numeric arguments lost precision")
		}
	}
	input = append(input, map[string]any{"type": "custom_tool_call", "namespace": "docs", "name": "patch", "call_id": "custom", "input": "patch text"}, map[string]any{"type": "custom_tool_call_output", "call_id": "custom", "output": "done"})
	updated := deepSeekRequest(t, "custom-history", input)
	if marker(t, updated, "input", "2", "name") != "codex_deferred_tool" || marker(t, updated, "input", "3", "type") != "function_call_output" {
		t.Fatal("custom history was not paired through dispatch")
	}
}

func TestDeepSeekDispatchStreamAndIsolation(t *testing.T) {
	for _, framing := range []string{"sse", "json"} {
		t.Run(framing, func(t *testing.T) {
			id := t.Name()
			request := deepSeekRequest(t, id, searchHistory("s", []any{discoveredNamespace("read")}))
			args := `{"namespace":"docs","name":"read","arguments":{"id":7}}`
			events := []map[string]any{
				searchEvent("response.output_item.added", "d", "codex_deferred_tool", "", 0),
				searchEvent("response.output_item.added", "s", "tool_search", "", 1),
				{"type": "response.function_call_arguments.delta", "item_id": "d", "output_index": 0, "delta": args},
				{"type": "response.function_call_arguments.done", "item_id": "d", "output_index": 0, "arguments": args},
				searchEvent("response.output_item.done", "d", "codex_deferred_tool", args, 0),
				searchEvent("response.output_item.done", "s", "tool_search", `{"query":"more"}`, 1),
				{"type": "response.completed", "response": map[string]any{"output": []any{
					searchEvent("done", "d", "codex_deferred_tool", args, 0)["item"], searchEvent("done", "s", "tool_search", `{"query":"more"}`, 1)["item"],
				}}},
			}
			var wire []byte
			for _, event := range events {
				wire = append(wire, searchWire(t, framing, event)...)
			}
			var output []byte
			for index, offset := 0, 0; offset < len(wire); index++ {
				end := min(offset+31, len(wire))
				body, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: deepSeekModel, Body: wire[offset:end], ChunkIndex: index})
				if !drop {
					output = append(output, body...)
				}
				offset = end
			}
			if bytes.Contains(output, []byte("codex_deferred_tool")) || !bytes.Contains(output, []byte(`"namespace":"docs"`)) || !bytes.Contains(output, []byte(`"type":"tool_search_call"`)) {
				t.Fatal("stream leaked shim or lost interleaved discovery")
			}
			if bytes.Count(output, []byte(`"name":"read"`)) != 3 {
				t.Fatal("dispatch added/done/completed not coherent")
			}
			// An already-normalized auth retry must reset attempt state and preserve body.
			if got := activateSearch(t, id, deepSeekModel, request); got != nil {
				t.Fatal("normalized auth retry rewrote prompt")
			}
		})
	}
	// No schemas cross chat/request boundaries; invalid calls fail closed.
	for _, args := range []string{`{"name":"read","namespace":"docs","arguments":{}}`, `{`, `{"name":"missing","arguments":{}}`, strings.Repeat("x", maxPendingFrame+1)} {
		id := t.Name() + string(rune(len(args)))
		deepSeekRequest(t, id, nil)
		response := hookedBody(t, "response.intercept_after", responseIntercept{RequestID: id, SourceFormat: "responses", Model: deepSeekModel, Body: jsonBytes(t, map[string]any{"output": []any{map[string]any{"type": "function_call", "name": "codex_deferred_tool", "arguments": args}}})})
		if marker(t, response, "status") != "failed" {
			t.Fatal("invalid/undiscovered dispatch did not fail closed")
		}
	}
}

func TestDeepSeekSearchGateAndNoSearchPassthrough(t *testing.T) {
	for _, model := range []string{deepSeekModel, "DeepSeek-V4.1-Flash-1017"} {
		if !searchRouteSupported(model, "", "openai") || searchRouteSupported(model, "", "claude") {
			t.Fatal("wrong upstream format gate")
		}
	}
	if searchRouteSupported("gpt-6.1-sol", "", "openai") || searchRouteSupported("claude-opus-5-5", "", "openai") {
		t.Fatal("unrelated provider broadened")
	}
	body := jsonBytes(t, map[string]any{"tools": []any{functionDefinition("resident")}, "instructions": "unchanged"})
	if got := activateSearch(t, "no-search", deepSeekModel, body); got != nil {
		t.Fatal("unadvertised search changed request")
	}
}

func TestDeepSeekDiscoveryCollisionDoesNotChangePrefix(t *testing.T) {
	first := deepSeekRequest(t, "collision-before", nil)
	input := searchHistory("s", []any{functionDefinition("tool_search"), functionDefinition("codex_deferred_tool")})
	second := deepSeekRequest(t, "collision-after", input)
	if !bytes.Equal(jsonBytes(t, marker(t, first, "tools")), jsonBytes(t, marker(t, second, "tools"))) {
		t.Fatal("discovered shim-name collision changed static prefix")
	}
	if marker(t, second, "input", "0", "name") != "tool_search" {
		t.Fatal("discovered shim collision misrouted search history")
	}
	forced := activateSearch(t, "collision-forced", deepSeekModel, jsonBytes(t, map[string]any{"tools": []any{functionDefinition("resident"), searchDefinition()}, "input": input, "tool_choice": map[string]any{"type": "tool_search"}}))
	if marker(t, forced, "tool_choice", "name") != "tool_search" {
		t.Fatal("forced search was confused with a discovered same-name function")
	}
}

func TestDeepSeekForcedToolRemainsRestricted(t *testing.T) {
	id := t.Name()
	input := searchHistory("s", []any{discoveredNamespace("read"), discoveredNamespace("write")})
	activateSearch(t, id, deepSeekModel, jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}, "input": input, "tool_choice": map[string]any{"type": "function", "namespace": "docs", "name": "read"}}))
	response := hookedBody(t, "response.intercept_after", responseIntercept{RequestID: id, SourceFormat: "responses", Model: deepSeekModel, Body: jsonBytes(t, map[string]any{"output": []any{map[string]any{"type": "function_call", "name": "codex_deferred_tool", "arguments": `{"namespace":"docs","name":"write","arguments":{}}`}}})})
	if marker(t, response, "status") != "failed" {
		t.Fatal("dispatcher broadened a forced tool choice")
	}
}

func TestDeepSeekDeferredCollaborationKeepsV2Marker(t *testing.T) {
	definition := map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{functionDefinition("spawn_agent")}}
	deepSeekRequest(t, t.Name(), searchHistory("s", []any{definition}))
	response := hookedBody(t, "response.intercept_after", responseIntercept{RequestID: t.Name(), SourceFormat: "responses", Model: deepSeekModel, Body: jsonBytes(t, map[string]any{"output": []any{map[string]any{"type": "function_call", "name": "codex_deferred_tool", "call_id": "spawn", "arguments": `{"namespace":"collaboration","name":"spawn_agent","arguments":{"message":"test"}}`}}})})
	if !bytes.Contains(response, []byte(`"encrypted_function_args":[]`)) {
		t.Fatal("deferred collaboration lost plaintext handoff marker")
	}
}

func TestDeepSeekDispatchRetryResetsFailure(t *testing.T) {
	id := t.Name()
	request := deepSeekRequest(t, id, searchHistory("s", []any{discoveredNamespace("read")}))
	invoke := func(args string) []byte {
		return hookedBody(t, "response.intercept_after", responseIntercept{RequestID: id, SourceFormat: "responses", Model: deepSeekModel, Body: jsonBytes(t, map[string]any{"output": []any{map[string]any{"type": "function_call", "name": "codex_deferred_tool", "arguments": args}}})})
	}
	if marker(t, invoke(`{`), "status") != "failed" {
		t.Fatal("invalid dispatch did not fail")
	}
	if got := activateSearch(t, id, deepSeekModel, request); got != nil {
		t.Fatal("normalized retry changed body")
	}
	if marker(t, invoke(`{"namespace":"docs","name":"read","arguments":{}}`), "output", "0", "name") != "read" {
		t.Fatal("retry retained failed dispatch state")
	}
}

func TestDeepSeekInheritedEffortAndPlaintextChildCompatibility(t *testing.T) {
	body := hookedBody(t, "request.intercept_before", requestIntercept{SourceFormat: "responses", Model: "DeepSeek-V4.1-Flash-1017", RequestedModel: deepSeekModel, Body: []byte(`{"reasoning":{"effort":"ultra"},"input":[{"type":"agent_message","content":[{"type":"input_text","text":"task"}]}]}`)})
	if marker(t, body, "reasoning", "effort") != "max" || marker(t, body, "input", "0", "role") != "user" {
		t.Fatal("DeepSeek local compatibility prerequisites regressed")
	}
}

func TestCPADeepSeekPrefixAndDispatchRoundTrip(t *testing.T) {
	initial := []any{map[string]any{"role": "user", "content": "Search docs."}}
	first := deepSeekRequest(t, "cpa-first", initial)
	input := append(append([]any{}, initial...), searchHistory("s", []any{discoveredNamespace("read")})...)
	second := deepSeekRequest(t, "cpa-second", input)
	input = append(input, map[string]any{"type": "function_call", "namespace": "docs", "name": "read", "call_id": "read", "arguments": `{"id":7}`}, map[string]any{"type": "function_call_output", "call_id": "read", "output": "document"})
	third := deepSeekRequest(t, "cpa-third", input)
	results := cpaFixture(t, "testdata/deepseek_cpa_probe.go", []map[string]any{
		{"Action": "request", "Request": json.RawMessage(first)},
		{"Action": "request", "Request": json.RawMessage(second)},
		{"Action": "request", "Request": json.RawMessage(third)},
	})
	for _, result := range results[1:] {
		if !bytes.Equal(jsonBytes(t, marker(t, results[0], "tools")), jsonBytes(t, marker(t, result, "tools"))) {
			t.Fatal("CPA translation changed static tools")
		}
	}
	for i := 1; i < len(results); i++ {
		before := marker(t, results[i-1], "messages").([]any)
		after := marker(t, results[i], "messages").([]any)
		if !reflect.DeepEqual(before, after[:len(before)]) {
			t.Fatal("CPA translation changed existing message prefix")
		}
	}
	if !bytes.Contains(results[2], []byte(`"name":"codex_deferred_tool"`)) || bytes.Contains(jsonBytes(t, marker(t, results[2], "tools")), []byte("docs")) {
		t.Fatal("translated dispatch/schema placement incorrect")
	}
	t.Log("CPA real translator: identical static tools and append-only message prefixes across discovery and invocation")
}
