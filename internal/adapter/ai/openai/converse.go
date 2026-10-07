package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/trypando/pando/internal/adapter/ai/aikit"
	"github.com/trypando/pando/internal/adapter/api"
)

// errDone ends a conversation from inside a call handler: the model answered.
var errDone = errors.New("done")

// converse holds one conversation through the Responses API: instructions,
// the first message, and the tools. Every function call the model makes goes
// to handle, whose output goes back to the model; handle returns errDone once
// the model has answered.
//
// Each turn continues the last by previous_response_id rather than resending
// the whole conversation, which works because OpenAI stores each response on
// its own servers. Pando stores none of it; the adapter's description says so
// where it is configured (design 10 §6.2). A model that answers in prose rather than through
// a tool is asked once more before that counts as a failure — tool use is
// asked for, not forced, as it is with every adapter.
func (a *Adapter) converse(
	ctx context.Context,
	model, instructions, input string,
	tools []aikit.Tool,
	handle func(name, arguments string) (string, bool, error),
) error {
	params := responses.ResponseNewParams{
		Model:             model,
		Instructions:      openai.String(instructions),
		Input:             responses.ResponseNewParamsInputUnion{OfString: openai.String(input)},
		Tools:             toolParams(tools),
		MaxOutputTokens:   openai.Int(maxOutputTokens),
		ParallelToolCalls: openai.Bool(true),
	}

	nudged := false
	for range maxIterations {
		// Checked before each round rather than relied on through the client,
		// so an expired budget ends the loop rather than the next HTTP call.
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := a.client.Responses.New(ctx, params)
		if err != nil {
			return fmt.Errorf("openai: %w", err)
		}

		var outputs responses.ResponseInputParam
		for _, item := range resp.Output {
			if item.Type != "function_call" {
				continue
			}
			call := item.AsFunctionCall()
			text, isError, err := handle(call.Name, call.Arguments)
			if errors.Is(err, errDone) {
				return nil
			}
			if err != nil {
				return err
			}
			if isError {
				text = "That did not work: " + text
			}
			out := responses.ResponseInputItemParamOfFunctionCallOutput(text)
			out.OfFunctionCallOutput.CallID = openai.String(call.CallID)
			outputs = append(outputs, out)
		}

		next := responses.ResponseNewParams{
			Model:              model,
			Instructions:       openai.String(instructions),
			PreviousResponseID: openai.String(resp.ID),
			Tools:              params.Tools,
			MaxOutputTokens:    params.MaxOutputTokens,
			ParallelToolCalls:  params.ParallelToolCalls,
		}
		switch {
		case len(outputs) > 0:
			next.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: outputs}
		case !nudged:
			nudged = true
			next.Input = responses.ResponseNewParamsInputUnion{
				OfString: openai.String("Submit your answer now by calling the tool for it."),
			}
		default:
			return errors.New("openai: the model finished without submitting a result")
		}
		params = next
	}
	return fmt.Errorf("openai: the call did not finish within %d rounds", maxIterations)
}

// toolParams are aikit's tools as the Responses API takes them. Not strict:
// OpenAI's strict mode needs every property required, and the amendment
// schema's optional fields are what let one shape carry every kind. Core
// validates every answer regardless.
func toolParams(tools []aikit.Tool) []responses.ToolUnionParam {
	out := make([]responses.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		p := responses.ToolParamOfFunction(t.Name, t.Schema(), false)
		p.OfFunction.Description = openai.String(t.Description)
		out = append(out, p)
	}
	return out
}

// RepairPlan reads a failed proposal and proposes amendments (R-106, R-336).
func (a *Adapter) RepairPlan(ctx context.Context, req api.ScreenRequest) (api.ScreenResult, error) {
	return a.screen(ctx, api.AIFunctionRepairPlan, req)
}

// AnswerQuestions answers detection's outstanding questions (R-338).
func (a *Adapter) AnswerQuestions(ctx context.Context, req api.ScreenRequest) (api.ScreenResult, error) {
	return a.screen(ctx, api.AIFunctionAnswerQuestions, req)
}

// RevisePlan acts on what a person reviewing the plan asked for (R-336).
func (a *Adapter) RevisePlan(ctx context.Context, req api.ScreenRequest) (api.ScreenResult, error) {
	return a.screen(ctx, api.AIFunctionRevisePlan, req)
}

