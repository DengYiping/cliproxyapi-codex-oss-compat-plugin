package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Preserve numeric schema enums/limits and unrelated payload fields exactly.
func decodeJSON(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

// This is a client-side protocol bridge, not Anthropic's native tool search.
// Only an advertised execution=client search enables response rewriting.
func searchSupported(model, requested string) bool {
	if requested != "" {
		model = requested
	}
	switch strings.ToLower(model) {
	case "claude-opus-5-5", "claude-opus-4-8", "claude-fable-5-1":
		return true
	}
	return false
}

type searchBridge struct {
	name     string
	calls    map[string]*searchCall
	buffered int
}

type searchCall struct {
	arguments string
}

func stringField(item map[string]any, key string) string {
	value, _ := item[key].(string)
	return value
}

func copyObject(original map[string]any) map[string]any {
	copy := make(map[string]any, len(original))
	for key, value := range original {
		copy[key] = value
	}
	return copy
}

func requestTools(tools, input []any) []any {
	all := append([]any(nil), tools...)
	for _, raw := range input {
		if item, ok := raw.(map[string]any); ok && item["type"] == "additional_tools" {
			additional, _ := item["tools"].([]any)
			all = append(all, additional...)
		}
	}
	return all
}

// Match CPA's namespace qualification, including already qualified MCP names.
func qualifiedToolName(namespace, name string) string {
	namespace, name = strings.TrimSpace(namespace), strings.TrimSpace(name)
	if namespace == "" || name == "" || strings.HasPrefix(name, "mcp__") ||
		name == namespace || strings.HasPrefix(name, namespace+"__") {
		return name
	}
	if strings.HasSuffix(namespace, "__") {
		return namespace + name
	}
	return namespace + "__" + name
}

// Merge by upstream callable identity. Current declarations win; newest search
// results win over older discoveries. Keep namespace containers for CPA's
// inverse response-name mapping, and never request native defer_loading.
func loadSearchTools(tools []any, input []any) []any {
	seen := make(map[string]bool)
	namespaces := make(map[string]map[string]any)
	register := func(raw any) {
		tool, ok := raw.(map[string]any)
		if !ok {
			return
		}
		name := stringField(tool, "name")
		if tool["type"] == "namespace" {
			if namespaces[name] == nil {
				namespaces[name] = tool
			}
			children, _ := tool["tools"].([]any)
			for _, rawChild := range children {
				if child, ok := rawChild.(map[string]any); ok {
					seen[qualifiedToolName(name, stringField(child, "name"))] = true
				}
			}
		} else if name != "" {
			seen[name] = true
		}
	}
	for _, tool := range requestTools(tools, input) {
		register(tool)
	}
	for i := len(input) - 1; i >= 0; i-- {
		item, ok := input[i].(map[string]any)
		if !ok || item["type"] != "tool_search_output" || item["execution"] != "client" || item["status"] != "completed" {
			continue
		}
		discovered, _ := item["tools"].([]any)
		for _, raw := range discovered {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name := stringField(tool, "name")
			if name == "" {
				continue
			}
			tool = copyObject(tool)
			delete(tool, "defer_loading")
			if tool["type"] == "function" {
				if _, ok := tool["parameters"].(map[string]any); !ok || seen[name] {
					continue
				}
				seen[name] = true
				tools = append(tools, tool)
			} else if tool["type"] == "namespace" {
				children, _ := tool["tools"].([]any)
				var added []any
				for _, rawChild := range children {
					child, ok := rawChild.(map[string]any)
					if !ok || stringField(child, "name") == "" {
						continue
					}
					if child["type"] != "function" && child["type"] != "custom" {
						continue
					}
					if child["type"] == "function" {
						if _, ok := child["parameters"].(map[string]any); !ok {
							continue
						}
					}
					identity := qualifiedToolName(name, stringField(child, "name"))
					if seen[identity] {
						continue
					}
					seen[identity] = true
					child = copyObject(child)
					delete(child, "defer_loading")
					added = append(added, child)
				}
				if len(added) == 0 {
					continue
				}
				if existing := namespaces[name]; existing != nil {
					children, _ := existing["tools"].([]any)
					existing["tools"] = append(children, added...)
				} else {
					// Copy the container: do not truncate the replayed search result.
					container := copyObject(tool)
					container["tools"] = added
					namespaces[name] = container
					tools = append(tools, container)
				}
			}
		}
	}
	return tools
}

func rewriteSearchRequest(body []byte, previous *searchBridge) ([]byte, *searchBridge) {
	var request map[string]any
	if decodeJSON(body, &request) != nil {
		return nil, nil
	}
	tools, _ := request["tools"].([]any)
	input, _ := request["input"].([]any)
	var definition map[string]any
	for _, raw := range requestTools(tools, input) {
		if tool, ok := raw.(map[string]any); ok && tool["type"] == "tool_search" && tool["execution"] == "client" {
			definition = tool
			break
		}
	}
	if definition == nil {
		// An auth retry may receive the already normalized request. Only reuse
		// activation from this execution, never infer it from a function name.
		return nil, previous
	}
	tools = loadSearchTools(tools, input)
	occupied := make(map[string]bool)
	for _, raw := range requestTools(tools, input) {
		if tool, ok := raw.(map[string]any); ok && tool["type"] != "tool_search" {
			if tool["type"] == "namespace" {
				children, _ := tool["tools"].([]any)
				for _, rawChild := range children {
					if child, ok := rawChild.(map[string]any); ok {
						occupied[qualifiedToolName(stringField(tool, "name"), stringField(child, "name"))] = true
					}
				}
			} else {
				occupied[stringField(tool, "name")] = true
			}
		}
	}
	name := "tool_search"
	for suffix := 1; occupied[name]; suffix++ {
		name = fmt.Sprintf("codex_tool_search_%d", suffix)
	}
	definition["type"], definition["name"] = "function", name
	delete(definition, "execution")
	delete(definition, "defer_loading")
	if name != "tool_search" {
		definition["description"] = stringField(definition, "description") + "\nCall " + name + " for tool discovery on this route."
	}
	if choice, ok := request["tool_choice"].(map[string]any); ok && choice["type"] == "tool_search" {
		request["tool_choice"] = map[string]any{"type": "function", "name": name}
	}
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || item["execution"] != "client" || stringField(item, "call_id") == "" {
			continue
		}
		switch item["type"] {
		case "tool_search_call":
			arguments, _ := json.Marshal(item["arguments"])
			item["type"], item["name"], item["arguments"] = "function_call", name, string(arguments)
			delete(item, "execution")
		case "tool_search_output":
			output, _ := json.Marshal(item)
			item["type"], item["output"] = "function_call_output", string(output)
			delete(item, "execution")
			delete(item, "tools")
			delete(item, "status")
		}
	}
	request["tools"] = tools
	updated, _ := json.Marshal(request)
	return updated, &searchBridge{name: name, calls: make(map[string]*searchCall)}
}

