package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/chatstate"
)

func (s *Server) chatControlRef(w http.ResponseWriter, r *http.Request) (chatstate.SessionRef, bool) {
	if strings.HasPrefix(r.URL.Path, "/api/conversations/") {
		_, c, ok := s.convByID(w, r)
		if !ok {
			return chatstate.SessionRef{}, false
		}
		return chatstate.SessionRef{ConversationID: c.ID}, true
	}
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return chatstate.SessionRef{}, false
	}
	seg, err := strconv.Atoi(r.URL.Query().Get("seg"))
	if err != nil || seg < 0 {
		writeErr(w, 400, "seg is required")
		return chatstate.SessionRef{}, false
	}
	if visible, err := s.m.pg.MainChatSegmentAvailable(r.Context(), t.ExpID, seg); err != nil || !visible {
		chatControlError(w, chatstate.ErrGone)
		return chatstate.SessionRef{}, false
	}
	id, _ := strconv.ParseInt(t.ID, 10, 64)
	return chatstate.SessionRef{TaskID: id, ExplorationID: t.ExpID, MainSeg: seg}, true
}

func chatControlError(w http.ResponseWriter, err error) {
	status := 400
	switch {
	case errors.Is(err, chatstate.ErrGone):
		status = 404
	case errors.Is(err, chatstate.ErrBusy), errors.Is(err, chatstate.ErrConflict), errors.Is(err, chatstate.ErrUnknownRun), errors.Is(err, chatstate.ErrAmbiguousHistory):
		status = 409
	}
	writeError(w, status, err)
}

func (s *Server) chatPromptDir(ref chatstate.SessionRef) string {
	if ref.ConversationID > 0 {
		return filepath.Join(s.m.dir, "sessions", ref.Key())
	}
	return filepath.Join(s.m.dir, "tasks", strconv.FormatInt(ref.TaskID, 10))
}

func (s *Server) validateChatPrompt(ref chatstate.SessionRef, prompt chatstate.Prompt, allowBlankTask ...bool) error {
	legacyTask := len(allowBlankTask) > 0 && allowBlankTask[0] && ref.TaskID > 0 && ref.ConversationID == 0
	if !legacyTask && strings.TrimSpace(prompt.Text) == "" && len(prompt.Attachments) == 0 {
		return fmt.Errorf("message is required")
	}
	base := s.chatPromptDir(ref)
	for _, a := range prompt.Attachments {
		cleaned := filepath.ToSlash(filepath.Clean(a.Path))
		if cleaned != a.Path || !strings.HasPrefix(cleaned, "uploads/") || strings.Contains(cleaned, "../") || filepath.IsAbs(a.Path) {
			return fmt.Errorf("invalid attachment path")
		}
		path := filepath.Join(base, filepath.FromSlash(cleaned))
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("attachment unavailable: %s", a.Name)
		}
		root, err := filepath.EvalSymlinks(filepath.Join(base, "uploads"))
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("attachment outside chat")
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Size() != a.Size {
			return fmt.Errorf("attachment unavailable: %s", a.Name)
		}
		if a.Abs != "" && filepath.Clean(a.Abs) != filepath.Clean(path) {
			return fmt.Errorf("attachment owner mismatch")
		}
	}
	_, err := composeChatMentionMessage(s.m.pg, prompt.Text)
	return err
}

