package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func searchWire(t *testing.T, framing string, payload map[string]any) []byte {
	t.Helper()
	body := jsonBytes(t, payload)
	if framing == "sse" {
		return append(append([]byte("event: "+stringField(payload, "type")+"\ndata: "), body...), []byte("\n\n")...)
	}
	return body
}

func TestSearchRetryResetsAttemptState(t *testing.T) {
	for _, framing := range []string{"json", "sse"} {
		for _, normalized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/normalized=%t", framing, normalized), func(t *testing.T) {
				id := t.Name()
				original := jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}})
				updated := activateSearch(t, id, "claude-opus-4-8", original)
				partial := []byte(`{"type":"response.created","response":{"id":"failed_attempt","unfinished":`)
				if framing == "sse" {
					partial = append([]byte("event: response.created\ndata: "), partial...)
				}
				if _, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-4-8", Body: partial}); !drop {
					t.Fatal("incomplete first attempt was delivered")
				}
				retry := original
				if normalized {
					retry = updated
				}
				activateSearch(t, id, "claude-opus-4-8", retry)
				var wire []byte
				for i, payload := range []map[string]any{
					{"type": "response.created", "response": map[string]any{"id": "retry"}},
					searchEvent("response.output_item.done", "retry", "tool_search", `{"query":"retry"}`, 0),
				} {
					out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-4-8", Body: searchWire(t, framing, payload), ChunkIndex: i})
					if drop {
						t.Fatal("complete retry event was dropped")
					}
					wire = append(wire, out...)
				}
				if bytes.Contains(wire, []byte("failed_attempt")) || !bytes.Contains(wire, []byte(`"type":"tool_search_call"`)) {
					t.Fatal("retry retained old bytes or lost search activation")
				}
			})
		}
	}
}

func TestSearchRetryResetsFramingAndArguments(t *testing.T) {
	id := t.Name()
	request := activateSearch(t, id, "claude-opus-5-5", jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}}))
	rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-5-5", Body: searchWire(t, "sse", searchEvent("response.output_item.added", "old", "tool_search", "", 0))})
	rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-5-5", Body: searchWire(t, "sse", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "old", "delta": `{"query":"old"}`}), ChunkIndex: 1})
	activateSearch(t, id, "claude-opus-5-5", request)
	streams.Lock()
	state := streams.items[id]
	clean := len(state.pending) == 0 && state.framing == framingUnknown && !state.passthrough && len(state.search.calls) == 0 && state.search.buffered == 0
	streams.Unlock()
	if !clean {
		t.Fatal("normalized retry retained framing or argument state")
	}
	// A retry can change executors and therefore switch from SSE to bare JSON.
	body := searchWire(t, "json", searchEvent("response.output_item.done", "new", "tool_search", `{"query":"new"}`, 0))
	out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-5-5", Body: body})
	if drop || marker(t, out, "item", "arguments", "query") != "new" {
		t.Fatal("retry could not select its own framing")
	}
}

func TestSearchOversizeStreamFailsClosed(t *testing.T) {
	for _, framing := range []string{"json", "sse"} {
		t.Run(framing, func(t *testing.T) {
			id := t.Name()
			activateSearch(t, id, "claude-opus-4-8", jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}}))
			partial := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", maxPendingFrame))
			if framing == "sse" {
				partial = append([]byte("data: "), partial...)
			}
			out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-4-8", Body: partial})
			if drop || !bytes.Contains(out, []byte(`"type":"response.failed"`)) || bytes.Contains(out, partial) {
				t.Fatal("oversized active stream was not replaced with a bounded failure")
			}
			if framing == "json" {
				if marker(t, out, "response", "error", "code") != "compatibility_buffer_exceeded" {
					t.Fatal("missing actionable failure code")
				}
			} else if !bytes.HasPrefix(out, []byte("event: response.failed\ndata: ")) || !bytes.HasSuffix(out, []byte("\n\n")) {
				t.Fatal("failure did not preserve SSE framing")
			}
			for i, body := range [][]byte{[]byte(`"}`), searchWire(t, framing, searchEvent("response.output_item.done", "small", "tool_search", `{"query":"docs"}`, 0))} {
				if out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-4-8", Body: body, ChunkIndex: i + 1}); !drop || len(out) != 0 {
					t.Fatal("failed stream leaked a later payload")
				}
			}
			// The same lifecycle ID may retry after a controlled stream failure.
			activateSearch(t, id, "claude-opus-4-8", jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}}))
			out, drop = rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-4-8", Body: searchWire(t, framing, searchEvent("response.output_item.done", "retry", "tool_search", `{"query":"docs"}`, 0))})
			if drop || !bytes.Contains(out, []byte(`"type":"tool_search_call"`)) {
				t.Fatal("retry inherited the terminal failure")
			}
		})
	}
}

