"use client";

import { useEffect, useRef, useState } from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { useI18n } from "@/lib/i18n";
import type { useChatControls } from "@/hooks/use-chat-controls";
import { useChatAttachments, useChatDraft } from "@/hooks/use-chat-attachments";
import { newChatID } from "@/lib/chat-attachment-state";
import { availableHistoryActions, parseChatHistoryPrompt } from "@/lib/chat-history-actions";
import type { Activity, ChatAttempt, ChatHistoryResult, ChatPrompt, ChatSessionRef } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { MentionTextarea } from "@/components/mention-textarea";
import { ChatAttachmentChips } from "@/components/chat-attachment-chips";
import { Transcript } from "@/components/transcript";

type Controls = ReturnType<typeof useChatControls>;
type Mutation = "retry" | "edit" | "branch";

function PromptEditor({
  id,
  prompt,
  session,
  onChange,
}: {
  id: string;
  prompt: ChatPrompt;
  session: ChatSessionRef;
  onChange: (value: ChatPrompt, ready: boolean) => void;
}) {
  const { t } = useI18n();
  const key = `history-edit:${id}`;
  const [text, setText] = useChatDraft(key, prompt.text);
  const attachments = useChatAttachments({
    draftKey: key,
    ensureOwner: async () =>
      session.conversation_id !== "0"
        ? { scope: "session", id: `conv-${session.conversation_id}` }
        : { scope: "task", id: session.task_id },
  });
  useEffect(() => {
    setText(prompt.text);
    attachments.loadUploaded(prompt.attachments);
    return () => {
      attachments.clear();
      setText("");
    };
  }, [id]);
  useEffect(
    () => onChange({ text, attachments: attachments.uploaded }, attachments.canSubmit),
    [text, attachments.items],
  );
  return (
    <>
      <MentionTextarea data-chat-draft={key} value={text} onValueChange={setText} onPaste={attachments.onPaste} />
      <ChatAttachmentChips attachments={attachments} />
      <input
        type="file"
        multiple
        aria-label={t("chat.uploadFile")}
        onChange={(event) => {
          attachments.addFiles(Array.from(event.target.files ?? []));
          event.target.value = "";
        }}
      />
    </>
  );
}

function AttemptTrace({ attempt, controls }: { attempt: ChatAttempt; controls: Controls }) {
  const { t, locale } = useI18n();
  const [items, setItems] = useState<Activity[] | null>(null);
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(false);
  async function load(before = 0) {
    setLoading(true);
    try {
      const page = await controls.attemptActivities(attempt.run_id, before);
      setItems((current) => (before ? [...page.items, ...(current ?? [])] : page.items));
      setMore(page.has_more);
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setLoading(false);
    }
  }
  const status =
    attempt.status === "completed"
      ? "chat.history.attemptCompleted"
      : attempt.status === "failed"
        ? "chat.history.attemptFailed"
        : attempt.status === "stopped"
          ? "chat.history.attemptStopped"
          : attempt.status === "running"
            ? "chat.history.attemptRunning"
            : "chat.history.attemptUnknown";
  return (
    <details
      className="border-t py-2"
      onToggle={(event) => {
        if (event.currentTarget.open && items === null && !loading) void load();
      }}
    >
      <summary className="cursor-pointer">
        {t("chat.history.readAttempt")} · {t(status)} · {new Date(attempt.created_at).toLocaleString(locale)}
      </summary>
      {loading && <Loader2 className="size-4 animate-spin" />}
      {more && (
        <Button size="sm" variant="ghost" disabled={loading} onClick={() => void load(items?.[0]?.seq ?? 0)}>
          {t("chat.loadMore")}
        </Button>
      )}
      {items && (
        <Transcript
          activity={items.map((item) => ({ ...item, inherited: true }))}
          chat
          fetchDetail={async (seq) => items.find((item) => item.seq === seq)?.detail ?? ""}
        />
      )}
    </details>
  );
}

