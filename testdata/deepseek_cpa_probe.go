// Synthetic OpenAI-compatible translator fixture; no network or credentials.
package main

import (
	"encoding/json"
	"os"

	responses "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
)

func main() {
	var operations []struct {
		Action  string
		Request json.RawMessage
	}
	if err := json.NewDecoder(os.Stdin).Decode(&operations); err != nil {
		panic(err)
	}
	var results []json.RawMessage
	for _, op := range operations {
		results = append(results, responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("DeepSeek-V4.1-Flash-1017", op.Request, false))
	}
	if err := json.NewEncoder(os.Stdout).Encode(results); err != nil {
		panic(err)
	}
}
