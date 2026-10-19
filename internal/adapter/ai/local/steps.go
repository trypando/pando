package local

import (
	"github.com/trypando/pando/internal/adapter/ai/aikit"
	"github.com/trypando/pando/internal/adapter/api"
)

// The servers the steps name, each in its address, model and link rows.
const (
	serverOllama   = "Ollama"
	serverLMStudio = "LM Studio"
	serverLlamaCpp = "llama.cpp"
	serverVLLM     = "vLLM"
)

// steps walk through the server's address and the model it serves.
func steps() []api.Step {
	return []api.Step{
		{ID: "server", Label: "Server", Title: "Connect to your model server",
			Do: []api.Action{
				{Text: "Enter the address of the OpenAI-compatible server you run, ending `/v1`. On this machine, by its default port:", Table: &api.Table{
					Columns: []string{"Server", "Address"},
					Rows: [][]string{
						{serverOllama, "http://host.docker.internal:11434/v1"},
						{serverLMStudio, "http://host.docker.internal:1234/v1"},
						{serverLlamaCpp, "http://host.docker.internal:9931/v1"},
						{serverVLLM, "http://host.docker.internal:8000/v1"},
					},
				}},
				{Text: "On another machine, use its address instead, such as `http://gpu.example:11434/v1`."},
				{Text: "On Linux, a server on this machine has to listen on an address other than `127.0.0.1`, and hosted apps can reach it there too."},
				{Text: "Enter an API key only if the server requires one: `--api-key` for `llama-server` or `vllm serve`, **Require Authentication** in LM Studio."},
			},
			Links: []api.Link{
				{Label: serverOllama, URL: "https://docs.ollama.com/api/openai-compatibility"},
				{Label: serverLMStudio, URL: "https://lmstudio.ai/docs/developer/core/server"},
				{Label: serverLlamaCpp, URL: "https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md"},
				{Label: serverVLLM, URL: "https://docs.vllm.ai/en/latest/serving/online_serving/openai_compatible_server/"},
			},
			Fields: []string{"base_url", "api_key"}},
		aikit.ModelStep(nil,
			api.Action{Text: "Enter the model's name as the server serves it:", Table: &api.Table{
				Columns: []string{"Server", "Model name"},
				Rows: [][]string{
					{serverOllama, "As `ollama list` shows it, after `ollama pull`"},
					{serverLMStudio, "Its identifier, as `lms ls` shows it"},
					{serverLlamaCpp, "Its `--alias`, or else the `-m` path"},
					{serverVLLM, "Its `--served-model-name`, or else the model given to `vllm serve`"},
				},
			}}),
	}
}
