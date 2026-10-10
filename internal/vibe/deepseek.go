package vibe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The built-in chat talks to DeepSeek only. The wire format is OpenAI's chat completions, so a
// second provider would be another URL and key, not another client.
const deepseekModel = "deepseek-chat"

// deepseekURL is a var only so tests can point it at a fake server.
var deepseekURL = "https://api.deepseek.com/chat/completions"

// Message is one chat completions message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a function call the model asked for.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type completionRequest struct {
	Model    string           `json:"model"`
	Messages []Message        `json:"messages"`
	Tools    []map[string]any `json:"tools,omitempty"`
	Stream   bool             `json:"stream"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int          `json:"index"`
				ID       string       `json:"id"`
				Function FunctionCall `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message Message `json:"message"`
	} `json:"choices"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
}

// toolDefinitions renders the registry as chat completions tools.
func toolDefinitions() []map[string]any {
	definitions := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		definitions = append(definitions, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.Schema()}})
	}
	return definitions
}

func (s *Service) post(ctx context.Context, key string, body completionRequest) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, deepseekURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("deepseek: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		var payload struct {
			Error apiError `json:"error"`
		}
		message := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &payload) == nil && payload.Error.Message != "" {
			message = payload.Error.Message
		}
		return nil, fmt.Errorf("deepseek answered %d: %s", response.StatusCode, message)
	}
	return response, nil
}

// stream sends one chat completion with the tools and streams the answer text to onDelta. It
// returns the whole assistant message, tool calls included.
func (s *Service) stream(ctx context.Context, key string, messages []Message, onDelta func(string)) (Message, error) {
	response, err := s.post(ctx, key, completionRequest{Model: deepseekModel, Messages: messages, Tools: toolDefinitions(), Stream: true})
	if err != nil {
		return Message{}, err
	}
	defer response.Body.Close()
	reply := Message{Role: "assistant"}
	var content strings.Builder
	calls := map[int]*ToolCall{}
	order := []int{}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return Message{}, fmt.Errorf("deepseek: unreadable stream chunk: %w", err)
		}
		if chunk.Error != nil {
			return Message{}, errors.New("deepseek: " + chunk.Error.Message)
		}
		for _, choice := range chunk.Choices {
			if text := choice.Delta.Content; text != "" {
				content.WriteString(text)
				onDelta(text)
			}
			for _, delta := range choice.Delta.ToolCalls {
				call, ok := calls[delta.Index]
				if !ok {
					call = &ToolCall{Type: "function"}
					calls[delta.Index] = call
					order = append(order, delta.Index)
				}
				if delta.ID != "" {
					call.ID = delta.ID
				}
				call.Function.Name += delta.Function.Name
				call.Function.Arguments += delta.Function.Arguments
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("deepseek: %w", err)
	}
	reply.Content = content.String()
	for _, index := range order {
		reply.ToolCalls = append(reply.ToolCalls, *calls[index])
	}
	return reply, nil
}

// complete sends one chat completion without tools and returns the answer text.
func (s *Service) complete(ctx context.Context, key string, messages []Message) (string, error) {
	response, err := s.post(ctx, key, completionRequest{Model: deepseekModel, Messages: messages})
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var chunk streamChunk
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&chunk); err != nil {
		return "", fmt.Errorf("deepseek: unreadable answer: %w", err)
	}
	if len(chunk.Choices) == 0 {
		return "", errors.New("deepseek: empty answer")
	}
	return strings.TrimSpace(chunk.Choices[0].Message.Content), nil
}
