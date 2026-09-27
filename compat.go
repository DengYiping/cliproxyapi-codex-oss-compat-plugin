package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
)

// Only models whose plaintext collaboration calls need the v2 marker are changed.
// The effort fallback follows the supported levels in the local Codex model catalog.
var effortFallback = map[string]string{
	"glm-5.3-flash": "high",
	"glm-5.3-cyber": "max",
}

const maxPendingFrame = 1 << 20

type requestIntercept struct {
	RequestID      string
	SourceFormat   string
	Model          string
	RequestedModel string
	Body           []byte
}

type responseIntercept struct {
	SourceFormat   string
	Model          string
	RequestedModel string
	Body           []byte
}

type chunkIntercept struct {
	RequestID      string
	SourceFormat   string
	Model          string
	RequestedModel string
	Body           []byte
	ChunkIndex     int
}

type streamState struct {
	pending     []byte
	passthrough bool
}

var streams = struct {
	sync.Mutex
	items map[string]*streamState
}{items: make(map[string]*streamState)}

func supported(model, requested string) (string, bool) {
	if fallback, ok := effortFallback[strings.ToLower(requested)]; ok {
		return fallback, true
	}
	fallback, ok := effortFallback[strings.ToLower(model)]
	return fallback, ok
}

func isResponses(format string) bool {
	return format == "openai-response" || format == "responses"
}

func rewriteEffort(body []byte, fallback string) []byte {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return nil
	}
	var reasoning map[string]json.RawMessage
	if json.Unmarshal(request["reasoning"], &reasoning) != nil {
		return nil
	}
	var effort string
	if json.Unmarshal(reasoning["effort"], &effort) != nil || effort != "ultra" {
		return nil
	}
	updatedEffort, _ := json.Marshal(fallback)
	reasoning["effort"] = updatedEffort
	updatedReasoning, _ := json.Marshal(reasoning)
	request["reasoning"] = updatedReasoning
	updated, _ := json.Marshal(request)
	return updated
}

func rewriteAgentMessageInput(body []byte) []byte {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return nil
	}
	var input []json.RawMessage
	if json.Unmarshal(request["input"], &input) != nil {
		return nil
	}
	changed := false
	for i, rawItem := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(rawItem, &item) != nil {
			continue
		}
		var kind string
		if json.Unmarshal(item["type"], &kind) != nil || kind != "agent_message" {
			continue
		}
		var content []map[string]json.RawMessage
		if json.Unmarshal(item["content"], &content) != nil || len(content) == 0 {
			continue
		}
		plaintext := true
		for _, part := range content {
			var partType, text string
			if json.Unmarshal(part["type"], &partType) != nil || partType != "input_text" ||
				json.Unmarshal(part["text"], &text) != nil {
				plaintext = false
				break
			}
		}
		if !plaintext {
			continue
		}
		item["type"] = json.RawMessage(`"message"`)
		item["role"] = json.RawMessage(`"user"`)
		delete(item, "author")
		delete(item, "recipient")
		delete(item, "internal_chat_message_metadata_passthrough")
		input[i], _ = json.Marshal(item)
		changed = true
	}
	if !changed {
		return nil
	}
	request["input"], _ = json.Marshal(input)
	updated, _ := json.Marshal(request)
	return updated
}

func collaborationName(item map[string]any) bool {
	name, _ := item["name"].(string)
	namespace, _ := item["namespace"].(string)
	if namespace == "collaboration" {
		return name == "spawn_agent" || name == "send_message" || name == "followup_task"
	}
	switch name {
	case "collaboration.spawn_agent", "collaboration.send_message", "collaboration.followup_task",
		"collaboration__spawn_agent", "collaboration__send_message", "collaboration__followup_task":
		return namespace == ""
	}
	return false
}

func markPlaintext(item map[string]any) bool {
	if item["type"] != "function_call" || !collaborationName(item) {
		return false
	}
	if marker, exists := item["encrypted_function_args"]; exists && marker != nil {
		return false
	}
	arguments, ok := item["arguments"].(string)
	if !ok {
		return false
	}
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return false
	}
	var message string
	if json.Unmarshal(args["message"], &message) != nil || strings.TrimSpace(message) == "" {
		return false
	}
	item["encrypted_function_args"] = []string{}
	return true
}

