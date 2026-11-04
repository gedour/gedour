import { Navigate, type RouteObject } from "react-router";

import { AppShell } from "@/components/AppShell";
import { NAV } from "@/nav";
import { NotFound } from "@/pages/NotFound";
import { PlaceholderPage } from "@/pages/PlaceholderPage";

export const routes: RouteObject[] = [
  {
    path: "/",
    element: <AppShell />,
    children: [
      { index: true, element: <Navigate to="/inbox" replace /> },
      ...NAV.map((item) => ({ path: item.path, element: <PlaceholderPage item={item} /> })),
      { path: "*", element: <NotFound /> },
    ],
  },
];
