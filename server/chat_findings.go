package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) domainToolRegistry() map[string]actool.CoreTool {
	registry := buildDomainReg(s.m.Assets())
	if registry == nil {
		registry = make(map[string]actool.CoreTool)
	}
	metadata := agent.NewToolSet(nil, "")
	for _, tool := range []actool.CoreTool{metadata.ReportFindingTool(), metadata.ListFindingsTool()} {
		registry[tool.Name()] = conversationFindingTool{CoreTool: tool, server: s}
	}
	return registry
}

// Task-bound tools already in a runner's base set retain precedence over these
// registry entries. Chat tools resolve their graph from server-owned context only.
type conversationFindingTool struct {
	actool.CoreTool
	server *Server
}

func (t conversationFindingTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	conversationID := intercept.ConvIDFromContext(ctx)
	if conversationID <= 0 {
		return actool.Errorf("conversation not found"), nil
	}
	store, agentKey, err := t.server.m.pg.ConversationExploration(ctx, conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return actool.Errorf("conversation not found"), nil
	}
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	tools := agent.NewToolSet(store, agentKey)
	if assets := t.server.m.Assets(); assets != nil {
		tools.SetAssetStore(assets, assets.Companies())
	}
	tools.SetFindingRecorder(t.server.evidenceStore())
	if t.Name() == "report_finding" {
		return tools.ReportFindingTool().Call(ctx, input, tc)
	}
	return tools.ListFindingsTool().Call(ctx, input, tc)
}

func (s *Server) pgConversationFindings(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	store, _, err := s.m.pg.ConversationExploration(r.Context(), c.ID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	findings, err := store.ListByKind(db.KindFinding, 1000)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, findings)
}
