import { useCallback, useEffect, useRef, useState } from "react";

import { readStorage, writeStorage } from "@/lib/utils";

export const SIDEBAR_MIN = 180;
export const SIDEBAR_MAX = 480;
export const SIDEBAR_DEFAULT = 248;
export const WIDTH_KEY = "gedour.sidebar.width";
export const COLLAPSED_KEY = "gedour.sidebar.collapsed";

export function clampWidth(width: number): number {
  if (!Number.isFinite(width)) return SIDEBAR_DEFAULT;
  return Math.min(SIDEBAR_MAX, Math.max(SIDEBAR_MIN, Math.round(width)));
}

function storedWidth(): number {
  const raw = readStorage(WIDTH_KEY);
  return raw === null ? SIDEBAR_DEFAULT : clampWidth(Number.parseInt(raw, 10));
}

export interface SidebarState {
  collapsed: boolean;
  width: number;
  toggleCollapsed: () => void;
  /** Updates the width in memory only; call commitWidth to persist it. */
  setWidth: (width: number) => void;
  commitWidth: () => void;
}

export function useSidebar(): SidebarState {
  const [collapsed, setCollapsed] = useState(() => readStorage(COLLAPSED_KEY) === "true");
  const [width, setWidthState] = useState(storedWidth);
  // Latest width without waiting for a re-render, so commitWidth right after setWidth sees it.
  const widthRef = useRef(width);

  useEffect(() => writeStorage(COLLAPSED_KEY, String(collapsed)), [collapsed]);

  const toggleCollapsed = useCallback(() => setCollapsed((prev) => !prev), []);

  const setWidth = useCallback((next: number) => {
    widthRef.current = clampWidth(next);
    setWidthState(widthRef.current);
  }, []);

  const commitWidth = useCallback(() => writeStorage(WIDTH_KEY, String(widthRef.current)), []);

  return { collapsed, width, toggleCollapsed, setWidth, commitWidth };
}
