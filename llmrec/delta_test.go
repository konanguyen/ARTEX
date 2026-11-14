package llmrec

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// S8/S19: only usage changes; zero-usage commit events remain at their positions.
func TestForwardedUsageIsDelta(t *testing.T) {
	p := Wrap(cumulativeUsageProvider{}, nil, "fixture", "", "", "", func() bool { return false })
	for range 2 {
		a := llm.NewAccumulator()
		var types []llm.StreamEventType
		for ev, err := range p.Stream(t.Context(), llm.CompletionRequest{}) {
			if err != nil {
				t.Fatal(err)
			}
			types = append(types, ev.Type)
			a.Add(ev)
		}
		want := llm.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60, CacheWriteTokens: 10}
		if a.Usage != want {
			t.Errorf("accumulator=%+v want %+v", a.Usage, want)
		}
		if len(types) != 4 || types[0] != llm.SEMessageStart || types[1] != llm.SEMessageDelta || types[2] != llm.SEMessageDelta || types[3] != llm.SEMessageStop {
			t.Errorf("events=%v", types)
		}
	}
}

type failedEventsProvider struct {
	completeProvider
	events []llm.StreamEvent
	err    error
}

func (p failedEventsProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		for _, ev := range p.events {
			if !y(ev, nil) {
				return
			}
		}
		y(llm.StreamEvent{}, p.err)
	}
}

// S19: the server commits even zero-usage deltas/stops. Preserve those events
// and the pre-event failure window exactly while converting only usage values.
func TestForwardingKeepsCommitEvents(t *testing.T) {
	failure := errors.New("fixture failure")
	for _, kind := range []llm.StreamEventType{"", llm.SEMessageStart, llm.SEMessageDelta, llm.SEMessageStop} {
		t.Run(string(kind), func(t *testing.T) {
			var events []llm.StreamEvent
			if kind != "" {
				events = append(events, llm.StreamEvent{Type: kind})
			}
			p := Wrap(failedEventsProvider{events: events, err: failure}, nil, "fixture", "", "", "", func() bool { return false })
			var got []llm.StreamEvent
			var err error
			for ev, e := range p.Stream(t.Context(), llm.CompletionRequest{}) {
				if e != nil {
					err = e
				} else {
					got = append(got, ev)
				}
			}
			if !reflect.DeepEqual(got, events) || err != failure {
				t.Fatalf("events=%v err=%v want %v / %v", got, err, events, failure)
			}
		})
	}
}

func TestPartialCumulativeForwarding(t *testing.T) {
	failure := errors.New("fixture stream interrupted")
	p := Wrap(failedEventsProvider{events: []llm.StreamEvent{{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 100, OutputTokens: 1}}, {Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 10}}, {Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 20}}}, err: failure}, nil, "fixture", "", "", "", func() bool { return false })
	a := llm.NewAccumulator()
	var err error
	for ev, e := range p.Stream(t.Context(), llm.CompletionRequest{}) {
		if e != nil {
			err = e
		} else {
			a.Add(ev)
		}
	}
	if err != failure || a.Usage.InputTokens != 100 || a.Usage.OutputTokens != 20 {
		t.Fatalf("partial usage=%+v error=%v", a.Usage, err)
	}
}
