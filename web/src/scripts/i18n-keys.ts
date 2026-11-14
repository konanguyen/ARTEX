/**
 * i18n key-parity helper.
 *
 * The catalogs are flat "a.b.c" → string maps (src/lib/i18n/catalogs/*.ts);
 * this compares a target locale's key set against the baseline and reports
 * missing and extra keys. The CLI (check-i18n-keys.ts) and the Vitest suite
 * share this pure logic, so the parity rule is defined in exactly one place.
 */

export interface KeyDiff {
  missing: string[]; // keys the baseline has and the target does not
  extra: string[]; // keys the target has and the baseline does not
}

/** Flatten a nested object into an "a.b.c" key set; arrays and primitives are leaves. */
export function flattenKeys(tree: unknown, prefix = ""): Set<string> {
  const out = new Set<string>();
  if (tree === null || typeof tree !== "object" || Array.isArray(tree)) {
    if (prefix) out.add(prefix);
    return out;
  }
  for (const [key, value] of Object.entries(tree as Record<string, unknown>)) {
    const path = prefix ? `${prefix}.${key}` : key;
    for (const leaf of flattenKeys(value, path)) out.add(leaf);
  }
  return out;
}

/** Missing and extra keys of `target` relative to `baseline`, both sorted. */
export function diffKeys(baseline: Set<string>, target: Set<string>): KeyDiff {
  const missing = [...baseline].filter((k) => !target.has(k)).sort();
  const extra = [...target].filter((k) => !baseline.has(k)).sort();
  return { missing, extra };
}

/** True when neither missing nor extra keys remain. */
export function isInSync(diff: KeyDiff): boolean {
  return diff.missing.length === 0 && diff.extra.length === 0;
}
