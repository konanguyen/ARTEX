package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/transcript"
)

func TestChatStartupRecoveryPrecedesDueTrigger(t *testing.T) {
	m := backportTestManager(t)
	c, err := m.pg.CreateConversation("auto", "blocked startup recovery", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.pg.DeleteConversation(c.ID) })
	ref := chatstate.SessionRef{ConversationID: c.ID}
	if _, err := m.pg.ChatControlState(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := chatstate.PrepareTranscript(t.Context(), transcript.NewStore(filepath.Join(m.dir, "transcripts")), ref, nil); err != nil {
		t.Fatal(err)
	}
	trigger, err := m.pg.CreateTrigger(&db.AgentTrigger{AgentKey: "startup-recovery-fixture", Enabled: true, IntervalSec: 3600, IntervalMessage: "due on startup"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.pg.DeleteTrigger(trigger.ID) })
	tx, err := m.pg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var ownerPID int
	if err := tx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&ownerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `SELECT id FROM conversations WHERE id=$1 FOR UPDATE`, c.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldAugment, oldResolve, oldPrompt := agent.ToolAugment, agent.ToolResolve, agent.PromptOverride
	defer func() { agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = oldAugment, oldResolve, oldPrompt }()
	type startupResult struct {
		s   *Server
		err error
	}
	started := make(chan startupResult, 1)
	go func() {
		s, err := newServer(ctx, m, m.dir, m.dir, m.dir)
		started <- startupResult{s, err}
	}()
	// Observe the actual cleanup query waiting on its owner; no test-only startup hook.
	deadline := time.Now().Add(20 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if err := m.pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT id FROM conversations WHERE id=$1 FOR UPDATE%')`, ownerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("constructor never reached blocked transcript recovery")
	}
	// The real scheduler's five-second tick must not fire while recovery is blocked.
	time.Sleep(5200 * time.Millisecond)
	var fired sql.NullTime
	if err := m.pg.QueryRow(`SELECT last_fire FROM agent_triggers WHERE id=$1`, trigger.ID).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired.Valid {
		t.Error("due trigger fired before startup recovery completed")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var s *Server
	select {
	case result := <-started:
		if result.err != nil {
			t.Fatal(result.err)
		}
		s = result.s
	case <-time.After(20 * time.Second):
		t.Fatal("constructor did not complete after recovery unblocked")
	}
	deadline = time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if err := m.pg.QueryRow(`SELECT last_fire FROM agent_triggers WHERE id=$1`, trigger.ID).Scan(&fired); err != nil {
			t.Fatal(err)
		}
		if fired.Valid {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !fired.Valid {
		t.Error("due trigger did not resume after recovery")
	}
	cancel()
	<-s.side.done
	s.archiveWG.Wait()
}

func TestChatStartupRecoveryFailureStopsConstruction(t *testing.T) {
	m := backportTestManager(t)
	if err := os.WriteFile(filepath.Join(m.dir, "transcripts"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t.Context(), m, m.dir, m.dir, m.dir)
	if err == nil || s != nil || !strings.Contains(err.Error(), "chat startup recovery: transcript recovery") {
		t.Fatalf("recovery failure returned a usable server: %v %v", s, err)
	}
}
