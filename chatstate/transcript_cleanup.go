package chatstate

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/norma/transcript"
)

func removeChatFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func preparedChatPath(store *transcript.Store, revision TranscriptRevision) (string, error) {
	id, err := hex.DecodeString(revision.ID)
	if store == nil || err != nil || len(id) != 16 || hex.EncodeToString(id) != revision.ID || revision.TranscriptID != "chat-rev-"+revision.ID {
		return "", fmt.Errorf("invalid prepared transcript identity")
	}
	path := store.MainPath(revision.TranscriptID)
	for _, name := range []string{path, path + ".manifest.json"} {
		info, err := os.Lstat(name)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("prepared transcript is not a regular file")
		}
	}
	return path, nil
}

// ReadPreparedTranscript accepts only this store's exact generated manifest names.
func ReadPreparedTranscript(store *transcript.Store, name string) (revision TranscriptRevision, err error) {
	const suffix = ".jsonl.manifest.json"
	if filepath.Base(name) != name || !strings.HasPrefix(name, "chat-rev-") || !strings.HasSuffix(name, suffix) {
		return revision, fmt.Errorf("invalid prepared transcript filename")
	}
	revision.TranscriptID = strings.TrimSuffix(name, suffix)
	revision.ID = strings.TrimPrefix(revision.TranscriptID, "chat-rev-")
	path, err := preparedChatPath(store, revision)
	if err != nil {
		return revision, err
	}
	data, err := os.ReadFile(path + ".manifest.json")
	if err != nil {
		return revision, err
	}
	var manifest TranscriptRevision
	if err = json.Unmarshal(data, &manifest); err != nil {
		return revision, err
	}
	if manifest.ID != revision.ID || manifest.TranscriptID != revision.TranscriptID || manifest.SessionKey == "" {
		return revision, fmt.Errorf("prepared transcript filename mismatch")
	}
	if _, err = preparedChatPath(store, manifest); err != nil {
		return revision, err
	}
	if err = ValidateTranscriptPrefix(store, manifest); err != nil {
		return revision, err
	}
	manifest.Directory = filepath.Dir(path)
	return manifest, nil
}

// RemovePreparedTranscript requires the caller to exclude publication under the
// owning gate lock. It preserves corrupt/unknown files rather than guessing ownership.
func RemovePreparedTranscript(store *transcript.Store, revision TranscriptRevision) error {
	if revision.SessionKey == "" {
		return fmt.Errorf("missing prepared transcript owner")
	}
	path, err := preparedChatPath(store, revision)
	if err != nil {
		return err
	}
	if err = ValidateTranscriptPrefix(store, revision); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() != revision.ByteLength {
		return fmt.Errorf("prepared transcript has additional bytes")
	}
	if err = removeChatFile(path); err != nil {
		return err
	}
	return removeChatFile(path + ".manifest.json")
}
