package chatstate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/transcript"
)

type obstructManifestContext struct {
	context.Context
	t        *testing.T
	dir      string
	calls    int
	manifest string
}

func (c *obstructManifestContext) Err() error {
	c.calls++
	if c.calls == 2 {
		entries, err := os.ReadDir(c.dir)
		if err != nil {
			c.t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".preparing") {
				c.manifest = filepath.Join(c.dir, strings.TrimSuffix(entry.Name(), ".preparing")+".manifest.json")
				if err := os.Mkdir(c.manifest, 0700); err != nil {
					c.t.Fatal(err)
				}
			}
		}
	}
	return nil
}

func TestPrepareTranscriptRemovesOwnFileAfterManifestFailure(t *testing.T) {
	dir := t.TempDir()
	ctx := &obstructManifestContext{Context: t.Context(), t: t, dir: dir}
	revision, err := PrepareTranscript(ctx, transcript.NewStore(dir), SessionRef{ConversationID: 1}, revisionRecords())
	if err == nil || ctx.manifest == "" {
		t.Fatal("manifest obstruction did not fail preparation", err)
	}
	for _, path := range []string{filepath.Join(dir, revision.TranscriptID+".jsonl"), filepath.Join(dir, revision.TranscriptID+".jsonl.preparing")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("failed preparation left its own bytes at %s: %v", path, err)
		}
	}
	if info, err := os.Stat(ctx.manifest); err != nil || !info.IsDir() {
		t.Fatal("removed obstruction it did not own", err)
	}
}

func TestPreparedTranscriptCleanupValidatesExactOwnedFiles(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	revision, err := PrepareTranscript(t.Context(), store, SessionRef{ConversationID: 1}, revisionRecords())
	if err != nil {
		t.Fatal(err)
	}
	path := store.MainPath(revision.TranscriptID)
	loaded, err := ReadPreparedTranscript(store, filepath.Base(path)+".manifest.json")
	if err != nil || loaded.ID != revision.ID || loaded.SessionKey != revision.SessionKey {
		t.Fatal(loaded, err)
	}
	for _, name := range []string{"../" + filepath.Base(path) + ".manifest.json", path + ".manifest.json", "chat-rev-../outside.jsonl.manifest.json", "chat-rev-INVALID.jsonl.manifest.json"} {
		if _, err := ReadPreparedTranscript(store, name); err == nil {
			t.Fatalf("accepted unsafe name %q", name)
		}
	}
	bad := revision
	bad.ID = "../../outside"
	if err := RemovePreparedTranscript(store, bad); err == nil {
		t.Fatal("removed unsafe identity")
	}
	bad = revision
	bad.SHA256 = "corrupt"
	if err := RemovePreparedTranscript(store, bad); err == nil {
		t.Fatal("removed unverified contents")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("uncertain file removed", err)
	}
	if err := RemovePreparedTranscript(store, revision); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + ".manifest.json"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatal("owned file survived cleanup", name, err)
		}
	}
}

func TestPreparedTranscriptCleanupPreservesSymlinks(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	revision, err := PrepareTranscript(t.Context(), store, SessionRef{ConversationID: 1}, revisionRecords())
	if err != nil {
		t.Fatal(err)
	}
	path := store.MainPath(revision.TranscriptID)
	external := filepath.Join(t.TempDir(), "external.jsonl")
	if err := os.Rename(path, external); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, path); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := RemovePreparedTranscript(store, revision); err == nil {
		t.Fatal("accepted symlink as owned transcript")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("removed uncertain link", err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatal("changed external file", err)
	}
}

func TestPreparedTranscriptCleanupPreservesAppendedBytes(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	revision, err := PrepareTranscript(t.Context(), store, SessionRef{ConversationID: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := store.MainPath(revision.TranscriptID)
	if err := os.WriteFile(path, []byte("unknown appended bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemovePreparedTranscript(store, revision); err == nil {
		t.Fatal("removed bytes outside the prepared prefix")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("uncertain appended bytes removed", err)
	}
}
