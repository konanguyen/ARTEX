"use client";

import * as React from "react";

import {
  BotIcon,
  CheckIcon,
  ChevronDownIcon,
  ChevronRightIcon,
  ClipboardListIcon,
  CopyIcon,
  RefreshCwIcon,
  ShieldAlertIcon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";

import { TablePagination } from "@/components/table-pagination";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useI18n } from "@/lib/i18n";
import { api } from "@/lib/api";
import type {
  InterceptApprovalFilter,
  InterceptApprovalRow,
  InterceptAudit,
  InterceptDetail,
  InterceptPending,
  InterceptReviewInput,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// Judge verdicts carry a [model] prefix; older rows may use [Model] or [模型].
function isModelReason(reason?: string) {
  if (!reason) return false;
  return reason.toLowerCase().startsWith("[model]") || reason.startsWith("[模型]");
}

function fmtTime(value?: string) {
  if (!value) return "—";
  return new Date(value).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function source(row: InterceptApprovalRow) {
  if (row.decision_source) return row.decision_source;
  if (row.rule_id) return "rule";
  return isModelReason(row.reason) ? "model" : "unknown";
}

function originLabel(row: InterceptApprovalRow, t?: (k: string, v?: Record<string, string | number>) => string) {
  if (row.task_id) return row.task_id;
  if (row.conversation_id) return t ? t("approval.origin.conversation", { id: row.conversation_id }) : `#${row.conversation_id}`;
  return "—";
}

function ApprovalOrigin({ row, detail = false }: { row: InterceptApprovalRow; detail?: boolean }) {
  const { t } = useI18n();
  const [locating, setLocating] = React.useState(false);
  const label = detail && row.task_id ? t("approval.origin.task", { id: row.task_id }) : originLabel(row, t);
  const query = new URLSearchParams({ approval: String(row.id) });
  let href: string | undefined;
  if (row.conversation_id) {
    query.set("c", String(row.conversation_id));
    href = `/chat?${query}`;
  } else if (row.task_id) {
    query.set("id", row.task_id);
    href = `/function/tasks/detail?${query}`;
  }
  return href ? (
    <a
      href={href}
      className="text-primary underline-offset-4 hover:underline"
      aria-label={t("approval.origin.locateAria", { id: row.id, label })}
      aria-busy={locating}
      onClick={async (e) => {
        e.stopPropagation();
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
        e.preventDefault();
        if (locating) return;
        setLocating(true);
        try {
          await api.interceptExecution(row.id, row.conversation_id ?? undefined);
          window.location.assign(href);
        } catch (error) {
          toast.error((error as Error).message || t("approval.origin.locateFailed"));
          setLocating(false);
        }
      }}
    >
      {label}
    </a>
  ) : (
    <span>{label}</span>
  );
}

function StatusBadge({ status }: { status: string }) {
  const { t } = useI18n();
  const labels: Record<string, string> = {
    pending: t("approval.filter.pending"),
    allowed: t("approval.filter.allowed"),
    denied: t("approval.filter.denied"),
    timeout: t("approval.filter.timeout"),
  };
  let variant: "default" | "destructive" | "secondary" | "outline" = "outline";
  if (status === "allowed") variant = "default";
  if (status === "denied") variant = "destructive";
  if (status === "pending") variant = "secondary";
  return <Badge variant={variant}>{labels[status] ?? status}</Badge>;
}

// decided_by is shown exactly as stored, so a row always sits under the
// "Decided by" filter option matching its badge. A decided row without an actor
// predates the column: it reads "Legacy" and matches no option.
function DecidedByBadge({ row }: { row: InterceptApprovalRow }) {
  const { t } = useI18n();
  if (row.status === "pending") return <span className="text-muted-foreground">—</span>;
  if (!row.decided_by) {
    return (
      <Badge variant="outline" className="text-muted-foreground">
        {t("approval.decidedBy.legacy")}
      </Badge>
    );
  }
  const labels: Record<string, string> = {
    human: t("approval.decidedBy.human"),
    rule: t("approval.decidedBy.rule"),
    model: t("approval.decidedBy.model"),
    system: t("approval.decidedBy.system"),
  };
  return <Badge variant="outline">{labels[row.decided_by] ?? row.decided_by}</Badge>;
}

function MatchCell({ row, showReason = true }: { row: InterceptApprovalRow; showReason?: boolean }) {
  const { t } = useI18n();
  const reason = row.reason?.replace(/^\[(?:模型|Model)\]\s*/i, "");
  return (
    <div className="flex min-w-0 flex-col gap-1">
      {source(row) === "model" ? (
        <Badge variant="outline">
          <BotIcon />
          {t("approval.filter.model")}
        </Badge>
      ) : (
        <span className="truncate">{row.rule_name || t("approval.rule.unrecordedOrDeleted")}</span>
      )}
      {showReason ? (
        <p className="truncate text-muted-foreground text-xs" title={reason}>
          {reason || t("approval.reason.unrecorded")}
        </p>
      ) : null}
    </div>
  );
}

function CodeBlock({ label, text, truncated = false }: { label: string; text: string; truncated?: boolean }) {
  const { t } = useI18n();
  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      toast.success(t("common.copied"));
    } catch {
      toast.error(t("common.copyFailed"));
    }
  }
  return (
    <section className="flex min-w-0 flex-col gap-2" aria-label={label}>
      <div className="flex items-center justify-between gap-2">
        <h3 className="font-medium text-muted-foreground text-xs">{label}</h3>
        {text ? (
          <Button variant="ghost" size="icon-xs" aria-label={t("approval.codeBlock.copyAria", { label })} onClick={() => void copy()}>
            <CopyIcon />
          </Button>
        ) : null}
      </div>
      <pre className="max-h-80 min-w-0 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-muted/60 p-3 font-mono text-xs leading-6 [overflow-wrap:anywhere]">
        {text || t("approval.unrecorded")}
      </pre>
      {truncated ? <p className="text-muted-foreground text-xs">{t("approval.truncatedHint")}</p> : null}
    </section>
  );
}

