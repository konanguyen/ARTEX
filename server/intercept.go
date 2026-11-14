package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
)

// judgeWorkerLane is the usage-ledger "worker" label for intercept fallback-judge
// calls, so judge spend can be queried apart from the worker/planner/main lanes.
const judgeWorkerLane = "judge"

// chatGuard returns a guard wired with the manager's interceptor, used for chat
// conversations. Called once per applyLLM so a new LLM config always gets a fresh guard.
func (s *Server) chatGuard() *guard.Guard {
	return guard.NewWithInterceptor(s.m.interceptor)
}

// wireInterceptReviewer installs the LLM fallback judge into the interceptor. The
// judge runs only on tool calls that matched no rule (see intercept.Judge). It
// resolves the configured judge profile (0 → active/default), builds a provider,
// runs a one-shot JSON classification with an explanation for every verdict.
func (s *Server) wireInterceptReviewer() {
	s.m.interceptor.SetReviewer(func(ctx context.Context, profileID int64, prompt string, input intercept.ReviewInput) (intercept.Decision, error) {
		prov, profileID, err := s.judgeProvider(ctx, profileID)
		if err != nil {
			return intercept.Decision{ProfileID: profileID}, err
		}
		ctx = intercept.WithProviderOrigin(ctx)
		ctx = withNativeRole(ctx, "judge")
		// Tag this call's usage as the "judge" lane so the config page can report
		// how much the fallback approval has spent, separate from model profiles.
		ctx = llmrec.WithWorker(ctx, judgeWorkerLane)
		text, err := reviewCompletion(ctx, prov, prompt, input)
		if err != nil {
			return intercept.Decision{ProfileID: profileID}, err
		}
		v := intercept.ParseVerdict(text)
		if v.Action == "" {
			return intercept.Decision{ProfileID: profileID}, fmt.Errorf("invalid model verdict format: must include verdict, actual operation, outcome on success, and matched rule")
		}
		return intercept.Decision{Action: v.Action, Message: v.Reason, ProfileID: profileID}, nil
	})
	// Closed mode: one call answers every question; intercept parses and decides.
	s.m.interceptor.SetCompleter(func(ctx context.Context, profileID int64, system, user string) (string, int64, error) {
		prov, profileID, err := s.judgeProvider(ctx, profileID)
		if err != nil {
			return "", profileID, err
		}
		ctx = intercept.WithProviderOrigin(ctx)
		ctx = withNativeRole(ctx, "judge")
		ctx = llmrec.WithWorker(ctx, judgeWorkerLane)
		text, err := streamCollectText(ctx, prov, system, user, closedJudgeMaxTokens)
		return text, profileID, err
	})
}

// closedJudgeMaxTokens bounds the closed-question reply: eight short answers
// with one evidence line of at most 200 characters each.
const closedJudgeMaxTokens = 1536

// judgeProvider resolves the judge profile (0 -> active/default) to a provider.
func (s *Server) judgeProvider(ctx context.Context, profileID int64) (llm.Provider, int64, error) {
	var prov llm.Provider
	var cfg agent.Config
	if origin, preferred := intercept.ReviewProviderOrigin(ctx); preferred {
		var ok bool
		cfg, ok = origin.Data.(agent.Config)
		if !ok {
			return nil, profileID, fmt.Errorf("judge model profile %d is unavailable", profileID)
		}
		profileID = origin.ProfileID
	} else {
		if profileID == 0 {
			if p, err := s.m.pg.ActiveProfile(); err == nil && p != nil {
				profileID = p.ID
			}
		}
		if profileID == 0 {
			return nil, 0, fmt.Errorf("no usable judge model is configured")
		}
		var ok bool
		prov, cfg, ok = s.providerForProfile(profileID)
		if !ok {
			return nil, profileID, fmt.Errorf("judge model profile %d is unavailable", profileID)
		}
	}
	if cfg.Format == llm.Format("ollama") {
		// JSON mode is specific to the judge; retain the same endpoint admission
		// gate as task inference so this separate provider cannot overfill VRAM.
		cfg = s.applyLLMPolicy(cfg)
		cfg.JSONOutput = true
		cfg.JSONSchema = map[string]any{"type": "object", "properties": map[string]any{"decision": map[string]any{"type": "string", "enum": []string{"allow", "ask", "deny"}}, "comment": map[string]any{"type": "string", "minLength": 10}}, "required": []string{"decision", "comment"}, "additionalProperties": false}
		var err error
		prov, err = cfg.NewProvider()
		if err != nil {
			return nil, profileID, err
		}
		prov = s.profileConcurrencyProvider(prov, cfg)
	}
	return prov, profileID, nil
}

