"use client";

import { useCallback, useEffect, useSyncExternalStore } from "react";
import { api } from "@/lib/api";
import { createAttachmentStore, newChatID, readyForSubmit, type ChatDraft, type UploadOwner } from "@/lib/chat-attachment-state";
import { classifyChatPaste, makePastedTextFile, restorePastedText } from "@/lib/chat-paste";
import type { ChatAttachment } from "@/lib/types";

// Browser state outlives individual chat views; SSR never writes to this store.
const store = createAttachmentStore(async (owner, files) => (await api.chatUpload(owner.scope, owner.id, files)).attachments);
const EMPTY: ReturnType<typeof store.items> = [];

export function useChatDraft(key: string, initial = ""): [string, (text: string) => void] {
  const text = useSyncExternalStore(store.subscribe, () => store.draft(key), () => initial);
  useEffect(() => store.seedDraft(key, initial), [key, initial]);
  const setText = useCallback((value: string) => store.setDraft(key, value), [key]);
  return [text, setText];
}

export function useChatAttachments({ draftKey, ensureOwner }: {
  draftKey: string;
  ensureOwner: () => Promise<UploadOwner>;
}) {
  const items = useSyncExternalStore(store.subscribe, () => store.items(draftKey), () => EMPTY);
  const addFiles = (files: File[]) => { if (files.length) void store.addFiles(draftKey, files, ensureOwner); };
  function writeDraft(text: string, caret: number) {
    store.setDraft(draftKey, text);
    requestAnimationFrame(() => {
      const target = document.querySelector<HTMLTextAreaElement>(`textarea[data-chat-draft="${CSS.escape(draftKey)}"]`);
      if (target) { target.focus(); target.setSelectionRange(caret, caret); }
    });
  }
  function onPaste(event: React.ClipboardEvent<HTMLTextAreaElement>) {
    const files = Array.from(event.clipboardData.items).filter((item) => item.kind === "file").map((item) => item.getAsFile()).filter((file): file is File => !!file);
    const paste = classifyChatPaste(files, event.clipboardData.getData("text/plain"));
    if (paste.kind === "inline") return;
    event.preventDefault();
    const source: ChatDraft = { text: event.currentTarget.value, start: event.currentTarget.selectionStart, end: event.currentTarget.selectionEnd };
    if (paste.pastedText !== undefined) {
      void store.addFiles(draftKey, [makePastedTextFile(paste.pastedText, newChatID())], ensureOwner, { pastedText: paste.pastedText, source });
    } else {
      if (paste.text) {
        const restored = restorePastedText(source.text, { draft: source.text, start: source.start, end: source.end }, paste.text);
        writeDraft(restored.text, restored.caret);
      }
      addFiles(paste.files);
    }
  }
  function insertAsText(id: string) {
    const item = store.items(draftKey).find((entry) => entry.id === id);
    if (item?.pastedText === undefined) return;
    const draft = store.draft(draftKey);
    const restored = restorePastedText(draft, { draft: item.source?.text ?? draft, start: item.source?.start ?? draft.length, end: item.source?.end ?? draft.length }, item.pastedText);
    writeDraft(restored.text, restored.caret);
    store.remove(draftKey, id);
  }
  return {
    items, onPaste, addFiles, insertAsText,
    owner: () => store.owner(draftKey, ensureOwner),
    uploading: items.some((item) => item.status === "pending" || item.status === "uploading"),
    canSubmit: readyForSubmit(items),
    uploaded: items.flatMap((item) => item.uploaded ? [item.uploaded] : []),
    retry: (id: string) => { void store.retry(draftKey, id, ensureOwner); },
    remove: (id: string) => store.remove(draftKey, id),
    clear: () => store.clear(draftKey),
    loadUploaded: (manifests: ChatAttachment[]) => store.loadUploaded(draftKey, manifests),
  };
}
