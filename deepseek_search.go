package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

func deepSeekSearch(model, requested string) bool {
	if requested != "" {
		model = requested
	}
	switch strings.ToLower(model) {
	case "deepseek-v4.1-flash-cyber", "deepseek-v4.1-flash-1017":
		return true
	}
	return false
}

func searchRouteSupported(model, requested, format string) bool {
	if deepSeekSearch(model, requested) {
		return format == "openai"
	}
	return searchSupported(model, requested) && format == "claude"
}

type deferredDispatch struct {
	name       string
	tools      map[string]map[string]any
	calls      map[string]*searchCall
	failed     bool
	responseID string
	only       string
}

func callableTools(tools []any) map[string]map[string]any {
	result := make(map[string]map[string]any)
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if tool["type"] == "namespace" {
			children, _ := tool["tools"].([]any)
			for _, child := range children {
				if child, ok := child.(map[string]any); ok {
					entry := copyObject(child)
					entry["namespace"] = tool["name"]
					result[qualifiedToolName(stringField(tool, "name"), stringField(child, "name"))] = entry
				}
			}
		} else if tool["type"] == "function" || tool["type"] == "custom" {
			result[stringField(tool, "name")] = tool
		}
	}
	return result
}

// Keep discovered schemas in their search-result messages, never in the tool
// prefix. A fixed dispatcher is an ordinary upstream function; the client sees
// only the real callable name and validates/executes it normally. No registry or
// transcript is shared across requests/chats.
func rewriteDeepSeekSearch(original, normalized []byte, bridge *searchBridge) ([]byte, *searchBridge) {
	var source, request map[string]any
	if decodeJSON(original, &source) != nil || decodeJSON(normalized, &request) != nil {
		return normalized, bridge
	}
	input, _ := source["input"].([]any)
	var discoveries []any
	for _, raw := range input {
		if item, ok := raw.(map[string]any); ok && item["type"] == "tool_search_output" {
			discoveries = append(discoveries, raw)
		}
	}
	loaded := callableTools(loadSearchTools(nil, discoveries))
	resident, _ := source["tools"].([]any)
	resident = requestTools(resident, input)
	// Current declarations take precedence over stale discovery results.
	for identity, tool := range callableTools(resident) {
		if loaded[identity] != nil {
			loaded[identity] = tool
		}
	}
	// additional_tools may contain newly loaded tools. Keep resident definitions
	// in their original order and omit only identities present in discovery.
	filter := func(tools []any) []any {
		var kept []any
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				kept = append(kept, raw)
				continue
			}
			if tool["type"] == "tool_search" {
				continue
			}
			tool = copyObject(tool)
			if tool["type"] == "namespace" {
				children, _ := tool["tools"].([]any)
				var remaining []any
				for _, child := range children {
					c, ok := child.(map[string]any)
					if !ok || loaded[qualifiedToolName(stringField(tool, "name"), stringField(c, "name"))] == nil {
						remaining = append(remaining, child)
					}
				}
				if len(remaining) == 0 {
					continue
				}
				tool["tools"] = remaining
			} else if loaded[stringField(tool, "name")] != nil {
				continue
			}
			kept = append(kept, tool)
		}
		return kept
	}
	tools := filter(resident)
	occupied := callableTools(tools)
	searchName := "tool_search"
	for suffix := 1; occupied[searchName] != nil; suffix++ {
		searchName = fmt.Sprintf("codex_tool_search_%d", suffix)
	}
	bridge.name = searchName
	name := "codex_deferred_tool"
	for occupied[name] != nil || name == bridge.name {
		name += "_"
	}
	dispatch := &deferredDispatch{name: name, tools: loaded, calls: make(map[string]*searchCall)}
	// Search must precede dispatch in every request, independent of discoveries.
	var search any
	for _, raw := range resident {
		if tool, ok := raw.(map[string]any); ok && tool["type"] == "tool_search" && tool["execution"] == "client" {
			search = copyObject(tool)
			search.(map[string]any)["type"] = "function"
			search.(map[string]any)["name"] = searchName
			delete(search.(map[string]any), "execution")
			delete(search.(map[string]any), "defer_loading")
			search.(map[string]any)["description"] = stringField(tool, "description") + "\nInvoke discovered tools through " + name + "; do not call their names directly."
			break
		}
	}
	tools = append(tools, search, map[string]any{
		"type": "function", "name": name,
		"description": "Invoke a deferred tool discovered by tool_search. Use its exact name and namespace from the search result, and follow its schema. For function tools pass arguments as an object; for custom tools pass input as a string. Do not call undiscovered tools.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"name": map[string]any{"type": "string"}, "namespace": map[string]any{"type": "string"},
			"arguments": map[string]any{"type": "object"}, "input": map[string]any{"type": "string"},
		}, "required": []any{"name"}, "additionalProperties": false},
	})
	request["tools"] = tools
	history, _ := request["input"].([]any)
	customCalls := make(map[string]bool)
	for index, raw := range history {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if original, ok := input[index].(map[string]any); ok && original["type"] == "tool_search_call" && original["execution"] == "client" {
			item["name"] = searchName
			continue
		}
		if item["type"] == "additional_tools" {
			item["tools"] = []any{}
			continue
		}
		if item["type"] == "custom_tool_call_output" && customCalls[stringField(item, "call_id")] {
			item["type"] = "function_call_output"
			continue
		}
		if item["type"] != "function_call" && item["type"] != "custom_tool_call" {
			continue
		}
		identity := qualifiedToolName(stringField(item, "namespace"), stringField(item, "name"))
		if loaded[identity] == nil {
			continue
		}
		args := map[string]any{"name": item["name"]}
		if ns := stringField(item, "namespace"); ns != "" {
			args["namespace"] = ns
		}
		if item["type"] == "custom_tool_call" {
			args["input"] = item["input"]
			customCalls[stringField(item, "call_id")] = true
		} else {
			var parsed any
			if decodeJSON([]byte(stringField(item, "arguments")), &parsed) != nil {
				parsed = item["arguments"]
			}
			args["arguments"] = parsed
		}
		encoded, _ := json.Marshal(args)
		item["type"], item["name"], item["arguments"] = "function_call", name, string(encoded)
		delete(item, "namespace")
		delete(item, "input")
	}
	if choice, ok := source["tool_choice"].(map[string]any); ok {
		identity := qualifiedToolName(stringField(choice, "namespace"), stringField(choice, "name"))
		if choice["type"] == "tool_search" {
			request["tool_choice"] = map[string]any{"type": "function", "name": searchName}
		} else if loaded[identity] != nil {
			request["tool_choice"] = map[string]any{"type": "function", "name": name}
			dispatch.only = identity
		}
	}
	bridge.dispatch = dispatch
	updated, _ := json.Marshal(request)
	return updated, bridge
}