const getContextLabels = (t: (key: string) => string): Record<string, string> => ({
  user: t("approval.context.userMessage"),
  assistant: t("approval.context.agentMessage"),
  text: t("approval.context.agentMessage"),
  tool_use: t("approval.context.toolRequest"),
  tool_result: t("approval.context.toolOutput"),
});

const getActionLabels = (t: (key: string) => string): Record<string, string> => ({
  allow: t("approval.action.allow"),
  ask: t("approval.action.askHuman"),
  deny: t("approval.action.deny"),
});

const getExecutionLabels = (t: (key: string) => string): Record<InterceptAudit["execution_status"], string> => ({
  not_started: t("approval.execution.notStarted"),
  not_executed: t("approval.execution.notExecuted"),
  awaiting_result: t("approval.execution.awaitingResult"),
  succeeded: t("approval.execution.succeeded"),
  failed: t("approval.execution.failed"),
  unknown: t("approval.execution.unknown"),
});

function ModelReviewContext({ input }: { input: InterceptReviewInput }) {
  const { t } = useI18n();
  return (
    <section className="flex min-w-0 flex-col gap-4" aria-label={t("approval.review.contextTitle")}>
      <div className="flex flex-col gap-2">
        <h3 className="font-medium text-sm">{t("approval.review.contextTitle")}</h3>
        <p className="text-muted-foreground text-xs">
          {t("approval.review.contextDesc")}
          {input.version < 4 ? t("approval.review.legacyInputDesc") : null}
        </p>
      </div>
      <CodeBlock
        label={t("approval.review.currentCall")}
        text={JSON.stringify({ tool_name: input.tool_name, arguments: input.arguments }, null, 2)}
      />
      {input.version >= 2 ? (
        input.background ? (
          <div className="flex min-w-0 flex-col gap-2">
            <CodeBlock
              label={input.background.source === "user_message" ? t("approval.review.bgUserMessage") : t("approval.review.bgWorkerSummary")}
              text={input.background.text}
              truncated={input.background.truncated}
            />
            <p className="text-muted-foreground text-xs">
              {input.background.source === "user_message"
                ? t("approval.review.bgUserMessageHint")
                : t("approval.review.bgWorkerSummaryHint")}
            </p>
          </div>
        ) : (
          <p className="text-muted-foreground text-xs">{t("approval.review.noBackground")}</p>
        )
      ) : (
        <>
          {input.turn_input ? (
            <CodeBlock label={t("approval.review.turnInputLegacy")} text={input.turn_input} truncated={input.background_truncated} />
          ) : null}
          {input.task ? (
            <>
              <CodeBlock label={t("approval.review.taskDescLegacy")} text={input.task.description} truncated={input.task.truncated} />
              <CodeBlock label={t("approval.review.taskGoalLegacy")} text={input.task.goal} truncated={input.task.truncated} />
              <CodeBlock label={t("approval.review.taskConstraintsLegacy")} text={JSON.stringify(input.task.constraints, null, 2)} />
            </>
          ) : null}
          {input.worker_intent ? (
            <CodeBlock label={t("approval.review.workerIntentLegacy")} text={input.worker_intent} truncated={input.background_truncated} />
          ) : null}
        </>
      )}
      {input.working_directory ? <CodeBlock label={t("approval.review.workingDir")} text={input.working_directory} /> : null}
      {input.version >= 3 ? (
        <p className="text-muted-foreground text-xs">{t("approval.review.noHistoryCalls")}</p>
      ) : (
        <div className="flex min-w-0 flex-col gap-3">
          <h4 className="font-medium text-muted-foreground text-xs">{t("approval.review.historyCallsTitle")}</h4>
          {input.history?.length ? (
            input.history.map((entry) => (
              <div key={entry.tool_use_id} className="flex min-w-0 flex-col gap-2 rounded-lg border p-3">
                <p className="break-words font-medium text-xs">
                  {entry.tool} · {entry.status === "succeeded" ? t("approval.review.success") : t("approval.review.failedSideEffects")}
                </p>
                <CodeBlock label={t("approval.review.historyArgs")} text={entry.arguments_preview} truncated={entry.truncated} />
                <CodeBlock label={t("approval.review.historyResult")} text={entry.result} truncated={entry.truncated} />
              </div>
            ))
          ) : (
            <p className="text-muted-foreground text-xs">{t("approval.review.noHistoryRecords")}</p>
          )}
          {input.history_truncated ? (
            <p className="text-muted-foreground text-xs">{t("approval.review.historyTruncated")}</p>
          ) : null}
        </div>
      )}

      <Collapsible>
        <CollapsibleTrigger asChild>
          <Button variant="outline" size="sm" className="self-start">
            <ChevronDownIcon data-icon="inline-start" />
            {t("approval.review.viewFullJson")}
          </Button>
        </CollapsibleTrigger>
        <CollapsibleContent className="pt-3">
          <CodeBlock label={t("approval.review.fullJsonLabel")} text={JSON.stringify(input, null, 2)} />
        </CollapsibleContent>
      </Collapsible>
    </section>
  );
}

