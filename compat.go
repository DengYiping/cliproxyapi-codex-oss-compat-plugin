package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
)

// Only models whose plaintext collaboration calls need the v2 marker are changed.
// The effort fallback follows the supported levels in the local Codex model catalog.
var effortFallback = map[string]string{
	"kimi-k3":         "high",
	"glm-5.3":         "high",
	"glm-5.3-flash":   "high",
	"glm-5.3-cyber":   "max",
	"claude-opus-5-5": "xhigh",
}

const maxPendingFrame = 1 << 20

type requestIntercept struct {
	RequestID      string
	SourceFormat   string
	ToFormat       string
	Model          string
	RequestedModel string
	Body           []byte
}

type responseIntercept struct {
	RequestID      string
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
	pending       []byte
	passthrough   bool
	failed        bool
	framing       streamFraming
	search        *searchBridge
	collaboration bool
	responseID    string
	nextSequence  int64
}

type streamFraming int

const (
	framingUnknown streamFraming = iota
	framingSSE
	// WebSocket-backed executors hand stream interceptors bare JSON events.
	framingJSON
)

var streams = struct {
	sync.Mutex
	items      map[string]*streamState
	quiescing  bool
	drainEpoch uint64
}{items: make(map[string]*streamState)}

var streamChanges = sync.NewCond(&streams.Mutex)

// CPA calls quiesce before replacing a native library, but does not pin a
// request to that library. Keep this instance active until its bridges finish,
// and reject new activations while the host loads the replacement.
func quiesceSearchRequests() {
	streams.Lock()
	defer streams.Unlock()
	streams.quiescing = true
	streams.drainEpoch++
	epoch := streams.drainEpoch
	for streams.quiescing && streams.drainEpoch == epoch {
		active := false
		for _, state := range streams.items {
			if state.search != nil {
				active = true
				break
			}
		}
		if !active {
			return
		}
		streamChanges.Wait()
	}
}

// A canceled/failed replacement re-registers the old instance. Resume without
// clearing its active requests, and wake any detached native quiesce call.
func resumeSearchRequests() {
	streams.Lock()
	streams.quiescing = false
	streams.drainEpoch++
	streamChanges.Broadcast()
	streams.Unlock()
}

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
	if decodeJSON(body, &payload) != nil {
		return nil
	}
	changed := false
	switch payload["type"] {
	case "response.output_item.done", "response.output_item.added":
		if item, ok := payload["item"].(map[string]any); ok {
			changed = markPlaintext(item)
		}
	case "response.completed", "response.incomplete", "response.failed":
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
	return rewriteFrameWith(frame, func(body []byte) ([]byte, bool) { return rewriteResponse(body), false })
}

type payloadRewriter func([]byte) ([]byte, bool)

func rewriteFrameWith(frame []byte, rewrite payloadRewriter) []byte {
	if framingOf(frame) == framingJSON {
		output, rest, ok := rewriteJSONValuesWith(frame, rewrite)
		if !ok || len(rest) > 0 {
			return frame
		}
		return output
	}
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
	updated, drop := rewrite(bytes.TrimSpace(line[len("data:"):]))
	if drop {
		return nil
	}
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

func framingOf(b []byte) streamFraming {
	trimmed := bytes.TrimLeft(b, " \t\r\n")
	if len(trimmed) == 0 {
		return framingUnknown
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return framingJSON
	}
	return framingSSE
}

// rewriteJSONValues rewrites each complete JSON value in b, preserving the
// whitespace between values. An incomplete trailing value is returned as rest.
func rewriteJSONValues(b []byte) (output, rest []byte, ok bool) {
	return rewriteJSONValuesWith(b, func(body []byte) ([]byte, bool) { return rewriteResponse(body), false })
}

func rewriteJSONValuesWith(b []byte, rewrite payloadRewriter) (output, rest []byte, ok bool) {
	pos := 0
	for {
		start := pos
		for pos < len(b) && strings.IndexByte(" \t\r\n", b[pos]) >= 0 {
			pos++
		}
		if pos == len(b) {
			return append(output, b[start:]...), nil, true
		}
		decoder := json.NewDecoder(bytes.NewReader(b[pos:]))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return output, b[start:], true
			}
			return nil, nil, false
		}
		end := pos + int(decoder.InputOffset())
		updated, drop := rewrite(b[pos:end])
		if !drop {
			output = append(output, b[start:pos]...)
			if len(updated) > 0 {
				output = append(output, updated...)
			} else {
				output = append(output, b[pos:end]...)
			}
		}
		pos = end
	}
}

