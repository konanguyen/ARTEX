"use client";

import { useState } from "react";
import { FileText, Loader2, X } from "lucide-react";
import { useI18n } from "@/lib/i18n";
import type { useChatAttachments } from "@/hooks/use-chat-attachments";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";

export function ChatAttachmentChips({ attachments }: { attachments: ReturnType<typeof useChatAttachments> }) {
  const { t } = useI18n();
  const [preview, setPreview] = useState<string | null>(null);
  const selected = attachments.items.find((item) => item.id === preview);
  return (
    <>
      <div className="flex flex-wrap gap-2" aria-live="polite">
        {attachments.items.map((item) => (
          <div
            key={item.id}
            className="flex max-w-full items-center gap-1 rounded-md border px-2 py-1 text-xs"
            data-attachment-id={item.id}
          >
            {item.status === "uploading" || item.status === "pending" ? (
              <Loader2 className="size-3 animate-spin" />
            ) : (
              <FileText className="size-3" />
            )}
            <button
              type="button"
              className="max-w-60 truncate"
              onClick={() => setPreview(item.id)}
              title={item.error}
            >
              {item.pastedText !== undefined
                ? t("chat.pastedContent")
                : item.uploaded?.name ?? item.file?.name}
            </button>
            {item.status === "failed" && (
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={() => attachments.retry(item.id)}
              >
                {t("chat.uploadRetry")}
              </Button>
            )}
            <button
              type="button"
              onClick={() => attachments.remove(item.id)}
              aria-label={t("chat.removeAttachment")}
            >
              <X className="size-3" />
            </button>
          </div>
        ))}
      </div>
      <Dialog
        open={preview !== null && !!selected}
        onOpenChange={(open) => {
          if (!open) setPreview(null);
        }}
      >
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>
              {selected?.file?.name ?? selected?.uploaded?.name ?? t("chat.pastedContent")}
            </DialogTitle>
          </DialogHeader>
          {selected?.pastedText !== undefined && (
            <>
              <pre className="max-h-[60vh] overflow-auto whitespace-pre-wrap break-words text-xs">
                {selected.pastedText}
              </pre>
              <Button
                type="button"
                onClick={() => {
                  attachments.insertAsText(selected.id);
                  setPreview(null);
                }}
              >
                {t("chat.insertAsText")}
              </Button>
            </>
          )}
          {selected?.error && (
            <p role="alert" className="text-sm text-destructive">
              {selected.error}
            </p>
          )}
        </DialogContent>
      </Dialog>
    </>
  );
}
