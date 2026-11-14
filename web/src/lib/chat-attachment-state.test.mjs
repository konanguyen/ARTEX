import assert from "node:assert/strict";
import test from "node:test";
import { createAttachmentStore, readyForSubmit, reduceAttachments } from "./chat-attachment-state.ts";

test("unready attachments block submission and deleted uploads cannot reappear", () => {
  const file = new File(["bytes"], "report.pdf");
  const item = { id: "one", ownerKey: "A", file, status: "uploading" };
  const added = reduceAttachments({}, { type: "add", key: "A", items: [item] });
  assert.equal(readyForSubmit(added.A), false);
  const removed = reduceAttachments(added, { type: "remove", key: "A", id: "one" });
  const late = reduceAttachments(removed, { type: "patch", key: "A", id: "one", patch: { status: "ready" } });
  assert.deepEqual(late.A, []);
});

test("rapid uploads share one owner and retain files after failure and retry", async () => {
  let owners = 0;
  let fail = true;
  const first = new File(["first"], "one.pdf");
  const second = new File(["second"], "two.pdf");
  const store = createAttachmentStore(async (_owner, files) => {
    if (fail) throw new Error("offline");
    return files.map((file) => ({ name: file.name, path: `uploads/${file.name}`, size: file.size }));
  });
  const owner = async () => { owners++; await Promise.resolve(); return { scope: "session", id: "conv-1" }; };
  await Promise.all([store.addFiles("A", [first], owner), store.addFiles("A", [second], owner)]);
  assert.equal(owners, 1);
  assert.equal(store.items("A").length, 2);
  assert.equal(store.items("A")[0].file, first);
  assert.equal(readyForSubmit(store.items("A")), false);
  fail = false;
  await store.retry("A", store.items("A")[0].id, owner);
  assert.equal(store.items("A")[0].uploaded.path, "uploads/one.pdf");
  assert.equal(store.items("A")[1].status, "failed");
});

test("late completion updates only its originating draft", async () => {
  let complete;
  const store = createAttachmentStore(() => new Promise((resolve) => { complete = resolve; }));
  const pending = store.addFiles("A", [new File(["x"], "a.txt")], async () => ({ scope: "task", id: "1" }));
  await new Promise((resolve) => setImmediate(resolve));
  store.setDraft("B", "other chat");
  complete([{ name: "a.txt", path: "uploads/a.txt", size: 1 }]);
  await pending;
  assert.equal(store.items("A")[0].status, "ready");
  assert.deepEqual(store.items("B"), []);
  assert.equal(store.draft("B"), "other chat");
});
