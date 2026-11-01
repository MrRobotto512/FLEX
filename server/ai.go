package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// asker is one AI provider: it sends system and prompt and returns the
// answer, JSON matching schema.
type asker interface {
	name() string
	ask(ctx context.Context, system, prompt string, schema map[string]any) (string, error)
}

// newAsker picks the provider from the config; nil without a key.
func newAsker(cfg Config) (asker, error) {
	if cfg.AIKey == "" {
		return nil, nil
	}
	switch strings.ToLower(cfg.AIProvider) {
	case "", "claude", "anthropic":
		return newClaude(cfg.AIKey, cfg.AIModel), nil
	case "openai", "chatgpt":
		return &openAI{key: cfg.AIKey, model: or(cfg.AIModel, openaiModel)}, nil
	case "gemini", "google":
		return &gemini{key: cfg.AIKey, model: or(cfg.AIModel, geminiModel)}, nil
	}
	return nil, fmt.Errorf("unknown AI provider %q (claude, openai or gemini)", cfg.AIProvider)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

var aiHTTP = &http.Client{Timeout: 10 * time.Minute}

// postJSON sends body as JSON and decodes the answer into out; an error
// answer becomes an error carrying the provider's message.
func postJSON(ctx context.Context, url string, header map[string]string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := aiHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error.Message)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

// ---------------------------------------------------------------- ChatGPT

var openaiAPI = "https://api.openai.com/v1" // var so tests can point it at a fake

const openaiModel = "gpt-6-astra"

// openAI asks through the Responses API with a strict JSON schema.
type openAI struct{ key, model string }

func (o *openAI) name() string { return "ChatGPT" }

func (o *openAI) ask(ctx context.Context, system, prompt string, schema map[string]any) (string, error) {
	body := map[string]any{
		"model":        o.model,
		"instructions": system,
		"input":        prompt,
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "home_rows", "strict": true, "schema": schema,
		}},
	}
	var res struct {
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := postJSON(ctx, openaiAPI+"/responses", map[string]string{"Authorization": "Bearer " + o.key}, body, &res); err != nil {
		return "", err
	}
	if res.Status == "incomplete" {
		return "", fmt.Errorf("the answer was cut off (%s)", res.IncompleteDetails.Reason)
	}
	var text strings.Builder
	for _, item := range res.Output {
		for _, c := range item.Content {
			switch c.Type {
			case "output_text":
				text.WriteString(c.Text)
			case "refusal":
				return "", errors.New("declined the request: " + c.Refusal)
			}
		}
	}
	return text.String(), nil
}

// ---------------------------------------------------------------- Gemini

var geminiAPI = "https://generativelanguage.googleapis.com/v1beta"

const geminiModel = "gemini-3.8-flash"

// gemini asks through generateContent with a response schema.
type gemini struct{ key, model string }

func (g *gemini) name() string { return "Gemini" }

func (g *gemini) ask(ctx context.Context, system, prompt string, schema map[string]any) (string, error) {
	text := func(s string) map[string]any { return map[string]any{"parts": []any{map[string]any{"text": s}}} }
	user := text(prompt)
	user["role"] = "user"
	body := map[string]any{
		"systemInstruction": text(system),
		"contents":          []any{user},
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   geminiSchema(schema),
		},
	}
	var res struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	u := geminiAPI + "/models/" + url.PathEscape(g.model) + ":generateContent"
	if err := postJSON(ctx, u, map[string]string{"x-goog-api-key": g.key}, body, &res); err != nil {
		return "", err
	}
	if r := res.PromptFeedback.BlockReason; r != "" {
		return "", errors.New("declined the request: " + r)
	}
	if len(res.Candidates) == 0 {
		return "", errors.New("no answer")
	}
	c := res.Candidates[0]
	switch c.FinishReason {
	case "", "STOP":
	case "MAX_TOKENS":
		return "", errors.New("the answer was cut off")
	default:
		return "", errors.New("stopped: " + c.FinishReason)
	}
	var out strings.Builder
	for _, p := range c.Content.Parts {
		if !p.Thought {
			out.WriteString(p.Text)
		}
	}
	return out.String(), nil
}

// geminiSchema turns a JSON schema into Gemini's responseSchema dialect:
// upper-case types, no additionalProperties.
func geminiSchema(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			switch k {
			case "additionalProperties":
			case "type":
				if s, ok := val.(string); ok {
					out[k] = strings.ToUpper(s)
				}
			default:
				out[k] = geminiSchema(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = geminiSchema(e)
		}
		return out
	}
	return v
}
