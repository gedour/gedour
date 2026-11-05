import { useQuery } from "@tanstack/react-query";
import { useRef, useState, type KeyboardEvent, type PointerEvent } from "react";
import { NavLink } from "react-router";

import { getVersion } from "@/api/client";
import { Tooltip } from "@/components/ui/tooltip";
import { useInboxCount } from "@/hooks/useInbox";
import { useMe } from "@/hooks/useMe";
import { SIDEBAR_MAX, SIDEBAR_MIN } from "@/hooks/useSidebar";
import { cn } from "@/lib/utils";
import { NAV } from "@/nav";
import type { Me } from "@/types/api";

export const COLLAPSED_WIDTH = 64;
const KEY_STEP = 16;

interface SidebarProps {
  collapsed: boolean;
  width: number;
  onResize: (width: number) => void;
  onResizeEnd: () => void;
  mobile: boolean;
  open: boolean;
  onNavigate: () => void;
}

export function Sidebar({
  collapsed,
  width,
  onResize,
  onResizeEnd,
  mobile,
  open,
  onNavigate,
}: SidebarProps) {
  const iconOnly = collapsed && !mobile;
  const unread = useInboxCount().data ?? 0;
  const isAdmin = (useMe().data as Me).is_admin;
  const drag = useRef<{ x: number; width: number } | null>(null);
  const [dragging, setDragging] = useState(false);

  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    e.preventDefault();
    e.currentTarget.setPointerCapture?.(e.pointerId);
    drag.current = { x: e.clientX, width };
    setDragging(true);
  };
  const onPointerMove = (e: PointerEvent<HTMLDivElement>) => {
    if (drag.current) onResize(drag.current.width + e.clientX - drag.current.x);
  };
  const endDrag = () => {
    if (!drag.current) return;
    drag.current = null;
    setDragging(false);
    onResizeEnd();
  };
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const next = {
      ArrowLeft: width - KEY_STEP,
      ArrowRight: width + KEY_STEP,
      Home: SIDEBAR_MIN,
      End: SIDEBAR_MAX,
    }[e.key];
    if (next !== undefined) {
      e.preventDefault();
      onResize(next);
      onResizeEnd();
    }
  };

  return (
    <aside
      id="app-sidebar"
      aria-label="Sidebar"
      data-collapsed={iconOnly}
      inert={mobile && !open}
      style={mobile ? undefined : { width: iconOnly ? COLLAPSED_WIDTH : width }}
      className={cn(
        "flex shrink-0 flex-col bg-harbor text-harbor-ink",
        mobile
          ? "fixed inset-y-0 left-0 z-40 w-72 pt-14 shadow-2xl transition-transform duration-200 motion-reduce:transition-none"
          : "relative",
        mobile && !open && "-translate-x-full",
        !mobile && !dragging && "transition-[width] duration-200 motion-reduce:transition-none",
      )}
    >
      <nav aria-label="Main" className="flex-1 overflow-y-auto overflow-x-hidden px-2 py-3">
        <ul className="flex flex-col gap-0.5">
          {NAV.filter((item) => isAdmin || !item.admin).map(({ path, label, icon: Icon }) => {
            const count = path === "/inbox" ? unread : 0;
            const name = count ? `${label}, ${count} unread` : label;
            return (
              <li key={path}>
                <Tooltip label={name} disabled={!iconOnly}>
                  <NavLink
                    to={path}
                    onClick={onNavigate}
                    // Expanded, an sr-only text names it: an aria-label that omits the visible badge fails WCAG 2.5.3.
                    aria-label={iconOnly ? name : undefined}
                    className={({ isActive }) =>
                      cn(
                        "relative flex h-10 items-center gap-3 rounded-md px-3 text-sm font-medium text-harbor-muted transition-colors hover:bg-harbor-hover hover:text-harbor-ink",
                        iconOnly && "justify-center px-0",
                        isActive &&
                          "bg-harbor-hover text-white before:absolute before:inset-y-2 before:left-0 before:w-1 before:rounded-r before:bg-harbor-beacon",
                      )
                    }
                  >
                    <Icon className="size-[18px] shrink-0" aria-hidden />
                    {!iconOnly && (
                      <span aria-hidden={count > 0} className="truncate">
                        {label}
                      </span>
                    )}
                    {count > 0 && !iconOnly && <span className="sr-only">{name}</span>}
                    {count > 0 && (
                      <span
                        aria-hidden
                        data-testid="unread-badge"
                        className={cn(
                          "rounded-full bg-harbor-beacon px-1.5 text-[11px] leading-[18px] font-semibold text-harbor tabular-nums",
                          iconOnly ? "absolute top-1 right-1 min-w-[18px] text-center" : "ml-auto",
                        )}
                      >
                        {count > 99 ? "99+" : count}
                      </span>
                    )}
                  </NavLink>
                </Tooltip>
              </li>
            );
          })}
        </ul>
      </nav>
      {!iconOnly && <VersionFooter />}
      {!mobile && !iconOnly && (
        <div
          role="separator"
          aria-orientation="vertical"
          aria-label="Resize sidebar"
          aria-valuemin={SIDEBAR_MIN}
          aria-valuemax={SIDEBAR_MAX}
          aria-valuenow={width}
          tabIndex={0}
          data-testid="sidebar-resize"
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={endDrag}
          onPointerCancel={endDrag}
          onKeyDown={onKeyDown}
          className="group absolute inset-y-0 -right-1.5 z-10 w-3 cursor-col-resize touch-none"
        >
          <span
            className={cn(
              "absolute inset-y-0 left-1/2 w-0.5 -translate-x-1/2 bg-transparent transition-colors group-hover:bg-harbor-beacon/70 group-focus-visible:bg-harbor-beacon",
              dragging && "bg-harbor-beacon",
            )}
          />
        </div>
      )}
    </aside>
  );
}

function VersionFooter() {
  const { data, isPending, isError } = useQuery({
    queryKey: ["version"],
    queryFn: getVersion,
    staleTime: Infinity,
  });
  let text = "Loading version";
  if (isError) text = "Version unavailable";
  else if (!isPending) text = `${data.version} (${data.commit.slice(0, 7)})`;
  return (
    <p
      data-testid="app-version"
      className="truncate border-t border-harbor-line px-5 py-3 text-xs text-harbor-muted"
    >
      {text}
    </p>
  );
}
