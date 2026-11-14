package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/artex/chatstate"
	"github.com/Autumn-27/norma/transcript"
)

func (s *Server) cleanupPreparedChatTranscript(revision chatstate.TranscriptRevision) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.removeUnpublishedChatTranscript(ctx, revision); err != nil {
		log.Printf("[chat] preserving prepared transcript %s: %v", revision.TranscriptID, err)
	}
}

func (s *Server) removeUnpublishedChatTranscript(ctx context.Context, revision chatstate.TranscriptRevision) error {
	return s.m.pg.WithUnpublishedChatTranscript(ctx, revision.SessionKey, revision.TranscriptID, func() error {
		return chatstate.RemovePreparedTranscript(transcript.NewStore(filepath.Join(s.m.dir, "transcripts")), revision)
	})
}

// recoverChatTranscripts runs only at single-owner startup, before preparation
// recovery deletes hidden owners and before any admission or dispatch can prepare files.
func (s *Server) recoverChatTranscripts(ctx context.Context) error {
	root := filepath.Join(s.m.dir, "transcripts")
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("transcript path is not a directory: %s", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	store := transcript.NewStore(root)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "chat-rev-") || !strings.HasSuffix(entry.Name(), ".jsonl.manifest.json") {
			continue
		}
		revision, err := chatstate.ReadPreparedTranscript(store, entry.Name())
		if err == nil {
			err = s.removeUnpublishedChatTranscript(ctx, revision)
		}
		if err != nil {
			log.Printf("[chat] preserving recovery candidate %s: %v", entry.Name(), err)
		}
	}
	return nil
}
