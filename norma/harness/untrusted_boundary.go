package harness

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
)

// The untrusted-output boundary (opt-in via agentcore.Options.UntrustedOutputBoundary)
// frames every tool result's text between nonce-tagged markers, so the model reads
// tool output as data rather than instructions. Pentest hosts feed target-controlled
// content through tools: a page can contain text addressed to an AI, and an unfamed
// tool result reads exactly like the agent's own instructions.
//
// The frame is code-owned — hosts cannot forget it and the model cannot opt out:
//   - each frame carries a fresh crypto/rand nonce, so a nonce seen earlier in the
//     conversation cannot close a later frame;
//   - content that itself contains a marker stem is neutralised first (the
//     replacement forms no new stem with its neighbours), so target-controlled text
//     can neither open nor close a frame whatever it copies.
const (
	toolDataStem        = "ARTEX_TOOL_DATA"
	toolDataOpenPrefix  = "[[" + toolDataStem + " nonce="
	toolDataClosePrefix = "[[END_" + toolDataStem + " nonce="
	toolDataNonceBytes  = 16
)

// toolDataNeutraliser rewrites every marker stem in tool output before framing.
// The replacement starts with a lowercase letter and holds no uppercase 'A', so
// the rewritten text cannot form a new stem with its neighbours.
var toolDataNeutraliser = strings.NewReplacer(toolDataStem, "tool-data-marker-removed")

// frameUntrustedToolOutput frames one tool-result text block.
func frameUntrustedToolOutput(text string) string {
	b := make([]byte, toolDataNonceBytes)
	_, _ = rand.Read(b) // never returns an error since Go 1.24: it crashes the program instead
	nonce := hex.EncodeToString(b)
	return toolDataOpenPrefix + nonce + "]]\n" +
		toolDataNeutraliser.Replace(text) + "\n" +
		toolDataClosePrefix + nonce + "]]"
}

// frameUntrustedResult frames every text block of a tool result. Non-text blocks
// pass through untouched. Runs after the output cap, so a spilled/truncated head
// is framed like any other text.
func frameUntrustedResult(res tool.Result) tool.Result {
	for i, b := range res.Content {
		if b.Type == llm.BlockText {
			res.Content[i].Text = frameUntrustedToolOutput(b.Text)
		}
	}
	return res
}