func reviewCompletion(ctx context.Context, prov llm.Provider, prompt string, input intercept.ReviewInput) (string, error) {
	user, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return streamCollectText(ctx, prov, prompt, string(user), 1024)
}

// streamCollectText runs a single non-streaming-style completion (thinking off,
// low temperature, bounded output) and returns the concatenated text. The
// budget includes the explanation and complete closing JSON delimiters.
func streamCollectText(ctx context.Context, prov llm.Provider, system, user string, maxTokens int) (string, error) {
	temp := 0.0
	req := llm.CompletionRequest{
		System:      []string{system},
		Messages:    []llm.Message{llm.UserText(user)},
		MaxTokens:   maxTokens,
		Temperature: &temp,
		Thinking:    "disabled",
	}
	var sb strings.Builder
	for ev, err := range prov.Stream(ctx, req) {
		if err != nil {
			return "", err
		}
		if ev.Type == llm.SETextDelta {
			sb.WriteString(ev.Text)
		}
	}
	return sb.String(), nil
}

// --- intercept rule CRUD ---

func (s *Server) interceptListRules(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	rules, err := pg.ListInterceptRules()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if rules == nil {
		rules = []db.InterceptRule{}
	}
	writeJSON(w, 200, map[string]any{"rules": rules})
}

func (s *Server) interceptCreateRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req interceptRuleReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := validateInterceptRuleReq(req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rule, err := pg.CreateInterceptRule(req.Name, req.MatchTarget, req.MatchType, req.Pattern, req.Action, req.Message, req.Priority, req.Enabled, req.TimeoutEnabled, req.TimeoutSeconds, req.TimeoutAction)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.m.interceptor.Invalidate()
	writeJSON(w, 200, rule)
}

func (s *Server) interceptUpdateRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad rule id")
		return
	}
	var req interceptRuleReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := validateInterceptRuleReq(req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rule, err := pg.UpdateInterceptRule(id, req.Name, req.MatchTarget, req.MatchType, req.Pattern, req.Action, req.Message, req.Priority, req.Enabled, req.TimeoutEnabled, req.TimeoutSeconds, req.TimeoutAction)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.m.interceptor.Invalidate()
	writeJSON(w, 200, rule)
}

func (s *Server) interceptDeleteRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad rule id")
		return
	}
	if err := pg.DeleteInterceptRule(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.m.interceptor.Invalidate()
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) interceptToggleRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad rule id")
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.ToggleInterceptRule(id, req.Enabled); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.m.interceptor.Invalidate()
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": req.Enabled})
}

// --- pending (ask) ---

func (s *Server) interceptListPending(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	pending, err := pg.ListPendingIntercepts()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if pending == nil {
		pending = []db.InterceptPending{}
	}
	writeJSON(w, 200, map[string]any{"pending": pending})
}

