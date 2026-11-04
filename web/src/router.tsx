import type { ReactNode } from "react";
import { Navigate, type RouteObject } from "react-router";

import { AppShell } from "@/components/AppShell";
import { NAV, type NavItem } from "@/nav";
import { AuditPage } from "@/pages/AuditPage";
import { ChannelsPage } from "@/pages/ChannelsPage";
import { CredentialsPage } from "@/pages/CredentialsPage";
import { InboxPage } from "@/pages/InboxPage";
import { NotFound } from "@/pages/NotFound";
import { PlaceholderPage } from "@/pages/PlaceholderPage";
import { RuleFormPage } from "@/pages/RuleFormPage";
import { RulesPage } from "@/pages/RulesPage";
import { ScriptFormRoute } from "@/pages/ScriptFormRoute";
import { ScriptsPage } from "@/pages/ScriptsPage";
import { SettingsPage } from "@/pages/SettingsPage";
import { SignedOutPage } from "@/pages/SignedOutPage";
import { WatchDetailPage } from "@/pages/WatchDetailPage";
import { WatchesPage } from "@/pages/WatchesPage";
import { WatchFormPage } from "@/pages/WatchFormPage";

const PAGES: Record<string, (item: NavItem) => ReactNode> = {
  "/inbox": (item) => <InboxPage item={item} />,
  "/channels": (item) => <ChannelsPage item={item} />,
  "/rules": (item) => <RulesPage item={item} />,
  "/scripts": (item) => <ScriptsPage item={item} />,
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
      { path: "/rules/new", element: <RuleFormPage /> },
      { path: "/rules/:id", element: <RuleFormPage /> },
      { path: "/scripts/new", element: <ScriptFormRoute /> },
      { path: "/scripts/:id", element: <ScriptFormRoute /> },
      { path: "/settings/credentials", element: <CredentialsPage /> },
      { path: "*", element: <NotFound /> },
    ],
  },
];
