package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/virtUOS/bbb-recap/internal/llmplan"
	"github.com/virtUOS/bbb-recap/internal/logging"
	"github.com/virtUOS/bbb-recap/internal/providers"
)

const toolLLMUsage = "Usage: recap tool llm complete|chunk < request.json"

// toolLLM is `recap tool llm complete` and `recap tool llm chunk`
// (contracts/providers/README.md). Both first load the providers file with
// the service's start rules; neither reads the service configuration. Logs
// go to stderr.
func toolLLM(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || (args[0] != "complete" && args[0] != "chunk") {
		fmt.Fprintln(stderr, toolLLMUsage)
		return exitUsage
	}
	prog := "recap tool llm " + args[0]
	level := slog.LevelInfo
	if v := os.Getenv("RECAP_LOG_LEVEL"); v != "" {
		var err error
		if level, err = logging.ParseLevel(v); err != nil {
			fmt.Fprintf(stderr, "%s: RECAP_LOG_LEVEL: %v\n", prog, err)
			return exitUsage
		}
	}
	log := logging.New(stderr, level)
	path := os.Getenv("RECAP_PROVIDERS_FILE")
	if path == "" {
		fmt.Fprintf(stderr, "%s: RECAP_PROVIDERS_FILE: required\n", prog)
		return exitUsage
	}
	set, err := providers.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid providers file (RECAP_PROVIDERS_FILE):\n%v\n", prog, err)
		return exitUsage
	}
	set.LogWarnings(log)
	if args[0] == "chunk" {
		return toolLLMChunk(prog, set, stdout, stderr)
	}
	return toolLLMComplete(prog, set, log, stdout, stderr)
}

// llmCompleteInput is the stdin of `recap tool llm complete`.
type llmCompleteInput struct {
	Profile string                `json:"profile"`
	Request *providers.LlmRequest `json:"request"`
}

func toolLLMComplete(prog string, set *providers.Set, log *slog.Logger, stdout, stderr io.Writer) int {
	var in llmCompleteInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fmt.Fprintf(stderr, "%s: stdin: want one JSON object: %v\n", prog, err)
		return exitUsage
	}
	req := in.Request
	switch {
	case req == nil:
		fmt.Fprintf(stderr, "%s: request: required\n", prog)
		return exitUsage
	case req.Output.Type != providers.OutputText && req.Output.Type != providers.OutputJSON:
		fmt.Fprintf(stderr, "%s: request.output.type: want text or json\n", prog)
		return exitUsage
	case req.Output.Type == providers.OutputJSON && len(req.Output.Schema) == 0:
		fmt.Fprintf(stderr, "%s: request.output.schema: required for json output\n", prog)
		return exitUsage
	case len(req.Messages) == 0:
		fmt.Fprintf(stderr, "%s: request.messages: required\n", prog)
		return exitUsage
	}
	profile, err := set.Get(providers.KindLLM, in.Profile)
	if err != nil {
		fmt.Fprintf(stderr, "%s: profile: %v\n", prog, err)
		return exitUsage
	}
	if req.MaxOutputTokens > profile.MaxOutputTokens {
		fmt.Fprintf(stderr, "%s: request.max_output_tokens: above the profile's max_output_tokens\n", prog)
		return exitUsage
	}
	adapter, err := newLLM(profile, log)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return exitUsage
	}
	resp, err := adapter.Complete(context.Background(), *req)
	if err != nil {
		return writeJSON(stdout, map[string]any{"response": nil, "error": errorJSON(err)})
	}
	return writeJSON(stdout, map[string]any{"response": resp, "error": nil})
}

// llmChunkInput is the stdin of `recap tool llm chunk`.
type llmChunkInput struct {
	Lines        []string `json:"lines"`
	BudgetTokens *int     `json:"budget_tokens"`
	Profile      string   `json:"profile"`
	PromptChars  int      `json:"prompt_chars"`
	Resplit      []int    `json:"resplit"`
}

func toolLLMChunk(prog string, set *providers.Set, stdout, stderr io.Writer) int {
	var in llmChunkInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fmt.Fprintf(stderr, "%s: stdin: want one JSON object: %v\n", prog, err)
		return exitUsage
	}
	var budget int
	if in.BudgetTokens != nil {
		budget = *in.BudgetTokens
	} else {
		profile, err := set.Get(providers.KindLLM, in.Profile)
		if err != nil {
			fmt.Fprintf(stderr, "%s: profile: %v\n", prog, err)
			return exitUsage
		}
		budget = llmplan.Budget(profile.ContextTokens, profile.MaxOutputTokens, in.PromptChars)
	}
	if budget < 1 {
		fmt.Fprintf(stderr, "%s: budget_tokens: want at least 1, got %d\n", prog, budget)
		return exitUsage
	}
	chunks := llmplan.Resplit(llmplan.Chunk(in.Lines, budget), in.Resplit)
	out := make([]map[string][]string, len(chunks))
	for i, c := range chunks {
		out[i] = map[string][]string{"lines": c}
	}
	return writeJSON(stdout, map[string]any{"budget_tokens": budget, "chunks": out})
}
