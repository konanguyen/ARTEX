"use client";

import * as React from "react";

import Link from "next/link";
import { useSearchParams } from "next/navigation";

import { ArrowLeftIcon, ArrowUpRightIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { CopyButton } from "@/components/copy-button";
import { FindingRetestPanel } from "@/components/finding-retest-panel";
import { FindingTrafficPanel } from "@/components/finding-traffic-panel";
import { Markdown } from "@/components/markdown";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { toBcp47, useI18n } from "@/lib/i18n";
import { api } from "@/lib/api";
import { statusMeta } from "@/lib/status";
import type { Finding, FindingStatus, Severity } from "@/lib/types";

import { VerificationBadge } from "../_components/findings-table";
import { FindingLineageView } from "./lineage";

const SEVERITIES: Severity[] = ["critical", "high", "medium", "low"];
const FINDING_STATUSES: FindingStatus[] = [
  "pending",
  "in_progress",
  "confirmed",
  "resolved",
  "fixed",
  "false_positive",
  "ignored",
  "duplicate",
  "risk_accepted",
];

function fmtTime(ts: string, locale?: string) {
  return new Date(ts).toLocaleString(locale ?? "zh-CN");
}

// FieldRow is one label/value line in the right-hand status panel.
function FieldRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-3 py-2.5">
      <span className="shrink-0 pt-0.5 text-xs text-muted-foreground">{label}</span>
      <div className="flex min-w-0 flex-col items-end gap-1 text-right text-sm">{children}</div>
    </div>
  );
}

