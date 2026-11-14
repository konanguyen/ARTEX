// Package sidequestion captures immutable main-agent checkpoints. It never owns
// an agent session or a tool executor.
package sidequestion

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

type Parent struct {
	MainSeg        *int  `json:"main_seg,omitempty"`
	ConversationID int64 `json:"conversation_id,omitempty"`
	TaskID         int64 `json:"task_id,omitempty"`
	ExplorationID  int64 `json:"exploration_id,omitempty"`
	IntentID       int64 `json:"intent_id,omitempty"`
}

func (p Parent) Key() string {
	if p.ConversationID > 0 {
		return fmt.Sprintf("conv-%d", p.ConversationID)
	}
	if p.IntentID > 0 {
		return fmt.Sprintf("task-%d-exp-%d-worker-i%d", p.TaskID, p.ExplorationID, p.IntentID)
	}
	if p.MainSeg != nil && *p.MainSeg > 0 {
		return fmt.Sprintf("task-%d-exp-%d-main-s%d", p.TaskID, p.ExplorationID, *p.MainSeg)
	}
	return fmt.Sprintf("task-%d-exp-%d-main", p.TaskID, p.ExplorationID)
}

// Model contains only configuration identity, never credentials or proxy URLs.
type Model struct {
	ProfileID    int64  `json:"profile_id"`
	Name         string `json:"name"`
	Format       string `json:"format"`
	Model        string `json:"model"`
	Identity     string `json:"identity"`
	Streaming    bool   `json:"streaming"`
	WindowTokens int    `json:"window_tokens"`
}

type Snapshot struct {
	HistoryRevision int64                 `json:"history_revision,string"`
	SourceRunID     string                `json:"source_run_id,omitempty"`
	Parent          Parent                `json:"parent"`
	RunID           int64                 `json:"run_id"`
	Version         int64                 `json:"version"`
	CapturedAt      time.Time             `json:"captured_at"`
	Model           Model                 `json:"model"`
	Request         llm.CompletionRequest `json:"request"`
}

func CloneRequest(req llm.CompletionRequest) (llm.CompletionRequest, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return llm.CompletionRequest{}, err
	}
	var out llm.CompletionRequest
	err = json.Unmarshal(b, &out)
	return out, err
}

type Publisher func(Snapshot)

// RequestEvent observes only main requests selected by QueryDeps. Usage belongs
// to AnchorRequest; Request may additionally include newly generated messages.
type RequestEvent struct {
	Request       llm.CompletionRequest
	AnchorRequest llm.CompletionRequest
	Model         Model
	Usage         llm.Usage
	Source        string
}
type RequestObserver func(context.Context, RequestEvent)
type observerKey struct{}

func WithRequestObserver(ctx context.Context, observer RequestObserver) context.Context {
	old, _ := ctx.Value(observerKey{}).([]RequestObserver)
	next := append(append([]RequestObserver{}, old...), observer)
	return context.WithValue(ctx, observerKey{}, next)
}

type workingSetProjector func(context.Context, []llm.Message) []llm.Message
type projectorKey struct{}

func WithWorkingSetProjector(ctx context.Context, project func(context.Context, []llm.Message) []llm.Message) context.Context {
	return context.WithValue(ctx, projectorKey{}, workingSetProjector(project))
}

type historyKey struct{}
type historyStamp struct {
	revision int64
	runID    string
}

func WithHistory(ctx context.Context, revision int64, runID string) context.Context {
	return context.WithValue(ctx, historyKey{}, historyStamp{revision, runID})
}

type publisherKey struct{}
type captureKey struct{}
type attemptKey struct{}

func WithPublisher(ctx context.Context, publish Publisher) context.Context {
	return context.WithValue(ctx, publisherKey{}, publish)
}

type Capture struct {
	mu        sync.Mutex
	parent    Parent
	runID     int64
	version   int64
	publish   Publisher
	last      *Snapshot
	project   workingSetProjector
	observers []RequestObserver
	ctx       context.Context
}

