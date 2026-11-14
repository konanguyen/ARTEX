export const LLM_PROFILE_PRESET_IDS = ["opencode-go"] as const;
export type LLMProfilePresetId = (typeof LLM_PROFILE_PRESET_IDS)[number];

export interface LLMProfilePresetFields {
  format: "openai" | "anthropic" | "openai-responses";
  model: string;
  base_url: string;
  thinking_type: "" | "disabled" | "enabled";
  reasoning_effort: "" | "low" | "medium" | "high" | "xhigh" | "max";
  context_window_k: number;
  max_tokens: number;
  max_tokens_field: "" | "max_completion_tokens";
  session_header_key: string;
}

const PRESETS: Record<LLMProfilePresetId, LLMProfilePresetFields> = {
  // OpenCode Go subscription gateway (@ai-sdk/openai-compatible at
  // https://opencode.ai/zen/go/v1). Requests without x-opencode-session are
  // rejected with 400 MissingSessionID, so the template prefills it.
  "opencode-go": {
    format: "openai",
    base_url: "https://opencode.ai/zen/go/v1",
    model: "glm-5.3-flash",
    thinking_type: "",
    reasoning_effort: "max",
    context_window_k: 0,
    max_tokens: 16384,
    max_tokens_field: "",
    session_header_key: "x-opencode-session",
  },
};

export function isLLMProfilePresetId(value: string): value is LLMProfilePresetId {
  return Object.hasOwn(PRESETS, value);
}

export function getLLMProfilePreset(id: LLMProfilePresetId): Readonly<LLMProfilePresetFields> {
  return PRESETS[id];
}

// Applies preset fields onto a new profile draft without mutating the input and
// keeping any already-entered API key intact.
export function applyLLMProfilePreset<T extends object>(draft: T, id: LLMProfilePresetId): T & LLMProfilePresetFields {
  return { ...draft, ...getLLMProfilePreset(id) };
}
