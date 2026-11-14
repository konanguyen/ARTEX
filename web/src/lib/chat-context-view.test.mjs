import test from "node:test";
import assert from "node:assert/strict";
import { formatChatContext, chatCompactDefaults, canCompactChatContext } from "./chat-context-view.ts";

const view = {
  source: "request",
  model: "small",
  window: 32000,
  used: 1000,
  headroom: 26904,
  output_reserve: 4096,
  estimated: true,
  categories: [{ name: "conversation", tokens: 900, estimated: true }],
};

test("context usage labels estimates in every supported locale", () => {
  for (const [locale, word] of [["en", "estimated"], ["en-US", "estimated"], ["zh-CN", "估算"], ["vi-VN", "estimated"]]) {
    const v = formatChatContext(view, locale);
    assert.ok(v.usage.includes(word), v.usage);
    assert.ok(v.categories[0].label.includes(word));
  }
});

test("unobserved capacity is explicit and lower model window changes the denominator", () => {
  assert.match(formatChatContext(null, "en").usage, /observed|chưa|观测/i);
  assert.match(formatChatContext(view, "en").usage, /32,000/);
  assert.match(formatChatContext({ ...view, estimated: false, measured_input: 1000 }, "en").usage, /measured/);
});

test("compact retains two exchanges by default and requires idle observed history", () => {
  assert.equal(chatCompactDefaults.keep_recent, 2);
  const state = { active: null, inputs: [], capabilities: { compact: true } };
  assert.equal(canCompactChatContext(state, view), true);
  assert.equal(canCompactChatContext({ ...state, active: { id: "run" } }, view), false);
  assert.equal(canCompactChatContext({ ...state, context_changing: true }, view), false);
  assert.equal(canCompactChatContext({ ...state, inputs: [{ status: "unknown" }] }, view), false);
  assert.equal(canCompactChatContext(state, null), false);
});
