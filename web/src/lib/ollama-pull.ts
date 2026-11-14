// Model names use [A-Za-z0-9._:/-]. Prose only ever adds "." or ":" right after a name,
// so a name continues past a word character, "/" or "-", or a "."/":" joined to more.
const NAME_CONTINUES = /^(?:[\w/-]|[.:][\w.:/-])/;

// The diagnostic error names the exact command; fall back to the entered model.
export function pullCommand(error: string, model: string) {
  if (!/ollama pull/.test(error)) return "";
  const entered = model.trim();
  // Prefer the configured model when the error names it whole, so names ending in "-"
  // or "." survive; the boundary check stops "qwen:4" from matching "qwen:4b".
  if (entered) {
    const prefix = `ollama pull ${entered}`;
    for (let at = error.indexOf(prefix); at >= 0; at = error.indexOf(prefix, at + 1)) {
      if (!NAME_CONTINUES.test(error.slice(at + prefix.length))) return prefix;
    }
  }
  // Otherwise drop one trailing prose separator (EN ": ", sentence "."); Korean particles
  // and zh "：" are outside the name characters and never join it.
  const named = [...error.matchAll(/ollama pull ([\w.:/-]+)/g)].map((m) => m[1].replace(/[.:]$/, "")).filter(Boolean);
  return `ollama pull ${named.at(-1) ?? entered}`;
}
