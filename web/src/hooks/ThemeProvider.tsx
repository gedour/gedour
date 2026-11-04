import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";

import { useMediaQuery } from "@/hooks/useMediaQuery";
import { ThemeContext } from "@/hooks/theme";
import { parseTheme, resolveTheme, THEME_KEY, type Theme } from "@/lib/theme";
import { readStorage, writeStorage } from "@/lib/utils";

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setThemeState] = useState<Theme>(() => parseTheme(readStorage(THEME_KEY)));
  const prefersDark = useMediaQuery("(prefers-color-scheme: dark)");
  const resolved = resolveTheme(theme, prefersDark);

  useEffect(() => {
    const root = document.documentElement;
    root.classList.toggle("dark", resolved === "dark");
    root.style.colorScheme = resolved;
  }, [resolved]);

  const setTheme = useCallback((next: Theme) => {
    writeStorage(THEME_KEY, next);
    setThemeState(next);
  }, []);

  const value = useMemo(() => ({ theme, resolved, setTheme }), [theme, resolved, setTheme]);
  return <ThemeContext value={value}>{children}</ThemeContext>;
}
