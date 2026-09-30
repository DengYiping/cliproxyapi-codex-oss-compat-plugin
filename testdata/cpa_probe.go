// This fixture runs in a temporary module under CPA's module prefix so it can
// exercise the real internal translators without adding plugin dependencies.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"

	responses "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/claude/openai/responses"
)

type operation struct {
	Action  string
	Request json.RawMessage
	Chunks  [][]byte
}

func main() {
	var operations []operation
	if err := json.NewDecoder(os.Stdin).Decode(&operations); err != nil {
		panic(err)
	}
	results := make([]json.RawMessage, 0, len(operations))
	for _, op := range operations {
		var result []byte
		var state any
		switch op.Action {
		case "request":
			result = responses.ConvertOpenAIResponsesRequestToClaudeWithCompat("claude-opus-5-5", op.Request, true)
		case "response":
			result = responses.ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-opus-5-5", op.Request, nil, bytes.Join(op.Chunks, []byte("\n")), &state)
		case "stream":
			var chunks [][]byte
			for _, chunk := range op.Chunks {
				chunks = append(chunks, responses.ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-opus-5-5", op.Request, nil, chunk, &state)...)
			}
			result, _ = json.Marshal(chunks)
		}
		results = append(results, result)
	}
	if err := json.NewEncoder(os.Stdout).Encode(results); err != nil {
		panic(err)
	}
}