func (s *Server) interceptGetOne(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad pending id")
		return
	}
	p, err := pg.GetInterceptPending(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if p == nil {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) interceptListTaskItems(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID := r.PathValue("taskID")
	if taskID == "" {
		writeErr(w, 400, "bad task id")
		return
	}
	q := r.URL.Query()
	filter, err := interceptFilterParams(q)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if q.Get("page") == "" && q.Get("size") == "" && filter == (db.InterceptApprovalFilter{}) {
		items, err := pg.ListTaskIntercepts(taskID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if items == nil {
			items = []db.InterceptApprovalRow{}
		}
		writeJSON(w, 200, map[string]any{"items": items, "total": len(items)})
		return
	}
	page, size := interceptPageParams(q)
	items, total, err := pg.ListTaskInterceptsPage(taskID, page, size, filter)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if items == nil {
		items = []db.InterceptApprovalRow{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "page": page, "page_size": size})
}

func (s *Server) interceptHistory(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	q := r.URL.Query()
	filter, err := interceptFilterParams(q)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if q.Get("page") == "" && q.Get("size") == "" && filter == (db.InterceptApprovalFilter{}) {
		items, err := pg.ListAllIntercepts(200)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if items == nil {
			items = []db.InterceptApprovalRow{}
		}
		writeJSON(w, 200, map[string]any{"items": items, "total": len(items)})
		return
	}
	page, size := interceptPageParams(q)
	items, total, err := pg.ListAllInterceptsPage(page, size, filter)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if items == nil {
		items = []db.InterceptApprovalRow{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "page": page, "page_size": size})
}

func interceptFilterParams(q url.Values) (db.InterceptApprovalFilter, error) {
	filter := db.InterceptApprovalFilter{Status: q.Get("status"), DecisionSource: q.Get("decision_source"), DecidedBy: q.Get("decided_by")}
	switch filter.Status {
	case "", "all", "pending", "allowed", "denied", "timeout":
		if filter.Status == "all" {
			filter.Status = ""
		}
	default:
		return filter, fmt.Errorf("status must be pending, allowed, denied or timeout")
	}
	switch filter.DecisionSource {
	case "", "all", "model", "rule", "unknown":
		if filter.DecisionSource == "all" {
			filter.DecisionSource = ""
		}
	default:
		return filter, fmt.Errorf("decision_source must be model, rule or unknown")
	}
	switch filter.DecidedBy {
	case "", "all", "human", "rule", "model", "system":
		if filter.DecidedBy == "all" {
			filter.DecidedBy = ""
		}
	default:
		return filter, fmt.Errorf("decided_by must be human, rule, model or system")
	}
	return filter, nil
}

func interceptPageParams(q url.Values) (int, int) {
	page := atoiDefault(q.Get("page"), 1)
	size := atoiDefault(q.Get("size"), 20)
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	return page, size
}

func (s *Server) interceptDecide(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad pending id")
		return
	}
	var req struct {
		Decision string `json:"decision"` // "allowed" | "denied"
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Decision != "allowed" && req.Decision != "denied" {
		writeErr(w, 400, "decision must be allowed or denied")
		return
	}
	if err := s.m.interceptor.Decide(id, req.Decision == "allowed"); err != nil {
		if errors.Is(err, intercept.ErrAlreadyDecided) {
			writeErr(w, 409, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// alwaysAllowPriority places "always allow" rules below every default and
// manual rule, so any deny or ask rule that matches the same call wins.
const alwaysAllowPriority = -1000

// interceptAlwaysAllow creates an argv allow rule from a pending Bash request.
// The request body is never read: the command comes from the stored row, so a
// caller cannot choose what gets allowed. The request itself stays pending.
func (s *Server) interceptAlwaysAllow(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad pending id")
		return
	}
	p, err := pg.GetInterceptPending(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if p == nil {
		writeErr(w, 404, "not found")
		return
	}
	if p.Status != "pending" {
		writeErr(w, 409, "the request is no longer pending")
		return
	}
	prefix, err := intercept.AlwaysAllowPrefix(p.ToolName, p.ToolInput)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	pattern, _ := json.Marshal(prefix)
	rules, err := pg.ListInterceptRules()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, rule := range rules {
		if rule.MatchType != "argv" || rule.Action != "allow" || !rule.Enabled || rule.MatchTarget != "tool_input" {
			continue
		}
		if tokens, err := intercept.ParseArgvPattern(rule.Pattern); err == nil && slices.Equal(tokens, prefix) {
			writeJSON(w, 200, map[string]any{"rule": rule, "created": false})
			return
		}
	}
	// Timeout fields take the rule form's defaults.
	rule, err := pg.CreateInterceptRule("always-allow: "+strings.Join(prefix, " "), "tool_input", "argv", string(pattern),
		"allow", "", alwaysAllowPriority, true, true, 60, "deny")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.m.interceptor.Invalidate()
	writeJSON(w, 200, map[string]any{"rule": rule, "created": true})
}

// --- tool-config (global tool intercept scope) ---

// interceptGetToolConfig returns the list of tool names that are currently
// configured to enter the intercept rule system.
func (s *Server) interceptGetToolConfig(w http.ResponseWriter, r *http.Request) {
	tools, err := s.m.interceptor.GetEnabledTools()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"enabled_tools": tools})
}

// interceptSetToolConfig replaces the list of tool names that should enter
// the intercept rule system.
func (s *Server) interceptSetToolConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnabledTools []string `json:"enabled_tools"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.EnabledTools == nil {
		req.EnabledTools = []string{}
	}
	if err := s.m.interceptor.SetEnabledTools(req.EnabledTools); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// --- LLM fallback judge config (global model fallback) ---

// interceptGetJudgeConfig returns the resolved judge configuration. Prompt is the
// effective prompt (built-in template when unset), so the UI can prefill it.
func (s *Server) interceptGetJudgeConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.m.interceptor.GetJudgeConfig())
}

// interceptSetJudgeConfig persists the judge configuration.
func (s *Server) interceptSetJudgeConfig(w http.ResponseWriter, r *http.Request) {
	var req intercept.JudgeConfig
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	switch req.Mode {
	case "", intercept.JudgeModeClosed, intercept.JudgeModeFreeform:
	default:
		writeErr(w, 400, "mode must be closed or freeform")
		return
	}
	switch req.FailAction {
	case "allow", "ask", "deny":
	default:
		writeErr(w, 400, "fail_action must be allow, ask or deny")
		return
	}
	switch req.AskTimeoutAction {
	case "allow", "deny":
	default:
		writeErr(w, 400, "ask_timeout_action must be allow or deny")
		return
	}
	if err := s.m.interceptor.SetJudgeConfig(req); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// interceptJudgeUsage returns the fallback judge's cumulative token spend plus a
// recent daily series, for the config page. ?days bounds the daily series (default 30).
func (s *Server) interceptJudgeUsage(w http.ResponseWriter, r *http.Request) {
	pg := s.m.PG()
	if pg == nil {
		writeJSON(w, 200, db.JudgeUsage{Daily: []db.JudgeDayUsage{}})
		return
	}
	days := atoiDefault(r.URL.Query().Get("days"), 30)
	usage, err := pg.JudgeUsageStats(days)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, usage)
}

// --- helpers ---

type interceptRuleReq struct {
	Name           string `json:"name"`
	Enabled        bool   `json:"enabled"`
	Priority       int    `json:"priority"`
	MatchTarget    string `json:"match_target"`
	MatchType      string `json:"match_type"`
	Pattern        string `json:"pattern"`
	Action         string `json:"action"`
	Message        string `json:"message"`
	TimeoutEnabled bool   `json:"timeout_enabled"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	TimeoutAction  string `json:"timeout_action"`
}

func validateInterceptRuleReq(req interceptRuleReq) error {
	if req.Name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	switch req.MatchTarget {
	case "tool_name", "tool_input":
	default:
		return fmt.Errorf("match_target must be tool_name or tool_input")
	}
	if req.Pattern == "" {
		return fmt.Errorf("pattern cannot be empty")
	}
	switch req.Action {
	case "allow", "deny", "ask":
	default:
		return fmt.Errorf("action must be allow, deny or ask")
	}
	switch req.MatchType {
	case "string":
	case "regex":
		if _, err := regexp.Compile(req.Pattern); err != nil {
			return fmt.Errorf("pattern is not a valid regex: %w", err)
		}
	case "argv":
		if req.MatchTarget != "tool_input" {
			return fmt.Errorf("an argv rule matches tool_input (the Bash command)")
		}
		tokens, err := intercept.ParseArgvPattern(req.Pattern)
		if err != nil {
			return fmt.Errorf("invalid argv pattern: %w", err)
		}
		if req.Action == "allow" {
			if err := intercept.CheckArgvAllow(tokens); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("match_type must be string, regex or argv")
	}
	return nil
}

func (s *Server) interceptDetail(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad approval id")
		return
	}
	detail, err := pg.GetInterceptDetail(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if detail == nil {
		writeErr(w, 404, "not found")
		return
	}
	// The "always allow" button label: the prefix the endpoint would allow,
	// derived here so the UI never parses the command.
	out := struct {
		*db.InterceptDetail
		AlwaysAllowPrefix string `json:"always_allow_prefix,omitempty"`
	}{InterceptDetail: detail}
	if detail.Status == "pending" {
		if prefix, err := intercept.AlwaysAllowPrefix(detail.ToolName, detail.ToolInput); err == nil {
			out.AlwaysAllowPrefix = strings.Join(prefix, " ")
		}
	}
	writeJSON(w, 200, out)
}

// The navigation endpoint returns only the original call and its paired result.
func (s *Server) interceptExecution(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad approval id")
		return
	}
	target, err := pg.GetInterceptExecution(id)
	if errors.Is(err, db.ErrInterceptTaskDeleted) || errors.Is(err, db.ErrInterceptSessionDeleted) {
		writeErr(w, http.StatusGone, err.Error())
		return
	}
	if errors.Is(err, db.ErrInterceptExecutionUnavailable) {
		writeErr(w, 409, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if target == nil {
		// Conversation deletion cascades approval rows. A stale source link still
		// carries its conversation ID, allowing a precise message without retaining
		// deleted conversations or changing their deletion semantics.
		if convID, parseErr := strconv.ParseInt(r.URL.Query().Get("conversation"), 10, 64); parseErr == nil && convID > 0 {
			conv, getErr := pg.GetConversation(convID)
			if getErr != nil {
				writeErr(w, 500, getErr.Error())
				return
			}
			if conv == nil {
				writeErr(w, http.StatusGone, "对话已被删除")
				return
			}
		}
		writeErr(w, 404, "approval record not found or deleted")
		return
	}
	writeJSON(w, 200, map[string]any{"conversation_id": target.ConversationID, "task_id": target.TaskID, "session": target.Session, "seq": target.Seq, "items": activityDTOs(target.Items)})
}
