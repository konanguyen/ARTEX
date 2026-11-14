import { en } from "./i18n/catalogs/en-us";
import { vi } from "./i18n/catalogs/vi-vn";
import { zhCN } from "./i18n/catalogs/zh-cn";
import { translate } from "./i18n/dictionary";
import { DEFAULT_LOCALE, resolveLocale, type Locale } from "./i18n/locale";
import { getLocale } from "./i18n/i18n-provider";
import type { ChatContextView, ChatControlState } from "./types";

const CATALOGS: Record<Locale, typeof zhCN> = {
  "zh-CN": zhCN,
  "en-US": en,
  "vi-VN": vi,
};

function normalizeLocale(locale: unknown): Locale {
  if (locale === "zh-Hans" || locale === "zh") return "zh-CN";
  return resolveLocale(locale);
}

function translateForLocale(locale: string | undefined, key: string): string {
  const resolved = normalizeLocale(locale);
  const dict = CATALOGS[resolved] ?? CATALOGS[DEFAULT_LOCALE];
  return translate(dict, zhCN, key);
}

export const chatCompactDefaults = { focus: "", keep_recent: 2 };

export function canCompactChatContext(state: ChatControlState | null, view: ChatContextView | null) {
  return (
    !!state?.capabilities.compact &&
    !state.active &&
    !state.context_changing &&
    !state.inputs.some((item) => item.status === "unknown") &&
    !!view &&
    view.source !== "unobserved"
  );
}

export function formatChatContext(view: ChatContextView | null, locale?: string) {
  const activeLocale = normalizeLocale(locale ?? getLocale());
  const t = (key: string) => translateForLocale(activeLocale, key);
  const number = (n: number) => new Intl.NumberFormat(activeLocale).format(n);
  const unknown = !view || view.source === "unobserved";
  const label = view?.estimated ? t("chat.context.estimated") : t("chat.context.measured");
  const names: Record<string, string> = {
    system: "chat.context.system",
    tools: "chat.context.tools",
    skills: "chat.context.skills",
    mcp: "chat.context.mcp",
    conversation: "chat.context.conversation",
    documents: "chat.context.documents",
    memory: "chat.context.memory",
    other: "chat.context.other",
  };
  return {
    usage: unknown
      ? t("chat.context.unknown")
      : `${number(view.used)} / ${view.window > 0 ? number(view.window) : "?"} · ${label}`,
    headroom: unknown || view.window <= 0 ? "?" : number(view.headroom),
    categories: (view?.categories ?? []).map((category) => ({
      name: t(names[category.name] ?? names.other),
      label: `${number(category.tokens)} · ${t("chat.context.estimated")}`,
    })),
  };
}
