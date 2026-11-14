package tool

import (
	"syscall"
	"testing"
)

// The watchdog cap reads NORMA_TASK_RSS_MB once: unset = 4096 MB, a value in
// MB converts to bytes, 0 or garbage disables it.
func TestTaskRSSCapFromEnv(t *testing.T) {
	t.Setenv("NORMA_TASK_RSS_MB", "")
	if got := taskRSSCapBytes(); got != 4096<<20 {
		t.Fatalf("unset cap = %d, want 4096 MiB", got)
	}
	t.Setenv("NORMA_TASK_RSS_MB", "1")
	if got := taskRSSCapBytes(); got != 1<<20 {
		t.Fatalf("1 MiB cap = %d bytes", got)
	}
	t.Setenv("NORMA_TASK_RSS_MB", "0")
	if got := taskRSSCapBytes(); got != 0 {
		t.Fatalf("0 should disable, got %d", got)
	}
	t.Setenv("NORMA_TASK_RSS_MB", "junk")
	if got := taskRSSCapBytes(); got != 0 {
		t.Fatalf("garbage should disable, got %d", got)
	}
}

// processTreeRSS sums the RSS of the process group: the test process belongs
// to its own group (Getpgrp), so the sum must be positive; a group id that
// owns no process sums to zero.
func TestProcessTreeRSSCountsOwnGroup(t *testing.T) {
	if rss := processTreeRSS(syscall.Getpgrp()); rss <= 0 {
		t.Fatalf("own process group RSS = %d, want > 0", rss)
	}
	if dead := processTreeRSS(-1); dead != 0 {
		t.Fatalf("empty group RSS = %d, want 0", dead)
	}
}
