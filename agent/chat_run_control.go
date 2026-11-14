package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// ErrChatPersistence means inference may have happened but its transcript cannot
// be acknowledged. The dispatcher must require recovery acknowledgement.
var ErrChatPersistence = errors.New("chat transcript persistence is uncertain")

type ChatRunControl interface {
	TakeSteer(context.Context, string) (*chatstate.Admission, error)
	SteerDelivered(context.Context, string, string) error
	RecordBoundary(context.Context, string, string, string) error
}
type chatRunBinding struct {
	mu              sync.Mutex
	run             chatstate.Run
	control         ChatRunControl
	store           *transcript.Store
	logicalID       string
	path            string
	before          int
	pending         map[string]string
	fault           error
	cancel          context.CancelCauseFunc
	start           string
	contextSequence int64
}
type chatRunKey struct{}

func WithChatRun(ctx context.Context, run chatstate.Run, control ChatRunControl) context.Context {
	ctx, cancel := context.WithCancelCause(ctx)
	return context.WithValue(ctx, chatRunKey{}, &chatRunBinding{run: run, control: control, pending: map[string]string{}, cancel: cancel})
}
func chatRunFrom(ctx context.Context) *chatRunBinding {
	b, _ := ctx.Value(chatRunKey{}).(*chatRunBinding)
	return b
}
func failChatRun(ctx context.Context, err error) {
	if b := chatRunFrom(ctx); b != nil {
		b.mu.Lock()
		if b.fault == nil {
			b.fault = fmt.Errorf("%w: %v", ErrChatPersistence, err)
			b.cancel(b.fault)
		}
		b.mu.Unlock()
	}
}
func bindChatRunOptions(ctx context.Context, opts *agentcore.Options) error {
	b := chatRunFrom(ctx)
	if b == nil {
		return nil
	}
	b.store = opts.Transcript
	b.logicalID = opts.SessionID
	if b.run.TranscriptID != "" {
		opts.SessionID = b.run.TranscriptID
	}
	if b.store != nil {
		b.path = b.store.MainPath(opts.SessionID)
		records, err := b.store.Load(b.path)
		if err != nil {
			return err
		}
		b.before = len(records)
		if recorder, ok := b.control.(interface {
			RecordChatTranscriptBaseline(context.Context, string, string) error
		}); ok {
			last := ""
			if len(records) > 0 {
				last = records[len(records)-1].UUID
			}
			if err = recorder.RecordChatTranscriptBaseline(ctx, b.run.ID, last); err != nil {
				return err
			}
		}
	}
	opts.Hooks = wrapChatSteering(opts.Hooks, b.run, b.control)
	opts.Provider = chatRunProvider{inner: opts.Provider, binding: b}
	return nil
}

// beforeRequest runs after Norma records complete tool pairs, immediately before
// the next inference. Tool-result stream events arrive before that durable write.
func (b *chatRunBinding) beforeRequest(ctx context.Context) error {
	if b.store == nil {
		return nil
	}
	records, err := b.store.Load(b.path)
	if err != nil {
		return err
	}
	if len(records) < b.before {
		return ErrChatPersistence
	}
	file, e := os.OpenFile(b.path, os.O_RDWR, 0)
	if e != nil {
		return e
	}
	e = file.Sync()
	file.Close()
	if e != nil {
		return e
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fault != nil {
		return b.fault
	}
	if b.start == "" {
		// Prompt records its user message before compaction or background reminders.
		// Identify it by this run's persisted baseline, never by user text prefixes.
		for _, r := range records[b.before:] {
			if r.Message != nil && r.Message.Role == llm.RoleUser && !chatHasToolResult(*r.Message) {
				b.start = r.UUID
				break
			}
		}
		if b.start == "" {
			return ErrChatPersistence
		}
		if b.control != nil {
			if err = b.control.RecordBoundary(ctx, b.run.ID, b.start, ""); err != nil {
				return err
			}
		}
	}
	for id, marker := range b.pending {
		found := false
		for _, r := range records[b.before:] {
			if r.Message != nil {
				raw, _ := json.Marshal(r.Message.Content)
				if strings.Contains(string(raw), marker) {
					found = true
					break
				}
			}
		}
		if found {
			if b.control != nil {
				if err = b.control.SteerDelivered(ctx, b.run.ID, id); err != nil {
					return err
				}
			}
			delete(b.pending, id)
		}
	}
	return nil
}
func finishChatTranscript(ctx context.Context, messages []llm.Message) error {
	b := chatRunFrom(ctx)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	fault := b.fault
	b.mu.Unlock()
	if fault != nil {
		return fault
	}
	if b.store == nil {
		return nil
	}
	if err := b.beforeRequest(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrChatPersistence, err)
	}
	records, err := b.store.Load(b.path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrChatPersistence, err)
	}
	if len(records) <= b.before || len(messages) == 0 {
		return ErrChatPersistence
	}
	last := records[len(records)-1]
	expected := messages[len(messages)-1]
	if last.Message == nil || !reflect.DeepEqual(*last.Message, expected) {
		return ErrChatPersistence
	}
	if b.control != nil {
		if err = b.control.RecordBoundary(ctx, b.run.ID, b.start, last.UUID); err != nil {
			return fmt.Errorf("%w: %v", ErrChatPersistence, err)
		}
	}
	return nil
}
func stampChatActivity(ctx context.Context, a db.Activity) db.Activity {
	if b := chatRunFrom(ctx); b != nil {
		a.RunID = b.run.ID
		a.TurnID = b.run.TurnID
		active := true
		a.ActiveAttempt = &active
	}
	return a
}

type chatRunProvider struct {
	inner   llm.Provider
	binding *chatRunBinding
}

func (p chatRunProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if err := p.binding.beforeRequest(ctx); err != nil {
		failChatRun(ctx, err)
		return llm.Message{}, "", llm.Usage{}, err
	}
	return p.inner.Complete(transcript.WithSessionID(ctx, p.binding.logicalID), req)
}
func (p chatRunProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if err := p.binding.beforeRequest(ctx); err != nil {
			failChatRun(ctx, err)
			yield(llm.StreamEvent{}, err)
			return
		}
		for ev, err := range p.inner.Stream(transcript.WithSessionID(ctx, p.binding.logicalID), req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

func chatHasToolResult(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Type == llm.BlockToolResult {
			return true
		}
	}
	return false
}

func chatNoaSession(ctx context.Context, logical string) string {
	if b := chatRunFrom(ctx); b != nil && b.run.HistoryRevision > 0 {
		return fmt.Sprintf("%s-h%d", logical, b.run.HistoryRevision)
	}
	return logical
}
