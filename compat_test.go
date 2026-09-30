package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func call(name, args string) map[string]any {
	return map[string]any{"type": "function_call", "name": name, "namespace": "collaboration", "arguments": args, "call_id": "call_1"}
}

func marker(t *testing.T, raw []byte, path ...string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	for _, part := range path {
		switch node := value.(type) {
		case map[string]any:
			value = node[part]
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				t.Fatalf("invalid array index %q", part)
			}
			value = node[index]
		default:
			t.Fatalf("unexpected JSON node %T at %q", node, part)
		}
	}
	return value
}

func TestPlaintextCollaborationOnly(t *testing.T) {
	plain := call("spawn_agent", `{"task_name":"math","message":"17 + 25"}`)
	encrypted := call("send_message", `{"target":"math","message":"opaque"}`)
	encrypted["encrypted_function_args"] = []any{"message"}
	other := call("wait_agent", `{"message":"some text"}`)
	invalid := call("followup_task", "ciphertext")
	full, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{plain, encrypted, other, invalid}}})
	updated := rewriteResponse(full)
	if got := marker(t, updated, "response", "output", "0", "encrypted_function_args"); len(got.([]any)) != 0 {
		t.Fatalf("plaintext marker = %#v", got)
	}
	var result map[string]any
	if err := json.Unmarshal(updated, &result); err != nil {
		t.Fatal(err)
	}
	items := result["response"].(map[string]any)["output"].([]any)
	if got := items[1].(map[string]any)["encrypted_function_args"].([]any); len(got) != 1 {
		t.Fatalf("encrypted marker changed: %#v", got)
	}
	for _, i := range []int{2, 3} {
		if _, ok := items[i].(map[string]any)["encrypted_function_args"]; ok {
			t.Fatalf("unrelated or opaque item %d was marked", i)
		}
	}
	if rewriteResponse(updated) != nil {
		t.Fatal("second pass should be byte-preserving")
	}
}

func TestDoneEventAndPlainResponse(t *testing.T) {
	for _, event := range []string{"response.output_item.done", "response.output_item.added"} {
		body, _ := json.Marshal(map[string]any{"type": event, "item": call("collaboration.followup_task", `{"target":"math","message":"next"}`)})
		// Flat names are used without a separate namespace.
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		delete(p["item"].(map[string]any), "namespace")
		body, _ = json.Marshal(p)
		updated := rewriteResponse(body)
		if got := marker(t, updated, "item", "encrypted_function_args"); len(got.([]any)) != 0 {
			t.Fatalf("event %s marker = %#v", event, got)
		}
	}
	response, _ := json.Marshal(map[string]any{"output": []any{call("send_message", `{"target":"math","message":"hi"}`)}})
	if got := marker(t, rewriteResponse(response), "output", "0", "encrypted_function_args"); len(got.([]any)) != 0 {
		t.Fatalf("plain response marker = %#v", got)
	}
}

func TestSSEFragmentsAndUnrelatedEvents(t *testing.T) {
	id := "sse-test"
	defer releaseStream(id)
	item := `{"type":"response.output_item.done","item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{\"message\":\"hello\"}","call_id":"c"}}`
	first := []byte("event: response.output_item.done\r\ndata: " + item[:len(item)/2])
	second := []byte(item[len(item)/2:] + "\r\n\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\r\n\r\n")
	req := chunkIntercept{RequestID: id, SourceFormat: "openai-response", Model: "glm-5.3-flash", ChunkIndex: 0, Body: first}
	if body, drop := rewriteChunk(req); len(body) != 0 || !drop {
		t.Fatalf("partial event body=%q drop=%v", body, drop)
	}
	req.ChunkIndex++
	req.Body = second
	body, drop := rewriteChunk(req)
	if drop || !bytes.Contains(body, []byte(`"encrypted_function_args":[]`)) || !bytes.Contains(body, []byte("\r\n\r\n")) {
		t.Fatalf("combined SSE events body=%q drop=%v", body, drop)
	}
	releaseStream(id)
	if len(streams.items) != 0 {
		t.Fatal("completion leaked request state")
	}
	if got := rewriteFrame([]byte("event: heartbeat\ndata: hello\n\n")); string(got) != "event: heartbeat\ndata: hello\n\n" {
		t.Fatalf("unrelated event changed: %q", got)
	}
}

