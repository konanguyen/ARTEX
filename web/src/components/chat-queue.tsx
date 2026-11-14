"use client";

import { useEffect, useState } from "react";
import { ArrowDown, ArrowUp, Loader2, Pencil, X } from "lucide-react";
import { toast } from "sonner";
import { useI18n } from "@/lib/i18n";
import { useChatAttachments, useChatDraft } from "@/hooks/use-chat-attachments";
import type { useChatControls } from "@/hooks/use-chat-controls";
import type { ChatAdmission } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { ChatAttachmentChips } from "@/components/chat-attachment-chips";
import { MentionTextarea } from "@/components/mention-textarea";

type Controls = ReturnType<typeof useChatControls>;

function QueueEditor({ item, controls, onClose }: { item: ChatAdmission; controls: Controls; onClose: () => void }) {
  const { t } = useI18n();
  const key = `queue:${item.id}`;
  const [text, setText] = useChatDraft(key, item.prompt.text);
  const [saving, setSaving] = useState(false);
  const attachments = useChatAttachments({
    draftKey: key,
    ensureOwner: async () =>
      item.session.conversation_id !== "0"
        ? { scope: "session", id: `conv-${item.session.conversation_id}` }
        : { scope: "task", id: item.session.task_id },
  });
  useEffect(() => {
    setText(item.prompt.text);
    attachments.loadUploaded(item.prompt.attachments);
  }, [item.id]);
  async function save() {
    setSaving(true);
    try {
      await controls.updateInput(item.id, item.revision, { text, attachments: attachments.uploaded });
      onClose();
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setSaving(false);
    }
  }
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("chat.queue.edit")}</DialogTitle>
          <DialogDescription>{t("chat.queue.title")}</DialogDescription>
        </DialogHeader>
        <MentionTextarea
          value={text}
          onValueChange={setText}
          data-chat-draft={key}
          onPaste={attachments.onPaste}
          disabled={saving}
        />
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
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("chat.history.cancelEdit")}
          </Button>
          <Button
            onClick={save}
            disabled={saving || !attachments.canSubmit || (!text.trim() && !attachments.uploaded.length)}
          >
            {t("chat.queue.save")}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function ChatQueue({ controls }: { controls: Controls }) {
  const { t } = useI18n();
  const { state, error } = controls;
  const [editing, setEditing] = useState<ChatAdmission | null>(null);
  const [pending, setPending] = useState(false);
  async function action(fn: () => Promise<unknown>) {
    setPending(true);
    try {
      await fn();
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setPending(false);
    }
  }
  if (!state && !error) return null;
  if (!state)
    return (
      <div role="alert" className="px-3 text-xs text-destructive">
        {error}
        <Button variant="ghost" onClick={() => void controls.reload()}>
          {t("chat.queue.reloadControls")}
        </Button>
      </div>
    );
  const queued = state.inputs.filter((a) => a.kind === "turn" && a.status === "queued");
  const steering = state.inputs.filter((a) => a.kind === "steer" && (a.status === "queued" || a.status === "claimed"));
  const warnings = state.inputs.filter((a) => a.status === "unknown" || a.status === "undelivered");
  function move(index: number, delta: number) {
    const ids = queued.map((a) => a.id);
    [ids[index], ids[index + delta]] = [ids[index + delta], ids[index]];
    void action(() => controls.reorder(state!.revision, ids));
  }
  return (
    <section className="border-t px-3 py-2 text-xs" aria-label={t("chat.queue.title")} data-chat-queue>
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">
          {t("chat.queue.title")} {queued.length ? `(${queued.length})` : ""}
        </span>
        {state.paused && <span>{t("chat.queue.paused")}</span>}
        <Button
          className="ml-auto h-7 text-xs"
          size="sm"
          variant="ghost"
          disabled={pending || (state.paused && warnings.some((a) => a.status === "unknown"))}
          onClick={() => void action(state.paused ? controls.resume : controls.pause)}
        >
          {pending ? <Loader2 className="size-3 animate-spin" /> : null}
          {t(state.paused ? "chat.queue.resume" : "chat.queue.pause")}
        </Button>
      </div>
      {state.session.task_id !== "0" && <p className="text-muted-foreground">{t("chat.queue.taskPauseHelp")}</p>}
      {error && (
        <p role="alert" className="text-destructive">
          {error}
        </p>
      )}
      <ol className="max-h-48 overflow-y-auto">
        {queued.map((item, index) => (
          <li key={item.id} data-queue-input={item.id} className="flex items-start gap-1 border-t py-1.5">
            <details className="min-w-0 flex-1">
              <summary className="cursor-pointer truncate">
                {item.prompt.text || item.prompt.attachments.map((a) => a.name).join(", ")}
              </summary>
              <p className="whitespace-pre-wrap break-words">{item.prompt.text}</p>
              {item.prompt.attachments.map((a) => (
                <p key={a.path}>{a.name}</p>
              ))}
            </details>
            {item.session.task_id !== "0" && (
              <span className="text-muted-foreground">
                {t("chat.queue.segment")} {item.session.main_seg + 1}
              </span>
            )}
            <Button
              size="icon"
              variant="ghost"
              className="size-6"
              aria-label={t("chat.queue.moveUp")}
              disabled={pending || index === 0}
              onClick={() => move(index, -1)}
            >
              <ArrowUp className="size-3" />
            </Button>
            <Button
              size="icon"
              variant="ghost"
              className="size-6"
              aria-label={t("chat.queue.moveDown")}
              disabled={pending || index === queued.length - 1}
              onClick={() => move(index, 1)}
            >
              <ArrowDown className="size-3" />
            </Button>
            <Button
              size="icon"
              variant="ghost"
              className="size-6"
              aria-label={t("chat.queue.edit")}
              disabled={pending}
              onClick={() => setEditing(item)}
            >
              <Pencil className="size-3" />
            </Button>
            <Button
              size="icon"
              variant="ghost"
              className="size-6"
              aria-label={t("chat.queue.remove")}
              disabled={pending}
              onClick={() => void action(() => controls.removeInput(item.id, item.revision))}
            >
              <X className="size-3" />
            </Button>
          </li>
        ))}
      </ol>
      {steering.map((item) => (
        <div role="status" key={item.id} data-steer-input={item.id} className="border-t py-2">
          <p>{t(item.status === "claimed" ? "chat.queue.steerDelivering" : "chat.queue.steerPending")}</p>
          <p className="break-all text-muted-foreground">
            {t("chat.queue.steerTarget")}: {item.target_run_id}
          </p>
          <details>
            <summary className="cursor-pointer truncate">{item.prompt.text}</summary>
            <p className="whitespace-pre-wrap break-words">{item.prompt.text}</p>
          </details>
        </div>
      ))}
      {warnings.map((item) => (
        <div role="status" key={item.id} className="border-t py-2">
          <p>{t(item.status === "unknown" ? "chat.queue.lostRun" : "chat.queue.steerUndelivered")}</p>
          <p className="whitespace-pre-wrap break-words">{item.prompt.text}</p>
          {item.status === "undelivered" && (
            <Button
              size="sm"
              variant="outline"
              disabled={pending}
              onClick={() => void action(() => controls.queueSteerFollowup(item.id, item.revision))}
            >
              {t("chat.queue.steerFollowUp")}
            </Button>
          )}
          {item.status === "unknown" && (
            <Button
              size="sm"
              variant="outline"
              disabled={pending}
              onClick={() => void action(() => controls.discardUnknown(item.id, state.revision))}
            >
              {t("chat.queue.discardLost")}
            </Button>
          )}
        </div>
      ))}
      {editing && <QueueEditor key={editing.id} item={editing} controls={controls} onClose={() => setEditing(null)} />}
    </section>
  );
}