function FindingDetailInner() {
  const { t, locale } = useI18n();
  const searchParams = useSearchParams();
  const id = searchParams.get("id") ?? "";
  const contextTaskId = searchParams.get("context_task") ?? "";
  const [finding, setFinding] = React.useState<Finding | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [tab, setTab] = React.useState("overview");

  const load = React.useCallback(() => {
    if (!id) {
      setLoaded(true);
      return;
    }
    api
      .getFinding(id, contextTaskId || undefined)
      .then((f) => setFinding(f))
      .catch(() => setFinding(null))
      .finally(() => setLoaded(true));
  }, [contextTaskId, id]);
  React.useEffect(() => {
    load();
  }, [load]);

  const changeSeverity = React.useCallback(
    async (next: Severity) => {
      if (!finding || finding.inherited || next === finding.severity) return;
      const prev = finding.severity;
      setFinding({ ...finding, severity: next });
      try {
        const updated = await api.setFindingSeverity(id, next);
        setFinding(updated);
        toast.success(t("findings.toast.severityChanged", { severity: statusMeta("severity", next).label }));
      } catch (e) {
        setFinding((cur) => (cur ? { ...cur, severity: prev } : cur));
        toast.error(t("findings.toast.updateFailed", { message: (e as Error).message }));
      }
    },
    [finding, id, t],
  );

  const changeStatus = React.useCallback(
    async (next: FindingStatus) => {
      if (!finding || finding.inherited || next === finding.status) return;
      const prev = finding.status;
      setFinding({ ...finding, status: next });
      try {
        const updated = await api.setFindingStatus(id, next);
        setFinding(updated);
        toast.success(t("findings.toast.statusChanged", { status: statusMeta("finding", next).label }));
      } catch (e) {
        setFinding((cur) => (cur ? { ...cur, status: prev } : cur));
        toast.error(t("findings.toast.updateFailed", { message: (e as Error).message }));
      }
    },
    [finding, id, t],
  );

  if (!finding) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3 p-10 text-center">
        <p className="text-muted-foreground">{loaded ? t("findings.detail.notFound", { id }) : t("findings.loading")}</p>
        {loaded && (
          <Button asChild variant="outline">
            <Link href="/function/findings">
              <ArrowLeftIcon /> {t("findings.detail.backToList")}
            </Link>
          </Button>
        )}
      </div>
    );
  }

  const title = finding.name || finding.vulnclass || t("findings.table.unclassified");

  return (
    <Tabs value={tab} onValueChange={setTab} className="flex flex-1 flex-col gap-0">
      {/* Sticky header */}
      <header className="sticky top-0 z-10 flex flex-col gap-2 border-b bg-background/95 px-4 py-2.5 backdrop-blur lg:px-6">
        <div className="flex flex-wrap items-center gap-2">
          <SidebarTrigger className="-ml-1" />
          <Button asChild variant="ghost" size="icon" className="size-7">
            <Link href="/function/findings">
              <ArrowLeftIcon />
            </Link>
          </Button>
          <ShieldAlertIcon className="size-4 text-muted-foreground" />
          <h1 className="max-w-md truncate text-sm font-semibold" title={title}>
            {title}
          </h1>
          <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">#{finding.id}</code>
          <Separator orientation="vertical" className="mx-1 h-4" />
          <StatusBadge domain="severity" value={finding.severity} dot />
          <StatusBadge domain="finding" value={finding.status} dot />
          <VerificationBadge finding={finding} />
          {finding.inherited && finding.source_task_id && (
            <Badge variant="outline">{t("findings.detail.sourceTaskReadOnly", { id: finding.source_task_id })}</Badge>
          )}
        </div>
        <TabsList>
          <TabsTrigger value="overview">{t("findings.detail.tabOverview")}</TabsTrigger>
          <TabsTrigger value="lineage">{t("findings.detail.tabLineage")}</TabsTrigger>
        </TabsList>
      </header>

      {/* Tab content */}
      <div className="flex-1 p-4 lg:p-6">
        {/* 概览：左（摘要 + 证据）/ 右（状态区） */}
        <TabsContent value="overview" className="mt-0">
          <div className="grid gap-4 lg:grid-cols-3">
            {/* 左栏 */}
            <div className="flex flex-col gap-4 lg:col-span-2">
              <Card>
                <CardHeader>
                  <CardTitle className="text-sm">{t("findings.detail.summary")}</CardTitle>
                </CardHeader>
                <CardContent>
                  <p className="text-sm leading-relaxed whitespace-pre-wrap">{finding.summary || t("findings.detail.noSummary")}</p>
                </CardContent>
              </Card>
              <FindingRetestPanel key={id} findingId={id} readOnly={finding.inherited} onCompleted={load} />
              <Card>
                <CardHeader>
                  <CardTitle className="text-sm">{t("findings.detail.evidenceTitle")}</CardTitle>
                </CardHeader>
                <CardContent>
                  {finding.evidence ? (
                    <pre className="max-h-[46vh] overflow-auto rounded-md bg-muted px-3 py-2 font-mono text-xs whitespace-pre-wrap">
                      {finding.evidence}
                    </pre>
                  ) : (
                    <p className="text-sm text-muted-foreground">{t("findings.detail.noEvidence")}</p>
                  )}
                </CardContent>
              </Card>
              {finding.counterevidence?.trim() && (
                <Card>
                  <CardHeader>
                    <CardTitle className="text-sm">{t("findings.detail.counterevidenceTitle")}</CardTitle>
                  </CardHeader>
                  <CardContent>
                    <p className="text-sm leading-relaxed whitespace-pre-wrap">{finding.counterevidence}</p>
                  </CardContent>
                </Card>
              )}
              {finding.confidence_rationale?.trim() && (
                <Card>
                  <CardHeader>
                    <CardTitle className="text-sm">{t("findings.detail.confidenceRationaleTitle")}</CardTitle>
                  </CardHeader>
                  <CardContent>
                    <p className="text-sm leading-relaxed whitespace-pre-wrap">{finding.confidence_rationale}</p>
                  </CardContent>
                </Card>
              )}
              <FindingTrafficPanel
                key={id}
                findingId={id}
                contextTask={contextTaskId || undefined}
                readOnly={finding.inherited}
                onChanged={load}
              />
              {/* 证据下方：详细报告(Markdown 渲染) */}
              <Card>
                <CardHeader className="flex-row items-center justify-between">
                  <CardTitle className="text-sm">{t("findings.detail.reportTitle")}</CardTitle>
                  {finding.report && <CopyButton text={finding.report} successMessage={t("findings.detail.reportCopied")} />}
                </CardHeader>
                <CardContent>
                  {finding.report_stale ? (
                    <Alert>
                      <AlertDescription>{t("findings.detail.reportStale")}</AlertDescription>
                    </Alert>
                  ) : null}
                  {finding.report ? (
                    <Markdown text={finding.report} />
                  ) : (
                    <p className="text-sm text-muted-foreground">{t("findings.detail.noDetailedReport")}</p>
                  )}
                </CardContent>
              </Card>
            </div>

            {/* 右栏：状态区 */}
            <Card className="h-fit lg:sticky lg:top-24">
              <CardHeader>
                <CardTitle className="text-sm">{t("common.status")}</CardTitle>
              </CardHeader>
              <CardContent className="divide-y">
                {/* 漏洞 ID */}
                <FieldRow label={t("findings.detail.fieldFindingId")}>
                  <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">
                    #{finding.id}
                  </code>
                </FieldRow>

                {/* 严重等级 */}
                <FieldRow label={t("findings.table.editSeverityLabel")}>
                  {finding.inherited ? (
                    <StatusBadge domain="severity" value={finding.severity} dot />
                  ) : (
                    <Select value={finding.severity} onValueChange={(v) => changeSeverity(v as Severity)}>
                      <SelectTrigger size="sm" className="h-7 w-auto border-none px-1 shadow-none focus-visible:ring-0">
                        <StatusBadge domain="severity" value={finding.severity} dot />
                      </SelectTrigger>
                      <SelectContent position="popper" align="end">
                        <SelectGroup>
                          {SEVERITIES.map((sv) => (
                            <SelectItem key={sv} value={sv}>
                              {statusMeta("severity", sv).label}
                            </SelectItem>
                          ))}
                        </SelectGroup>
                      </SelectContent>
                    </Select>
                  )}
                </FieldRow>

                {/* 处理状态 */}
                <FieldRow label={t("findings.detail.fieldStatus")}>
                  {finding.inherited ? (
                    <StatusBadge domain="finding" value={finding.status} dot />
                  ) : (
                    <Select value={finding.status} onValueChange={(v) => changeStatus(v as FindingStatus)}>
                      <SelectTrigger size="sm" className="h-7 w-auto border-none px-1 shadow-none focus-visible:ring-0">
                        <StatusBadge domain="finding" value={finding.status} dot />
                      </SelectTrigger>
                      <SelectContent position="popper" align="end">
                        <SelectGroup>
                          {FINDING_STATUSES.map((st) => (
                            <SelectItem key={st} value={st}>
                              {statusMeta("finding", st).label}
                            </SelectItem>
                          ))}
                        </SelectGroup>
                      </SelectContent>
                    </Select>
                  )}
                </FieldRow>

                {/* Verification: automated outcome; the note explains any state other than verified */}
                {finding.verification_status && (
                  <FieldRow label={t("findings.detail.fieldVerification")}>
                    <VerificationBadge finding={finding} />
                    {finding.verification_status !== "verified" && finding.verification_note?.trim() && (
                      <p className="whitespace-pre-wrap break-words text-muted-foreground text-xs">
                        {finding.verification_note}
                      </p>
                    )}
                  </FieldRow>
                )}

                {/* 漏洞类型 */}
                <FieldRow label={t("findings.detail.fieldType")}>
                  {finding.vulnclass ? (
                    <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{finding.vulnclass}</code>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </FieldRow>

                {/* 涉及资产 */}
                <FieldRow label={t("findings.detail.fieldAssets")}>
                  {finding.assets && finding.assets.length > 0 ? (
                    <div className="flex flex-wrap justify-end gap-1">
                      {finding.assets.map((a) => (
                        <code
                          key={a.id}
                          className="max-w-[16rem] truncate rounded bg-muted px-1.5 py-0.5 font-mono text-xs"
                          title={`${a.type} · ${a.label}`}
                        >
                          {a.label}
                        </code>
                      ))}
                    </div>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </FieldRow>

                {/* 所属任务 */}
                <FieldRow label={t("findings.table.task")}>
                  {finding.task_id ? (
                    <Link
                      href={`/function/tasks/detail?id=${finding.task_id}`}
                      className="inline-flex max-w-[16rem] items-center gap-1 text-primary hover:underline"
                      title={finding.task_description}
                    >
                      <span className="truncate">{finding.task_description || `#${finding.task_id}`}</span>
                      <ArrowUpRightIcon className="size-3 shrink-0" />
                    </Link>
                  ) : (
                    <span className="text-muted-foreground">—{t("findings.detail.taskDeleted")}</span>
                  )}
                </FieldRow>

                {/* 发现时间 */}
                <FieldRow label={t("findings.table.time")}>
                  <span className="tabular-nums">{fmtTime(finding.ts, toBcp47(locale))}</span>
                </FieldRow>
              </CardContent>
            </Card>
          </div>
        </TabsContent>

        {/* 链路图：从任务初始节点回溯到本漏洞节点的攻击链路 */}
        <TabsContent value="lineage" className="mt-0">
          <FindingLineageView findingId={finding.id} />
        </TabsContent>
      </div>
    </Tabs>
  );
}

// useSearchParams must sit under a Suspense boundary for static export.
export default function FindingDetailPage() {
  return (
    <React.Suspense fallback={null}>
      <FindingDetailInner />
    </React.Suspense>
  );
}
