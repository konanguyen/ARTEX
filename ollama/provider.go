package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Autumn-27/norma/llm"
)

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  map[string]any  `json:"parameters,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolCall struct {
	ID       string       `json:"id,omitempty"`
	Function toolFunction `json:"function"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type chatRequest struct {
	Model     string         `json:"model"`
	Messages  []chatMessage  `json:"messages"`
	Tools     []chatTool     `json:"tools,omitempty"`
	Stream    bool           `json:"stream"`
	KeepAlive string         `json:"keep_alive"`
	Options   map[string]any `json:"options"`
	Think     any            `json:"think,omitempty"`
	Format    any            `json:"format,omitempty"`
}

type chatResponse struct {
	Message               chatMessage `json:"message"`
	Done                  bool        `json:"done"`
	DoneReason            string      `json:"done_reason"`
	Error                 string      `json:"error"`
	PromptEvalCount       int         `json:"prompt_eval_count"`
	PromptEvalCachedCount int         `json:"prompt_eval_cached_count"`
	EvalCount             int         `json:"eval_count"`
	LoadDuration          int64       `json:"load_duration"`
	EvalDuration          int64       `json:"eval_duration"`
}

type Provider struct {
	client            *Client
	model             string
	opts              Options
	nextID            atomic.Uint64
	thinkingMu        sync.Mutex
	thinkingKnown     bool
	thinkingSupported bool
}

func NewProvider(client *Client, model string, opts Options) *Provider {
	if model == "" {
		model = DefaultModel
	}
	copyClient := *client
	copyClient.connectAttempts = opts.ConnectAttempts
	copyClient.connectInterval = opts.ConnectInterval
	return &Provider{client: &copyClient, model: model, opts: opts}
}

func (p *Provider) wire(ctx context.Context, req llm.CompletionRequest, stream bool) (chatRequest, error) {
	opts := p.opts
	if req.MaxTokens > 0 {
		opts.MaxTokens = req.MaxTokens
	}
	out := chatRequest{Model: p.model, Messages: []chatMessage{}, Stream: stream, KeepAlive: keepAlive(opts), Options: options(opts)}
	if req.Temperature != nil {
		out.Options["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		out.Options["stop"] = req.Stop
	}
	if opts.Schema != nil {
		out.Format = opts.Schema
	} else if opts.JSON {
		out.Format = "json"
	}
	thinking := opts.ThinkingType
	if req.Thinking != "" {
		thinking = req.Thinking
	}
	if thinking == "auto" {
		p.thinkingMu.Lock()
		if !p.thinkingKnown {
			info, err := p.client.Show(ctx, p.model)
			if err != nil {
				p.thinkingMu.Unlock()
				return out, err
			}
			p.thinkingSupported = info.Supports("thinking")
			p.thinkingKnown = true
		}
		if p.thinkingSupported {
			out.Think = true
		}
		p.thinkingMu.Unlock()
	}
	switch thinking {
	case "enabled", "true":
		out.Think = true
	case "disabled", "false":
		out.Think = false
	case "low", "medium", "high":
		out.Think = thinking
	}
	if len(req.System) > 0 {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: strings.Join(req.System, "\n\n")})
	}
	toolNames := map[string]string{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				toolNames[b.ID] = b.Name
			}
		}
	}
	for _, m := range req.Messages {
		msg := chatMessage{Role: string(m.Role)}
		flush := func() {
			if msg.Content != "" || msg.Thinking != "" || len(msg.ToolCalls) > 0 {
				out.Messages = append(out.Messages, msg)
				msg = chatMessage{Role: string(m.Role)}
			}
		}
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				msg.Content += b.Text
			case llm.BlockThinking:
				msg.Thinking += b.Thinking
			case llm.BlockToolUse:
				args, err := toolArguments(b.Name, b.Input)
				if err != nil {
					return out, err
				}
				msg.ToolCalls = append(msg.ToolCalls, toolCall{ID: b.ID, Function: toolFunction{Name: b.Name, Arguments: args}})
			case llm.BlockToolResult:
				flush()
				var parts []string
				for _, content := range b.Content {
					if content.Type != llm.BlockText {
						return out, fmt.Errorf("ollama: unsupported message block type %q", content.Type)
					}
					parts = append(parts, content.Text)
				}
				result := strings.Join(parts, "\n")
				if b.IsError {
					result = "[tool error] " + result
				}
				out.Messages = append(out.Messages, chatMessage{Role: "tool", Content: result, ToolName: toolNames[b.ToolUseID], ToolCallID: b.ToolUseID})
			default:
				return out, fmt.Errorf("ollama: unsupported message block type %q", b.Type)
			}
		}
		flush()
	}
	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, chatTool{Type: "function", Function: toolFunction{Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema}})
	}
	return out, nil
}

// Native servers may encode an argument object as a JSON string.
func toolArguments(name string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, fmt.Errorf("ollama: invalid arguments for tool %s", name)
		}
		raw = bytes.TrimSpace([]byte(encoded))
	}
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		return nil, fmt.Errorf("ollama: invalid arguments for tool %s", name)
	}
	return raw, nil
}

func (p *Provider) warnNearContextLimit(resp chatResponse) {
	window := contextWindow(p.opts)
	if resp.PromptEvalCount > 0 && resp.PromptEvalCount >= window-64 {
		log.Printf("[ollama] Native Ollama prompt token count is near the configured context limit (model %s, prompt_eval_count %d, num_ctx %d); this heuristic does not prove truncation", p.model, resp.PromptEvalCount, window)
	}
}