func rewriteChunk(req chunkIntercept) (body []byte, drop bool) {
	if req.ChunkIndex < 0 || !isResponses(req.SourceFormat) {
		return nil, false
	}
	_, collaboration := supported(req.Model, req.RequestedModel)
	if !collaboration && !searchSupported(req.Model, req.RequestedModel) {
		return nil, false
	}
	if req.RequestID == "" {
		return rewriteFrame(req.Body), false
	}
	streams.Lock()
	defer streams.Unlock()
	state := streams.items[req.RequestID]
	if state == nil {
		if !collaboration {
			return nil, false
		}
		state = &streamState{collaboration: collaboration}
		streams.items[req.RequestID] = state
	}
	if state.failed {
		return nil, true
	}
	if state.passthrough {
		return nil, false
	}
	state.pending = append(state.pending, req.Body...)
	if state.framing == framingUnknown {
		state.framing = framingOf(state.pending)
	}
	var output []byte
	switch state.framing {
	case framingJSON:
		rewritten, rest, ok := rewriteJSONValuesWith(state.pending, state.rewritePayload)
		if !ok {
			if state.search != nil {
				return state.failStream("compatibility_invalid_stream", "Tool discovery compatibility received an invalid JSON stream."), false
			}
			state.passthrough = true
			output, state.pending = state.pending, nil
			return output, false
		}
		output, state.pending = rewritten, rest
	case framingSSE:
		for {
			end := frameBoundary(state.pending)
			if end < 0 {
				break
			}
			output = append(output, rewriteFrameWith(state.pending[:end], state.rewritePayload)...)
			state.pending = state.pending[end:]
		}
		if completeUndelimitedEvent(state.pending) {
			output = append(output, rewriteFrameWith(state.pending, state.rewritePayload)...)
			state.pending = nil
		}
	}
	if len(state.pending) > maxPendingFrame {
		if state.search != nil {
			output = append(output, state.failStream("compatibility_buffer_exceeded", "Tool discovery compatibility could not buffer an incomplete event larger than 1 MiB.")...)
			return output, false
		}
		state.passthrough = true
		output = append(output, state.pending...)
		state.pending = nil
	}
	if len(output) == 0 {
		return nil, true
	}
	return output, false
}

func (state *streamState) rewritePayload(body []byte) ([]byte, bool) {
	var metadata struct {
		SequenceNumber *int64 `json:"sequence_number"`
		Response       struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &metadata) == nil {
		if sequence := metadata.SequenceNumber; sequence != nil && *sequence >= state.nextSequence && *sequence < 1<<63-1 {
			state.nextSequence = *sequence + 1
		}
		if metadata.Response.ID != "" {
			state.responseID = metadata.Response.ID
		}
	}
	original := body
	if state.collaboration {
		if updated := rewriteResponse(body); len(updated) > 0 {
			body = updated
		}
	}
	if state.search != nil {
		if updated, drop := state.search.rewritePayload(body); drop {
			return nil, true
		} else if len(updated) > 0 {
			body = updated
		}
	}
	if bytes.Equal(original, body) {
		return nil, false
	}
	return body, false
}

// Search bridging is required for correctness: forwarding the ordinary shim
// after a parser failure would invoke Codex's search handler with the wrong
// payload type. Emit one terminal error, discard incomplete bytes, and suppress
// the rest of this attempt. Non-search compatibility remains best-effort.
func (state *streamState) failStream(code, message string) []byte {
	state.failed = true
	state.pending = nil
	state.search.calls = make(map[string]*searchCall)
	state.search.buffered = 0
	response := map[string]any{
		"object": "response", "status": "failed", "output": []any{},
		"error": map[string]any{"type": "server_error", "code": code, "message": message},
	}
	if state.responseID != "" {
		response["id"] = state.responseID
	}
	body, _ := json.Marshal(map[string]any{"type": "response.failed", "sequence_number": state.nextSequence, "response": response})
	if state.framing == framingJSON {
		return body
	}
	return append(append([]byte("event: response.failed\ndata: "), body...), []byte("\n\n")...)
}

// Request hooks run before CPA translation. Keep only response-routing state;
// definitions are reloaded from this request's history, never a shared cache.
func interceptSearchRequest(req requestIntercept) (updated []byte, reject bool) {
	streams.Lock()
	defer streams.Unlock()
	state := streams.items[req.RequestID]
	var previous *searchBridge
	if state != nil {
		previous = state.search
	}
	updated, bridge := rewriteSearchRequest(req.Body, previous)
	if bridge != nil && streams.quiescing && previous == nil {
		return nil, true
	}
	if bridge != nil && req.RequestID != "" {
		_, collaboration := supported(req.Model, req.RequestedModel)
		// The lifecycle ID survives upstream retries; parser framing, pending
		// bytes, failure flags, and argument tracking belong to one attempt only.
		streams.items[req.RequestID] = &streamState{
			search:        &searchBridge{name: bridge.name, calls: make(map[string]*searchCall)},
			collaboration: collaboration,
		}
	}
	return updated, false
}

func interceptSearchResponse(req responseIntercept) []byte {
	streams.Lock()
	defer streams.Unlock()
	state := streams.items[req.RequestID]
	if state == nil {
		return nil
	}
	if state.search != nil {
		updated, _ := state.search.rewritePayload(req.Body)
		return updated
	}
	return nil
}

func releaseStream(requestID string) {
	streams.Lock()
	delete(streams.items, requestID)
	streamChanges.Broadcast()
	streams.Unlock()
}
