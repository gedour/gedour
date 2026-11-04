import type { ReactNode } from "react";
import { Navigate, type RouteObject } from "react-router";

import { AppShell } from "@/components/AppShell";
import { NAV, type NavItem } from "@/nav";
import { AuditPage } from "@/pages/AuditPage";
import { CredentialsPage } from "@/pages/CredentialsPage";
import { NotFound } from "@/pages/NotFound";
import { PlaceholderPage } from "@/pages/PlaceholderPage";
import { SettingsPage } from "@/pages/SettingsPage";
import { SignedOutPage } from "@/pages/SignedOutPage";
import { WatchDetailPage } from "@/pages/WatchDetailPage";
import { WatchesPage } from "@/pages/WatchesPage";
import { WatchFormPage } from "@/pages/WatchFormPage";

const PAGES: Record<string, (item: NavItem) => ReactNode> = {
  "/audit": (item) => <AuditPage item={item} />,
  "/watches": (item) => <WatchesPage item={item} />,
  "/settings": (item) => <SettingsPage item={item} />,
};

export const routes: RouteObject[] = [
  { path: "/auth/signed-out", element: <SignedOutPage /> },
  {
    path: "/",
    element: <AppShell />,
    children: [
      { index: true, element: <Navigate to="/inbox" replace /> },
      ...NAV.map((item) => ({
        path: item.path,
        element: PAGES[item.path]?.(item) ?? <PlaceholderPage item={item} />,
      })),
      { path: "/watches/new", element: <WatchFormPage /> },
      { path: "/watches/:id", element: <WatchDetailPage /> },
      { path: "/watches/:id/edit", element: <WatchFormPage /> },
      { path: "/settings/credentials", element: <CredentialsPage /> },
      { path: "*", element: <NotFound /> },
    ],
  },
];