func TestSearchMalformedJSONStreamFailsClosed(t *testing.T) {
	id := t.Name()
	activateSearch(t, id, "claude-fable-5-1", jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}}))
	out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-fable-5-1", Body: []byte(`{"type":]`)})
	if drop || marker(t, out, "response", "error", "code") != "compatibility_invalid_stream" {
		t.Fatal("malformed activated JSON stream did not fail closed")
	}
	if out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-fable-5-1", Body: searchWire(t, "json", searchEvent("response.output_item.done", "later", "tool_search", `{}`, 0)), ChunkIndex: 1}); !drop || len(out) != 0 {
		t.Fatal("malformed stream leaked a later search shim")
	}
}

func TestSearchFailureKeepsResponseIdentityAndSequence(t *testing.T) {
	id := t.Name()
	activateSearch(t, id, "claude-opus-5-5", jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}}))
	created := searchWire(t, "json", map[string]any{"type": "response.created", "sequence_number": 40, "response": map[string]any{"id": "known-response"}})
	rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-5-5", Body: created})
	partial := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", maxPendingFrame))
	out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-5-5", Body: partial, ChunkIndex: 1})
	if drop || marker(t, out, "response", "id") != "known-response" || marker(t, out, "sequence_number") != float64(41) {
		t.Fatal("terminal compatibility error lost the response identity or sequence")
	}
	streams.Lock()
	state := streams.items[id]
	bounded := len(state.pending) == 0 && state.search.buffered == 0 && len(state.search.calls) == 0
	streams.Unlock()
	if !bounded {
		t.Fatal("terminal failure retained buffered payload state")
	}
}

func TestNonSearchOversizeStreamRemainsBestEffort(t *testing.T) {
	id := t.Name()
	t.Cleanup(func() { releaseStream(id) })
	partial := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", maxPendingFrame))
	out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "glm-5.3-flash", Body: partial})
	if drop || !bytes.Equal(out, partial) {
		t.Fatal("non-search overflow no longer preserves best-effort passthrough")
	}
	if out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "glm-5.3-flash", Body: []byte(`"}`), ChunkIndex: 1}); drop || out != nil {
		t.Fatal("non-search passthrough state changed")
	}
}

