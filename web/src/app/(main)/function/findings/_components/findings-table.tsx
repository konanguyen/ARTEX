"use client";

import * as React from "react";

import Link from "next/link";

import {
  ArrowUpRightIcon,
  ChevronRightIcon,
  FileTextIcon,
  FlaskConicalIcon,
  RotateCcwIcon,
  ShieldAlertIcon,
  Trash2Icon,
} from "lucide-react";

import { toBcp47, useI18n } from "@/lib/i18n";
import { CopyButton } from "@/components/copy-button";
import { Markdown } from "@/components/markdown";
import { StatusBadge } from "@/components/status-badge";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { statusMeta, type Tone, toneClasses } from "@/lib/status";
import type { ActiveFindingRetest, Finding, FindingStatus, Severity, VerificationStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

export const SEVERITIES: Severity[] = ["critical", "high", "medium", "low"];

export const FINDING_STATUSES: FindingStatus[] = [
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

export const VERIFICATION_STATUSES: VerificationStatus[] = [
  "verified",
  "unverified",
  "blocked",
  "not_assessed",
  "false_positive",
];

// Verification outcome → label/tone, on the same tone tokens StatusBadge uses.
export const VERIFICATION_META: Record<VerificationStatus, { labelKey: string; label: string; tone: Tone }> = {
  verified: { labelKey: "findings.verification.verified", label: "Verified", tone: "green" },
  unverified: { labelKey: "findings.verification.unverified", label: "Unverified", tone: "slate" },
  blocked: { labelKey: "findings.verification.blocked", label: "Blocked", tone: "amber" },
  not_assessed: { labelKey: "findings.verification.not_assessed", label: "Not assessed", tone: "red" },
  false_positive: { labelKey: "findings.verification.false_positive", label: "False positive", tone: "neutral" },
};

// VerificationBadge shows the automated verification outcome ("Verified L3" when a
// proof-of-exploitation level is declared). It renders nothing when the finding carries
// no verification status (in-task nodes, payloads from before the column existed).
export function VerificationBadge({ finding }: { finding: Pick<Finding, "verification_status" | "poe_level"> }) {
  const { t } = useI18n();
  const meta = finding.verification_status ? VERIFICATION_META[finding.verification_status] : undefined;
  if (!meta) return null;
  const baseLabel = t(meta.labelKey);
  const label =
    finding.verification_status === "verified" && finding.poe_level
      ? `${baseLabel} L${finding.poe_level}`
      : baseLabel;
  return (
    <span
      className={cn(
        "inline-flex w-fit items-center whitespace-nowrap rounded-md border px-2 py-0.5 font-medium text-xs",
        toneClasses[meta.tone],
      )}
    >
      {label}
    </span>
  );
}

export const UNASSIGNED_TASK = "__unassigned__";

// 行内编辑缓冲:当前展开行的名称/类别/严重等级。
export interface FindingEdit {
  name: string;
  vulnclass: string;
  severity: Severity;
}

export type FindingReport = { status: "loading" | "done" | "error"; text: string };

// Exploration node ids are only unique inside a task. Prefer the persisted
// finding id and otherwise namespace the node id by task so editing one group
// cannot update a similarly-named node in another expanded group.
export function findingRowKey(finding: Finding): string {
  if (finding.finding_id) return `finding:${finding.finding_id}`;
  return `node:${finding.task_id ?? UNASSIGNED_TASK}:${finding.id}`;
}

export function isSameFinding(left: Finding, right: Finding): boolean {
  return findingRowKey(left) === findingRowKey(right);
}

export function fmtTime(ts: string, locale?: string) {
  return new Date(ts).toLocaleString(locale ?? "zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

const COLUMN_COUNT = 9;

interface FindingsTableProps {
  items: Finding[];
  selectedIds: Set<string>;
  onToggleSelected: (id: string, checked: boolean) => void;
  onToggleSelectedPage: (ids: string[], checked: boolean) => void;
  /** 当前展开行的 findingRowKey;null = 全部收起。 */
  expandedKey: string | null;
  onToggleRow: (finding: Finding) => void;
  reports: Record<string, FindingReport>;
  edit: FindingEdit | null;
  onEditChange: React.Dispatch<React.SetStateAction<FindingEdit | null>>;
  saving: boolean;
  onSave: (finding: Finding) => void;
  onStatusChange: (finding: Finding, next: FindingStatus) => void;
  onRetest: (finding: Finding) => void;
  activeRetests: Record<string, ActiveFindingRetest>;
  onDeepen: (finding: Finding) => void;
  onDelete: (finding: Finding) => void;
  /** 全选框的无障碍标签,平铺视图与分组视图措辞不同。 */
  selectAllLabel?: string;
}

// FindingsTable 是发现列表的表格主体,平铺视图与按任务分组视图共用同一份行渲染
// (勾选 / 行内展开 / 行内改名与改状态 / 复测 / 深入 / 删除),差异只在外层容器与分页。
export function FindingsTable({
  items,
  selectedIds,
  onToggleSelected,
  onToggleSelectedPage,
  expandedKey,
  onToggleRow,
  reports,
  edit,
  onEditChange,
  saving,
  onSave,
  onStatusChange,
  onRetest,
  activeRetests,
  onDeepen,
  onDelete,
  selectAllLabel,
}: FindingsTableProps) {
  const { t, locale } = useI18n();
  const effectiveSelectAllLabel = selectAllLabel ?? t("findings.table.selectAll");
  const selectableIds = items.map((finding) => finding.finding_id).filter((id): id is string => Boolean(id));
  const selectedCount = selectableIds.filter((id) => selectedIds.has(id)).length;
  let headerChecked: boolean | "indeterminate" = false;
  if (selectableIds.length > 0 && selectedCount === selectableIds.length) {
    headerChecked = true;
  } else if (selectedCount > 0) {
    headerChecked = "indeterminate";
  }

  return (
    /* 固定列宽保证展开内容不撑开表格；窄屏只在表格内部横向滚动。 */
    <Table className="min-w-[60rem] table-fixed">
      <TableHeader>
        <TableRow>
          <TableHead className="w-8">
            <Checkbox
              checked={headerChecked}
              onCheckedChange={(checked) => onToggleSelectedPage(selectableIds, checked === true)}
              aria-label={effectiveSelectAllLabel}
            />
          </TableHead>
          <TableHead className="w-8" />
          <TableHead className="w-20">{t("findings.table.severity")}</TableHead>
          <TableHead>{t("findings.table.name")}</TableHead>
          <TableHead className="w-44">{t("findings.table.asset")}</TableHead>
          <TableHead className="w-28">{t("findings.table.status")}</TableHead>
          <TableHead className="w-32">{t("findings.table.task")}</TableHead>
          <TableHead className="w-24">{t("findings.table.time")}</TableHead>
          <TableHead className="w-48">{t("findings.table.actions")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {items.map((f) => {
          const rowKey = findingRowKey(f);
          const open = expandedKey === rowKey;
          const retest = f.finding_id ? activeRetests[f.finding_id] : undefined;
          return (
            <React.Fragment key={rowKey}>
              <TableRow
                className="cursor-pointer"
                role="button"
                tabIndex={0}
                aria-expanded={open}
                onClick={() => onToggleRow(f)}
                onKeyDown={(event) => {
                  if (event.target !== event.currentTarget || (event.key !== "Enter" && event.key !== " ")) return;
                  event.preventDefault();
                  onToggleRow(f);
                }}
              >
                <TableCell onClick={(e) => e.stopPropagation()}>
                  {f.finding_id && (
                    <Checkbox
                      checked={selectedIds.has(f.finding_id)}
                      onCheckedChange={(c) => onToggleSelected(f.finding_id as string, c === true)}
                      aria-label={t("findings.table.selectFinding")}
                    />
                  )}
                </TableCell>
                <TableCell>
                  <ChevronRightIcon
                    className={cn("size-4 text-muted-foreground transition-transform", open && "rotate-90")}
                  />
                </TableCell>
                <TableCell>
                  <StatusBadge domain="severity" value={f.severity} dot />
                </TableCell>
                <TableCell className="max-w-md">
                  <div className="flex min-w-0 flex-col gap-0.5">
                    {f.finding_id ? (
                      <Link
                        href={`/function/findings/detail?id=${f.finding_id}`}
                        onClick={(e) => e.stopPropagation()}
                        className="truncate font-medium hover:text-primary hover:underline"
                        title={t("findings.table.viewDetail")}
                      >
                        {f.name || f.vulnclass || t("findings.table.unclassified")}
                      </Link>
                    ) : (
                      <span className="truncate font-medium">{f.name || f.vulnclass || t("findings.table.unclassified")}</span>
                    )}
                    <span className="truncate text-xs text-muted-foreground">{f.summary}</span>
                    <div className="flex flex-wrap items-center gap-1">
                      <Badge variant="outline">{t("findings.table.trafficEvidence", { count: f.traffic_count ?? 0 })}</Badge>
                      <VerificationBadge finding={f} />
                    </div>
                  </div>
                </TableCell>
                <TableCell>
                  {f.assets && f.assets.length > 0 ? (
                    <div className="flex flex-wrap gap-1">
                      {f.assets.slice(0, 3).map((a) => (
                        <code
                          key={a.id}
                          className="max-w-full truncate rounded bg-muted px-1.5 py-0.5 font-mono text-xs"
                          title={`${a.type} · ${a.label}`}
                        >
                          {a.label}
                        </code>
                      ))}
                      {f.assets.length > 3 && (
                        <span className="text-xs text-muted-foreground">+{f.assets.length - 3}</span>
                      )}
                    </div>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>
                <TableCell onClick={(e) => e.stopPropagation()}>
                  {f.finding_id ? (
                    <Select value={f.status} onValueChange={(v) => onStatusChange(f, v as FindingStatus)}>
                      <SelectTrigger size="sm" className="h-7 w-full border-none px-1 shadow-none focus-visible:ring-0">
                        <StatusBadge domain="finding" value={f.status} dot />
                      </SelectTrigger>
                      <SelectContent position="popper" align="end">
                        {FINDING_STATUSES.map((st) => (
                          <SelectItem key={st} value={st}>
                            {statusMeta("finding", st).label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  ) : (
                    <StatusBadge domain="finding" value={f.status} dot />
                  )}
                </TableCell>
                <TableCell>
                  {f.task_id ? (
                    <Link
                      href={`/function/tasks/detail?id=${f.task_id}`}
                      onClick={(e) => e.stopPropagation()}
                      className="inline-flex max-w-full items-center gap-1 text-primary hover:underline"
                      title={f.task_description}
                    >
                      <span className="truncate">{f.task_description}</span>
                      <ArrowUpRightIcon className="size-3 shrink-0" />
                    </Link>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>
                <TableCell className="text-xs text-muted-foreground tabular-nums">{fmtTime(f.ts, toBcp47(locale))}</TableCell>
                <TableCell onClick={(e) => e.stopPropagation()}>
                  <div className="flex items-center gap-1">
                    {retest ? (
                      <Button asChild size="sm" variant="ghost">
                        <Link href={`/chat?c=${retest.conversation_id}`} title={t("findings.table.retestLink")}>
                          <Spinner data-icon="inline-start" />
                          {t("findings.table.retesting")}
                        </Link>
                      </Button>
                    ) : null}
                    {!retest && f.finding_id && !f.inherited ? (
                      <Button size="sm" variant="ghost" onClick={() => onRetest(f)} title={t("findings.table.retestTitle")}>
                        <RotateCcwIcon data-icon="inline-start" />
                        {t("findings.table.retest")}
                      </Button>
                    ) : null}
                    {f.finding_id && f.task_id && (
                      <Button size="sm" variant="ghost" onClick={() => onDeepen(f)}>
                        <FlaskConicalIcon data-icon="inline-start" />
                        {t("findings.table.deepen")}
                      </Button>
                    )}
                    {f.finding_id && (
                      <AlertDialog>
                        <AlertDialogTrigger asChild>
                          <Button
                            size="icon"
                            variant="ghost"
                            className="size-7 text-muted-foreground hover:text-destructive"
                            aria-label={t("findings.table.delete")}
                          >
                            <Trash2Icon className="size-4" />
                          </Button>
                        </AlertDialogTrigger>
                        <AlertDialogContent>
                          <AlertDialogHeader>
                            <AlertDialogTitle>{t("findings.table.deleteConfirm")}</AlertDialogTitle>
                            <AlertDialogDescription className="break-words">
                              {locale === "zh-CN" ? "「" : '"'}
                              <span className="break-all">
                                {f.name || f.vulnclass || f.summary || `#${f.finding_id}`}
                              </span>
                              {locale === "zh-CN" ? "」" : '" '}{t("findings.table.deleteWarning")}
                            </AlertDialogDescription>
                          </AlertDialogHeader>
                          <AlertDialogFooter>
                            <AlertDialogCancel>{t("findings.table.cancel")}</AlertDialogCancel>
                            <AlertDialogAction onClick={() => onDelete(f)}>{t("findings.table.delete")}</AlertDialogAction>
                          </AlertDialogFooter>
                        </AlertDialogContent>
                      </AlertDialog>
                    )}
                  </div>
                </TableCell>
              </TableRow>
              {open && (
                <TableRow className="hover:bg-transparent">
                  {/* whitespace-normal 覆盖 TableCell 默认的 nowrap,否则展开区文字
                      被强制单行、直接溢出单元格。 */}
                  <TableCell colSpan={COLUMN_COUNT} className="bg-muted/30 whitespace-normal">
                    <div className="flex flex-col gap-2 px-2 py-1">
                      {/* 行内编辑:名称/类别/严重等级,可改并保存(仅独立 finding 行)。 */}
                      {f.finding_id && edit && (
                        <div className="flex flex-wrap items-end gap-3 rounded-md border bg-background px-3 py-2.5">
                          <div className="flex min-w-[12rem] flex-1 flex-col gap-1">
                            <Label className="text-xs text-muted-foreground">{t("findings.table.editNameLabel")}</Label>
                            <Input
                              value={edit.name}
                              onChange={(e) => onEditChange((s) => (s ? { ...s, name: e.target.value } : s))}
                              placeholder={t("findings.table.editNamePlaceholder")}
                            />
                          </div>
                          <div className="flex min-w-[10rem] flex-col gap-1">
                            <Label className="text-xs text-muted-foreground">{t("findings.table.editClassLabel")}</Label>
                            <Input
                              value={edit.vulnclass}
                              onChange={(e) => onEditChange((s) => (s ? { ...s, vulnclass: e.target.value } : s))}
                              placeholder={t("findings.table.editClassPlaceholder")}
                            />
                          </div>
                          <div className="flex flex-col gap-1">
                            <Label className="text-xs text-muted-foreground">{t("findings.table.editSeverityLabel")}</Label>
                            <Select
                              value={edit.severity}
                              onValueChange={(v) => onEditChange((s) => (s ? { ...s, severity: v as Severity } : s))}
                            >
                              <SelectTrigger size="sm" className="w-28">
                                <SelectValue />
                              </SelectTrigger>
                              <SelectContent>
                                {SEVERITIES.map((sv) => (
                                  <SelectItem key={sv} value={sv}>
                                    {statusMeta("severity", sv).label}
                                  </SelectItem>
                                ))}
                              </SelectContent>
                            </Select>
                          </div>
                          <Button size="sm" disabled={saving} onClick={() => onSave(f)}>
                            {saving ? t("findings.table.saving") : t("findings.table.save")}
                          </Button>
                        </div>
                      )}
                      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                        <ShieldAlertIcon className="size-3.5" />
                        {t("findings.table.evidence")}
                        {f.vulnclass && (
                          <span>
                            · {t("findings.table.typeLabel")}
                            <code className="rounded bg-muted px-1.5 py-0.5 font-mono">{f.vulnclass}</code>
                          </span>
                        )}
                        {f.param_id && <code className="rounded bg-muted px-1.5 py-0.5 font-mono">{f.param_id}</code>}
                        {f.assets && f.assets.length > 0 && (
                          <span className="flex flex-wrap items-center gap-1">
                            · {t("findings.table.assetLabel")}
                            {f.assets.map((a) => (
                              <code key={a.id} className="rounded bg-muted px-1.5 py-0.5 font-mono" title={a.type}>
                                {a.label}
                              </code>
                            ))}
                          </span>
                        )}
                      </div>
                      <pre className="overflow-x-auto rounded-md bg-muted px-3 py-2 font-mono text-xs whitespace-pre-wrap">
                        {f.evidence}
                      </pre>

                      {/* 详细报告(Markdown):展开时按 finding_id 懒加载,免进详情页即可查看。 */}
                      {f.finding_id && (
                        <div className="flex flex-col gap-1.5">
                          <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
                            <span className="flex items-center gap-2">
                              <FileTextIcon className="size-3.5" />
                              {t("findings.table.report")}
                            </span>
                            {reports[rowKey]?.status === "done" && reports[rowKey]?.text.trim() && (
                              <CopyButton
                                text={reports[rowKey]?.text}
                                successMessage={t("findings.table.reportCopied")}
                                variant="ghost"
                                className="h-6 px-2 text-xs"
                              />
                            )}
                          </div>
                          {(() => {
                            const rep = reports[rowKey];
                            if (!rep || rep.status === "loading")
                              return <p className="text-xs text-muted-foreground">{t("findings.table.loading")}</p>;
                            if (rep.status === "error")
                              return <p className="text-xs text-muted-foreground">{t("findings.table.reportLoadError")}</p>;
                            if (!rep.text.trim())
                              return <p className="text-xs text-muted-foreground">{t("findings.table.noReport")}</p>;
                            return (
                              // break-words 会继承到段落/列表,pre 另加
                              // whitespace-pre-wrap 让代码块也换行——否则长代码行/长 URL
                              // 会撑宽 colSpan 单元格,把整张表挤出横向滚动条。
                              <div className="min-w-0 break-words rounded-md border bg-background px-3 py-2 [&_pre]:whitespace-pre-wrap">
                                <Markdown text={rep.text} />
                              </div>
                            );
                          })()}
                        </div>
                      )}
                    </div>
                  </TableCell>
                </TableRow>
              )}
            </React.Fragment>
          );
        })}
        {items.length === 0 && (
          <TableRow>
            <TableCell colSpan={COLUMN_COUNT} className="py-12 text-center text-sm text-muted-foreground">
              {t("findings.table.noFindings")}
            </TableCell>
          </TableRow>
        )}
      </TableBody>
    </Table>
  );
}