func (dispatch *deferredDispatch) rewriteItem(item map[string]any, arguments string) bool {
	if item["type"] != "function_call" || item["name"] != dispatch.name || stringField(item, "namespace") != "" {
		return false
	}
	if value := stringField(item, "arguments"); value != "" {
		arguments = value
	}
	var args map[string]any
	if len(arguments) > maxPendingFrame || decodeJSON([]byte(arguments), &args) != nil {
		dispatch.failed = true
		return false
	}
	name, namespace := stringField(args, "name"), stringField(args, "namespace")
	identity := qualifiedToolName(namespace, name)
	tool := dispatch.tools[identity]
	if tool == nil || (dispatch.only != "" && dispatch.only != identity) || (tool["type"] != "function" && tool["type"] != "custom") {
		dispatch.failed = true
		return false
	}
	item["name"] = tool["name"]
	delete(item, "namespace")
	if ns := stringField(tool, "namespace"); ns != "" {
		item["namespace"] = ns
	}
	if tool["type"] == "custom" {
		value, ok := args["input"].(string)
		if !ok {
			dispatch.failed = true
			return false
		}
		item["type"], item["input"] = "custom_tool_call", value
		delete(item, "arguments")
	} else {
		value, ok := args["arguments"].(map[string]any)
		if !ok {
			dispatch.failed = true
			return false
		}
		encoded, _ := json.Marshal(value)
		item["arguments"] = string(encoded)
	}
	delete(item, "encrypted_function_args")
	// Dispatch happens after the normal collaboration rewrite; restored
	// plaintext collaboration calls still need Codex v2's marker.
	markPlaintext(item)
	return true
}

