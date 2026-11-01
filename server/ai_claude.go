package server

import (
	"context"
	"errors"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

var anthropicAPI = "" // the SDK's default; tests point it at a fake

const claudeModel = "claude-opus-5-5"

// claude asks through the Messages API with a JSON schema output format.
type claude struct {
	client anthropic.Client
	model  string
}

func newClaude(key, model string) *claude {
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if anthropicAPI != "" {
		opts = append(opts, option.WithBaseURL(anthropicAPI))
	}
	return &claude{client: anthropic.NewClient(opts...), model: or(model, claudeModel)}
}

func (c *claude) name() string { return "Claude" }

func (c *claude) ask(ctx context.Context, system, prompt string, schema map[string]any) (string, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     c.model,
		MaxTokens: 32000,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt))},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffortMedium,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: schema},
		},
	}
	// a declined request is answered by Anthropic's recommended fallback
	// model instead (the models that take the "default" form)
	switch c.model {
	case "claude-opus-5-5", "claude-opus-5", "claude-fable-5-1", "claude-sonnet-5-5":
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
	}
	// streamed: with thinking on, a long answer can outlast a plain request
	stream := c.client.Beta.Messages.NewStreaming(ctx, params)
	var msg anthropic.BetaMessage
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return "", err
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return "", errors.New("declined the request")
	case anthropic.BetaStopReasonMaxTokens:
		return "", errors.New("the answer was cut off")
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if tb, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(tb.Text)
		}
	}
	return text.String(), nil
}
