export const LARGE_PASTE_THRESHOLD = 3000;

export type ChatPaste =
  | { kind: "inline" }
  | { kind: "attach"; files: File[]; text: string; pastedText?: string };

function clipboardFilename(value: string): string {
  let path = value.trim();
  if (path.startsWith("file://")) {
    try {
      path = decodeURIComponent(new URL(path).pathname);
    } catch {
      return path;
    }
  }
  return path.split(/[/\\]/).at(-1) ?? path;
}

export function classifyChatPaste(files: File[], plainText: string): ChatPaste {
  if (files.length) {
    const lines = plainText.trim().split(/\r?\n/);
    const duplicate = lines.length === files.length && lines.every((line, index) => clipboardFilename(line) === files[index].name);
    return { kind: "attach", files, text: duplicate ? "" : plainText };
  }
  if (plainText.length > LARGE_PASTE_THRESHOLD) {
    return { kind: "attach", files: [], text: "", pastedText: plainText };
  }
  return { kind: "inline" };
}

export function makePastedTextFile(text: string, id: string): File {
  return new File([text], `pasted_content_${id}.txt`, { type: "text/plain;charset=utf-8" });
}

export function restorePastedText(
  draft: string,
  source: { draft: string; start: number; end: number },
  pastedText: string,
): { text: string; caret: number } {
  const same = draft === source.draft;
  const start = same ? Math.max(0, Math.min(draft.length, source.start)) : draft.length;
  const end = same ? Math.max(start, Math.min(draft.length, source.end)) : draft.length;
  return { text: draft.slice(0, start) + pastedText + draft.slice(end), caret: start + pastedText.length };
}