func (bridge *searchBridge) matches(item map[string]any) bool {
	return item["type"] == "function_call" && item["name"] == bridge.name && stringField(item, "namespace") == ""
}

func (bridge *searchBridge) rewriteItem(item map[string]any, arguments string) bool {
	if !bridge.matches(item) {
		return false
	}
	if value, ok := item["arguments"].(string); ok {
		if value != "" {
			arguments = value
		}
		var parsed any
		if arguments == "" {
			parsed = map[string]any{}
		} else if decodeJSON([]byte(arguments), &parsed) != nil {
			// Preserve malformed input as a JSON value so Codex's search handler
			// reports a normal argument error instead of executing a function.
			parsed = arguments
		}
		item["arguments"] = parsed
	} else if _, exists := item["arguments"]; !exists {
		item["arguments"] = map[string]any{}
	}
	item["type"], item["execution"] = "tool_search_call", "client"
	delete(item, "name")
	delete(item, "namespace")
	delete(item, "encrypted_function_args")
	return true
}

func (bridge *searchBridge) bufferArguments(call *searchCall, value string) {
	bridge.buffered -= len(call.arguments)
	if bridge.buffered+len(value) > maxPendingFrame {
		// A malformed JSON value makes missing final arguments fail through
		// Codex's normal search error path. Never execute truncated arguments.
		value = "[tool search arguments exceeded compatibility buffer]"
		if bridge.buffered+len(value) > maxPendingFrame {
			value = ""
		}
	}
	call.arguments = value
	bridge.buffered += len(value)
}

func callKeys(payload map[string]any, item map[string]any) []string {
	var keys []string
	id := stringField(payload, "item_id")
	if item != nil {
		id = stringField(item, "id")
	}
	if id != "" {
		keys = append(keys, "id:"+id)
	}
	if index, ok := payload["output_index"]; ok {
		keys = append(keys, fmt.Sprintf("index:%v", index))
	}
	return keys
}

func (bridge *searchBridge) rewritePayload(body []byte) ([]byte, bool) {
	var payload map[string]any
	if decodeJSON(body, &payload) != nil {
		return nil, false
	}
	changed := false
	switch payload["type"] {
	case "response.created":
		bridge.calls = make(map[string]*searchCall)
		bridge.buffered = 0
	case "response.output_item.added", "response.output_item.done":
		item, _ := payload["item"].(map[string]any)
		if !bridge.matches(item) {
			break
		}
		keys := callKeys(payload, item)
		call := &searchCall{}
		if payload["type"] == "response.output_item.done" {
			for _, key := range keys {
				if existing := bridge.calls[key]; existing != nil {
					call = existing
					break
				}
			}
		}
		for _, key := range keys {
			if len(bridge.calls) < 512 {
				bridge.calls[key] = call
			}
		}
		changed = bridge.rewriteItem(item, call.arguments)
		if payload["type"] == "response.output_item.done" {
			bridge.bufferArguments(call, "")
		}
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		for _, key := range callKeys(payload, nil) {
			if call := bridge.calls[key]; call != nil {
				if payload["type"] == "response.function_call_arguments.done" {
					bridge.bufferArguments(call, stringField(payload, "arguments"))
				} else {
					delta := stringField(payload, "delta")
					if bridge.buffered+len(delta) > maxPendingFrame {
						bridge.bufferArguments(call, "[tool search arguments exceeded compatibility buffer]")
					} else {
						bridge.bufferArguments(call, call.arguments+delta)
					}
				}
				return nil, true
			}
		}
	default:
		response := payload
		if nested, ok := payload["response"].(map[string]any); ok {
			response = nested
		}
		items, _ := response["output"].([]any)
		for _, raw := range items {
			if item, ok := raw.(map[string]any); ok && bridge.rewriteItem(item, "") {
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	updated, _ := json.Marshal(payload)
	return updated, false
}
