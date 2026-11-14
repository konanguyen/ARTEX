import type { CompanyScopeKind, CompanyScopeRule } from "@/lib/types";

export const MAX_COMPANY_SCOPE_VALUE_LENGTH = 1024;

export type CompanyScopeTextIssue = {
  line: number;
  rule?: CompanyScopeRule;
  error?: string;
};

export type ParsedCompanyScopeText = {
  rules: CompanyScopeRule[];
  errors: Array<{ line: number; error: string }>;
};

export const COMPANY_SCOPE_KINDS: Record<CompanyScopeKind, true> = {
  domain: true,
  host: true,
  ip: true,
  cidr: true,
  icp: true,
  keyword: true,
};

// A textarea line written as `host:<name>` is a `host` rule; every other dotted
// name is a `domain` rule. Saved host rules are shown with the prefix so they
// keep their kind when the scope is edited.
export const HOST_SCOPE_PREFIX = "host:";

export function companyScopeRuleText(rule: CompanyScopeRule): string {
  return rule.kind === "host" ? `${HOST_SCOPE_PREFIX}${rule.value}` : rule.value;
}

export function isCompanyScopeKind(value: unknown): value is CompanyScopeKind {
  return typeof value === "string" && Object.hasOwn(COMPANY_SCOPE_KINDS, value);
}

function isIPv4(value: string): boolean {
  const parts = value.split(".");
  return (
    parts.length === 4 && parts.every((part) => (part === "0" || /^[1-9]\d{0,2}$/.test(part)) && Number(part) <= 255)
  );
}

function isIPv6(value: string): boolean {
  if (!value.includes(":") || /[^0-9a-f:.]/i.test(value) || value.includes(":::")) return false;
  const halves = value.split("::");
  if (halves.length > 2) return false;
  const countSegments = (half: string): number | null => {
    if (!half) return 0;
    const segments = half.split(":");
    let count = 0;
    for (const [index, segment] of segments.entries()) {
      if (segment.includes(".")) {
        if (index !== segments.length - 1 || !isIPv4(segment)) return null;
        count += 2;
      } else {
        if (!/^[0-9a-f]{1,4}$/i.test(segment)) return null;
        count++;
      }
    }
    return count;
  };
  const left = countSegments(halves[0]);
  const right = countSegments(halves[1] ?? "");
  if (left === null || right === null) return false;
  return halves.length === 2 ? left + right < 8 : left === 8;
}

function ipVersion(value: string): 4 | 6 | null {
  if (isIPv4(value)) return 4;
  if (isIPv6(value)) return 6;
  return null;
}

function looksLikeIPAddress(value: string): boolean {
  const trimmed = value.trim();
  if (trimmed.includes("://") || /\s/.test(trimmed)) return false;
  if ((trimmed.match(/:/g) ?? []).length >= 2) {
    const parts = trimmed.split(":");
    let validSegments = 0;
    for (const part of parts) {
      if (!part) {
        if (validSegments > 0 || trimmed.startsWith("::")) return true;
        continue;
      }
      if (part.length > 4 || !/^[0-9a-f]+$/i.test(part)) return false;
      validSegments++;
      if (validSegments >= 2) return true;
    }
    return false;
  }
  return trimmed.includes(".") && /^[0-9.]+$/.test(trimmed);
}

function domainHostname(value: string): string | null {
  try {
    let candidate = value;
    if (value.startsWith("//")) candidate = `http:${value}`;
    else if (!value.includes("://")) candidate = `http://${value}`;
    const url = new URL(candidate);
    return (
      url.hostname
        .replace(/^\[|\]$/g, "")
        .replace(/\.$/, "")
        .toLowerCase() || null
    );
  } catch {
    return null;
  }
}

export function companyScopeRuleError(
  rule: CompanyScopeRule,
  t?: (key: string, values?: Record<string, string | number>) => string,
): string {
  const tr = (k: string, fallback: string, vals?: Record<string, string | number>) => {
    if (!t) return fallback;
    const res = t(k, vals);
    return res && res !== k ? res : fallback;
  };
  const value = rule.value.trim();
  if (!value) return tr("assets.scopeError.required", "请填写范围值");
  if (Array.from(value).length > MAX_COMPANY_SCOPE_VALUE_LENGTH) {
    return tr("assets.scopeError.maxLength", `最多 ${MAX_COMPANY_SCOPE_VALUE_LENGTH} 个字符`, {
      max: MAX_COMPANY_SCOPE_VALUE_LENGTH,
    });
  }
  // host shares the domain hostname syntax; the kinds differ only in matching breadth.
  if (rule.kind === "domain" || rule.kind === "host") {
    if (/\s/.test(value)) return tr("assets.scopeError.domainNoSpaces", "请输入不含空格的有效域名或 URL");
    const hostname = domainHostname(value);
    if (!hostname || ipVersion(hostname) !== null)
      return tr("assets.scopeError.validDomainOrUrl", "请输入有效域名或 URL");
    const labels = hostname.split(".");
    if (
      hostname.length > 253 ||
      labels.length < 2 ||
      labels.some(
        (label) =>
          !label || label.length > 63 || label.startsWith("-") || label.endsWith("-") || !/^[a-z0-9-]+$/i.test(label),
      )
    ) {
      return tr("assets.scopeError.validDomainOrUrl", "请输入有效域名或 URL");
    }
  }
  if (rule.kind === "ip" && ipVersion(value) === null) return tr("assets.scopeError.validIp", "请输入有效 IP");
  if (rule.kind === "cidr") {
    const separator = value.lastIndexOf("/");
    if (separator <= 0) return tr("assets.scopeError.validCidr", "请输入 CIDR 网段");
    const address = value.slice(0, separator);
    const prefixText = value.slice(separator + 1);
    const version = ipVersion(address);
    if (version === null || !/^\d+$/.test(prefixText)) return tr("assets.scopeError.validCidr", "请输入 CIDR 网段");
    const prefix = Number(prefixText);
    if (version === 4 && (prefix < 16 || prefix > 32))
      return tr("assets.scopeError.ipv4Prefix", "IPv4 网段前缀需为 /16 至 /32");
    if (version === 6 && (prefix < 32 || prefix > 128))
      return tr("assets.scopeError.ipv6Prefix", "IPv6 网段前缀需为 /32 至 /128");
  }
  return "";
}