func TestSSEUndelimitedCompleteEvent(t *testing.T) {
	id := "undelimited-test"
	defer releaseStream(id)
	body := []byte(`event: response.output_item.done` + "\n" + `data: {"type":"response.output_item.done","item":{"type":"function_call","namespace":"collaboration","name":"send_message","arguments":"{\"target\":\"child\",\"message\":\"hello\"}","call_id":"c"}}`)
	updated, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "openai-response", Model: "glm-5.3-flash", ChunkIndex: 0, Body: body})
	if drop || !bytes.Contains(updated, []byte(`"encrypted_function_args":[]`)) {
		t.Fatalf("complete event without blank line must be delivered: drop=%v body=%q", drop, updated)
	}
	// A later terminator must not cause the already-delivered event to repeat.
	ending, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "openai-response", Model: "glm-5.3-flash", ChunkIndex: 1, Body: []byte("\n\n")})
	if drop || string(ending) != "\n\n" {
		t.Fatalf("delimiter was not preserved: drop=%v body=%q", drop, ending)
	}
}

func TestRawJSONChunksFromWebSocketStream(t *testing.T) {
	id := "websocket-json-test"
	defer releaseStream(id)
	item := `{"type":"response.output_item.done","item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{\"message\":\"hello\"}","call_id":"c"}}`
	req := chunkIntercept{RequestID: id, SourceFormat: "openai-response", Model: "glm-5.3-flash", ChunkIndex: 0, Body: []byte(item)}
	body, drop := rewriteChunk(req)
	if drop || !bytes.Contains(body, []byte(`"encrypted_function_args":[]`)) || bytes.HasPrefix(body, []byte("data:")) {
		t.Fatalf("complete raw JSON event body=%q drop=%v", body, drop)
	}

	req.ChunkIndex++
	req.Body = []byte(item[:len(item)/2])
	if body, drop := rewriteChunk(req); len(body) != 0 || !drop {
		t.Fatalf("partial raw JSON event body=%q drop=%v", body, drop)
	}
	req.ChunkIndex++
	req.Body = []byte(item[len(item)/2:] + "\n" + `{"type":"response.created","response":{"id":"r"}}`)
	body, drop = rewriteChunk(req)
	if drop || !bytes.Contains(body, []byte(`"encrypted_function_args":[]`)) || !bytes.HasSuffix(body, []byte("\n"+`{"type":"response.created","response":{"id":"r"}}`)) {
		t.Fatalf("split and batched raw JSON events body=%q drop=%v", body, drop)
	}

	unrelated := []byte(`{"type":"response.created","response":{"id":"r2"}}`)
	req.ChunkIndex++
	req.Body = unrelated
	if body, drop := rewriteChunk(req); drop || !bytes.Equal(body, unrelated) {
		t.Fatalf("unrelated raw JSON event body=%q drop=%v", body, drop)
	}
}

func TestInvalidRawJSONStreamPassesThrough(t *testing.T) {
	id := "websocket-invalid-json-test"
	defer releaseStream(id)
	req := chunkIntercept{RequestID: id, SourceFormat: "openai-response", Model: "glm-5.3-flash", Body: []byte(`{"type":]`)}
	if body, drop := rewriteChunk(req); drop || string(body) != `{"type":]` {
		t.Fatalf("invalid raw JSON body=%q drop=%v", body, drop)
	}
	req.ChunkIndex++
	req.Body = []byte(`{"type":"response.created"}`)
	if body, drop := rewriteChunk(req); drop || len(body) != 0 {
		t.Fatalf("passthrough stream should leave later chunks unchanged: body=%q drop=%v", body, drop)
	}
}

func TestUltraFallbackAndAllowlist(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"kimi-k3", "high"},
		{"glm-5.3", "high"},
		{"glm-5.3-flash", "high"},
		{"glm-5.3-cyber", "max"},
		{"claude-opus-5-5", "xhigh"},
	} {
		fallback, ok := supported(tc.model, "")
		if !ok {
			t.Fatalf("model %q not allowed", tc.model)
		}
		body := rewriteEffort([]byte(`{"model":"`+tc.model+`","reasoning":{"effort":"ultra","summary":"auto"},"stream":true}`), fallback)
		if got := marker(t, body, "reasoning", "effort"); got != tc.want {
			t.Fatalf("model %q effort=%v", tc.model, got)
		}
		if !strings.Contains(string(body), `"summary":"auto"`) || rewriteEffort(body, fallback) != nil {
			t.Fatal("mapping did not preserve other fields or was not idempotent")
		}
	}
	if _, ok := supported("gpt-5.5", ""); ok {
		t.Fatal("must not rewrite proprietary models")
	}
	if rewriteEffort([]byte(`{"reasoning":{"effort":"medium"}}`), "high") != nil {
		t.Fatal("non-ultra effort changed")
	}
}

