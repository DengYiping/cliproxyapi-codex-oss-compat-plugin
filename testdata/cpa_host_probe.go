// Synthetic native-host lifecycle probe. Runs in a temporary module under
// CPA's module prefix; never starts a server or executes an upstream request.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func hostConfig(dir string) *config.Config {
	enabled := true
	return &config.Config{Plugins: config.PluginsConfig{
		Enabled: true, Dir: dir,
		Configs: map[string]config.PluginInstanceConfig{"codex-oss-compat": {Enabled: &enabled}},
	}}
}

func main() {
	var operations []struct{ Library string }
	if err := json.NewDecoder(os.Stdin).Decode(&operations); err != nil || len(operations) != 1 {
		panic("expected one native library operation")
	}
	library, err := os.ReadFile(operations[0].Library)
	if err != nil {
		panic(err)
	}
	dir, err := os.MkdirTemp("", "compat-native-drain-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	stages := []string{filepath.Join(dir, "old"), filepath.Join(dir, "replacement")}
	for _, stage := range stages {
		platform := filepath.Join(stage, runtime.GOOS, runtime.GOARCH)
		if err := os.MkdirAll(platform, 0700); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(platform, filepath.Base(operations[0].Library)), library, 0600); err != nil {
			panic(err)
		}
	}
	ctx := context.Background()
	host := pluginhost.New()
	host.ApplyConfig(ctx, hostConfig(stages[0]))
	if !host.PluginRegistered("codex-oss-compat") {
		panic("candidate library did not register")
	}
	streamID, nonstreamID := "native-drain-stream", "native-drain-nonstream"
	defer func() {
		for _, id := range []string{streamID, nonstreamID, "native-drain-replacement"} {
			host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: id, Outcome: pluginapi.RequestCompletionSucceeded})
		}
		host.ShutdownAll()
	}()
	request := []byte(`{"tools":[{"type":"tool_search","execution":"client","description":"Synthetic discovery","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}]}`)
	activate := func(id, model string) pluginapi.RequestInterceptResponse {
		return host.InterceptRequestAfterAuth(ctx, pluginapi.RequestInterceptRequest{RequestID: id, SourceFormat: "responses", ToFormat: "claude", Model: model, Body: request})
	}
	for id, model := range map[string]string{streamID: "claude-opus-4-8", nonstreamID: "claude-fable-5-1"} {
		if response := activate(id, model); !bytes.Contains(response.Body, []byte(`"name":"tool_search"`)) {
			panic("native activation failed")
		}
	}
	event := []byte("event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","item":{"type":"function_call","name":"tool_search","call_id":"native","arguments":"{\"query\":\"docs\"}"}}` + "\n\n")
	first := host.InterceptStreamChunk(ctx, pluginapi.StreamChunkInterceptRequest{RequestID: streamID, SourceFormat: "responses", Model: "claude-opus-4-8", Body: event[:len(event)/2]})
	if !first.DropChunk {
		panic("native stream did not buffer the partial event")
	}
	reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	reloaded := make(chan struct{})
	go func() {
		host.ApplyConfig(reloadCtx, hostConfig(stages[1]))
		close(reloaded)
	}()
	// No private state inspection: rejection proves the native quiesce hook ran.
	rejected := false
	for attempt, deadline := 0, time.Now().Add(3*time.Second); time.Now().Before(deadline); attempt++ {
		id := fmt.Sprintf("native-drain-admission-%d", attempt)
		response := activate(id, "claude-opus-5-5")
		if response.Terminate && response.StatusCode == 503 {
			rejected = true
			break
		}
		host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: id, Outcome: pluginapi.RequestCompletionSucceeded})
		time.Sleep(time.Millisecond)
	}
	if !rejected {
		panic("reload did not close bridge admission")
	}
	select {
	case <-reloaded:
		panic("host replaced the library with active bridges")
	default:
	}
	stream := host.InterceptStreamChunk(ctx, pluginapi.StreamChunkInterceptRequest{RequestID: streamID, SourceFormat: "responses", Model: "claude-opus-4-8", Body: event[len(event)/2:], ChunkIndex: 1})
	streamDrained := !stream.DropChunk && bytes.Contains(stream.Body, []byte(`"type":"tool_search_call"`)) && bytes.Contains(stream.Body, []byte(`"arguments":{"query":"docs"}`))
	host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: streamID, Outcome: pluginapi.RequestCompletionSucceeded})
	select {
	case <-reloaded:
		panic("host replaced the library before the nonstream bridge completed")
	default:
	}
	body := []byte(`{"output":[{"type":"function_call","name":"tool_search","call_id":"native","arguments":"{\"query\":\"docs\"}"}]}`)
	nonstream := host.InterceptResponse(ctx, pluginapi.ResponseInterceptRequest{RequestID: nonstreamID, SourceFormat: "responses", Model: "claude-fable-5-1", Body: body})
	nonstreamDrained := bytes.Contains(nonstream.Body, []byte(`"type":"tool_search_call"`))
	host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: nonstreamID, Outcome: pluginapi.RequestCompletionSucceeded})
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		panic("host did not replace the library after bridges drained")
	}
	if reloadCtx.Err() != nil {
		panic("reload exceeded its bounded deadline")
	}
	activated := activate("native-drain-replacement", "claude-opus-5-5")
	replacement := host.InterceptResponse(ctx, pluginapi.ResponseInterceptRequest{RequestID: "native-drain-replacement", SourceFormat: "responses", Model: "claude-opus-5-5", Body: body})
	result := []map[string]bool{{
		"new_requests_rejected": rejected, "stream_drained": streamDrained, "nonstream_drained": nonstreamDrained,
		"replacement_loaded":        host.PluginRegistered("codex-oss-compat"),
		"replacement_bridge_active": !activated.Terminate && bytes.Contains(replacement.Body, []byte(`"type":"tool_search_call"`)),
	}}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		panic(err)
	}
}