export function ChatMessageActions({
  activity,
  controls,
  onChanged,
}: {
  activity: Activity;
  controls: Controls;
  onChanged: (result: ChatHistoryResult) => void | Promise<void>;
}) {
  const { t } = useI18n();
  const state = controls.state;
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  const [dialog, setDialog] = useState<{ kind: Mutation; id: string } | null>(null);
  const [pending, setPending] = useState(false);
  const [prompt, setPrompt] = useState<ChatPrompt | null>(null);
  const [ready, setReady] = useState(false);
  const [title, setTitle] = useState("");
  const [error, setError] = useState("");
  const [attempts, setAttempts] = useState<ChatAttempt[] | null>(null);
  const [showAttempts, setShowAttempts] = useState(false);
  useEffect(() => {
    if (dialog?.kind !== "edit" || !activity.turn_id) return;
    let live = true;
    setPrompt(null);
    setError("");
    void controls
      .prompt(activity.turn_id)
      .then((value) => {
        if (live) {
          setPrompt(parseChatHistoryPrompt(value));
          setReady(true);
        }
      })
      .catch((error) => {
        if (live) setError((error as Error).message);
      });
    return () => {
      live = false;
    };
  }, [dialog?.id, activity.turn_id]);
  if (!state || (activity.kind !== "user" && activity.kind !== "result")) return null;
  const actions = availableHistoryActions(activity, state);
  async function submit() {
    if (!dialog) return;
    setPending(true);
    try {
      const result =
        dialog.kind === "retry"
          ? await controls.retry(dialog.id)
          : dialog.kind === "edit"
            ? await controls.edit({
                kind: "edit",
                client_request_id: dialog.id,
                turn_id: activity.turn_id,
                expected_revision: state!.revision,
                replacement: prompt!,
              })
            : await controls.branch({
                kind: "branch",
                client_request_id: dialog.id,
                turn_id: activity.turn_id,
                expected_revision: state!.revision,
                title,
              });
      if (alive.current) {
        setDialog(null);
        await onChanged(result);
      }
    } catch (error) {
      if (alive.current) setError((error as Error).message);
    } finally {
      if (alive.current) setPending(false);
    }
  }
  function open(kind: Mutation) {
    if (kind === "edit") {
      setPrompt(null);
      setReady(false);
    }
    setError("");
    setTitle("");
    setDialog({ kind, id: newChatID() });
  }
  async function readAttempts() {
    setShowAttempts(true);
    setAttempts(null);
    try {
      const page = await controls.attempts(activity.turn_id!);
      if (alive.current) setAttempts(page.attempts.filter((item) => !item.active));
    } catch (error) {
      toast.error((error as Error).message);
    }
  }
  return (
    <div
      className={`flex flex-wrap items-center gap-1 text-xs ${activity.kind === "user" ? "justify-end" : ""}`}
      data-chat-actions={activity.turn_id}
    >
      {actions.edit && (
        <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => open("edit")}>
          {t("chat.history.edit")}
        </Button>
      )}
      {actions.retry && (
        <Button
          size="sm"
          variant="ghost"
          className="h-7 text-xs"
          title={t("chat.history.retryEffect")}
          onClick={() => open("retry")}
        >
          {t("chat.history.retry")}
        </Button>
      )}
      {actions.branch && (
        <Button
          size="sm"
          variant="ghost"
          className="h-7 text-xs"
          title={t("chat.history.branchEffect")}
          onClick={() => open("branch")}
        >
          {t("chat.history.branch")}
        </Button>
      )}
      {!actions.edit && !actions.retry && !actions.branch && activity.history_mutable === false && (
        <span className="text-muted-foreground" title={t("chat.history.unavailable")}>
          ⓘ {t("chat.history.unavailable")}
        </span>
      )}
      {activity.kind === "result" && activity.turn_id && (
        <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => void readAttempts()}>
          {t("chat.history.previousAttempts")}
        </Button>
      )}
      <Dialog
        open={!!dialog}
        onOpenChange={(open) => {
          if (!open && !pending) setDialog(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {t(
                dialog?.kind === "edit"
                  ? "chat.history.edit"
                  : dialog?.kind === "branch"
                    ? "chat.history.branch"
                    : "chat.history.retry",
              )}
            </DialogTitle>
            <DialogDescription>
              {t(dialog?.kind === "branch" ? "chat.history.branchEffect" : "chat.history.retryEffect")}
            </DialogDescription>
          </DialogHeader>
          {dialog?.kind === "edit" && activity.turn_id !== state.latest_turn_id && (
            <p className="text-sm">{t("chat.history.editEarlier")}</p>
          )}
          {state.session.task_id !== "0" &&
            (dialog?.kind === "branch" || (dialog?.kind === "edit" && activity.turn_id !== state.latest_turn_id)) && (
              <p className="text-sm">{t("chat.history.sharedTask")}</p>
            )}
          {dialog?.kind === "edit" &&
            (prompt ? (
              <PromptEditor
                id={dialog.id}
                prompt={prompt}
                session={state.session}
                onChange={(value, ready) => {
                  setPrompt(value);
                  setReady(ready);
                }}
              />
            ) : (
              <Loader2 className="size-4 animate-spin" />
            ))}
          {dialog?.kind === "branch" && (
            <Input
              value={title}
              onChange={(event) => setTitle(event.target.value)}
              aria-label={t("chat.history.branchTitle")}
            />
          )}
          {error && (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <div className="flex justify-end gap-2">
            <Button variant="ghost" disabled={pending} onClick={() => setDialog(null)}>
              {t("chat.history.cancelEdit")}
            </Button>
            <Button
              onClick={() => void submit()}
              disabled={
                pending ||
                !!state.active ||
                (dialog?.kind === "edit" && (!prompt || !ready || (!prompt.text.trim() && !prompt.attachments.length)))
              }
            >
              {pending && <Loader2 className="size-4 animate-spin" />}
              {t(
                dialog?.kind === "edit"
                  ? "chat.history.saveAndRun"
                  : dialog?.kind === "branch"
                    ? "chat.history.branch"
                    : "chat.history.retry",
              )}
            </Button>
          </div>
        </DialogContent>
      </Dialog>
      <Dialog open={showAttempts} onOpenChange={setShowAttempts}>
        <DialogContent className="max-w-3xl">
          <DialogHeader>
            <DialogTitle>{t("chat.history.previousAttempts")}</DialogTitle>
            <DialogDescription>{t("chat.history.retryEffect")}</DialogDescription>
          </DialogHeader>
          <div className="max-h-[65vh] overflow-auto">
            {attempts === null ? (
              <Loader2 className="size-4 animate-spin" />
            ) : attempts.length ? (
              attempts.map((attempt) => <AttemptTrace key={attempt.run_id} attempt={attempt} controls={controls} />)
            ) : (
              <p>{t("chat.history.noPreviousAttempts")}</p>
            )}
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}

export function ChatParentLink({
  controls,
  onNavigate,
}: {
  controls: Controls;
  onNavigate: (ref: ChatSessionRef) => void | Promise<void>;
}) {
  const { t } = useI18n();
  const state = controls.state;
  if (!state?.parent) return null;
  return (
    <aside className="border-b px-4 py-2 text-xs text-muted-foreground">
      {state.parent_available ? (
        <Button variant="link" size="sm" className="h-auto p-0 text-xs" onClick={() => void onNavigate(state.parent!)}>
          {t("chat.history.parentConversation")}
        </Button>
      ) : (
        <span>{t("chat.history.parentUnavailable")}</span>
      )}
      <p>{t(state.parent.task_id !== "0" ? "chat.history.sharedTask" : "chat.history.copiedDocuments")}</p>
    </aside>
  );
}
