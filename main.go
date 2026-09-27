package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct { uint32_t abi_version; void* host_ctx; void* call; void* free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
    uint32_t abi_version;
    cliproxy_plugin_call_fn call;
    cliproxy_plugin_free_fn free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

type envelope struct {
	OK     bool `json:"ok"`
	Result any  `json:"result,omitempty"`
	Error  any  `json:"error,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil || method == nil || requestLen > C.size_t(1<<26) {
		return 1
	}
	response.ptr = nil
	response.len = 0
	var raw []byte
	if request != nil && requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, err := handleMethod(C.GoString(method), raw)
	var reply envelope
	if err != nil {
		reply = envelope{Error: map[string]string{"code": "plugin_error", "message": err.Error()}}
	} else {
		reply = envelope{OK: true, Result: result}
	}
	encoded, marshalErr := json.Marshal(reply)
	if marshalErr != nil {
		return 1
	}
	response.ptr = C.CBytes(encoded)
	response.len = C.size_t(len(encoded))
	if err != nil {
		return 1
	}
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	streams.Lock()
	streams.items = make(map[string]*streamState)
	streams.Unlock()
}

func handleMethod(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return map[string]any{
			"schema_version": 6,
			"metadata": map[string]any{
				"Name": "codex-oss-compat", "Version": "0.1.0", "Author": "local",
				"GitHubRepository": "https://github.com/router-for-me/CLIProxyAPI", "ConfigFields": []any{},
			},
			"capabilities": map[string]bool{
				"request_interceptor": true, "request_lifecycle_plugin": true,
				"response_interceptor": true, "response_stream_interceptor": true,
			},
		}, nil
	case "request.intercept_before", "request.intercept_after":
		var req requestIntercept
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode request interceptor: %w", err)
		}
		if !isResponses(req.SourceFormat) {
			return struct{}{}, nil
		}
		fallback, ok := supported(req.Model, req.RequestedModel)
		if !ok {
			return struct{}{}, nil
		}
		if updated := rewriteEffort(req.Body, fallback); len(updated) > 0 {
			return struct{ Body []byte }{updated}, nil
		}
		return struct{}{}, nil
	case "response.intercept_after":
		var req responseIntercept
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode response interceptor: %w", err)
		}
		if isResponses(req.SourceFormat) {
			if _, ok := supported(req.Model, req.RequestedModel); ok {
				if updated := rewriteResponse(req.Body); len(updated) > 0 {
					return struct{ Body []byte }{updated}, nil
				}
			}
		}
		return struct{}{}, nil
	case "response.intercept_stream_chunk":
		var req chunkIntercept
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode stream interceptor: %w", err)
		}
		updated, drop := rewriteChunk(req)
		return struct {
			Body      []byte
			DropChunk bool
		}{updated, drop}, nil
	case "request.complete":
		var req struct{ RequestID string }
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode request completion: %w", err)
		}
		releaseStream(req.RequestID)
		return struct{}{}, nil
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}
