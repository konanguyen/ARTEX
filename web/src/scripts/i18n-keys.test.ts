import { describe, expect, it } from "vitest";

import { diffKeys, flattenKeys, isInSync } from "./i18n-keys";

describe("flattenKeys", () => {
  it("flattens nested objects to dotted leaf keys", () => {
    const keys = flattenKeys({ a: { b: { c: "x" } }, d: "y", e: [1, 2], f: null });
    expect([...keys].sort()).toEqual(["a.b.c", "d", "e", "f"]);
  });
});

describe("diffKeys / isInSync", () => {
  it("reports missing and extra keys sorted", () => {
    const base = flattenKeys({ a: "1", b: "2", c: "3" });
    const target = flattenKeys({ b: "2", c: "3", d: "4" });
    const diff = diffKeys(base, target);
    expect(diff.missing).toEqual(["a"]);
    expect(diff.extra).toEqual(["d"]);
    expect(isInSync(diff)).toBe(false);
  });

  it("is in sync for identical key sets", () => {
    const base = flattenKeys({ x: { y: "1" } });
    expect(isInSync(diffKeys(base, flattenKeys({ x: { y: "1" } })))).toBe(true);
  });
});