// screen is the conversation every screening function has: read the
// repository through the budgeted reader, then submit findings.
func (a *Adapter) screen(ctx context.Context, fn api.AIFunction, req api.ScreenRequest) (api.ScreenResult, error) {
	if !a.ready {
		return api.ScreenResult{}, errors.New("openai: not configured")
	}
	if !a.screensPlans() {
		return api.ScreenResult{}, errors.New("openai: this adapter is set not to assist detection")
	}
	if req.Source == nil {
		return api.ScreenResult{}, errors.New("openai: no readable copy of the repository was supplied")
	}

	src := aikit.NewReader(req.Source, aikit.Limit(req.Budget.MaxFiles, a.cfg.MaxFiles), aikit.Limit(req.Budget.MaxBytes, a.cfg.MaxBytes))
	model := a.model(req.Model)
	var result api.ScreenResult
	err := a.converse(ctx, model, aikit.SystemPrompt(fn),
		aikit.UserPrompt(fn, req)+aikit.Preloaded(src, req.Known),
		aikit.ScreenTools(fn, req.Questions, req.Values),
		func(name, arguments string) (string, bool, error) {
			if name == aikit.ToolSubmitFindings {
				r, err := aikit.Findings(arguments, src, model)
				if err != nil {
					return "", false, fmt.Errorf("openai: %w", err)
				}
				result = r
				return "", false, errDone
			}
			text, isError := aikit.Call(name, arguments, src)
			return text, isError, nil
		})
	if err != nil {
		return api.ScreenResult{}, err
	}
	return result, nil
}

// submit runs one administrative task and decodes its answer into out. A
// task with a Lookup also has the lookup tools (O-54), run within
// aikit.MaxLookups lookups and maxIterations rounds.
func (a *Adapter) submit(ctx context.Context, model string, task aikit.Task, out any) error {
	if !a.ready {
		return errors.New("openai: not configured")
	}
	looker := aikit.NewLooker(task.Lookup)
	return a.converse(ctx, model, task.System+aikit.AnswerThroughTool, task.User, task.Tools(),
		func(name, arguments string) (string, bool, error) {
			if looker.Handles(name) {
				text, isError := looker.Call(ctx, name, arguments)
				return text, isError, nil
			}
			if name != task.Tool.Name {
				return "There is no tool by that name. Answer with " + task.Tool.Name + ".", true, nil
			}
			if err := json.Unmarshal([]byte(arguments), out); err != nil {
				return "", false, fmt.Errorf("openai: the answer could not be read: %w", err)
			}
			return "", false, errDone
		})
}

// DraftAccess drafts a role and, optionally, a group (R-343).
func (a *Adapter) DraftAccess(ctx context.Context, req api.AccessRequest) (api.AccessDraft, error) {
	var out api.AccessDraft
	model := a.model(req.Model)
	if err := a.submit(ctx, model, aikit.AccessTask(req), &out); err != nil {
		return api.AccessDraft{}, err
	}
	out.Model = model
	return out, nil
}

// DraftPolicy proposes changes to host policy (R-344).
func (a *Adapter) DraftPolicy(ctx context.Context, req api.PolicyRequest) (api.PolicyDraft, error) {
	var out api.PolicyDraft
	model := a.model(req.Model)
	if err := a.submit(ctx, model, aikit.PolicyTask(req), &out); err != nil {
		return api.PolicyDraft{}, err
	}
	out.Model = model
	return out, nil
}

// SearchAudit turns a question into one audit filter (R-345).
func (a *Adapter) SearchAudit(ctx context.Context, req api.AuditSearchRequest) (api.AuditSearch, error) {
	var out api.AuditSearch
	model := a.model(req.Model)
	if err := a.submit(ctx, model, aikit.AuditSearchTask(req), &out); err != nil {
		return api.AuditSearch{}, err
	}
	out.Model = model
	return out, nil
}

// SummarizeAudit summarizes the records core found (R-345).
func (a *Adapter) SummarizeAudit(ctx context.Context, req api.AuditSummaryRequest) (api.AuditSummary, error) {
	var out api.AuditSummary
	model := a.model(req.Model)
	if err := a.submit(ctx, model, aikit.AuditSummaryTask(req), &out); err != nil {
		return api.AuditSummary{}, err
	}
	out.Model = model
	return out, nil
}

// AnswerReference answers from the generated reference (R-346).
func (a *Adapter) AnswerReference(ctx context.Context, req api.ReferenceRequest) (api.ReferenceAnswer, error) {
	var out api.ReferenceAnswer
	model := a.model(req.Model)
	if err := a.submit(ctx, model, aikit.ReferenceTask(req), &out); err != nil {
		return api.ReferenceAnswer{}, err
	}
	out.Model = model
	return out, nil
}
