"use client";

import * as React from "react";

import { CheckIcon, CopyIcon, FileTextIcon } from "lucide-react";
import { toast } from "sonner";

import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import { useI18n } from "@/lib/i18n";
import { copyText } from "@/lib/utils";

// A different task must never render or copy the previous task's report: the
// wrapper remounts the stateful TaskReport per taskId.
export function ReportTab({ taskId }: { taskId: string }) {
  return <TaskReport key={taskId} taskId={taskId} />;
}

type ReportState = { status: "loading" | "error" } | { status: "ready"; text: string };

function TaskReport({ taskId }: { taskId: string }) {
  const { t } = useI18n();
  const [state, setState] = React.useState<ReportState>({ status: "loading" });
  const [attempt, setAttempt] = React.useState(0);
  const [copied, setCopied] = React.useState(false);
  const report = state.status === "ready" ? state.text : "";

  // biome-ignore lint/correctness/useExhaustiveDependencies: attempt intentionally restarts the request after an explicit retry.
  React.useEffect(() => {
    let active = true;
    setState({ status: "loading" });
    api
      .report(taskId)
      .then((text) => {
        if (active) setState({ status: "ready", text });
      })
      .catch(() => {
        if (active) setState({ status: "error" });
      });
    return () => {
      active = false;
    };
  }, [taskId, attempt]);

  function retry() {
    setState({ status: "loading" });
    setAttempt((current) => current + 1);
  }

  async function copy() {
    if (!report) return;
    const ok = await copyText(report);
    if (ok) {
      setCopied(true);
      toast.success(t("tasks.detail.report.copied"));
      setTimeout(() => setCopied(false), 1500);
    } else {
      toast.error(t("common.copyFailed"));
    }
  }

  let content: React.ReactNode;
  if (state.status === "loading") {
    content = (
      <div
        role="status"
        className="flex flex-col items-center justify-center gap-2 rounded-md border border-dashed py-16 text-muted-foreground text-sm"
      >
        <FileTextIcon className="size-8 opacity-40" />
        {t("tasks.detail.report.loading")}
      </div>
    );
  } else if (state.status === "error") {
    content = (
      <div
        role="alert"
        className="flex flex-col items-center justify-center gap-2 rounded-md border border-dashed py-16 text-sm"
      >
        <p>{t("tasks.detail.report.loadError")}</p>
        <Button size="sm" variant="outline" onClick={retry}>
          {t("action.retry")}
        </Button>
      </div>
    );
  } else if (report) {
    content = (
      <div className="max-h-[60vh] overflow-auto rounded-md border bg-muted/20 p-4">
        <Markdown text={report} />
      </div>
    );
  } else {
    content = (
      <div className="flex flex-col items-center justify-center gap-2 rounded-md border border-dashed py-16 text-muted-foreground text-sm">
        <FileTextIcon className="size-8 opacity-40" />
        {t("tasks.detail.report.empty")}
      </div>
    );
  }

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between">
        <CardTitle className="flex items-center gap-2 text-sm">
          <FileTextIcon className="size-4" /> {t("tasks.detail.report.title")}
        </CardTitle>
        <div className="flex gap-2">
          {report && (
            <Button size="sm" variant="outline" onClick={copy}>
              {copied ? <CheckIcon /> : <CopyIcon />} {t("common.copy")}
            </Button>
          )}
        </div>
      </CardHeader>
      <CardContent>{content}</CardContent>
    </Card>
  );
}
