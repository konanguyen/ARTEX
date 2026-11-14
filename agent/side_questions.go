package agent

import (
	"context"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
)

func attachSideCapture(ctx context.Context, opts *agentcore.Options) context.Context {
	ri := RunInfoFrom(ctx)
	p := sidequestion.Parent{TaskID: ri.TaskID, ExplorationID: ri.ExplorationID, IntentID: ri.IntentID}
	if strings.HasPrefix(ri.SessionID, "conv-") {
		p.ConversationID, _ = strconv.ParseInt(strings.TrimPrefix(ri.SessionID, "conv-"), 10, 64)
	}
	if p.ConversationID == 0 && (p.TaskID == 0 || p.ExplorationID == 0) {
		return ctx
	}
	if b := chatRunFrom(ctx); b != nil {
		ctx = sidequestion.WithHistory(ctx, b.run.HistoryRevision, b.run.ID)
		if p.TaskID > 0 && p.IntentID == 0 {
			seg := b.run.Session.MainSeg
			p.MainSeg = &seg
		}
	}
	if projector, ok := opts.Compactor.(harness.ContextView); ok {
		ctx = sidequestion.WithWorkingSetProjector(ctx, projector.View)
	}
	ctx, opts.Deps = sidequestion.Attach(ctx, p, opts.Deps, opts.Provider)
	return ctx
}
