import { act, renderHook, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { useTheme } from "@/hooks/theme";
import { parseTheme, THEME_KEY } from "@/lib/theme";
import { setMedia } from "@/test/media";
import { renderApp } from "@/test/render";

const DARK_QUERY = "(prefers-color-scheme: dark)";
const isDark = () => document.documentElement.classList.contains("dark");

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn().mockReturnValue(new Promise(() => {})));
});

async function pick(label: string) {
  await userEvent.click(screen.getByRole("button", { name: /^Theme:/ }));
  await userEvent.click(await screen.findByRole("menuitemradio", { name: label }));
}

describe("theme", () => {
  // 001-AC25: with no stored choice the theme follows the OS, live.
  it("follows the OS by default", () => {
    setMedia(DARK_QUERY, true);
    renderApp();
    expect(isDark()).toBe(true);
    expect(screen.getByRole("button", { name: "Theme: system" })).toBeInTheDocument();
    act(() => setMedia(DARK_QUERY, false));
    expect(isDark()).toBe(false);
    expect(document.documentElement.style.colorScheme).toBe("light");
  });

  // 001-AC25: a forced choice wins over the OS and persists.
  it("forces dark and light and persists the choice", async () => {
    renderApp();
    await pick("Dark");
    expect(isDark()).toBe(true);
    expect(localStorage.getItem(THEME_KEY)).toBe("dark");

    setMedia(DARK_QUERY, true);
    await pick("Light");
    expect(isDark()).toBe(false);
    expect(localStorage.getItem(THEME_KEY)).toBe("light");

    await pick("System");
    expect(isDark()).toBe(true);
    expect(localStorage.getItem(THEME_KEY)).toBe("system");
  });

  // 001-AC25: the stored choice is restored on load.
  it("restores a stored choice", () => {
    localStorage.setItem(THEME_KEY, "dark");
    renderApp();
    expect(isDark()).toBe(true);
    expect(screen.getByRole("button", { name: "Theme: dark" })).toBeInTheDocument();
  });

  it.each([
    ["light", "light"],
    ["dark", "dark"],
    ["system", "system"],
    ["purple", "system"],
    [null, "system"],
  ])("parses %s as %s", (raw, want) => {
    expect(parseTheme(raw)).toBe(want);
  });

  // The CSP blocks <style> elements; a modal Radix menu would inject one through react-remove-scroll.
  it("opens the menu without injecting a style element", async () => {
    renderApp();
    await userEvent.click(screen.getByRole("button", { name: /^Theme:/ }));
    expect(await screen.findByRole("menu")).toBeInTheDocument();
    expect(document.querySelectorAll("style")).toHaveLength(0);
  });

  it("refuses use outside the provider", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => renderHook(() => useTheme())).toThrow("ThemeProvider");
  });
});