func rewriteResponse(body []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	changed := false
	switch payload["type"] {
	case "response.output_item.done", "response.output_item.added":
		if item, ok := payload["item"].(map[string]any); ok {
			changed = markPlaintext(item)
		}
	case "response.completed":
		if response, ok := payload["response"].(map[string]any); ok {
			if items, ok := response["output"].([]any); ok {
				for _, rawItem := range items {
					if item, ok := rawItem.(map[string]any); ok && markPlaintext(item) {
						changed = true
					}
				}
			}
		}
	default:
		if items, ok := payload["output"].([]any); ok {
			for _, rawItem := range items {
				if item, ok := rawItem.(map[string]any); ok && markPlaintext(item) {
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return updated
}

func rewriteFrame(frame []byte) []byte {
	// The proxy emits one JSON object per SSE data line. Preserve framing and
	// other event fields, and leave unsupported multi-line data untouched.
	lines := bytes.Split(frame, []byte("\n"))
	dataLine := -1
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if dataLine != -1 {
				return frame
			}
			dataLine = i
		}
	}
	if dataLine < 0 {
		return frame
	}
	hasCR := bytes.HasSuffix(lines[dataLine], []byte("\r"))
	line := bytes.TrimSuffix(lines[dataLine], []byte("\r"))
	updated := rewriteResponse(bytes.TrimSpace(line[len("data:"):]))
	if len(updated) == 0 {
		return frame
	}
	lines[dataLine] = append([]byte("data: "), updated...)
	if hasCR {
		lines[dataLine] = append(lines[dataLine], '\r')
	}
	return bytes.Join(lines, []byte("\n"))
}

func frameBoundary(b []byte) int {
	lf := bytes.Index(b, []byte("\n\n"))
	crlf := bytes.Index(b, []byte("\r\n\r\n"))
	if crlf >= 0 && (lf < 0 || crlf < lf) {
		return crlf + 4
	}
	if lf >= 0 {
		return lf + 2
	}
	return -1
}

func completeUndelimitedEvent(frame []byte) bool {
	// CLIProxyAPI also accepts and forwards a complete JSON data line without
	// the blank-line terminator. Do not hold such an event until stream close.
	var payload []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if payload != nil {
			return false
		}
		payload = bytes.TrimSpace(line[len("data:"):])
	}
	return len(payload) > 0 && (json.Valid(payload) || bytes.Equal(payload, []byte("[DONE]")))
}

func rewriteChunk(req chunkIntercept) (body []byte, drop bool) {
	if req.ChunkIndex < 0 || !isResponses(req.SourceFormat) {
		return nil, false
	}
	if _, ok := supported(req.Model, req.RequestedModel); !ok {
		return nil, false
	}
	if req.RequestID == "" {
		return rewriteFrame(req.Body), false
	}
	streams.Lock()
	defer streams.Unlock()
	state := streams.items[req.RequestID]
	if state == nil {
		state = &streamState{}
		streams.items[req.RequestID] = state
	}
	if state.passthrough {
		return nil, false
	}
	state.pending = append(state.pending, req.Body...)
	var output []byte
	for {
		end := frameBoundary(state.pending)
		if end < 0 {
			break
		}
		output = append(output, rewriteFrame(state.pending[:end])...)
		state.pending = state.pending[end:]
	}
	if completeUndelimitedEvent(state.pending) {
		output = append(output, rewriteFrame(state.pending)...)
		state.pending = nil
	}
	if len(state.pending) > maxPendingFrame {
		state.passthrough = true
		output = append(output, state.pending...)
		state.pending = nil
	}
	if len(output) == 0 {
		return nil, true
	}
	return output, false
}

func releaseStream(requestID string) {
	streams.Lock()
	delete(streams.items, requestID)
	streams.Unlock()
}
