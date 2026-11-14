package db

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestConversationExplorationIsDurableAndSharedByConcurrentCalls(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	conversation, err := d.CreateConversation("pentest", "Conversation graph fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var id *int64
		_ = d.QueryRow(`SELECT exploration_id FROM conversations WHERE id=$1`, conversation.ID).Scan(&id)
		_ = d.DeleteConversation(conversation.ID)
		if id != nil {
			_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, *id)
		}
	})
	var exists bool
	if err := d.QueryRow(`SELECT exploration_id IS NOT NULL FROM conversations WHERE id=$1`, conversation.ID).Scan(&exists); err != nil || exists {
		t.Fatal("a normal conversation eagerly created a finding graph", err)
	}

	const callers = 8
	ids := make(chan int64, callers)
	errors := make(chan error, callers)
	var workers sync.WaitGroup
	for range callers {
		workers.Go(func() {
			store, key, err := d.ConversationExploration(t.Context(), conversation.ID)
			if err != nil {
				errors <- err
				return
			}
			if key != "pentest" {
				errors <- fmt.Errorf("unexpected agent key %q", key)
				return
			}
			ids <- store.ID()
		})
	}
	workers.Wait()
	close(ids)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	var first int64
	for id := range ids {
		if first == 0 {
			first = id
		} else if first != id {
			t.Fatalf("concurrent calls created different graphs: %d and %d", first, id)
		}
	}
	if first == 0 {
		t.Fatal("no finding graph was created")
	}
	store, _, err := d.ConversationExploration(t.Context(), conversation.ID)
	if err != nil || store.ID() != first {
		t.Fatal("a later call did not reuse the persisted graph", err)
	}
	if _, _, err := d.ConversationExploration(t.Context(), 0); err == nil {
		t.Fatal("missing conversation created a finding graph")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := d.ConversationExploration(ctx, conversation.ID); err == nil {
		t.Fatal("cancelled call ignored its context")
	}
}
