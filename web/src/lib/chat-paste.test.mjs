import assert from "node:assert/strict";
import test from "node:test";
import { classifyChatPaste, makePastedTextFile, restorePastedText } from "./chat-paste.ts";

test("short clipboard text keeps native caret insertion including the 3000 boundary", () => {
  assert.deepEqual(classifyChatPaste([], "x".repeat(3000)), { kind: "inline" });
  assert.deepEqual(classifyChatPaste([], "😀".repeat(1500)), { kind: "inline" });
  assert.deepEqual(classifyChatPaste([], "/home/user/report.pdf"), { kind: "inline" });
});

test("long clipboard text becomes a lossless text attachment", async () => {
  const text = "  한글\r\n中文\n" + "x".repeat(3001);
  assert.deepEqual(classifyChatPaste([], text), { kind: "attach", files: [], text: "", pastedText: text });
  const file = makePastedTextFile(text, "fixed");
  assert.equal(file.name, "pasted_content_fixed.txt");
  assert.equal(file.type, "text/plain;charset=utf-8");
  assert.equal(await file.text(), text);
  assert.deepEqual(new Uint8Array(await file.arrayBuffer()), new TextEncoder().encode(text));
});

test("clipboard files keep ordering and suppress only duplicated filenames or paths", () => {
  const files = [new File(["first"], "report.pdf"), new File(["second"], "notes.docx")];
  assert.deepEqual(classifyChatPaste(files, "report.pdf\nnotes.docx"), { kind: "attach", files, text: "" });
  assert.equal(classifyChatPaste(files, "file:///home/me/report.pdf\n/home/me/notes.docx").text, "");
  assert.equal(classifyChatPaste(files, "Please review report.pdf").text, "Please review report.pdf");
  assert.equal(classifyChatPaste(files, "Useful notes\nreport.pdf").text, "Useful notes\nreport.pdf");
  assert.equal(classifyChatPaste(files, "Please review").text, "Please review");
});

test("restoring pasted text replaces the original selection without deleting later edits", () => {
  assert.deepEqual(restorePastedText("one old two", { draft: "one old two", start: 4, end: 7 }, "new"), { text: "one new two", caret: 7 });
  assert.deepEqual(restorePastedText("later edits", { draft: "old", start: 0, end: 3 }, "pasted"), { text: "later editspasted", caret: 17 });
});