func (p *Provider) id(id string) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("ollama-call-%d", p.nextID.Add(1))
}

func usage(resp chatResponse) llm.Usage {
	return llm.Usage{InputTokens: resp.PromptEvalCount, OutputTokens: resp.EvalCount, CacheReadTokens: resp.PromptEvalCachedCount}
}

func stopReason(resp chatResponse, hasTools bool) string {
	if hasTools {
		return "tool_use"
	}
	if resp.DoneReason == "length" {
		return "max_tokens"
	}
	return "end_turn"
}

func (p *Provider) completeOnce(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	msg := llm.Message{Role: llm.RoleAssistant}
	wire, err := p.wire(ctx, req, false)
	if err != nil {
		return msg, "", llm.Usage{}, err
	}
	var resp chatResponse
	if err := p.client.decode(ctx, "POST", "/api/chat", wire, &resp); err != nil {
		return msg, "", llm.Usage{}, err
	}
	if resp.Error != "" {
		return msg, "", llm.Usage{}, fmt.Errorf("ollama: %s", resp.Error)
	}
	if !resp.Done {
		return msg, "", llm.Usage{}, fmt.Errorf("ollama: incomplete response (missing done)")
	}
	p.warnNearContextLimit(resp)
	if resp.Message.Thinking != "" {
		msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockThinking, Thinking: resp.Message.Thinking})
	}
	if resp.Message.Content != "" {
		msg.Content = append(msg.Content, llm.TextBlock(resp.Message.Content))
	}
	for _, call := range resp.Message.ToolCalls {
		args, err := toolArguments(call.Function.Name, call.Function.Arguments)
		if err != nil {
			return msg, "", usage(resp), err
		}
		msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockToolUse, ID: p.id(call.ID), Name: call.Function.Name, Input: args})
	}
	if len(msg.Content) == 0 {
		return msg, "", usage(resp), errEmpty
	}
	return msg, stopReason(resp, len(resp.Message.ToolCalls) > 0), usage(resp), nil
}

func (p *Provider) streamOnce(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		wire, err := p.wire(ctx, req, true)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		resp, err := p.client.request(ctx, "POST", "/api/chat", wire)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		defer resp.Body.Close()
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart}, nil) {
			return
		}
		decoder := json.NewDecoder(resp.Body)
		hasTools := false
		hasContent := false
		for {
			var chunk chatResponse
			if err := decoder.Decode(&chunk); err != nil {
				if err == io.EOF {
					err = fmt.Errorf("ollama: incomplete stream (missing done)")
				} else {
					err = fmt.Errorf("ollama: invalid stream response: %w", err)
				}
				yield(llm.StreamEvent{}, err)
				return
			}
			if chunk.Error != "" {
				yield(llm.StreamEvent{}, fmt.Errorf("ollama: %s", chunk.Error))
				return
			}
			if chunk.Message.Thinking != "" || chunk.Message.Content != "" || len(chunk.Message.ToolCalls) > 0 {
				hasContent = true
			}
			if chunk.Message.Thinking != "" && !yield(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: chunk.Message.Thinking}, nil) {
				return
			}
			if chunk.Message.Content != "" && !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: chunk.Message.Content}, nil) {
				return
			}
			for _, call := range chunk.Message.ToolCalls {
				args, err := toolArguments(call.Function.Name, call.Function.Arguments)
				if err != nil {
					yield(llm.StreamEvent{}, err)
					return
				}
				hasTools = true
				id := p.id(call.ID)
				if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: id, ToolName: call.Function.Name}, nil) {
					return
				}
				if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, ToolID: id, Text: string(args)}, nil) {
					return
				}
			}
			if chunk.Done {
				p.warnNearContextLimit(chunk)
				if !hasContent {
					yield(llm.StreamEvent{}, errEmpty)
					return
				}
				if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stopReason(chunk, hasTools), Usage: usage(chunk)}, nil) {
					return
				}
				yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
				return
			}
		}
	}
}

var errEmpty = errors.New("ollama: model returned no content")

func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	for attempt := 0; ; attempt++ {
		msg, stop, usage, err := p.completeOnce(ctx, req)
		if !errors.Is(err, errEmpty) || attempt >= retries(p.opts.EmptyAttempts, 2) {
			return msg, stop, usage, err
		}
		if err := retryWait(ctx, attempt, p.opts.EmptyInterval); err != nil {
			return msg, "", usage, err
		}
	}
}

func (p *Provider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for attempt := 0; ; attempt++ {
			var empty bool
			var start *llm.StreamEvent
			consumed := false
			stopped := false
			for event, err := range p.streamOnce(ctx, req) {
				if event.Type == llm.SEMessageStart {
					copyEvent := event
					start = &copyEvent
					continue
				}
				if errors.Is(err, errEmpty) && !consumed && attempt < retries(p.opts.EmptyAttempts, 2) {
					empty = true
					break
				}
				if start != nil {
					if !yield(*start, nil) {
						stopped = true
						break
					}
					start = nil
				}
				consumed = true
				if !yield(event, err) {
					stopped = true
					break
				}
			}
			if stopped || !empty {
				return
			}
			if err := retryWait(ctx, attempt, p.opts.EmptyInterval); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
		}
	}
}
