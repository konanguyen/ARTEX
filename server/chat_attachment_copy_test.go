package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/chatstate"
)

func TestCopyChatAttachmentsRetainsDocument(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	data := []byte("document bytes\n한글")
	path := filepath.Join(source, "uploads", "report.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	prompt := chatstate.Prompt{Text: "review", Attachments: []chatstate.Attachment{{Name: "report.txt", Path: "uploads/report.txt", Size: int64(len(data)), Abs: path}}}
	copied, err := CopyChatAttachments(t.Context(), source, target, prompt)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(target, "uploads", "report.txt")
	if copied.Text != prompt.Text || len(copied.Attachments) != 1 || copied.Attachments[0].Abs != child {
		t.Fatalf("child prompt = %+v", copied)
	}
	if prompt.Attachments[0].Abs != path {
		t.Fatal("copy changed the parent manifest")
	}
	if _, err := CopyChatAttachments(t.Context(), source, target, prompt); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	// A standalone child must retain its bytes after the parent is deleted.
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(child)
	if err != nil || string(got) != string(data) {
		t.Fatalf("retained document = %q, err = %v", got, err)
	}
}

func TestCopyChatAttachmentsRejectsInvalidSource(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	path := filepath.Join(source, "uploads", "report.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("doc"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		attachment chatstate.Attachment
	}{
		{"traversal", chatstate.Attachment{Path: "uploads/../../outside.txt", Size: 3}},
		{"absolute", chatstate.Attachment{Path: path, Size: 3}},
		{"owner mismatch", chatstate.Attachment{Path: "uploads/report.txt", Size: 3, Abs: filepath.Join(target, "uploads", "report.txt")}},
		{"size mismatch", chatstate.Attachment{Path: "uploads/report.txt", Size: 4}},
		{"missing source", chatstate.Attachment{Path: "uploads/missing.txt", Size: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CopyChatAttachments(t.Context(), source, target, chatstate.Prompt{Attachments: []chatstate.Attachment{tc.attachment}}); err == nil {
				t.Fatal("invalid attachment was accepted")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(target, "uploads")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid source wrote child uploads: %v", err)
	}
}

func TestCopyChatAttachmentsPreservesConflictingChild(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	for dir, content := range map[string]string{source: "new", target: "old"} {
		path := filepath.Join(dir, "uploads", "report.txt")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	prompt := chatstate.Prompt{Attachments: []chatstate.Attachment{{Path: "uploads/report.txt", Size: 3}}}
	if _, err := CopyChatAttachments(t.Context(), source, target, prompt); !errors.Is(err, chatstate.ErrConflict) {
		t.Fatalf("conflicting copy: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "uploads", "report.txt"))
	if err != nil || string(got) != "old" {
		t.Fatalf("existing child = %q, err = %v", got, err)
	}
}
