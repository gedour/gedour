// Pure theme logic, shared by the app and vite.config.ts (which injects THEME_KEY into index.html).

export type Theme = "light" | "dark" | "system";

export const THEME_KEY = "gedour.theme";
export const THEME_KEY_PLACEHOLDER = "%GEDOUR_THEME_KEY%";

export function parseTheme(raw: string | null): Theme {
  return raw === "light" || raw === "dark" ? raw : "system";
}

export function resolveTheme(theme: Theme, prefersDark: boolean): "light" | "dark" {
  if (theme === "system") return prefersDark ? "dark" : "light";
  return theme;
}

export function injectThemeKey(html: string): string {
  if (!html.includes(THEME_KEY_PLACEHOLDER)) {
    throw new Error(
      `index.html lacks ${THEME_KEY_PLACEHOLDER}: the pre-paint theme script drifted`,
    );
  }
  return html.replaceAll(THEME_KEY_PLACEHOLDER, THEME_KEY);
}
