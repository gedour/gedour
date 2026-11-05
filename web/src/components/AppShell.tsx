import { Menu, PanelLeftClose, PanelLeftOpen, X } from "lucide-react";
import { useEffect, useState } from "react";
import { Outlet, useLocation } from "react-router";

import { ActingBanner } from "@/components/ActingBanner";
import { Sidebar } from "@/components/Sidebar";
import { ToastProvider } from "@/components/Toaster";
import { ThemeToggle } from "@/components/ThemeToggle";
import { UserMenu } from "@/components/UserMenu";
import { Wordmark } from "@/components/Wordmark";
import { Button } from "@/components/ui/button";
import { TooltipProvider } from "@/components/ui/tooltip";
import { useInboxStream } from "@/hooks/useInbox";
import { useMediaQuery } from "@/hooks/useMediaQuery";
import { useSidebar } from "@/hooks/useSidebar";

export const MOBILE_QUERY = "(max-width: 767px)";

// Ctrl/Cmd+B means "bold" or a field's own binding while typing; leave those alone.
function isEditable(target: EventTarget | null): boolean {
  return (
    target instanceof HTMLElement &&
    (["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName) ||
      target.closest("[contenteditable]:not([contenteditable='false'])") !== null)
  );
}

export function AppShell() {
  const { collapsed, width, toggleCollapsed, setWidth, commitWidth } = useSidebar();
  const mobile = useMediaQuery(MOBILE_QUERY);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const { pathname } = useLocation();
  useInboxStream();

  // Close the drawer on route or breakpoint change, during render rather than in an effect.
  const drawerKey = `${pathname}|${mobile}`;
  const [lastDrawerKey, setLastDrawerKey] = useState(drawerKey);
  if (lastDrawerKey !== drawerKey) {
    setLastDrawerKey(drawerKey);
    setDrawerOpen(false);
  }

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || isEditable(e.target)) return;
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "b") {
        e.preventDefault();
        if (mobile) setDrawerOpen((o) => !o);
        else toggleCollapsed();
      } else if (e.key === "Escape") {
        setDrawerOpen(false);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [mobile, toggleCollapsed]);

  let toggle = {
    label: collapsed ? "Expand sidebar" : "Collapse sidebar",
    expanded: !collapsed,
    Icon: collapsed ? PanelLeftOpen : PanelLeftClose,
    onClick: toggleCollapsed,
  };
  if (mobile) {
    toggle = {
      label: drawerOpen ? "Close navigation" : "Open navigation",
      expanded: drawerOpen,
      Icon: drawerOpen ? X : Menu,
      onClick: () => setDrawerOpen((o) => !o),
    };
  }

  return (
    <TooltipProvider delayDuration={300}>
      <ToastProvider>
        <div className="flex h-dvh flex-col bg-harbor">
          <a
            href="#main"
            className="sr-only z-50 rounded-md bg-surface px-3 py-2 text-ink focus:not-sr-only focus:absolute focus:top-2 focus:left-2"
          >
            Skip to content
          </a>
          <ActingBanner />
          <header className="relative z-50 flex h-14 shrink-0 items-center gap-2 px-3">
            <Button
              variant="harbor"
              size="icon"
              aria-label={toggle.label}
              aria-expanded={toggle.expanded}
              aria-controls="app-sidebar"
              title={mobile ? undefined : `${toggle.label} (Ctrl+B)`}
              onClick={toggle.onClick}
            >
              <toggle.Icon aria-hidden />
            </Button>
            <Wordmark />
            <div className="ml-auto flex items-center gap-1">
              <ThemeToggle />
              <UserMenu />
            </div>
          </header>
          <div className="relative flex min-h-0 flex-1">
            {mobile && drawerOpen && (
              <div
                data-testid="drawer-backdrop"
                aria-hidden
                className="fixed inset-0 z-30 bg-black/50"
                onClick={() => setDrawerOpen(false)}
              />
            )}
            <Sidebar
              collapsed={collapsed}
              width={width}
              onResize={setWidth}
              onResizeEnd={commitWidth}
              mobile={mobile}
              open={drawerOpen}
              onNavigate={() => setDrawerOpen(false)}
            />
            <main
              id="main"
              tabIndex={-1}
              className="min-w-0 flex-1 overflow-y-auto bg-bg outline-none md:rounded-tl-2xl"
            >
              <Outlet />
            </main>
          </div>
        </div>
      </ToastProvider>
    </TooltipProvider>
  );
}
