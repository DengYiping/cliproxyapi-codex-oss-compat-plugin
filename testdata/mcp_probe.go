// Read-only stdio MCP fixture. No network, filesystem, or real tool execution.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

func main() {
	parent := os.Getppid()
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if os.Getppid() != parent {
				os.Exit(0)
			}
		}
	}()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage
			Method string
			Params map[string]any
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": request.Params["protocolVersion"], "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "compat-search-probe", "version": "1.0"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Read-only compatibility test: echo a deterministic marker.", "annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"marker": map[string]any{"type": "string"}}, "required": []any{"marker"}, "additionalProperties": false}}}}
		case "tools/call":
			args, _ := request.Params["arguments"].(map[string]any)
			marker, _ := args["marker"].(string)
			if request.Params["name"] != "echo" {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unknown test tool"}}}
			} else {
				result = map[string]any{"isError": false, "content": []any{map[string]any{"type": "text", "text": "compat_probe_ok:" + marker}}}
			}
		case "ping":
			result = map[string]any{}
		default:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "unsupported fixture method"}})
			continue
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
}
