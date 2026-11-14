/**
 * Script: check-i18n-keys.ts
 *
 * Key-parity guard for the locale catalogs (src/lib/i18n/catalogs/*.ts).
 * zh-cn is the source locale; en-us and vi-vn must define exactly the same
 * key set — missing keys fall back to the source at runtime, so drift is
 * silent and only this guard catches it. Exits non-zero on drift.
 * Run: npm run check:i18n
 */

import { diffKeys, flattenKeys, isInSync } from "./i18n-keys";
import { en } from "../lib/i18n/catalogs/en-us";
import { vi } from "../lib/i18n/catalogs/vi-vn";
import { zhCN } from "../lib/i18n/catalogs/zh-cn";

const LOCALES: Record<string, Record<string, string>> = {
  "en-us": en,
  "vi-vn": vi,
};
const BASELINE = "zh-cn";

function preview(keys: string[]): string {
  const shown = keys.slice(0, 20).join(", ");
  return keys.length > 20 ? `${shown} …` : shown;
}

const baseline = flattenKeys(zhCN);
let failed = false;

console.log("─".repeat(64));
console.log(`i18n key-parity check (baseline: ${BASELINE}, ${baseline.size} keys)`);
console.log("─".repeat(64));

for (const [locale, catalog] of Object.entries(LOCALES)) {
  const diff = diffKeys(baseline, flattenKeys(catalog));
  if (isInSync(diff)) {
    console.log(`[OK] ${locale} — in sync`);
    continue;
  }
  failed = true;
  console.log(`[FAIL] ${locale} — drift`);
  if (diff.missing.length) console.log(`   missing ${diff.missing.length}: ${preview(diff.missing)}`);
  if (diff.extra.length) console.log(`   extra ${diff.extra.length}: ${preview(diff.extra)}`);
}

console.log("─".repeat(64));
if (failed) {
  console.error("i18n keys drifted: align the missing/extra keys and re-run.");
  process.exit(1);
}