func startSearchQuiesce(t *testing.T) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := handleMethod("plugin.quiesce", nil)
		done <- err
	}()
	// Reconfiguration also unblocks a canceled/rolled-back native quiesce call.
	t.Cleanup(func() { _, _ = handleMethod("plugin.reconfigure", nil) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		streams.Lock()
		quiescing := streams.quiescing
		streams.Unlock()
		if quiescing {
			return done
		}
		select {
		case err := <-done:
			t.Fatalf("quiesce returned before requests drained: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("quiesce did not begin")
	return done
}

func TestSearchQuiesceDrainsRequestsBeforeReplacement(t *testing.T) {
	request := jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}})
	streamID, nonstreamID := t.Name()+"/stream", t.Name()+"/nonstream"
	normalized := activateSearch(t, streamID, "claude-opus-4-8", request)
	activateSearch(t, nonstreamID, "claude-fable-5-1", request)
	done := startSearchQuiesce(t)
	ordinary := jsonBytes(t, map[string]any{"tools": []any{functionDefinition("tool_search")}})
	if updated := activateSearch(t, t.Name()+"/ordinary", "claude-opus-5-5", ordinary); updated != nil {
		t.Fatal("quiesce changed an unadvertised ordinary search function")
	}
	newID := t.Name() + "/new"
	rejected, err := handleMethod("request.intercept_after", jsonBytes(t, requestIntercept{RequestID: newID, SourceFormat: "responses", ToFormat: "claude", Model: "claude-opus-5-5", Body: request}))
	if err != nil {
		t.Fatal(err)
	}
	rejection := jsonBytes(t, rejected)
	if marker(t, rejection, "Terminate") != true || marker(t, rejection, "StatusCode") != float64(503) || marker(t, rejection, "Body") != nil {
		t.Fatal("new bridge was not rejected before upstream normalization")
	}
	streams.Lock()
	_, inserted := streams.items[newID]
	streams.Unlock()
	if inserted {
		t.Fatal("draining instance activated a new bridge")
	}
	// Existing attempts, including auth retries, must continue while draining.
	activateSearch(t, streamID, "claude-opus-4-8", normalized)
	out, drop := rewriteChunk(chunkIntercept{RequestID: streamID, SourceFormat: "responses", Model: "claude-opus-4-8", Body: searchWire(t, "json", searchEvent("response.output_item.done", "last", "tool_search", `{"query":"docs"}`, 0))})
	if drop || marker(t, out, "item", "type") != "tool_search_call" {
		t.Fatal("quiesce interrupted an active stream")
	}
	hookedBody(t, "request.complete", map[string]any{"RequestID": streamID})
	select {
	case err := <-done:
		t.Fatalf("quiesce missed the active nonstream request: %v", err)
	default:
	}
	out = hookedBody(t, "response.intercept_after", responseIntercept{RequestID: nonstreamID, SourceFormat: "responses", Model: "claude-fable-5-1", Body: jsonBytes(t, map[string]any{"output": []any{map[string]any{"type": "function_call", "name": "tool_search", "call_id": "last", "arguments": `{}`}}})})
	if marker(t, out, "output", "0", "type") != "tool_search_call" {
		t.Fatal("quiesce interrupted an active nonstream request")
	}
	hookedBody(t, "request.complete", map[string]any{"RequestID": nonstreamID})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("quiesce did not finish after the final request completed")
	}
	// A host rollback calls reconfigure; it must resume without erasing requests.
	_, _ = handleMethod("plugin.reconfigure", nil)
	if updated := activateSearch(t, newID, "claude-opus-5-5", request); len(updated) == 0 {
		t.Fatal("reconfiguration did not resume activation")
	}
}

func TestSearchQuiesceRollbackResumesActiveInstance(t *testing.T) {
	id := t.Name()
	request := jsonBytes(t, map[string]any{"tools": []any{searchDefinition()}})
	activateSearch(t, id, "claude-opus-5-5", request)
	done := startSearchQuiesce(t)
	_, _ = handleMethod("plugin.reconfigure", nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("rollback left the canceled native quiesce call blocked")
	}
	out, drop := rewriteChunk(chunkIntercept{RequestID: id, SourceFormat: "responses", Model: "claude-opus-5-5", Body: searchWire(t, "json", searchEvent("response.output_item.done", "kept", "tool_search", `{"query":"docs"}`, 0))})
	if drop || marker(t, out, "item", "type") != "tool_search_call" {
		t.Fatal("rollback discarded an active request's state")
	}
	if updated := activateSearch(t, id+"/new", "claude-opus-5-5", request); len(updated) == 0 {
		t.Fatal("rollback did not allow new requests")
	}
}
