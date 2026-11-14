package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel 实现飞书（含 Lark）自定义机器人，走交互式卡片。
//
// 平台特性：
//   - 加签算法与钉钉**不同**，且极易写错，见 feishuSign 注释。
//   - 与钉钉一样把业务错误塞在 HTTP 200 的 body 里（code != 0）。
//   - 卡片 header 支持颜色模板，用级别映射配色，让人在消息列表里一眼看出严重程度。
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// 飞书自定义机器人约 5 次/秒，折合 100 次/分钟。
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook 地址末段即机器人唯一标识，属凭据。
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 同理：改 Webhook 地址必须对新地址重新表态签名密钥。
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Missing webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Invalid webhook URL: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 加签参数与消息同层，且只在配置了 secret 时出现。
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 部分版本的飞书 hook 用这套字段名，一并兼容。
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Failed to parse Feishu response: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("Feishu returned error %d: %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("Feishu returned error %d: %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign 按飞书官方规则计算签名。
//
// 这里特别容易踩坑：官方样例是
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// 也就是 **key = timestamp + "\n" + secret，message 为空**，而不是直觉上的
// 「key=secret, message=stringToSign」——那正是钉钉的算法。两边算法刚好反过来，
// 照着另一家的实现写必然签名校验失败（报 19021）。
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate 把漏洞级别映射到卡片 header 配色模板。
// 未知级别用 grey——不用 blue，免得和 low 混淆。
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes is a conservative cap on card content. Feishu custom bots
// reject a request body over 20 KB as a whole (not truncated); pick a value
// clearly below that limit to also cover JSON wrapping overhead.
const feishuMaxCardBytes = 16000

// feishuEnvelopeBytes is the size of what Send serializes around the card when a
// signing secret is configured (without one the request is smaller):
//
//	{"card":…,"msg_type":"interactive","sign":"<44 characters>","timestamp":"<10 digits>"}
const feishuEnvelopeBytes = 113

// feishuCard 构造交互式卡片，返回卡片与**实际写入的条目数**。
// kept 的用途同 markdownBody：只有真正进了卡片的条目才该被标记为已送达。
func feishuCard(m Message) (map[string]any, int) {
	lim := m.limits()
	// budget is the final truncation backstop for one item's text, the same role
	// TruncateBytes plays in markdownBody. With the summary or asset limit set to
	// 0 an item has no other bound, and an oversized card loses the whole message.
	budget := feishuMaxCardBytes - markdownReservedBytes
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	elements := []any{}
	kept := 0
	if m.Batch {
		// digest assembles the elements of a digest card around the lines of its
		// first n items. The intro must state "the remaining N items will continue
		// in the next message", and N must come from the number of items that
		// actually fit.
		digest := func(n int, lines []any) []any {
			out := []any{feishuMarkdownDiv(markdownBatchIntro(m, m.Items[:n], len(m.Items)))}
			out = append(out, lines...)
			if m.HomeURL != "" {
				out = append(out, feishuButton("View All on Platform", m.HomeURL))
			}
			return out
		}
		// Pack whole items (see packItemCount) against the request body as it will
		// be serialized, not against the text of the lines. Each line is sent as
		// its own element: about 52 bytes of JSON around the text, plus whatever
		// the encoding escapes. Counted as text alone, several hundred short lines
		// left as one request of more than twice the 20 KB limit. The rest of the
		// body is measured too: it changes with the number of items kept (the
		// intro) and with the platform link.
		//
		// The first item is always kept: otherwise one oversized item would jam
		// the whole batch in place. Its text is truncated to budget.
		var lines []any
		used := 0
		for i, it := range m.Items {
			line := feishuMarkdownDiv(TruncateBytes(feishuBatchLine(it, i+1, lim), budget))
			used += feishuJSONSize(line) + 1 // the element and its separating comma
			card["elements"] = digest(i+1, nil)
			if i > 0 && feishuEnvelopeBytes+feishuJSONSize(card)+used > feishuMaxCardBytes {
				break
			}
			lines = append(lines, line)
		}
		kept = len(lines)
		elements = digest(kept, lines)
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(TruncateBytes(feishuItemLines(it, lim), budget)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("View Details", it.DetailURL))
		}
	}

	card["elements"] = elements
	return card, kept
}

// feishuJSONSize returns the number of bytes v takes in the request body.
func feishuJSONSize(v any) int {
	raw, _ := json.Marshal(v)
	return len(raw)
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines 渲染单个漏洞的 lark_md 正文。
//
// lark_md 与 markdown 是同族的文本格式，同样会解析链接与强调，所以来自外部
// 的字段一律过 markdownText（单行化 + 转义）——否则一条漏洞标题就能在
// 飞书里变成可点击的外链。
func feishuItemLines(it Item, lim Limits) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**Status Change**: %s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**Type**: %s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, lim.AssetsShown); a != "" {
		out += fmt.Sprintf("\n**Asset**: %s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, lim.SummaryRunes); s != "" {
			out += fmt.Sprintf("\n**Summary**: %s", s)
		}
	}
	return out
}

// feishuBatchLine 渲染汇总卡片里的一条。
func feishuBatchLine(it Item, index int, lim Limits) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, lim.AssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