type Decide = (id: number, decision: "allowed" | "denied") => Promise<void>;

function DecisionActions({ row, busy, decide }: { row: InterceptApprovalRow; busy: boolean; decide: Decide }) {
  const { t } = useI18n();
  if (row.status !== "pending") return null;
  return (
    <div className="flex flex-wrap gap-2">
      <Button size="sm" disabled={busy} onClick={() => void decide(row.id, "allowed")}>
        <CheckIcon data-icon="inline-start" />
        {t("approval.action.allow")}
      </Button>
      <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide(row.id, "denied")}>
        <XIcon data-icon="inline-start" />
        {t("approval.action.deny")}
      </Button>
    </div>
  );
}

// AlwaysAllowAction creates an argv allow rule from a pending Bash request.
// The prefix comes from the server; the request itself stays pending.
function AlwaysAllowAction({ id, prefix }: { id: number; prefix?: string }) {
  const { t } = useI18n();
  const [busy, setBusy] = React.useState(false);
  async function create() {
    setBusy(true);
    try {
      const res = await api.interceptAlwaysAllow(id);
      toast.success(res.created ? t("approval.alwaysAllow.created") : t("approval.alwaysAllow.exists"));
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="flex flex-col gap-1">
      <Button size="sm" variant="outline" className="self-start" disabled={busy} onClick={() => void create()}>
        {prefix ? t("approval.alwaysAllow.buttonPrefix", { prefix }) : t("approval.alwaysAllow.button")}
      </Button>
      <p className="text-muted-foreground text-xs">{t("approval.alwaysAllow.hint")}</p>
    </div>
  );
}

export function ApprovalDetail({
  row,
  busy,
  decide,
  revision,
  readOnly = false,
  defaultExpanded = false,
  onResolved,
}: {
  row: InterceptApprovalRow;
  busy: boolean;
  decide: Decide;
  revision: number;
  readOnly?: boolean;
  defaultExpanded?: boolean;
  onResolved?: (status: "allowed" | "denied" | "timeout") => void;
}) {
  const { t } = useI18n();
  const [detail, setDetail] = React.useState<InterceptDetail | null>(null);
  const [error, setError] = React.useState("");
  const [retry, setRetry] = React.useState(0);
  const [more, setMore] = React.useState(defaultExpanded);

  // biome-ignore lint/correctness/useExhaustiveDependencies: Status, refresh and retry invalidate details without closing the panel.
  React.useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    async function load() {
      try {
        const next = await api.interceptDetail(row.id);
        if (cancelled) return;
        setDetail(next);
        setError("");
        if (next.status !== "pending") onResolved?.(next.status);
        if (next.status === "pending" || next.audit?.execution_status === "awaiting_result")
          timer = setTimeout(() => void load(), 5000);
      } catch (e) {
        if (!cancelled) setError((e as Error).message || t("approval.detail.loadDetailFailed"));
      }
    }
    void load();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [row.id, row.status, revision, retry, onResolved, t]);

  // Row updates are authoritative until the lazy detail has caught up.
  const current = detail?.status === row.status ? detail : row;
  const audit = detail?.audit;
  const executionLabels = getExecutionLabels(t);
  const actionLabels = getActionLabels(t);
  let execution = audit ? executionLabels[audit.execution_status] : t("approval.unrecorded");
  if (row.status === "pending") execution = t("approval.execution.notStarted");
  if (audit && audit.correlation !== "exact" && audit.effective_action === "allow") execution = t("approval.detail.unlinkedResult");
  const command = typeof row.tool_input?.command === "string" ? row.tool_input.command : undefined;
  let initialLabel = source(row) === "model" ? t("approval.detail.modelInitial") : t("approval.detail.ruleInitial");
  if (audit?.model_fallback) initialLabel = t("approval.detail.modelFallback");

  return (
    <div className="flex min-w-0 flex-col gap-4 p-3 sm:p-5">
      <div className="grid min-w-0 gap-5 rounded-xl border bg-muted/20 p-4 lg:grid-cols-2">
        <div className="flex min-w-0 flex-col gap-3">
          <CodeBlock
            label={`${current.agent_name || current.conv_agent_key || "Agent"} · ${t("approval.context.toolRequest")}`}
            text={JSON.stringify(row.tool_input ?? {}, null, 2)}
          />
          {command ? (
            <Collapsible>
              <CollapsibleTrigger asChild>
                <Button variant="ghost" size="sm">
                  <ChevronDownIcon data-icon="inline-start" />
                  {t("approval.detail.viewCommand")}
                </Button>
              </CollapsibleTrigger>
              <CollapsibleContent className="pt-2">
                <CodeBlock label={t("approval.detail.commandContent")} text={command} />
              </CollapsibleContent>
            </Collapsible>
          ) : null}
          <p className="text-muted-foreground text-xs">
            {t("approval.detail.executionResult")}：<span className="text-foreground">{detail ? execution : t("action.loading")}</span>
          </p>
        </div>
        <div className="flex min-w-0 flex-col gap-4 lg:border-l lg:pl-5">
          <h3 className="font-medium text-muted-foreground text-xs">
            {current.status === "pending" ? t("approval.detail.status") : t("approval.detail.decision")}
          </h3>
          <div className="flex flex-wrap items-center gap-2">
            <StatusBadge status={current.status} />
            <MatchCell row={current} showReason={false} />
          </div>
          <p className="whitespace-pre-wrap break-words text-sm leading-7 [overflow-wrap:anywhere]">
            {current.reason?.replace(/^\[(?:模型|Model)\]\s*/i, "") || t("approval.detail.noReason")}
          </p>
          {audit?.decision_reason ? <p className="text-sm">{audit.decision_reason}</p> : null}
          {audit?.effective_action ? <p className="text-sm">{t("approval.detail.effectiveAction", { action: actionLabels[audit.effective_action] ?? audit.effective_action })}</p> : null}
          <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-2 text-xs">
            <dt className="text-muted-foreground">{t("approval.th.source")}</dt>
            <dd className="break-words">
              <ApprovalOrigin row={current} detail />
            </dd>
            <dt className="text-muted-foreground">{t("approval.th.appliedAt")}</dt>
            <dd>{fmtTime(row.created_at)}</dd>
            <dt className="text-muted-foreground">{t("approval.th.decidedAt")}</dt>
            <dd>{fmtTime(current.decided_at)}</dd>
            <dt className="text-muted-foreground">{t("approval.th.decidedBy")}</dt>
            <dd>
              <DecidedByBadge row={current} />
            </dd>
            {audit?.rule_name ? (
              <>
                <dt className="text-muted-foreground">{t("approval.detail.ruleSnapshot")}</dt>
                <dd>{audit.rule_name}</dd>
              </>
            ) : null}
            {audit?.profile_id ? (
              <>
                <dt className="text-muted-foreground">{t("approval.detail.profileConfig")}</dt>
                <dd>#{audit.profile_id}</dd>
              </>
            ) : null}
          </dl>
          {!readOnly ? <DecisionActions row={current} busy={busy} decide={decide} /> : null}
          {!readOnly && current.status === "pending" && row.tool_name === "Bash" ? (
            <AlwaysAllowAction id={row.id} prefix={detail?.always_allow_prefix} />
          ) : null}
        </div>
      </div>
      {error ? (
        <Alert variant="destructive">
          <AlertDescription>
            <div className="flex flex-wrap items-center gap-2">
              <span>{t("approval.detail.loadDetailFailed")}：{error}</span>
              <Button variant="outline" size="sm" onClick={() => setRetry((v) => v + 1)}>
                {t("approval.detail.retryDetail")}
              </Button>
            </div>
          </AlertDescription>
        </Alert>
      ) : null}
      {!detail && !error ? <Skeleton className="h-8 w-60" /> : null}
      {detail && !audit ? (
        <Alert>
          <AlertDescription>{t("approval.detail.noSnapshot")}</AlertDescription>
        </Alert>
      ) : null}
      {audit ? (
        <Collapsible open={more} onOpenChange={setMore}>
          <CollapsibleTrigger asChild>
            <Button variant="ghost" size="sm">
              <ChevronDownIcon data-icon="inline-start" className={cn(more && "rotate-180")} />
              {more ? t("approval.detail.hideContext") : t("approval.detail.viewContext")}
            </Button>
          </CollapsibleTrigger>
          <CollapsibleContent className="pt-4">
            <div className="flex min-w-0 flex-col gap-5">
              {audit.model_input ? (
                <ModelReviewContext input={audit.model_input} />
              ) : (
                <Alert>
                  <AlertDescription>
                    {audit.model_input_digest
                      ? t("approval.detail.digestOnlyHint")
                      : t("approval.detail.noModelInputHint")}
                  </AlertDescription>
                </Alert>
              )}
              {audit.user_message || audit.context?.length ? (
                <Collapsible>
                  <CollapsibleTrigger asChild>
                    <Button variant="ghost" size="sm">
                      <ChevronDownIcon data-icon="inline-start" />
                      {audit.model_input && audit.model_input.version >= 3
                        ? t("approval.detail.viewAuditSnippetUnsent")
                        : t("approval.detail.viewAuditSnippet")}
                    </Button>
                  </CollapsibleTrigger>
                  <CollapsibleContent className="pt-3">
                    <div className="flex min-w-0 flex-col gap-3">
                      {audit.user_message ? (
                        <CodeBlock
                          label={t("approval.detail.currentTurnAudit")}
                          text={audit.user_message}
                          truncated={audit.user_truncated}
                        />
                      ) : null}
                      <section className="flex min-w-0 flex-col gap-3">
                        <h3 className="font-medium text-muted-foreground text-xs">{t("approval.detail.visibleContext")}</h3>
                        <p className="text-muted-foreground text-xs">
                          {t("approval.detail.contextCapturedAt", { time: fmtTime(audit.captured_at) })}
                        </p>
                        {audit.context_truncated ? (
                          <p className="text-muted-foreground text-xs">{t("approval.detail.contextTruncatedHint")}</p>
                        ) : null}
                        {audit.context?.length ? (
                          audit.context.map((entry, index) => {
                            const contextLabels = getContextLabels(t);
                            return (
                              <CodeBlock
                                key={`${entry.kind}-${entry.tool_use_id || index}`}
                                label={`${contextLabels[entry.kind] ?? entry.kind}${entry.tool ? ` · ${entry.tool}` : ""}${entry.is_error ? ` · ${t("approval.detail.error")}` : ""}`}
                                text={entry.text}
                                truncated={entry.truncated}
                              />
                            );
                          })
                        ) : (
                          <p className="text-muted-foreground text-sm">{t("approval.detail.noContextRecorded")}</p>
                        )}
                      </section>
                    </div>
                  </CollapsibleContent>
                </Collapsible>
              ) : null}
              <CodeBlock
                label={`${initialLabel}：${actionLabels[audit.initial_action] ?? audit.initial_action}`}
                text={audit.initial_reason.replace(/^\[(?:模型|Model)\]\s*/i, "")}
              />
              {audit.judge_answers?.length ? (
                <CodeBlock
                  label={t("approval.detail.judgeAnswers")}
                  text={audit.judge_answers.map((a) => `${a.id}: ${a.answer}${a.evidence ? ` - ${a.evidence}` : ""}`).join("\n")}
                />
              ) : null}
              <CodeBlock
                label={t("approval.detail.executionOutput")}
                text={audit.output ?? (audit.execution_status === "not_executed" ? t("approval.detail.toolNotExecuted") : t("approval.detail.noExecutionOutput"))}
                truncated={audit.output_truncated}
              />
              {audit.correlation !== "exact" ? (
                <Alert>
                  <AlertDescription>
                    {audit.correlation === "ambiguous"
                      ? t("approval.detail.correlationAmbiguous")
                      : t("approval.detail.noCorrelatedId")}
                  </AlertDescription>
                </Alert>
              ) : null}
              <dl className="grid gap-2 text-muted-foreground text-xs [overflow-wrap:anywhere]">
                <div>{t("approval.detail.toolUseId")}：{audit.tool_use_id || t("approval.unrecorded")}</div>
                <div>{t("approval.detail.inputDigest")}：{audit.input_digest}</div>
                <div>{t("approval.detail.configDigest")}：{audit.config_digest || t("approval.unrecorded")}</div>
                {audit.model_input_digest ? <div>{t("approval.detail.modelInputDigest")}：{audit.model_input_digest}</div> : null}
                {audit.execution_ended_at ? <div>{t("approval.detail.endedAt")}：{fmtTime(audit.execution_ended_at)}</div> : null}
              </dl>
            </div>
          </CollapsibleContent>
        </Collapsible>
      ) : null}
    </div>
  );
}

