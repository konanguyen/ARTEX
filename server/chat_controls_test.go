package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/chatstate"
)

func TestChatInputPatchAttachmentManifest(t *testing.T) {
	s, _ := newRetestServer(t)
	c, err := s.m.pg.CreateConversation("auto", "queued manifest fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.pg.DeleteConversation(c.ID) })
	ref := chatstate.SessionRef{ConversationID: c.ID}
	uploads := filepath.Join(s.chatPromptDir(ref), "uploads")
	if err := os.MkdirAll(uploads, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "report.txt"), []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/conversations/{id}/controls/{rest...}", s.chatControls)
	for _, tc := range []struct {
		name, attachments string
		invalid           bool
	}{
		{"omitted", "", false},
		{"null", `,"attachments":null`, false},
		{"empty", `,"attachments":[]`, false},
		{"nonempty", `,"attachments":[{"name":"report.txt","path":"uploads/report.txt","size":4}]`, false},
		{"string", `,"attachments":"invalid"`, true},
		{"object", `,"attachments":{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admitted, err := s.m.pg.AdmitChatInput(t.Context(), ref, tc.name, "queue", "", chatstate.Prompt{Text: tc.name})
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.m.pg.ChatControlState(t.Context(), ref)
			if err != nil {
				t.Fatal(err)
			}
			readRaw := func() string {
				t.Helper()
				var raw string
				if err := s.m.pg.QueryRow(`SELECT to_jsonb(i)::text FROM chat_control_inputs i WHERE id=$1`, admitted.ID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			rawBefore := readRaw()
			patch := func(revision int64) *httptest.ResponseRecorder {
				body := fmt.Sprintf(`{"expected_revision":%q,"message":"revised"%s}`, fmt.Sprint(revision), tc.attachments)
				r := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/conversations/%d/controls/inputs/%s", c.ID, admitted.ID), strings.NewReader(body))
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				return w
			}
			w := patch(admitted.Revision)
			after, err := s.m.pg.ChatControlState(t.Context(), ref)
			if err != nil {
				t.Fatal(err)
			}
			if tc.invalid {
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot unmarshal "+tc.name) || !strings.Contains(w.Body.String(), "attachments") || !reflect.DeepEqual(after, before) || readRaw() != rawBefore {
					t.Fatalf("non-array PATCH must reject without writes: HTTP %d %s, state %+v", w.Code, w.Body.String(), after)
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("PATCH: HTTP %d %s", w.Code, w.Body.String())
			}
			var edited chatstate.Admission
			if err := json.Unmarshal(w.Body.Bytes(), &edited); err != nil {
				t.Fatal(err)
			}
			want := []chatstate.Attachment{}
			if tc.name == "nonempty" {
				want = []chatstate.Attachment{{Name: "report.txt", Path: "uploads/report.txt", Size: 4}}
			}
			if edited.Prompt.Text != "revised" || !reflect.DeepEqual(edited.Prompt.Attachments, want) || edited.Revision != admitted.Revision+1 || edited.Ordinal != admitted.Ordinal || after.Revision != before.Revision+1 {
				t.Errorf("PATCH did not preserve canonical prompt/revision/order: %+v", edited)
			}
			var stored struct{ Prompt chatstate.Prompt }
			if err := json.Unmarshal([]byte(readRaw()), &stored); err != nil || !reflect.DeepEqual(stored.Prompt, edited.Prompt) || stored.Prompt.Attachments == nil {
				t.Errorf("PATCH stored absent or changed manifest: %+v, %v", stored, err)
			}
			rawAfter := readRaw()
			stale := patch(admitted.Revision)
			final, err := s.m.pg.ChatControlState(t.Context(), ref)
			if stale.Code != http.StatusConflict || err != nil || !reflect.DeepEqual(final, after) || readRaw() != rawAfter {
				t.Fatalf("stale PATCH wrote data: HTTP %d %s, error %v", stale.Code, stale.Body.String(), err)
			}
		})
	}
}