export function classifyCompanyScopeLine(
  raw: string,
  line: number,
  preservedRule?: CompanyScopeRule,
  t?: (key: string, values?: Record<string, string | number>) => string,
): CompanyScopeTextIssue {
  const tr = (k: string, fallback: string, vals?: Record<string, string | number>) => {
    if (!t) return fallback;
    const res = t(k, vals);
    return res && res !== k ? res : fallback;
  };
  const value = raw.trim();
  if (Array.from(value).length > MAX_COMPANY_SCOPE_VALUE_LENGTH) {
    return {
      line,
      error: tr("assets.scopeError.maxLength", `最多 ${MAX_COMPANY_SCOPE_VALUE_LENGTH} 个字符`, {
        max: MAX_COMPANY_SCOPE_VALUE_LENGTH,
      }),
    };
  }
  if (value.toLowerCase().startsWith(HOST_SCOPE_PREFIX)) {
    const rule: CompanyScopeRule = { kind: "host", value: value.slice(HOST_SCOPE_PREFIX.length).trim() };
    const error = companyScopeRuleError(rule, t);
    return error ? { line, error } : { line, rule };
  }
  if (preservedRule) {
    const rule = { kind: preservedRule.kind, value };
    const error = companyScopeRuleError(rule, t);
    return error ? { line, error } : { line, rule };
  }

  const separator = value.lastIndexOf("/");
  if (
    separator > 0 &&
    (ipVersion(value.slice(0, separator)) !== null || looksLikeIPAddress(value.slice(0, separator)))
  ) {
    const rule: CompanyScopeRule = { kind: "cidr", value };
    const error = companyScopeRuleError(rule, t);
    return error ? { line, error } : { line, rule };
  }
  if (ipVersion(value) !== null) return { line, rule: { kind: "ip", value } };
  if (looksLikeIPAddress(value)) return { line, error: tr("assets.scopeError.validIp", "请输入有效 IP") };

  const looksLikeDomain = value.includes("://") || (!/\s/.test(value) && value.includes("."));
  if (looksLikeDomain) {
    const hostname = domainHostname(value);
    if (hostname && ipVersion(hostname) !== null) return { line, rule: { kind: "ip", value: hostname } };
    const rule: CompanyScopeRule = { kind: "domain", value };
    const error = companyScopeRuleError(rule, t);
    return error ? { line, error } : { line, rule };
  }
  if (/icp|备案/i.test(value)) return { line, rule: { kind: "icp", value } };
  return { line, rule: { kind: "keyword", value } };
}

export function parseCompanyScopeText(
  value: string,
  options: {
    preservedRules?: CompanyScopeRule[];
    t?: (key: string, values?: Record<string, string | number>) => string;
  } = {},
): ParsedCompanyScopeText {
  const nonEmpty = value
    .split(/\r?\n/)
    .map((raw, index) => ({ raw, line: index + 1 }))
    .filter(({ raw }) => raw.trim());
  const preserved = new Map<string, CompanyScopeRule[]>();
  for (const rule of options.preservedRules ?? []) {
    const key = rule.value.trim();
    preserved.set(key, [...(preserved.get(key) ?? []), rule]);
  }
  const issues = nonEmpty.map(({ raw, line }) => {
    const key = raw.trim();
    const queue = preserved.get(key);
    const preservedRule = queue?.shift();
    return classifyCompanyScopeLine(raw, line, preservedRule, options.t);
  });
  return {
    rules: issues.flatMap((item) => (item.rule ? [item.rule] : [])),
    errors: issues.flatMap((item) => (item.error ? [{ line: item.line, error: item.error }] : [])),
  };
}

export function normalizeCompanyScopeValue(rule: CompanyScopeRule): string {
  const value = rule.value.trim();
  if (rule.kind === "domain" || rule.kind === "host") return domainHostname(value) ?? value.toLowerCase();
  if (rule.kind === "icp") return value.toLowerCase().replace(/\s/gu, "");
  if (rule.kind === "keyword") return value.toLowerCase().split(/\s+/u).filter(Boolean).join(" ");
  return value.toLowerCase();
}
