"use client";

import * as React from "react";

import { Badge } from "@/components/ui/badge";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Textarea } from "@/components/ui/textarea";
import { useI18n } from "@/lib/i18n";
import type { ParsedCompanyScopeText } from "@/lib/company-scope";
import type { CompanyScopeKind } from "@/lib/types";

export function ScopeTextEditor({
  id,
  value,
  onValueChange,
  parsed,
  label,
  description,
}: {
  id: string;
  value: string;
  onValueChange: (value: string) => void;
  parsed: ParsedCompanyScopeText;
  label?: string;
  description?: string;
}) {
  const { t } = useI18n();
  const resolvedLabel = label ?? t("scope.editor.label");
  const resolvedDescription = description ?? t("scope.editor.description");

  const scopeKindLabels: Record<CompanyScopeKind, string> = {
    domain: t("scope.kind.domain"),
    host: t("companies.scopeKindHost"),
    ip: t("scope.kind.ip"),
    cidr: t("scope.kind.cidr"),
    icp: t("scope.kind.icp"),
    keyword: t("scope.kind.keyword"),
  };

  // Hover hints explain the breadth difference between domain and host rules.
  const scopeKindHints: Partial<Record<CompanyScopeKind, string>> = {
    domain: t("companies.scopeHintDomain"),
    host: t("companies.scopeHintHost"),
  };

  const counts = React.useMemo(() => {
    const result = new Map<CompanyScopeKind, number>();
    for (const rule of parsed.rules) result.set(rule.kind, (result.get(rule.kind) ?? 0) + 1);
    return result;
  }, [parsed.rules]);

  return (
    <Field data-invalid={parsed.errors.length > 0}>
      <FieldLabel htmlFor={id}>{resolvedLabel}</FieldLabel>
      <FieldDescription>{resolvedDescription}</FieldDescription>
      <Textarea
        id={id}
        rows={8}
        value={value}
        aria-invalid={parsed.errors.length > 0}
        placeholder={t("scope.editor.placeholder")}
        className="min-h-36 resize-y font-mono text-sm"
        onChange={(event) => onValueChange(event.target.value)}
      />
      {parsed.rules.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 text-muted-foreground text-xs">
          <span>{t("scope.editor.recognizedCount", { count: parsed.rules.length })}</span>
          {Object.entries(scopeKindLabels).map(([kind, kindLabel]) => {
            const count = counts.get(kind as CompanyScopeKind) ?? 0;
            return count > 0 ? (
              <Badge
                key={kind}
                variant="secondary"
                className="font-mono tabular-nums"
                title={scopeKindHints[kind as CompanyScopeKind]}
              >
                {kindLabel} {count}
              </Badge>
            ) : null;
          })}
        </div>
      )}
      {parsed.errors.length > 0 && (
        <FieldError>
          {parsed.errors.slice(0, 5).map((item) => (
            <span key={`${item.line}-${item.error}`} className="block">
              {t("scope.editor.lineError", { line: item.line, error: item.error })}
            </span>
          ))}
          {parsed.errors.length > 5 && (
            <span className="block">
              {t("scope.editor.moreErrors", { count: parsed.errors.length - 5 })}
            </span>
          )}
        </FieldError>
      )}
    </Field>
  );
}
