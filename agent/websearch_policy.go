package agent

import (
	"context"
	"encoding/json"

	runtimeconfig "github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

// DeepSeekLocalOnlyMessage is shared with the search connectivity probe.
const WebSearchLocalOnlyMessage = "Web search is unavailable while local_llm_only is enabled"

// DeepSeekLocalOnlyMessage remains an alias for callers using the former name.
const DeepSeekLocalOnlyMessage = WebSearchLocalOnlyMessage

// WebsearchAllowed reports whether web search is permitted under the current local policy.
func WebsearchAllowed() bool {
	strict, _ := runtimeconfig.LocalLLMPolicy()
	return !strict
}

// secureWebSearch retains the SDK tool contract and checks live policy before
// every backend invocation, including sessions created before a policy change.
func secureWebSearch(opts *agentcore.Options, policy func() (bool, []string)) {
	if !opts.EnableWebSearch {
		return
	}
	opts.EnableWebSearch = false
	search, err := actool.NewWebSearch(actool.WebSearchConfig{
		Backend: opts.WebSearchBackend, BraveAPIKey: opts.BraveSearchAPIKey, TavilyAPIKey: opts.TavilySearchAPIKey,
		DeepSeekBaseURL: opts.DeepSeekSearchBaseURL, DeepSeekAPIKey: opts.DeepSeekSearchAPIKey, DeepSeekModel: opts.DeepSeekSearchModel,
		Proxy: opts.WebSearchProxy, CACert: opts.WebSearchCACert, InsecureTLS: opts.WebSearchInsecureTLS,
	})
	// Match agentcore's existing behavior: a misconfigured search backend omits
	// the tool and leaves the rest of the session available.
	if err != nil {
		return
	}
	if policy == nil {
		policy = runtimeconfig.LocalLLMPolicy
	}
	opts.Tools = append(opts.Tools, &localOnlyDeepSeekSearch{CoreTool: search, policy: policy})
}

type localOnlyDeepSeekSearch struct {
	actool.CoreTool
	policy func() (bool, []string)
}

func (t *localOnlyDeepSeekSearch) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if strict, _ := t.policy(); strict {
		return actool.Errorf(DeepSeekLocalOnlyMessage), nil
	}
	return t.CoreTool.Call(ctx, input, tc)
}