// Attach uses QueryDeps rather than a global provider hook so compaction and
// other auxiliary completions cannot replace the main conversation checkpoint.
func Attach(ctx context.Context, parent Parent, deps harness.QueryDeps, provider llm.Provider) (context.Context, harness.QueryDeps) {
	publish, _ := ctx.Value(publisherKey{}).(Publisher)
	observers, _ := ctx.Value(observerKey{}).([]RequestObserver)
	if publish == nil && len(observers) == 0 {
		return ctx, deps
	}
	project, _ := ctx.Value(projectorKey{}).(workingSetProjector)
	c := &Capture{project: project, parent: parent, runID: time.Now().UnixNano(), publish: publish, observers: observers, ctx: ctx}
	stream, complete := deps.CallModel, deps.CallModelSync
	if stream == nil {
		stream = provider.Stream
	}
	if complete == nil {
		complete = provider.Complete
	}
	deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return stream(context.WithValue(ctx, attemptKey{}, c), req)
	}
	deps.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return complete(context.WithValue(ctx, attemptKey{}, c), req)
	}
	return context.WithValue(ctx, captureKey{}, c), deps
}

func (c *Capture) save(req llm.CompletionRequest, model Model) {
	copy, err := CloneRequest(req)
	if err != nil {
		return
	} // Observability must not break the main run.
	c.mu.Lock()
	c.version++
	stamp, _ := c.ctx.Value(historyKey{}).(historyStamp)
	s := Snapshot{HistoryRevision: stamp.revision, SourceRunID: stamp.runID, Parent: c.parent, RunID: c.runID, Version: c.version, CapturedAt: time.Now().UTC(), Model: model, Request: copy}
	c.last = &s
	c.mu.Unlock()
	if c.publish != nil {
		c.publish(s)
	}
}

func (c *Capture) observe(req, anchor llm.CompletionRequest, model Model, usage llm.Usage, source string) {
	for _, observe := range c.observers {
		observe(c.ctx, RequestEvent{Request: req, AnchorRequest: anchor, Model: model, Usage: usage, Source: source})
	}
}

// Finish adds tool results that landed after the final model request. Norma's
// first API message is its host reminder, absent from Terminal.Messages.
func Finish(ctx context.Context, messages []llm.Message) {
	c, _ := ctx.Value(captureKey{}).(*Capture)
	if c == nil {
		return
	}
	c.mu.Lock()
	s := c.last
	c.mu.Unlock()
	if s == nil {
		return
	}
	req := s.Request
	var prefix []llm.Message
	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Text(), "<system-reminder>") {
		prefix = append(prefix, req.Messages[0])
	}
	working := llm.MessagesForAPI(messages)
	if c.project != nil {
		working = c.project(ctx, messages)
	}
	req.Messages = append(prefix, working...)
	c.save(req, s.Model)
	c.observe(req, llm.CompletionRequest{}, s.Model, llm.Usage{}, "working_set")
}

type boundProvider struct {
	inner llm.Provider
	model Model
}

// Bind belongs immediately around a concrete provider, inside any routing pool.
func Bind(inner llm.Provider, model Model) llm.Provider { return &boundProvider{inner, model} }

func (p *boundProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		c, _ := ctx.Value(attemptKey{}).(*Capture)
		if c == nil {
			for ev, err := range p.inner.Stream(ctx, req) {
				if !yield(ev, err) {
					return
				}
			}
			return
		}
		c.save(req, p.model)
		c.observe(req, req, p.model, llm.Usage{}, "request")
		anchor := req
		acc := llm.NewAccumulator()
		failed := false
		complete := false
		for ev, err := range p.inner.Stream(ctx, req) {
			if err != nil {
				failed = true
			} else {
				acc.Add(ev)
				if ev.Type == llm.SEMessageStop {
					complete = true
				}
			}
			if !yield(ev, err) {
				return
			}
		}
		if !failed && complete && ctx.Err() == nil {
			msg := acc.Message()
			req.Messages = llm.MessagesForAPI(append(append([]llm.Message{}, req.Messages...), msg))
			c.save(req, p.model)
		}
		c.observe(req, anchor, p.model, acc.Usage, "response")
	}
}

func (p *boundProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	c, _ := ctx.Value(attemptKey{}).(*Capture)
	if c != nil {
		c.save(req, p.model)
		c.observe(req, req, p.model, llm.Usage{}, "request")
	}
	anchor := req
	msg, stop, usage, err := p.inner.Complete(ctx, req)
	if c != nil && err == nil && ctx.Err() == nil {
		req.Messages = llm.MessagesForAPI(append(append([]llm.Message{}, req.Messages...), msg))
		c.save(req, p.model)
	}
	if c != nil {
		c.observe(req, anchor, p.model, usage, "response")
	}
	return msg, stop, usage, err
}
