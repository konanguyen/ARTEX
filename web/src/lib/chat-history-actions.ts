import type { Activity, ChatControlState, ChatPrompt } from "./types.ts";

export function availableHistoryActions(
  activity: Activity,
  state: ChatControlState,
): { edit: boolean; retry: boolean; branch: boolean } {
  const available =
    !!activity.turn_id &&
    !!activity.run_id &&
    activity.active_attempt !== false &&
    activity.history_mutable === true &&
    !state.active &&
    !state.inputs.some((input) => input.status === "unknown");
  const latest = activity.turn_id === state.latest_turn_id;
  return {
    edit: available && activity.kind === "user" && !!state.capabilities.edit,
    retry:
      available &&
      latest &&
      !!state.capabilities.retry &&
      (activity.kind === "result" || (activity.kind === "user" && state.latest_run_status !== "completed")),
    branch: available && activity.kind === "result" && !!state.capabilities.branch,
  };
}
// Prompt APIs provide the original structured DTO. Plain strings, including JSON
// written by the user, remain text and are never interpreted as attachments.
export function parseChatHistoryPrompt(value: unknown): ChatPrompt {
  if (typeof value === "string") return { text: value, attachments: [] };
  if (
    !value ||
    typeof value !== "object" ||
    !("text" in value) ||
    typeof value.text !== "string" ||
    !("attachments" in value) ||
    (value.attachments !== null && !Array.isArray(value.attachments))
  )
    throw new Error("Invalid chat prompt");
  const attachments = (value.attachments ?? []) as unknown[];
  for (const item of attachments) {
    if (
      !item ||
      typeof item !== "object" ||
      !("name" in item) ||
      typeof item.name !== "string" ||
      !("path" in item) ||
      typeof item.path !== "string" ||
      !/^uploads\/[^\\]+$/.test(item.path) ||
      item.path.split("/").some((part) => part === ".." || part === ".") ||
      !("size" in item) ||
      typeof item.size !== "number" ||
      !Number.isSafeInteger(item.size) ||
      item.size < 0
    )
      throw new Error("Invalid attachment manifest");
  }
  return { text: value.text, attachments: attachments as ChatPrompt["attachments"] };
}
