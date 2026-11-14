import type { ChatAttachment } from "./types.ts";

export type UploadOwner = { scope: "task" | "session"; id: string };
export type ChatDraft = { text: string; start: number; end: number };
export type DraftAttachment = {
  id: string;
  ownerKey: string;
  file?: File;
  status: "pending" | "uploading" | "ready" | "failed";
  uploaded?: ChatAttachment;
  error?: string;
  pastedText?: string;
  source?: ChatDraft;
};
type State = Record<string, DraftAttachment[]>;
type Event =
  | { type: "add"; key: string; items: DraftAttachment[] }
  | { type: "patch"; key: string; id: string; patch: Partial<DraftAttachment> }
  | { type: "remove"; key: string; id: string }
  | { type: "clear"; key: string };
const EMPTY: DraftAttachment[] = [];

export function readyForSubmit(items: DraftAttachment[]): boolean {
  return items.every((item) => item.status === "ready" && !!item.uploaded);
}

export function reduceAttachments(state: State, event: Event): State {
  const items = state[event.key] ?? EMPTY;
  const next = event.type === "add" ? [...items, ...event.items]
    : event.type === "patch" ? items.map((item) => item.id === event.id ? { ...item, ...event.patch } : item)
      : event.type === "remove" ? items.filter((item) => item.id !== event.id) : [];
  return { ...state, [event.key]: next };
}

export function newChatID(): string {
  return globalThis.crypto?.randomUUID?.() ?? `chat-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

export function createAttachmentStore(upload: (owner: UploadOwner, files: File[]) => Promise<ChatAttachment[]>) {
  let state: State = {};
  const drafts = new Map<string, string>();
  const owners = new Map<string, Promise<UploadOwner>>();
  const listeners = new Set<() => void>();
  const notify = () => { for (const listener of listeners) listener(); };
  const dispatch = (event: Event) => { state = reduceAttachments(state, event); notify(); };
  function owner(key: string, ensureOwner: () => Promise<UploadOwner>) {
    if (!owners.has(key)) owners.set(key, Promise.resolve().then(ensureOwner).catch((error) => { owners.delete(key); throw error; }));
    return owners.get(key)!;
  }
  async function send(key: string, entries: DraftAttachment[], ensureOwner: () => Promise<UploadOwner>) {
    for (const item of entries) dispatch({ type: "patch", key, id: item.id, patch: { status: "uploading", error: undefined } });
    try {
      const target = await owner(key, ensureOwner);
      const results = await upload(target, entries.map((item) => item.file!));
      if (results.length !== entries.length) throw new Error("Incomplete attachment upload");
      entries.forEach((item, index) => dispatch({ type: "patch", key, id: item.id, patch: { status: "ready", uploaded: results[index] } }));
    } catch (error) {
      for (const item of entries) dispatch({ type: "patch", key, id: item.id, patch: { status: "failed", error: error instanceof Error ? error.message : String(error) } });
    }
  }
  return {
    owner,
    subscribe(listener: () => void) { listeners.add(listener); return () => { listeners.delete(listener); }; },
    items(key: string) { return state[key] ?? EMPTY; },
    draft(key: string) { return drafts.get(key) ?? ""; },
    seedDraft(key: string, text: string) { if (!drafts.has(key)) { drafts.set(key, text); notify(); } },
    setDraft(key: string, text: string) { drafts.set(key, text); notify(); },
    async addFiles(key: string, files: File[], ensureOwner: () => Promise<UploadOwner>, meta?: Pick<DraftAttachment, "pastedText" | "source">) {
      const entries = files.map((file) => ({ id: newChatID(), ownerKey: key, file, status: "pending" as const, ...meta }));
      dispatch({ type: "add", key, items: entries });
      await send(key, entries, ensureOwner);
    },
    async retry(key: string, id: string, ensureOwner: () => Promise<UploadOwner>) {
      const item = state[key]?.find((entry) => entry.id === id);
      if (item?.file && item.status === "failed") await send(key, [item], ensureOwner);
    },
    loadUploaded(key: string, manifests: ChatAttachment[]) {
      dispatch({ type: "clear", key });
      dispatch({ type: "add", key, items: manifests.map((uploaded) => ({ id: newChatID(), ownerKey: key, status: "ready", uploaded })) });
    },
    remove(key: string, id: string) { dispatch({ type: "remove", key, id }); },
    clear(key: string) { owners.delete(key); dispatch({ type: "clear", key }); },
  };
}
