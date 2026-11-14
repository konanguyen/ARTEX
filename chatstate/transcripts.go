package chatstate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type TranscriptRevision struct {
	SourceRunID     string            `json:"-"`
	ImportedPrompts map[string]Prompt `json:"-"`
	ID              string            `json:"id"`
	SessionKey      string            `json:"session_key"`
	TranscriptID    string            `json:"transcript_id"`
	EndRecordUUID   string            `json:"end_record_uuid"`
	SHA256          string            `json:"sha256"`
	HistoryRevision int64             `json:"history_revision,string"`
	// The checksum protects the prepared prefix. Normal turns append after it.
	ByteLength int64  `json:"byte_length"`
	Directory  string `json:"-"` // preparation-only authority; never accepted from HTTP
}

func recordID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value[:])
}
func PrepareTranscript(ctx context.Context, store *transcript.Store, session SessionRef, records []transcript.Record) (out TranscriptRevision, err error) {
	if err = session.Validate(); err != nil {
		return
	}
	if store == nil {
		return out, fmt.Errorf("transcript store unavailable")
	}
	if err = ctx.Err(); err != nil {
		return
	}
	out = TranscriptRevision{ID: recordID(), SessionKey: session.Key()}
	out.TranscriptID = "chat-rev-" + out.ID
	path := store.MainPath(out.TranscriptID)
	out.Directory = filepath.Dir(path)
	normalized := make([]transcript.Record, len(records))
	seen := map[string]bool{}
	parent := ""
	for i, record := range records {
		if record.UUID == "" {
			record.UUID = recordID()
		}
		if seen[record.UUID] {
			return out, ErrAmbiguousHistory
		}
		seen[record.UUID] = true
		record.SessionID = out.TranscriptID
		record.ParentUUID = parent
		record.AgentID = ""
		record.IsSidechain = false
		if record.Timestamp == "" {
			record.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
		}
		normalized[i] = record
		parent = record.UUID
	}
	if err = ValidateChatRecords(normalized); err != nil {
		return
	}
	out.EndRecordUUID = parent
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	for _, r := range normalized {
		if err = encoder.Encode(r); err != nil {
			return
		}
	}
	out.ByteLength = int64(data.Len())
	sum := sha256.Sum256(data.Bytes())
	out.SHA256 = hex.EncodeToString(sum[:])
	if err = os.MkdirAll(out.Directory, 0700); err != nil {
		return
	}
	temp := path + ".preparing"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return out, err
	}
	renamed, manifestCreated := false, false
	defer func() {
		file.Close()
		if err != nil {
			owned := temp
			if renamed {
				owned = path
			}
			err = errors.Join(err, removeChatFile(owned))
			if manifestCreated {
				err = errors.Join(err, removeChatFile(path+".manifest.json"))
			}
		}
	}()
	if _, err = file.Write(data.Bytes()); err != nil {
		return
	}
	if err = file.Sync(); err != nil {
		return
	}
	if err = file.Close(); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if err = os.Rename(temp, path); err != nil {
		return
	}
	renamed = true
	manifest, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if err = writeChatManifest(path+".manifest.json", manifest); err != nil {
		return
	}
	manifestCreated = true
	if err = syncChatDirectory(out.Directory); err != nil {
		return
	}
	err = ValidateTranscriptRevision(store, out)
	return
}
func writeChatManifest(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		if err != nil {
			err = errors.Join(err, removeChatFile(path))
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
func syncChatDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	err = dir.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}
func ValidateTranscriptRevision(store *transcript.Store, revision TranscriptRevision) error {
	return validateTranscriptRevision(store, revision, false)
}

// ValidateTranscriptPrefix verifies the immutable prepared bytes, allowing a
// lost run's damaged appended suffix to be discarded from a known baseline.
func ValidateTranscriptPrefix(store *transcript.Store, revision TranscriptRevision) error {
	return validateTranscriptRevision(store, revision, true)
}
func validateTranscriptRevision(store *transcript.Store, revision TranscriptRevision, prefixOnly bool) error {
	if store == nil || revision.ID == "" || revision.TranscriptID != "chat-rev-"+revision.ID || strings.ContainsAny(revision.ID, "/\\.") || revision.ByteLength < 0 {
		return fmt.Errorf("invalid transcript manifest")
	}
	path := store.MainPath(revision.TranscriptID)
	manifestData, err := os.ReadFile(path + ".manifest.json")
	if err != nil {
		return err
	}
	var manifest TranscriptRevision
	if err = json.Unmarshal(manifestData, &manifest); err != nil {
		return err
	}
	if manifest.ID != revision.ID || manifest.TranscriptID != revision.TranscriptID || manifest.SessionKey != revision.SessionKey || manifest.SHA256 != revision.SHA256 || manifest.ByteLength != revision.ByteLength || manifest.EndRecordUUID != revision.EndRecordUUID {
		return fmt.Errorf("transcript manifest mismatch")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyN(h, f, revision.ByteLength)
	if err != nil || n != revision.ByteLength {
		return fmt.Errorf("incomplete prepared transcript")
	}
	if hex.EncodeToString(h.Sum(nil)) != revision.SHA256 {
		return fmt.Errorf("transcript checksum mismatch")
	}
	var records []transcript.Record
	if prefixOnly {
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		decoder := json.NewDecoder(io.LimitReader(f, revision.ByteLength))
		for {
			var r transcript.Record
			err = decoder.Decode(&r)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			records = append(records, r)
		}
	} else {
		records, err = store.Load(path)
		if err != nil {
			return err
		}
	}
	if err = ValidateChatRecords(records); err != nil {
		return err
	}
	var prefix bytes.Buffer
	encoder := json.NewEncoder(&prefix)
	end := ""
	for _, r := range records {
		if int64(prefix.Len()) >= revision.ByteLength {
			break
		}
		if r.SessionID != revision.TranscriptID {
			return fmt.Errorf("transcript session mismatch")
		}
		if err = encoder.Encode(r); err != nil {
			return err
		}
		end = r.UUID
	}
	if int64(prefix.Len()) != revision.ByteLength || end != revision.EndRecordUUID {
		return fmt.Errorf("transcript boundary mismatch")
	}
	return nil
}

// ValidateChatRecords rejects orphan tool/reasoning chains before publication.
// Boundary snapshots form a new working set and may reuse earlier tool IDs.
func ValidateChatRecords(records []transcript.Record) error {
	pending := map[string]bool{}
	seen := map[string]bool{}
	parent := ""
	for _, r := range records {
		if r.UUID == "" || seen[r.UUID] {
			return ErrAmbiguousHistory
		}
		seen[r.UUID] = true
		if r.ParentUUID != "" && r.ParentUUID != parent {
			return fmt.Errorf("broken transcript parent chain")
		}
		parent = r.UUID
		if r.Type == "boundary" {
			if len(pending) > 0 {
				return fmt.Errorf("boundary splits tool exchange")
			}
			continue
		}
		if r.Type != "message" || r.Message == nil {
			return fmt.Errorf("invalid transcript record")
		}
		hadPending := len(pending) > 0
		if hadPending && r.Message.Role != llm.RoleUser {
			return fmt.Errorf("tool results must immediately follow the assistant tool call")
		}
		boundary := false
		for _, b := range r.Message.Content {
			if b.Type == llm.BlockCompactBoundary {
				boundary = true
			}
		}
		if boundary && len(pending) > 0 {
			return fmt.Errorf("boundary splits tool exchange")
		}
		for _, b := range r.Message.Content {
			switch b.Type {
			case llm.BlockToolUse:
				if r.Message.Role != llm.RoleAssistant {
					return fmt.Errorf("tool use has invalid role")
				}
				if b.ID == "" || pending[b.ID] {
					return fmt.Errorf("invalid tool use")
				}
				pending[b.ID] = true
			case llm.BlockToolResult:
				if r.Message.Role != llm.RoleUser {
					return fmt.Errorf("tool result has invalid role")
				}
				if !pending[b.ToolUseID] {
					return fmt.Errorf("orphan tool result %s", b.ToolUseID)
				}
				delete(pending, b.ToolUseID)
			}
		}
		if hadPending && len(pending) > 0 {
			return fmt.Errorf("incomplete tool result batch")
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("incomplete tool exchange")
	}
	return nil
}

// MapLegacyChatTurns uses complete chronological prompt values. It never guesses
// from summaries or from a transcript containing reconstructed boundary copies.
func MapLegacyChatTurns(records []transcript.Record, prompts []string) ([]string, error) {
	var candidates []transcript.Record
	for _, r := range records {
		if r.Type == "boundary" {
			return nil, ErrAmbiguousHistory
		}
		if r.Message == nil {
			continue
		}
		if _, ok := llm.ParseBoundaryMeta(*r.Message); ok {
			return nil, ErrAmbiguousHistory
		}
		if r.Message.Role == llm.RoleUser {
			tool := false
			for _, b := range r.Message.Content {
				if b.Type == llm.BlockToolResult {
					tool = true
				}
			}
			if !tool {
				candidates = append(candidates, r)
			}
		}
	}
	if len(candidates) != len(prompts) {
		return nil, ErrAmbiguousHistory
	}
	ids := make([]string, len(prompts))
	for i, prompt := range prompts {
		if candidates[i].UUID == "" || candidates[i].Message.Text() != prompt {
			return nil, ErrAmbiguousHistory
		}
		ids[i] = candidates[i].UUID
	}
	return ids, nil
}

// CanonicalChatRecords strips recorded working-set copies while retaining every
// original birth record. Unknown or inconsistent legacy snapshots fail closed.
func CanonicalChatRecords(records []transcript.Record) ([]transcript.Record, error) {
	var births []transcript.Record
	var working []llm.Message
	for i := 0; i < len(records); i++ {
		r := records[i]
		if r.Type == "boundary" {
			continue
		}
		if r.Message == nil {
			births = append(births, r)
			continue
		}
		if !llm.IsBoundaryMarker(*r.Message) {
			births = append(births, r)
			working = append(working, *r.Message)
			continue
		}
		meta, ok := llm.ParseBoundaryMeta(*r.Message)
		if !ok {
			return nil, ErrAmbiguousHistory
		}
		var host struct {
			Count int `json:"host_working_set_count"`
		}
		_ = json.Unmarshal(r.Message.Content[0].Input, &host)
		count := host.Count
		if count == 0 {
			if meta.MessagesSummarized <= 0 {
				return nil, ErrAmbiguousHistory
			}
			count = 2 + len(meta.ActiveSkills) + len(working) - meta.MessagesSummarized
		}
		if count < 2 || i+count > len(records) {
			return nil, ErrAmbiguousHistory
		}
		snapshot := make([]llm.Message, 0, count-1)
		for j := i + 1; j < i+count; j++ {
			m := records[j].Message
			if records[j].Type != "message" || m == nil || llm.IsBoundaryMarker(*m) {
				return nil, ErrAmbiguousHistory
			}
			snapshot = append(snapshot, *m)
		}
		tail := count - 2 - len(meta.ActiveSkills)
		if tail < 0 || tail > len(working) {
			return nil, ErrAmbiguousHistory
		}
		a, _ := json.Marshal(working[len(working)-tail:])
		b, _ := json.Marshal(snapshot[len(snapshot)-tail:])
		if !bytes.Equal(a, b) {
			return nil, ErrAmbiguousHistory
		}
		working = snapshot
		i += count - 1
	}
	return births, nil
}

func LoadChatTranscriptThrough(store *transcript.Store, sessionID, endUUID string) ([]transcript.Record, error) {
	if endUUID == "" {
		return nil, nil
	}
	file, err := os.Open(store.MainPath(sessionID))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var records []transcript.Record
	for {
		var r transcript.Record
		if err = decoder.Decode(&r); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, ErrAmbiguousHistory
			}
			return nil, err
		}
		records = append(records, r)
		if r.UUID == endUUID {
			return records, ValidateChatRecords(records)
		}
	}
}

// ChatRecordOrigins identifies a retained message's original birth record. In
// particular, equal user-authored text is not evidence that it is a snapshot
// copy. Canonical validation proves the compactor's retained suffix first.
func ChatRecordOrigins(records []transcript.Record) (map[string]string, error) {
	if _, err := CanonicalChatRecords(records); err != nil {
		return nil, err
	}
	origins := make(map[string]string, len(records))
	var working []string
	for i := 0; i < len(records); i++ {
		r := records[i]
		if r.Message == nil {
			continue
		}
		if !llm.IsBoundaryMarker(*r.Message) {
			origins[r.UUID] = r.UUID
			working = append(working, r.UUID)
			continue
		}
		meta, _ := llm.ParseBoundaryMeta(*r.Message)
		var host struct {
			Count int `json:"host_working_set_count"`
		}
		_ = json.Unmarshal(r.Message.Content[0].Input, &host)
		count := host.Count
		if count == 0 {
			count = 2 + len(meta.ActiveSkills) + len(working) - meta.MessagesSummarized
		}
		tail := count - 2 - len(meta.ActiveSkills)
		next := make([]string, count-1)
		for j := 0; j < tail; j++ {
			origin := working[len(working)-tail+j]
			index := count - 1 - tail + j
			next[index] = origin
			origins[records[i+1+index].UUID] = origin
		}
		working = next
		i += count - 1
	}
	return origins, nil
}