func (bridge *searchBridge) rewriteDispatchPayload(body []byte) ([]byte, bool, bool) {
	d := bridge.dispatch
	if d.failed {
		return nil, true, true
	}
	var payload map[string]any
	if decodeJSON(body, &payload) != nil {
		return nil, false, false
	}
	changed, drop := false, false
	switch payload["type"] {
	case "response.created":
		d.calls = make(map[string]*searchCall)
		if response, ok := payload["response"].(map[string]any); ok {
			d.responseID = stringField(response, "id")
		}
	case "response.output_item.added", "response.output_item.done":
		item, _ := payload["item"].(map[string]any)
		if item["type"] != "function_call" || item["name"] != d.name || stringField(item, "namespace") != "" {
			break
		}
		if payload["type"] == "response.output_item.added" {
			if len(d.calls) >= 512 {
				d.failed = true
				break
			}
			call := &searchCall{item: copyObject(item)}
			for _, key := range callKeys(payload, item) {
				d.calls[key] = call
			}
			drop = true
		} else {
			changed = d.rewriteItem(item, "")
		}
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		for _, key := range callKeys(payload, nil) {
			call := d.calls[key]
			if call == nil {
				continue
			}
			if payload["type"] == "response.function_call_arguments.delta" {
				if bridge.buffered+len(stringField(payload, "delta")) > maxPendingFrame {
					d.failed = true
					break
				}
				bridge.bufferArguments(call, call.arguments+stringField(payload, "delta"))
				drop = true
			} else {
				args := stringField(payload, "arguments")
				if args == "" {
					args = call.arguments
				}
				item := copyObject(call.item)
				item["arguments"] = args
				if !d.rewriteItem(item, args) {
					break
				}
				bridge.bufferArguments(call, "")
				if item["type"] == "custom_tool_call" {
					item["input"] = ""
				} else {
					item["arguments"] = ""
				}
				delete(payload, "arguments")
				delete(payload, "item_id")
				payload["type"], payload["item"] = "response.output_item.added", item
				changed = true
			}
			break
		}
	default:
		response := payload
		if nested, ok := payload["response"].(map[string]any); ok {
			response = nested
		}
		items, _ := response["output"].([]any)
		for _, raw := range items {
			if item, ok := raw.(map[string]any); ok && d.rewriteItem(item, "") {
				changed = true
			}
		}
	}
	if d.failed {
		d.calls = make(map[string]*searchCall)
		bridge.calls = make(map[string]*searchCall)
		bridge.buffered = 0
		response := map[string]any{"object": "response", "status": "failed", "output": []any{}, "error": map[string]any{"type": "server_error", "code": "compatibility_invalid_deferred_call", "message": "Deferred tool arguments were malformed, too large, or referenced an undiscovered tool. No tool was dispatched."}}
		if d.responseID != "" {
			response["id"] = d.responseID
		} else if id := stringField(payload, "id"); id != "" {
			response["id"] = id
		}
		if payload["type"] != nil {
			payload = map[string]any{"type": "response.failed", "sequence_number": payload["sequence_number"], "response": response}
		} else {
			payload = response
		}
		changed, drop = true, false
	}
	if !changed {
		return nil, drop, drop
	}
	updated, _ := json.Marshal(payload)
	return updated, true, false
}