func TestPlaintextChildInputBecomesUserMessage(t *testing.T) {
	input := []byte(`{"model":"glm-5.3-flash","input":[{"type":"agent_message","author":"/root","recipient":"/root/child","content":[{"type":"input_text","text":"Message Type: NEW_TASK\\nPayload: 42"}],"internal_chat_message_metadata_passthrough":{"turn_id":"1"}},{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]}]}`)
	updated := rewriteAgentMessageInput(input)
	if got := marker(t, updated, "input", "0", "type"); got != "message" {
		t.Fatalf("child message type = %v", got)
	}
	if got := marker(t, updated, "input", "0", "role"); got != "user" {
		t.Fatalf("child message role = %v", got)
	}
	if got := marker(t, updated, "input", "0", "content", "0", "text"); !strings.Contains(got.(string), "Payload: 42") {
		t.Fatalf("child payload was lost: %v", got)
	}
	if marker(t, updated, "input", "0", "author") != nil || marker(t, updated, "input", "0", "recipient") != nil {
		t.Fatal("internal routing metadata leaked to upstream user message")
	}
	if got := marker(t, updated, "input", "1", "role"); got != "developer" {
		t.Fatalf("second item changed: %v", got)
	}
	if rewriteAgentMessageInput(updated) != nil {
		t.Fatal("second pass should be byte-preserving")
	}
}

func TestEncryptedChildInputIsUntouched(t *testing.T) {
	for _, content := range []string{
		`[{"type":"encrypted_content","encrypted_content":"opaque"}]`,
		`[{"type":"input_text","text":"heading"},{"type":"encrypted_content","encrypted_content":"opaque"}]`,
	} {
		input := []byte(`{"input":[{"type":"agent_message","author":"/root","content":` + content + `}]}`)
		if updated := rewriteAgentMessageInput(input); updated != nil {
			t.Fatalf("encrypted content was changed: %s", updated)
		}
	}
}

func TestRPCEnvelopeAndScope(t *testing.T) {
	registration, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	registrationJSON, _ := json.Marshal(registration)
	if got := marker(t, registrationJSON, "capabilities", "response_stream_interceptor"); got != true {
		t.Fatalf("missing stream capability: %#v", got)
	}
	request := []byte(`{"SourceFormat":"openai-response","Model":"glm-5.3-flash","Body":"eyJyZWFzb25pbmciOnsiZWZmb3J0IjoidWx0cmEifX0="}`)
	result, err := handleMethod("request.intercept_before", request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if got := marker(t, raw, "Body"); got == nil {
		t.Fatal("request hook failed to return a body")
	} else {
		var body []byte
		if err := json.Unmarshal(raw, &struct{ Body *[]byte }{Body: &body}); err != nil {
			t.Fatal(err)
		}
		if effort := marker(t, body, "reasoning", "effort"); effort != "high" {
			t.Fatalf("raw ultra did not map to high: %v", effort)
		}
	}
	child := []byte(`{"model":"glm-5.3-flash","input":[{"type":"agent_message","author":"/root","recipient":"/root/child","content":[{"type":"input_text","text":"task marker"}]}]}`)
	reqJSON, _ := json.Marshal(requestIntercept{SourceFormat: "openai-response", Model: "glm-5.3-flash", Body: child})
	result, err = handleMethod("request.intercept_before", reqJSON)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(result)
	var converted struct{ Body []byte }
	if err := json.Unmarshal(raw, &converted); err != nil {
		t.Fatal(err)
	}
	if role := marker(t, converted.Body, "input", "0", "role"); role != "user" {
		t.Fatalf("child task was not converted for upstream: %v", role)
	}
	unrelated := []byte(`{"SourceFormat":"chat-completions","Model":"glm-5.3-flash","Body":"eyJyZWFzb25pbmciOnsiZWZmb3J0IjoidWx0cmEifX0="}`)
	result, err = handleMethod("request.intercept_before", unrelated)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(result)
	if string(raw) != `{}` {
		t.Fatalf("unrelated request was changed: %s", raw)
	}
}
