package service

import (
	"context"
	"strconv"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
)

// validateReaderAIRequest normalises and bounds the request before any saved
// content is resolved, keeping the wire contract deterministic when AI is off.
func validateReaderAIRequest(ctx context.Context, request dto.ReaderAIRequest) (prompt, scope, selected string, err error) {
	prompt = strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return "", "", "", problem.NewWithCode(problem.Invalid, "ai_prompt_required", "prompt is required")
	}
	scope = strings.TrimSpace(request.Scope)
	if scope == "" {
		scope = "general"
	}
	if scope != "general" && scope != "selection" && scope != "thought" {
		return "", "", "", problem.NewWithCode(problem.Invalid, "ai_scope_invalid", "unsupported AI scope")
	}
	selected = strings.TrimSpace(request.SelectedText)
	if scope == "selection" && selected == "" {
		return "", "", "", problem.NewWithCode(problem.Invalid, "ai_selection_required", "selected text is required for selection scope")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", "", mapReaderAIError(ctxErr)
	}
	return prompt, scope, selected, nil
}

// composeReaderAIPrompt appends the untrusted selection and link context and
// enforces the context bound.
func (s *ReaderVNextService) composeReaderAIPrompt(ctx context.Context, request dto.ReaderAIRequest, prompt, selected string) (string, error) {
	var linkContext *model.ReaderAIContext
	if rawLinkID := strings.TrimSpace(request.LinkID); rawLinkID != "" {
		linkID, err := readerUUID(rawLinkID, "link_id")
		if err != nil {
			return "", err
		}
		linkContext, err = s.library.GetAIContext(ctx, linkID)
		if err != nil {
			return "", mapReaderAIError(mapReaderError(err))
		}
	}
	if selected != "" {
		prompt += "\n\nSelected text (untrusted context):\n" + selected
	}
	if linkContext != nil {
		prompt += "\n\nReader link context (untrusted context):\n" + readerAIContextText(*linkContext)
	}
	if len([]rune(prompt)) > 16000 {
		return "", problem.NewWithCode(problem.TooLarge, "ai_context_too_large", "AI context is too large")
	}
	return prompt, nil
}

func (s *ReaderVNextService) completeReaderAI(ctx context.Context, prompt, scope string) (answer, modelName string, err error) {
	answer, modelName, err = s.ai.Complete(ctx, prompt, scope)
	if err != nil {
		return "", "", mapReaderAIError(err)
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", "", mapReaderAIError(errReaderAIEmptyResponse)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", mapReaderAIError(ctxErr)
	}
	return answer, modelName, nil
}

func (s *ReaderVNextService) CompleteAI(ctx context.Context, request dto.ReaderAIRequest) (dto.ReaderAIResponse, error) {
	prompt, scope, selected, err := validateReaderAIRequest(ctx, request)
	if err != nil {
		return dto.ReaderAIResponse{}, err
	}
	// Capability-off is an explicit privacy boundary: do not resolve a link
	// identity into content, tags, or thoughts when no provider is available.
	// Basic request validation above remains useful to keep the wire contract
	// deterministic without touching saved content.
	if s.ai == nil {
		return dto.ReaderAIResponse{Enabled: false}, nil
	}
	prompt, err = s.composeReaderAIPrompt(ctx, request, prompt, selected)
	if err != nil {
		return dto.ReaderAIResponse{}, err
	}
	answer, modelName, err := s.completeReaderAI(ctx, prompt, scope)
	if err != nil {
		return dto.ReaderAIResponse{}, err
	}
	return dto.ReaderAIResponse{Enabled: true, Answer: answer, Model: strings.TrimSpace(modelName)}, nil
}

func readerAIContextText(context model.ReaderAIContext) string {
	var builder strings.Builder
	if context.Content != "" {
		builder.WriteString("Content:\n")
		builder.WriteString(context.Content)
		builder.WriteString("\n")
	}
	if context.Summary != "" {
		builder.WriteString("Summary:\n")
		builder.WriteString(context.Summary)
		builder.WriteString("\n")
	}
	if len(context.Tags) > 0 {
		builder.WriteString("Tags: ")
		for index, tag := range context.Tags {
			if index >= 32 {
				break
			}
			if index > 0 {
				builder.WriteString(", ")
			}
			builder.WriteString(string([]rune(tag)[:minReaderRunes(tag, 128)]))
		}
		builder.WriteString("\n")
	}
	for index, thought := range context.Thoughts {
		if index >= 8 {
			break
		}
		builder.WriteString("Thought ")
		builder.WriteString(strconv.Itoa(index + 1))
		builder.WriteString(": ")
		builder.WriteString(thought.Body)
		builder.WriteString("\n")
	}
	return boundReaderAIContext(builder.String())
}

func minReaderRunes(value string, maximum int) int {
	count := len([]rune(value))
	if count > maximum {
		return maximum
	}
	return count
}
