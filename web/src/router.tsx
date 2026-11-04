import { Navigate, type RouteObject } from "react-router";

import { AppShell } from "@/components/AppShell";
import { NAV } from "@/nav";
import { AuditPage } from "@/pages/AuditPage";
import { NotFound } from "@/pages/NotFound";
import { PlaceholderPage } from "@/pages/PlaceholderPage";
import { SignedOutPage } from "@/pages/SignedOutPage";

export const routes: RouteObject[] = [
  { path: "/auth/signed-out", element: <SignedOutPage /> },
  {
    path: "/",
    element: <AppShell />,
    children: [
      { index: true, element: <Navigate to="/inbox" replace /> },
      ...NAV.map((item) => ({
        path: item.path,
        element:
          item.path === "/audit" ? <AuditPage item={item} /> : <PlaceholderPage item={item} />,
      })),
      { path: "*", element: <NotFound /> },
    ],
  },
];
