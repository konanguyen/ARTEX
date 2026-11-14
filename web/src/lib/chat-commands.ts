import type { ChatCompactOptions, ChatSessionRef } from "./types.ts";

export type ChatCommand =
  | { kind: "queue" | "steer"; text: string }
  | { kind: "retry" | "context" | "usage" }
  | { kind: "branch"; title: string }
  | { kind: "compact"; options: ChatCompactOptions }
  | { kind: "invalid"; message: string };
export function parseChatCommand(text: string): ChatCommand | null {
  const match = /^\/(queue|steer|retry|branch|context|usage|compress)(?:\s+([\s\S]*))?$/.exec(text.trim());
  if (!match) return null;
  const [, name, raw = ""] = match;
  const payload = raw.trim();
  const invalid: ChatCommand = { kind: "invalid", message: "chatCommandInvalid" };
  if (name === "queue" || name === "steer") return payload ? { kind: name, text: raw } : invalid;
  if (name === "branch") return { kind: "branch", title: payload };
  if (name === "compress") {
    const count = /(?:^|\s)(-?\d+)$/.exec(payload);
    const keep = count ? Number(count[1]) : 2;
    if (!Number.isSafeInteger(keep) || keep < 1) return invalid;
    return {
      kind: "compact",
      options: { focus: count ? payload.slice(0, count.index).trim() : payload, keep_recent: keep },
    };
  }
  if (payload) return invalid;
  return { kind: name as "retry" | "context" | "usage" };
}
export function chatControlsBase(ref: ChatSessionRef): string {
  return ref.conversation_id !== "0"
    ? `/conversations/${encodeURIComponent(ref.conversation_id)}/controls`
    : `/tasks/${encodeURIComponent(ref.task_id)}/chat/controls?seg=${ref.main_seg}`;
}
export function chatControlsURL(ref: ChatSessionRef, path = "", query = ""): string {
  const [base, search] = chatControlsBase(ref).split("?");
  return `${base}${path ? `/${path}` : ""}${search || query ? `?${[search, query].filter(Boolean).join("&")}` : ""}`;
}