func (s *Server) chatControls(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	ref, ok := s.chatControlRef(w, r)
	if !ok {
		return
	}
	if err := s.ensureLegacyChatHistory(r.Context(), ref); err != nil {
		chatControlError(w, err)
		return
	}
	rest := strings.Trim(r.PathValue("rest"), "/")
	parts := strings.Split(rest, "/")
	if r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "turns" && parts[2] == "prompt" {
		prompt, err := s.m.pg.ChatTurnPrompt(r.Context(), ref, parts[1])
		if err != nil {
			chatControlError(w, err)
			return
		}
		writeJSON(w, 200, prompt)
		return
	}
	if r.Method == http.MethodGet && parts[0] == "attempts" {
		s.chatAttemptsHTTP(w, r, ref, parts)
		return
	}
	if r.Method == http.MethodPost && rest == "compact" {
		s.chatCompactHTTP(w, r, ref)
		return
	}
	if r.Method == http.MethodGet && rest == "context" {
		s.chatContextHTTP(w, r, ref)
		return
	}
	if r.Method == http.MethodGet && rest == "" {
		state, err := s.m.pg.ChatControlState(r.Context(), ref)
		if err != nil {
			chatControlError(w, err)
			return
		}
		writeJSON(w, 200, state)
		return
	}
	if r.Method == http.MethodPost && (rest == "retry" || rest == "edit" || rest == "branch") {
		s.chatHistoryHTTP(w, r, ref, rest)
		return
	}
	if r.Method == http.MethodPost && rest == "inputs" {
		s.admitChatHTTP(w, r, ref)
		return
	}
	if len(parts) >= 2 && parts[0] == "inputs" {
		id := parts[1]
		if r.Method == http.MethodPatch {
			var req struct {
				Expected    int64            `json:"expected_revision,string"`
				Message     string           `json:"message"`
				Attachments []chatAttachment `json:"attachments"`
			}
			if !decodeConversationRequest(w, r, &req) {
				return
			}
			prompt := chatstate.Prompt{Text: req.Message, Attachments: req.Attachments}
			if err := s.validateChatPrompt(ref, prompt); err != nil {
				chatControlError(w, err)
				return
			}
			a, err := s.m.pg.UpdateChatInput(r.Context(), ref, id, req.Expected, prompt)
			if err != nil {
				chatControlError(w, err)
				return
			}
			writeJSON(w, 200, a)
			return
		}
		if r.Method == http.MethodDelete {
			expected, err := strconv.ParseInt(r.URL.Query().Get("expected_revision"), 10, 64)
			if err == nil {
				err = s.m.pg.CancelChatInput(r.Context(), ref, id, expected)
			}
			if err != nil {
				chatControlError(w, err)
				return
			}
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "followup" {
			var req struct {
				Expected int64 `json:"expected_revision,string"`
			}
			if !decodeConversationRequest(w, r, &req) {
				return
			}
			admission, err := s.m.pg.QueueUndeliveredSteer(r.Context(), ref, id, req.Expected)
			if err != nil {
				chatControlError(w, err)
				return
			}
			s.dispatchChatGate(ref.GateKey())
			writeJSON(w, 200, admission)
			return
		}
		if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "discard" {
			var req struct {
				Expected int64 `json:"expected_revision,string"`
			}
			if !decodeConversationRequest(w, r, &req) {
				return
			}
			if err := s.discardUnknownChatRun(r.Context(), ref, id, req.Expected); err != nil {
				chatControlError(w, err)
				return
			}
			state, err := s.m.pg.ChatControlState(r.Context(), ref)
			if err != nil {
				chatControlError(w, err)
				return
			}
			writeJSON(w, 200, state)
			return
		}
	}
	if r.Method == http.MethodPost && (rest == "pause" || rest == "resume" || rest == "queue/order") {
		var req struct {
			Expected int64    `json:"expected_revision,string"`
			IDs      []string `json:"input_ids"`
		}
		if !decodeConversationRequest(w, r, &req) {
			return
		}
		var err error
		switch rest {
		case "pause":
			err = s.m.pg.PauseChatGate(r.Context(), ref, req.Expected)
		case "resume":
			err = s.m.pg.ResumeChatGate(r.Context(), ref, req.Expected)
		case "queue/order":
			err = s.m.pg.ReorderChatInputs(r.Context(), ref, req.Expected, req.IDs)
		}
		if err != nil {
			chatControlError(w, err)
			return
		}
		if rest == "resume" {
			s.dispatchChatGate(ref.GateKey())
		}
		state, err := s.m.pg.ChatControlState(r.Context(), ref)
		if err != nil {
			chatControlError(w, err)
			return
		}
		writeJSON(w, 200, state)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) admitChatHTTP(w http.ResponseWriter, r *http.Request, ref chatstate.SessionRef) {
	var req struct {
		ClientID    string           `json:"client_request_id"`
		Mode        string           `json:"mode"`
		Message     string           `json:"message"`
		Attachments []chatAttachment `json:"attachments"`
		TargetRunID string           `json:"target_run_id"`
	}
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	a, ok := s.admitChatPrompt(w, r, ref, req.ClientID, req.Mode, req.TargetRunID, chatstate.Prompt{Text: req.Message, Attachments: req.Attachments})
	if !ok {
		return
	}
	s.dispatchChatGate(ref.GateKey())
	writeJSON(w, 202, a)
}

func (s *Server) admitChatPrompt(w http.ResponseWriter, r *http.Request, ref chatstate.SessionRef, clientID, mode, targetRunID string, prompt chatstate.Prompt, legacyTask ...bool) (chatstate.Admission, bool) {
	if s.ctx.Err() != nil {
		writeErr(w, http.StatusServiceUnavailable, "server is shutting down")
		return chatstate.Admission{}, false
	}
	if ref.TaskID > 0 && s.engine.IsDeleting(strconv.FormatInt(ref.TaskID, 10)) {
		chatControlError(w, chatstate.ErrGone)
		return chatstate.Admission{}, false
	}
	if err := s.validateChatPrompt(ref, prompt, legacyTask...); err != nil {
		chatControlError(w, err)
		return chatstate.Admission{}, false
	}
	if ref.ConversationID > 0 {
		c, err := s.m.pg.GetConversation(ref.ConversationID)
		if err != nil {
			chatControlError(w, err)
			return chatstate.Admission{}, false
		}
		if c == nil {
			chatControlError(w, chatstate.ErrGone)
			return chatstate.Admission{}, false
		}
		if s.resolveChatAgent(c) == nil {
			writeErr(w, 503, s.chatUnavailableReason())
			return chatstate.Admission{}, false
		}
	}
	// Admission and the shutdown barrier share the same lock.
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if s.chatClosing || s.ctx.Err() != nil {
		writeErr(w, http.StatusServiceUnavailable, "server is shutting down")
		return chatstate.Admission{}, false
	}
	var a chatstate.Admission
	var err error
	if len(legacyTask) > 0 && legacyTask[0] && ref.TaskID > 0 && ref.ConversationID == 0 {
		a, err = s.m.pg.AdmitLegacyTaskChatInput(r.Context(), ref, clientID, prompt)
	} else {
		a, err = s.m.pg.AdmitChatInput(r.Context(), ref, clientID, mode, targetRunID, prompt)
	}
	if err != nil {
		chatControlError(w, err)
		return chatstate.Admission{}, false
	}
	return a, true
}

// Existing send endpoints enter exactly the same durable admission path. A
// server-generated ID preserves their pre-idempotency compatibility contract.
func (s *Server) admitCompatibilityChat(w http.ResponseWriter, r *http.Request, ref chatstate.SessionRef, message string, attachments []chatAttachment) {
	rule := false
	if ref.TaskID > 0 {
		task := s.m.ResolveTask(strconv.FormatInt(ref.TaskID, 10))
		rule = !s.taskRuntimeAvailable(task, "mainagent")
	}
	a, ok := s.admitChatPrompt(w, r, ref, rand.Text(), "send", "", chatstate.Prompt{Text: message, Attachments: attachments}, ref.TaskID > 0)
	if !ok {
		return
	}
	if err := s.dispatchChatInput(ref.GateKey(), ""); err != nil {
		err = s.cancelCompatibilityInput(a, err)
		log.Printf("[chat %s] compatibility dispatch: %v", a.ID, err)
		compatibilityChatError(w, err)
		return
	}
	if ref.ConversationID > 0 {
		writeJSON(w, 202, map[string]string{"status": "accepted"})
		return
	}
	if !rule {
		writeJSON(w, 202, map[string]string{"status": "accepted", "mode": "llm"})
		return
	}
	reply, err := s.waitCompatibilityReply(r.Context(), a)
	if err != nil {
		log.Printf("[chat %s] compatibility completion: %v", a.ID, err)
		compatibilityChatError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"reply": reply, "mode": "rule"})
}

// Read the one durable run's reply; fallbackChat writes hints/intents and must
// never be called again just to construct the legacy synchronous response.
func (s *Server) waitCompatibilityReply(request context.Context, admission chatstate.Admission) (reply string, waitErr error) {
	ctx, cancel := context.WithTimeout(request, 5*time.Second)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	defer func() {
		if errors.Is(waitErr, context.DeadlineExceeded) {
			waitErr = s.cancelCompatibilityInput(admission, waitErr)
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status, reply string
		var paused bool
		err := s.m.pg.QueryRowContext(ctx, `SELECT i.status,g.paused,COALESCE(a.summary,'')
FROM chat_control_inputs i
JOIN chat_control_gates g ON g.gate_key=i.gate_key
LEFT JOIN chat_control_runs r ON r.input_id=i.id
LEFT JOIN activity a ON a.run_id=r.id AND a.worker='mainagent' AND a.kind='text'
WHERE i.id=$1 ORDER BY a.id DESC NULLS LAST LIMIT 1`, admission.ID).Scan(&status, &paused, &reply)
		if errors.Is(err, sql.ErrNoRows) {
			return "", chatstate.ErrGone
		}
		if err != nil {
			return "", err
		}
		switch status {
		case "completed":
			if reply == "" {
				return "", agent.ErrChatPersistence
			}
			return reply, nil
		case "unknown":
			return "", chatstate.ErrUnknownRun
		case "stopped", "cancelled":
			return "", chatstate.ErrBusy
		case "failed":
			return "", agent.ErrChatPersistence
		case "queued":
			if paused {
				return "", s.cancelCompatibilityInput(admission, chatstate.ErrBusy)
			}
		case "claimed":
		default:
			return "", agent.ErrChatPersistence
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func compatibilityChatError(w http.ResponseWriter, err error) {
	if errors.Is(err, chatstate.ErrBusy) || errors.Is(err, chatstate.ErrGone) || errors.Is(err, chatstate.ErrUnknownRun) {
		chatControlError(w, err)
		return
	}
	writeError(w, 500, chatstate.ErrGone)
}

// Cancel only our unchanged, unclaimed input. A claim/revision conflict retains
// the durable work because execution may already have produced side effects.
func (s *Server) cancelCompatibilityInput(admission chatstate.Admission, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), time.Second)
	defer cancel()
	err := s.m.pg.CancelChatInput(ctx, admission.Session, admission.ID, admission.Revision)
	if err != nil && !errors.Is(err, chatstate.ErrConflict) && !errors.Is(err, chatstate.ErrGone) && !errors.Is(err, sql.ErrNoRows) {
		return errors.Join(cause, err)
	}
	return cause
}
