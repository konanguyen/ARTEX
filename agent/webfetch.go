package agent

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"

	"github.com/Autumn-27/artex/egress"
	actool "github.com/Autumn-27/norma/tool"
)

// withWebFetch returns tools plus the WebFetch tool of an agent session. Each
// call goes through the shared egress governor (see governedFetch). tools is
// not modified.
func withWebFetch(ctx context.Context, tools []actool.CoreTool, proxy, caCert string) []actool.CoreTool {
	webFetch := actool.NewWebFetch(actool.WebFetchConfig{Proxy: proxy, CACert: caCert})
	return append(tools[:len(tools):len(tools)], governedFetch{webFetch, egress.Shared()})
}

// governedFetch runs each WebFetch call as one send under the egress governor
// (off by default; then the call runs unchanged), under a noise profile:
// the rate, jitter and concurrency of the URL's host, and, within one task,
// identical calls (WebFetch only sends GET) coalesced and cached. A failed call
// is not cached. WebFetch ignores tc, so a shared result fits every caller.
// Norma owns the HTTP client, so redirect hops are not paced separately and a
// 429/503 is not retried.
type governedFetch struct {
	actool.CoreTool
	g *egress.Governor
}

func (f governedFetch) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	var in struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(input, &in) != nil {
		return f.CoreTool.Call(ctx, input, tc)
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Host == "" {
		return f.CoreTool.Call(ctx, input, tc) // the tool reports the bad URL
	}
	r, err := egress.Do(f.g, ctx, u, string(input), func() (actool.Result, bool, error) {
		r, err := f.CoreTool.Call(ctx, input, tc)
		return r, err == nil && !r.IsError, err
	})
	r.Content = slices.Clone(r.Content) // each caller gets its own slice
	return r, err
}
