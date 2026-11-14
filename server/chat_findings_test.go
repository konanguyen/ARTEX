package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestPentestChatPersistsFindingsWithoutTask(t *testing.T) {
	m := backportTestManager(t)
	oldAugment, oldResolve, oldPrompt := agent.ToolAugment, agent.ToolResolve, agent.PromptOverride
	oldBinding := agent.FindingTrafficBindingEnabled
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = oldAugment, oldResolve, oldPrompt
		agent.FindingTrafficBindingEnabled = oldBinding
	})
	s := New(t.Context(), m, t.TempDir(), t.TempDir(), t.TempDir())
	conv, err := m.pg.CreateConversation("pentest", "Standalone finding fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteConversation(conv.ID) })
	summary := fmt.Sprintf("standalone chat finding %d", conv.ID)
	evidence := "Offline fixture command returned the expected proof."
	var tasksBefore int
	if err := m.pg.QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasksBefore); err != nil {
		t.Fatal(err)
	}
	turn := 0
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		turn++
		if turn == 1 {
			input, _ := json.Marshal(map[string]any{"vulnclass": "TEST", "severity": "low", "summary": summary, "evidence": evidence})
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "save-finding", Name: "report_finding", Input: input}}}, "tool_use", llm.Usage{}, nil
		}
		if turn == 2 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "read-findings", Name: "list_findings", Input: json.RawMessage(`{}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "Saved."}}}, "end_turn", llm.Usage{}, nil
	}}
	chat := agent.NewChatAgent(provider, "fixture", m.dir, nil, 10000)
	chat.SetNonStreaming(func() bool { return true })
	ctx := intercept.WithConvID(t.Context(), conv.ID)
	var recorded db.RecordedFinding
	var listed []map[string]any
	_, err = chat.Chat(ctx, "pentest", s.convBusyKey(conv.ID), "Save the supplied offline finding fixture.", 6, 0, false, func(activity db.Activity) {
		if activity.Kind != "tool_result" {
			return
		}
		if activity.IsError {
			t.Errorf("%s failed: %s", activity.Tool, activity.Detail)
			return
		}
		switch activity.Tool {
		case "report_finding":
			parts := strings.SplitN(activity.Detail, "\n", 2)
			if len(parts) != 2 {
				t.Errorf("missing finding identifiers: %s", activity.Detail)
				return
			}
			if err := json.Unmarshal([]byte(parts[1]), &recorded); err != nil {
				t.Error(err)
			}
		case "list_findings":
			if err := json.Unmarshal([]byte(activity.Detail), &listed); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if recorded.FindingID == 0 || recorded.NodeID == 0 {
		t.Fatal("standalone chat did not return persistent finding and node IDs")
	}
	t.Cleanup(func() { _, _ = m.pg.DeleteFinding(recorded.FindingID) })
	finding, err := m.pg.GetFinding(recorded.FindingID)
	if err != nil || finding == nil || finding.TaskID != nil || finding.NodeID == nil || *finding.NodeID != recorded.NodeID || finding.Worker != "pentest" || finding.Evidence != evidence {
		t.Fatalf("finding was not preserved independently of tasks: %+v, %v", finding, err)
	}
	if len(listed) != 1 || listed[0]["summary"] != summary {
		t.Fatalf("chat could not read its saved finding: %+v", listed)
	}
	var tasksAfter int
	if err := m.pg.QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasksAfter); err != nil || tasksAfter != tasksBefore {
		t.Fatalf("saving a chat finding created an autonomous task: %d -> %d, %v", tasksBefore, tasksAfter, err)
	}

	// A fresh turn resolves fresh tools, so this also checks persistence beyond one run.
	tools, _, cleanup := agent.AugmentTools(ctx, "pentest", actool.DefaultTools())
	defer cleanup()
	result := workflowCall(t, ctx, workflowTool(t, tools, "list_findings"), map[string]any{}, false)
	if !strings.Contains(result, summary) {
		t.Fatal("a later turn lost its finding")
	}
	other, err := m.pg.CreateConversation("pentest", "Isolated conversation", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteConversation(other.ID) })
	otherCtx := intercept.WithConvID(t.Context(), other.ID)
	result = workflowCall(t, otherCtx, workflowTool(t, tools, "list_findings"), map[string]any{"conversation_id": conv.ID}, false)
	if result != "[]" {
		t.Fatalf("another conversation could read this finding: %s", result)
	}
	workflowCall(t, t.Context(), workflowTool(t, tools, "report_finding"), map[string]any{"vulnclass": "TEST", "severity": "low", "summary": "no conversation"}, true)

	// The normal report/evidence APIs must accept the IDs returned to chat.
	trafficResult := workflowCall(t, ctx, s.toolGetFindingTraffic(), map[string]any{"finding_id": strconv.FormatInt(recorded.FindingID, 10)}, false)
	if !strings.Contains(trafficResult, "version") {
		t.Fatal("standalone finding has no readable evidence version")
	}
	workflowCall(t, ctx, s.toolUpdateFindingReport(), map[string]any{"finding_id": recorded.NodeID, "report": "Verified standalone fixture.", "evidence_version": recorded.Traffic.Version}, false)

	// Captured evidence uses the same atomic recorder as task findings.
	if m.traffic == nil {
		m.traffic, err = traffic.Open(filepath.Join(m.dir, "chat-traffic"), ":0")
		if err != nil {
			t.Fatal(err)
		}
	}
	previousBinding := m.pg.GetBool(settingAgentTrafficBinding, false)
	if err := m.pg.SetBool(settingAgentTrafficBinding, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.SetBool(settingAgentTrafficBinding, previousBinding) })
	seedServerEvidenceFlow(t, s, "chat-proof", []byte("saved chat proof"))
	result = workflowCall(t, ctx, workflowTool(t, tools, "report_finding"), map[string]any{
		"vulnclass": "TEST", "severity": "low", "summary": "captured chat fixture", "evidence": evidence,
		"traffic_refs": []db.TrafficRef{{TrafficID: "chat-proof", Role: "proof"}},
	}, false)
	var bound db.RecordedFinding
	if err := json.Unmarshal([]byte(strings.SplitN(result, "\n", 2)[1]), &bound); err != nil || bound.Traffic == nil || len(bound.Traffic.Bindings) != 1 {
		t.Fatalf("chat lost its bound traffic: %s, %v", result, err)
	}
	t.Cleanup(func() { _, _ = m.pg.DeleteFinding(bound.FindingID) })
	for _, extra := range []map[string]any{
		{"traffic_refs": []db.TrafficRef{{TrafficID: "missing-chat-proof"}}},
		{"intent_id": recorded.NodeID},
		{"evidence_hint_id": recorded.NodeID},
	} {
		input := map[string]any{"vulnclass": "TEST", "severity": "low", "summary": "invalid chat fixture"}
		for key, value := range extra {
			input[key] = value
		}
		workflowCall(t, ctx, workflowTool(t, tools, "report_finding"), input, true)
	}
	result = workflowCall(t, ctx, workflowTool(t, tools, "list_findings"), map[string]any{}, false)
	if err := json.Unmarshal([]byte(result), &listed); err != nil || len(listed) != 2 {
		t.Fatalf("failed reports left partial findings: %s, %v", result, err)
	}
	if err := m.pg.DeleteConversation(conv.ID); err != nil {
		t.Fatal(err)
	}
	if finding, err := m.pg.GetFinding(recorded.FindingID); err != nil || finding == nil || finding.Evidence != evidence {
		t.Fatal("deleting a conversation lost its durable finding", err)
	}
}
