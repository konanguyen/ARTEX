package server

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// fakeProvider records the peak number of concurrent Stream calls and holds each
// call open for hold, so a test can observe whether an admission semaphore bounds
// the overlap.
type fakeProvider struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	hold     time.Duration
}

func (f *fakeProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		f.mu.Lock()
		f.inFlight++
		if f.inFlight > f.peak {
			f.peak = f.inFlight
		}
		f.mu.Unlock()
		select {
		case <-time.After(f.hold):
		case <-ctx.Done():
		}
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "ok"}, nil)
	}
}

func (f *fakeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	for range f.Stream(ctx, req) {
	}
	return llm.Message{Role: "assistant"}, "stop", llm.Usage{}, nil
}

func (f *fakeProvider) Peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// TestConcurrencyProviderBoundsInFlight verifies the semaphore never lets more
// than cap requests overlap, regardless of how many goroutines stream at once.
func TestConcurrencyProviderBoundsInFlight(t *testing.T) {
	const capN = 2
	fake := &fakeProvider{hold: 50 * time.Millisecond}
	p := &concurrencyProvider{inner: fake, sem: make(chan struct{}, capN)}

	var wg sync.WaitGroup
	var got atomic.Int64
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range p.Stream(context.Background(), llm.CompletionRequest{}) {
				got.Add(1)
			}
		}()
	}
	wg.Wait()

	if fake.Peak() > capN {
		t.Errorf("peak concurrency %d exceeded cap %d", fake.Peak(), capN)
	}
	if got.Load() != 10 {
		t.Errorf("expected 10 completed streams, got %d", got.Load())
	}
}

// TestConcurrencyProviderUnlimitedWhenZeroCap verifies cap 0 passes through
// without bounding (the unlimited default).
func TestConcurrencyProviderUnlimitedWhenZeroCap(t *testing.T) {
	fake := &fakeProvider{hold: 30 * time.Millisecond}
	p := &concurrencyProvider{inner: fake, sem: make(chan struct{}, 0)} // unlimited

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range p.Stream(context.Background(), llm.CompletionRequest{}) {
			}
		}()
	}
	wg.Wait()
	if fake.Peak() < 2 {
		t.Errorf("expected overlapping streams with cap 0, peak was %d", fake.Peak())
	}
}

// TestConcurrencyProviderHonorsContextCancel verifies a waiter blocked on a full
// semaphore returns ctx.Err() instead of hanging.
func TestConcurrencyProviderHonorsContextCancel(t *testing.T) {
	fake := &fakeProvider{hold: 2 * time.Second}
	p := &concurrencyProvider{inner: fake, sem: make(chan struct{}, 1)}

	// Occupy the only slot deterministically: acquire directly, then release only
	// when the test signals, so the waiter is guaranteed to contend.
	p.sem <- struct{}{}
	t.Cleanup(func() { <-p.sem })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	errCh := make(chan error, 1)
	go func() {
		var streamErr error
		for _, err := range p.Stream(ctx, llm.CompletionRequest{}) {
			if err != nil {
				streamErr = err
			}
		}
		errCh <- streamErr
	}()
	err := <-errCh
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context deadline exceeded, got %v", err)
	}
	if time.Since(start) > 1*time.Second {
		t.Errorf("cancel took too long: %v", time.Since(start))
	}
}

// TestLLMMaxConcurrentRoundtrip verifies the settings accessors persist and read back
// the cap, including the unlimited (0) default and invalid-value fallback.
func TestLLMMaxConcurrentRoundtrip(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("no database available: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	prev := m.LLMMaxConcurrent()
	t.Cleanup(func() { _ = m.SetLLMMaxConcurrent(prev) })

	// Reset to unlimited first so the test is independent of prior DB state.
	if err := m.SetLLMMaxConcurrent(0); err != nil {
		t.Fatalf("SetLLMMaxConcurrent(0) reset: %v", err)
	}
	if got := m.LLMMaxConcurrent(); got != 0 {
		t.Errorf("LLMMaxConcurrent after reset = %d, want 0", got)
	}

	// Persist and read back a real cap.
	if err := m.SetLLMMaxConcurrent(3); err != nil {
		t.Fatalf("SetLLMMaxConcurrent(3): %v", err)
	}
	if got := m.LLMMaxConcurrent(); got != 3 {
		t.Errorf("LLMMaxConcurrent after set = %d, want 3", got)
	}

	// Back to unlimited.
	if err := m.SetLLMMaxConcurrent(0); err != nil {
		t.Fatalf("SetLLMMaxConcurrent(0): %v", err)
	}
	if got := m.LLMMaxConcurrent(); got != 0 {
		t.Errorf("LLMMaxConcurrent after reset = %d, want 0", got)
	}

	// Negative is rejected.
	if err := m.SetLLMMaxConcurrent(-1); err == nil {
		t.Error("SetLLMMaxConcurrent(-1) = nil, want error")
	}
}

// TestNewConcurrencyProviderPassthrough verifies newConcurrencyProvider returns the
// inner provider unchanged when the cap is 0 (unlimited), and wraps it when > 0.
func TestNewConcurrencyProviderPassthrough(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("no database available: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	prev := m.LLMMaxConcurrent()
	t.Cleanup(func() { _ = m.SetLLMMaxConcurrent(prev) })

	fake := &fakeProvider{hold: time.Millisecond}

	// Unlimited: same instance, no wrapper.
	if err := m.SetLLMMaxConcurrent(0); err != nil {
		t.Fatalf("SetLLMMaxConcurrent(0): %v", err)
	}
	if got := newConcurrencyProvider(fake, m); got != llm.Provider(fake) {
		t.Errorf("cap 0: newConcurrencyProvider returned a wrapper, want pass-through")
	}

	// Capped: wrapped in concurrencyProvider.
	if err := m.SetLLMMaxConcurrent(2); err != nil {
		t.Fatalf("SetLLMMaxConcurrent(2): %v", err)
	}
	got := newConcurrencyProvider(fake, m)
	if _, ok := got.(*concurrencyProvider); !ok {
		t.Errorf("cap 2: newConcurrencyProvider returned %T, want *concurrencyProvider", got)
	}
}