// Hidden cells do not occupy table columns. Keep the detail span in sync so
// expansion cannot create empty columns and leave a gap in the selected row.
const approvalBreakpoints = ["(min-width: 40rem)", "(min-width: 48rem)", "(min-width: 64rem)", "(min-width: 80rem)"];
function subscribeColumns(onChange: () => void) {
  const queries = approvalBreakpoints.map((query) => window.matchMedia(query));
  for (const query of queries) query.addEventListener("change", onChange);
  return () => {
    for (const query of queries) query.removeEventListener("change", onChange);
  };
}
function visibleColumnCount() {
  const matches = approvalBreakpoints.map((query) => window.matchMedia(query).matches);
  return 3 + Number(matches[0]) + Number(matches[1]) + 3 * Number(matches[2]) + 2 * Number(matches[3]);
}
function serverColumnCount() {
  return 10;
}

function ApprovalTable({
  rows,
  busy,
  decide,
  revision,
  label,
}: {
  rows: InterceptApprovalRow[];
  busy: boolean;
  decide: Decide;
  revision: number;
  label: string;
}) {
  const { t } = useI18n();
  const [expanded, setExpanded] = React.useState<Set<number>>(() => new Set());
  const columns = React.useSyncExternalStore(subscribeColumns, visibleColumnCount, serverColumnCount);
  const prefix = React.useId();
  function toggle(id: number) {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }
  return (
    <Table className="table-fixed" aria-label={label}>
      <TableHeader>
        <TableRow>
          <TableHead className="w-10">
            <span className="sr-only">Toggle</span>
          </TableHead>
          <TableHead className="hidden w-14 sm:table-cell">#</TableHead>
          <TableHead className="w-28">{t("approval.th.tool")}</TableHead>
          <TableHead className="hidden md:table-cell">{t("approval.th.source")}</TableHead>
          <TableHead className="hidden lg:table-cell">{t("approval.th.rule")}</TableHead>
          <TableHead className="hidden xl:table-cell">{t("approval.th.args")}</TableHead>
          <TableHead className="w-24">{t("approval.th.status")}</TableHead>
          <TableHead className="hidden w-24 lg:table-cell">{t("approval.th.decidedBy")}</TableHead>
          <TableHead className="hidden w-36 lg:table-cell">{t("approval.th.appliedAt")}</TableHead>
          <TableHead className="hidden w-36 xl:table-cell">{t("approval.th.decidedAt")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => {
          const open = expanded.has(row.id);
          const panelID = `${prefix}-${row.id}`;
          return (
            <React.Fragment key={row.id}>
              <TableRow
                data-state={open ? "selected" : undefined}
                className="cursor-pointer"
                onClick={() => toggle(row.id)}
              >
                <TableCell>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={open ? t("approval.aria.collapse", { id: row.id }) : t("approval.aria.expand", { id: row.id })}
                    aria-expanded={open}
                    aria-controls={open ? panelID : undefined}
                    onClick={(e) => {
                      e.stopPropagation();
                      toggle(row.id);
                    }}
                  >
                    {open ? <ChevronDownIcon /> : <ChevronRightIcon />}
                  </Button>
                </TableCell>
                <TableCell className="hidden text-muted-foreground sm:table-cell">{row.id}</TableCell>
                <TableCell>
                  <code className="block truncate text-xs">{row.tool_name}</code>
                </TableCell>
                <TableCell className="hidden md:table-cell">
                  <div className="flex flex-col gap-1">
                    <span className="truncate" title={originLabel(row, t)}>
                      <ApprovalOrigin row={row} />
                    </span>
                    <span className="truncate text-muted-foreground text-xs">
                      {row.agent_name || row.conv_agent_key}
                    </span>
                  </div>
                </TableCell>
                <TableCell className="hidden lg:table-cell">
                  <MatchCell row={row} />
                </TableCell>
                <TableCell className="hidden xl:table-cell">
                  <code className="block truncate text-muted-foreground text-xs">{JSON.stringify(row.tool_input)}</code>
                </TableCell>
                <TableCell>
                  <StatusBadge status={row.status} />
                </TableCell>
                <TableCell className="hidden lg:table-cell">
                  <DecidedByBadge row={row} />
                </TableCell>
                <TableCell className="hidden text-muted-foreground text-xs lg:table-cell">
                  {fmtTime(row.created_at)}
                </TableCell>
                <TableCell className="hidden text-muted-foreground text-xs xl:table-cell">
                  {fmtTime(row.decided_at)}
                </TableCell>
              </TableRow>
              {open ? (
                <TableRow className="hover:bg-transparent has-aria-expanded:bg-transparent">
                  <TableCell colSpan={columns} className="whitespace-normal p-0">
                    <section id={panelID} aria-label={t("approval.aria.detail", { id: row.id })}>
                      <ApprovalDetail row={row} busy={busy} decide={decide} revision={revision} />
                    </section>
                  </TableCell>
                </TableRow>
              ) : null}
            </React.Fragment>
          );
        })}
      </TableBody>
    </Table>
  );
}

