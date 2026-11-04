import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import {
  injectThemeKey,
  parseTheme,
  resolveTheme,
  THEME_KEY,
  THEME_KEY_PLACEHOLDER,
  type Theme,
} from "@/lib/theme";
import { setMedia } from "@/test/media";

// Vitest runs from web/.
const rawHtml = readFileSync("index.html", "utf8");

function inlineScript(html: string): string {
  const match = /<script>([\s\S]*?)<\/script>/.exec(html);
  if (!match?.[1]) throw new Error("no inline script in index.html");
  return match[1];
}

describe("pre-paint theme script", () => {
  it("reads the storage key from the TS constant, not a literal", () => {
    expect(rawHtml).toContain(THEME_KEY_PLACEHOLDER);
    expect(rawHtml).not.toContain(THEME_KEY);
    const html = injectThemeKey(rawHtml);
    expect(html).not.toContain(THEME_KEY_PLACEHOLDER);
    expect(inlineScript(html)).toContain(`"${THEME_KEY}"`);
  });

  it("refuses an index.html without the placeholder", () => {
    expect(() => injectThemeKey("<script>localStorage.getItem('x')</script>")).toThrow(
      THEME_KEY_PLACEHOLDER,
    );
  });

  // 001-AC25: the inline script resolves exactly like the app, so the first paint never flips.
  const stored: (string | null)[] = [null, "light", "dark", "system", "neon"];
  const cases = stored.flatMap((s) => [true, false].map((os) => [s, os] as const));
  it.each(cases)("stored %s with OS dark=%s matches resolveTheme", (value, osDark) => {
    if (value !== null) localStorage.setItem(THEME_KEY, value);
    setMedia("(prefers-color-scheme: dark)", osDark);
    new Function(inlineScript(injectThemeKey(rawHtml)))();
    const want = resolveTheme(parseTheme(value), osDark);
    expect(document.documentElement.classList.contains("dark")).toBe(want === "dark");
    expect(document.documentElement.style.colorScheme).toBe(want);
  });
});

describe("resolveTheme", () => {
  it.each<[Theme, boolean, string]>([
    ["light", true, "light"],
    ["dark", false, "dark"],
    ["system", true, "dark"],
    ["system", false, "light"],
  ])("%s with OS dark=%s is %s", (theme, osDark, want) => {
    expect(resolveTheme(theme, osDark)).toBe(want);
  });
});
