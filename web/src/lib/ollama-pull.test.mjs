import { pullCommand } from "./ollama-pull.ts";
import assert from "node:assert/strict";
import test from "node:test";

const body = `{"error":"model 'absent-model:1b' not found"}`;

test("pull hint keeps the exact model tag in every locale", () => {
  const en = `ollama: HTTP 404: model absent-model:1b was not found; run ollama pull absent-model:1b: ${body}`;
  const ko = `ollama: HTTP 404: 모델 absent-model:1b을 찾을 수 없습니다. ollama pull absent-model:1b을 실행하세요: ${body}`;
  const zh = `ollama：HTTP 404：未找到模型 absent-model:1b；请运行 ollama pull absent-model:1b：${body}`;
  for (const error of [en, ko, zh]) assert.equal(pullCommand(error, "typed"), "ollama pull absent-model:1b");
});

test("pull hint preserves registry ports, namespaces and dotted tags", () => {
  const model = "registry.local:5000/team/qwen3.5:7b-q4_K_M";
  assert.equal(pullCommand(`run ollama pull ${model}: ${body}`, "x"), `ollama pull ${model}`);
  assert.equal(pullCommand(`run ollama pull ${model}.`, "x"), `ollama pull ${model}`);
});

test("pull hint keeps configured names ending in a hyphen or dot in every locale", () => {
  for (const model of ["registry.local:5000/team/custom-", "team/custom:v1.", "custom:q4-"]) {
    const errors = [
      `ollama: HTTP 404: model ${model} was not found; run ollama pull ${model}: ${body}`,
      `ollama: HTTP 404: 모델 ${model}을 찾을 수 없습니다. ollama pull ${model}을 실행하세요: ${body}`,
      `ollama：HTTP 404：未找到模型 ${model}；请运行 ollama pull ${model}：${body}`,
      `run ollama pull ${model}.`,
      `run ollama pull ${model}`,
    ];
    for (const error of errors) assert.equal(pullCommand(error, model), `ollama pull ${model}`);
  }
});

test("pull hint ignores a configured model that is only a prefix of the named one", () => {
  for (const named of ["qwen:4b", "qwen:4.5", "qwen:4-q8", "qwen:4/x"]) {
    assert.equal(pullCommand(`run ollama pull ${named}: ${body}`, "qwen:4"), `ollama pull ${named}`);
  }
  // Without the configured name, trailing "-" survives and one prose separator is dropped.
  assert.equal(pullCommand(`run ollama pull team/custom-: ${body}`, "other"), "ollama pull team/custom-");
  assert.equal(pullCommand(`run ollama pull team/custom-.`, "other"), "ollama pull team/custom-");
});

test("pull hint falls back to the entered model and is absent for unrelated errors", () => {
  assert.equal(pullCommand("run ollama pull", " llama3:8b "), "ollama pull llama3:8b");
  assert.equal(pullCommand("ollama: HTTP 500: connection refused", "llama3:8b"), "");
  assert.equal(pullCommand("", "llama3:8b"), "");
});