export function ApprovalRecords({ taskId }: { taskId?: string }) {
  const { t } = useI18n();
  const [rows, setRows] = React.useState<InterceptApprovalRow[]>([]);
  const [pendingRows, setPendingRows] = React.useState<InterceptPending[]>([]);
  const [page, setPage] = React.useState(1);
  const [pageSize, setPageSize] = React.useState(20);
  const [total, setTotal] = React.useState(0);
  const [filter, setFilter] = React.useState<InterceptApprovalFilter>({});
  const [loading, setLoading] = React.useState(true);
  const [refreshing, setRefreshing] = React.useState(false);
  const [error, setError] = React.useState("");
  const [pendingError, setPendingError] = React.useState("");
  const [deciding, setDeciding] = React.useState(false);
  const [revision, setRevision] = React.useState(0);
  const request = React.useRef(0);
  const decisionLock = React.useRef(false);
  const filterID = React.useId();
  const filtered = Boolean(filter.status || filter.decision_source || filter.decided_by);

  // A task can stay mounted while the user switches between task details.
  // Reset the cursor so the new scope always starts at its newest records.
  // biome-ignore lint/correctness/useExhaustiveDependencies: taskId intentionally resets pagination when scope changes.
  React.useEffect(() => {
    setPage(1);
  }, [taskId]);

  const load = React.useCallback(
    async (manual = false) => {
      const id = ++request.current;
      if (manual) setRefreshing(true);
      try {
        // The approval queue is independent of the current history page and its
        // filter. Older requests must remain actionable even when newer decisions
        // fill the page or a filter would hide them.
        const [history, pending] = await Promise.allSettled([
          taskId ? api.interceptTaskPage(taskId, page, pageSize, filter) : api.interceptHistoryPage(page, pageSize, filter),
          api.interceptPending(),
        ]);
        if (id !== request.current) return;
        if (history.status === "fulfilled") {
          setRows(history.value.items);
          setTotal(history.value.total);
          setError("");
        } else {
          setError((history.reason as Error).message || t("common.loadFailed"));
        }
        if (pending.status === "fulfilled") {
          setPendingRows(pending.value);
          setPendingError("");
        } else {
          setPendingError((pending.reason as Error).message || t("common.loadFailed"));
        }
        if (manual) setRevision((v) => v + 1);
      } catch (e) {
        if (id === request.current) setError((e as Error).message || t("common.loadFailed"));
      } finally {
        if (id === request.current) {
          setLoading(false);
          setRefreshing(false);
        }
      }
    },
    [taskId, page, pageSize, filter, t],
  );
  const latestLoad = React.useRef(load);
  latestLoad.current = load;

  React.useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), 5000);
    return () => {
      request.current++;
      clearInterval(timer);
    };
  }, [load]);

  const changePageSize = (next: number) => {
    setPageSize(next);
    setPage(1);
  };

  const changeFilter = (next: InterceptApprovalFilter) => {
    // An in-flight response for the old filter must not repopulate the table.
    request.current++;
    setFilter(next);
    setPage(1);
    setRows([]);
    setTotal(0);
    setError("");
    setLoading(true);
  };

  const decide: Decide = async (id, decision) => {
    if (decisionLock.current) return;
    decisionLock.current = true;
    setDeciding(true);
    try {
      await api.interceptDecide(id, decision);
      // Invalidate a list request started before this decision.
      request.current++;
      // Optimistically drop the decided item from the independent pending queue
      // for instant feedback; the re-fetch below reconciles with server truth.
      setRows((prev) =>
        prev.map((row) =>
          row.id === id ? { ...row, status: decision, decided_by: "human", decided_at: new Date().toISOString() } : row,
        ),
      );
      setPendingRows((prev) => prev.filter((row) => row.id !== id));
      setRevision((v) => v + 1);
      toast.success(decision === "allowed" ? t("approval.toast.allowed") : t("approval.toast.denied"));
      // Re-fetch counts and rows: a decided item may no longer match the filter.
      await latestLoad.current(true);
    } catch (e) {
      toast.error((e as Error).message);
      await latestLoad.current(true);
    } finally {
      decisionLock.current = false;
      setDeciding(false);
    }
  };

  const historyById = new Map(rows.map((row) => [row.id, row]));
  const pending: InterceptApprovalRow[] = pendingRows
    .filter((row) => !taskId || row.task_id === taskId)
    .map((row) => ({
      conv_title: "",
      conv_agent_key: "",
      rule_name: row.rule_id ? t("approval.rule.number", { id: row.rule_id }) : "",
      ...historyById.get(row.id),
      ...row,
    }));
  const title = taskId ? t("approval.taskTitle") : t("approval.title");
  return (
    <div className="flex min-w-0 flex-col gap-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <ClipboardListIcon className="size-5 text-muted-foreground" />
          <h1 className="font-semibold text-xl">{title}</h1>
          {pending.length ? <Badge variant="secondary">{t("approval.pending", { count: pending.length })}</Badge> : null}
        </div>
        <Button variant="outline" size="sm" onClick={() => void load(true)} disabled={loading || refreshing}>
          <RefreshCwIcon data-icon="inline-start" className={cn(refreshing && "animate-spin")} />
          {t("action.refresh")}
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">{t("approval.description")}</p>
      <FieldGroup className="flex-row flex-wrap items-end gap-3" aria-label={t("approval.title")}>
        <Field className="w-full sm:w-40">
          <FieldLabel htmlFor={`${filterID}-status`}>{t("approval.filter.status")}</FieldLabel>
          <Select
            value={filter.status ?? "all"}
            onValueChange={(value) =>
              changeFilter({
                ...filter,
                status: value === "all" ? undefined : (value as InterceptApprovalFilter["status"]),
              })
            }
          >
            <SelectTrigger id={`${filterID}-status`}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value="all">{t("approval.filter.allStatus")}</SelectItem>
                <SelectItem value="denied">{t("approval.filter.denied")}</SelectItem>
                <SelectItem value="pending">{t("approval.filter.pending")}</SelectItem>
                <SelectItem value="allowed">{t("approval.filter.allowed")}</SelectItem>
                <SelectItem value="timeout">{t("approval.filter.timeout")}</SelectItem>
              </SelectGroup>
            </SelectContent>
          </Select>
        </Field>
        <Field className="w-full sm:w-40">
          <FieldLabel htmlFor={`${filterID}-source`}>{t("approval.filter.source")}</FieldLabel>
          <Select
            value={filter.decision_source ?? "all"}
            onValueChange={(value) =>
              changeFilter({
                ...filter,
                decision_source: value === "all" ? undefined : (value as InterceptApprovalFilter["decision_source"]),
              })
            }
          >
            <SelectTrigger id={`${filterID}-source`}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value="all">{t("approval.filter.allSources")}</SelectItem>
                <SelectItem value="model">{t("approval.filter.model")}</SelectItem>
                <SelectItem value="rule">{t("approval.filter.rule")}</SelectItem>
                <SelectItem value="unknown">{t("approval.filter.unknown")}</SelectItem>
              </SelectGroup>
            </SelectContent>
          </Select>
        </Field>
        <Field className="w-full sm:w-40">
          <FieldLabel htmlFor={`${filterID}-decided-by`}>{t("approval.filter.decidedBy")}</FieldLabel>
          <Select
            value={filter.decided_by ?? "all"}
            onValueChange={(value) =>
              changeFilter({
                ...filter,
                decided_by: value === "all" ? undefined : (value as InterceptApprovalFilter["decided_by"]),
              })
            }
          >
            <SelectTrigger id={`${filterID}-decided-by`}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value="all">{t("approval.filter.allActors")}</SelectItem>
                <SelectItem value="model">{t("approval.filter.actorModel")}</SelectItem>
                <SelectItem value="human">{t("approval.filter.actorHuman")}</SelectItem>
                <SelectItem value="rule">{t("approval.filter.actorRule")}</SelectItem>
                <SelectItem value="system">{t("approval.filter.actorSystem")}</SelectItem>
              </SelectGroup>
            </SelectContent>
          </Select>
        </Field>
        {filtered ? (
          <Button variant="ghost" size="sm" onClick={() => changeFilter({})}>
            {t("approval.filter.clear")}
          </Button>
        ) : null}
      </FieldGroup>
      {error ? (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}
      {pendingError ? (
        <Alert variant="destructive">
          <AlertDescription>{pendingError}</AlertDescription>
        </Alert>
      ) : null}
      {pending.length ? (
        <section className="overflow-hidden rounded-xl border">
          <div className="flex items-center gap-2 border-b bg-muted/40 px-4 py-3 font-medium text-sm">
            <ShieldAlertIcon className="size-4" />
            {t("approval.pendingSection", { count: pending.length })}<span className="text-muted-foreground text-xs">{t("approval.pendingHint")}</span>
          </div>
          <ApprovalTable rows={pending} busy={deciding} decide={decide} revision={revision} label={t("approval.taskTitle")} />
        </section>
      ) : null}
      <section className="overflow-hidden rounded-xl border">
        <div className="border-b px-4 py-3 font-medium text-sm">
          {filtered ? t("approval.resultsFiltered", { count: total }) : t("approval.resultsAll", { count: total })}
        </div>
        {loading ? (
          <div className="flex flex-col gap-3 p-4">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        ) : null}
        {!loading && !rows.length && !error ? (
          <Empty>
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <ClipboardListIcon />
              </EmptyMedia>
              <EmptyTitle>{filtered ? t("approval.emptyFiltered") : t("approval.emptyAll")}</EmptyTitle>
              <EmptyDescription>
                {filtered
                  ? t("approval.emptyFilteredHint")
                  : t("approval.emptyAllHint")}
              </EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : null}
        {rows.length ? (
          <ApprovalTable rows={rows} busy={deciding} decide={decide} revision={revision} label={t("approval.title")} />
        ) : null}
        {!loading && !error ? (
          <TablePagination
            page={page}
            pageSize={pageSize}
            total={total}
            onPageChange={setPage}
            onPageSizeChange={changePageSize}
            pageSizeOptions={[10, 20, 50]}
          />
        ) : null}
      </section>
    </div>
  );
}
