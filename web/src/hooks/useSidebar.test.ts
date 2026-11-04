import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import {
  clampWidth,
  COLLAPSED_KEY,
  SIDEBAR_DEFAULT,
  SIDEBAR_MAX,
  SIDEBAR_MIN,
  useSidebar,
  WIDTH_KEY,
} from "@/hooks/useSidebar";

describe("clampWidth", () => {
  // 001-AC23: width is bounded to 180..480 px.
  it.each([
    [100, SIDEBAR_MIN],
    [180, 180],
    [300.4, 300],
    [480, 480],
    [9999, SIDEBAR_MAX],
    [Number.NaN, SIDEBAR_DEFAULT],
  ])("clamps %s to %s", (input, want) => {
    expect(clampWidth(input)).toBe(want);
  });
});

describe("useSidebar", () => {
  // 001-AC23: width and collapsed state are restored from storage, sanitised.
  it.each([
    [null, SIDEBAR_DEFAULT],
    ["320", 320],
    ["10", SIDEBAR_MIN],
    ["5000", SIDEBAR_MAX],
    ["wide", SIDEBAR_DEFAULT],
  ])("restores stored width %s as %s", (stored, want) => {
    if (stored !== null) localStorage.setItem(WIDTH_KEY, stored);
    const { result } = renderHook(() => useSidebar());
    expect(result.current.width).toBe(want);
  });

  // 001-AC23: collapse toggles and persists.
  it("toggles and persists the collapsed state", () => {
    localStorage.setItem(COLLAPSED_KEY, "true");
    const { result } = renderHook(() => useSidebar());
    expect(result.current.collapsed).toBe(true);
    act(() => result.current.toggleCollapsed());
    expect(result.current.collapsed).toBe(false);
    expect(localStorage.getItem(COLLAPSED_KEY)).toBe("false");
  });

  // 001-AC23: a resize is clamped, kept in memory, and stored only on commit.
  it("persists a clamped width on commit only", () => {
    const { result } = renderHook(() => useSidebar());
    act(() => result.current.setWidth(1000));
    expect(result.current.width).toBe(SIDEBAR_MAX);
    expect(localStorage.getItem(WIDTH_KEY)).toBeNull();
    act(() => {
      result.current.setWidth(300);
      result.current.commitWidth();
    });
    expect(localStorage.getItem(WIDTH_KEY)).toBe("300");
  });
});
