import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";

import { ThemeProvider } from "@/hooks/ThemeProvider";
import { routes } from "@/router";
import { ME } from "@/test/api";
import type { Me } from "@/types/api";

/**
 * Renders the app at path. With a user (default ME) the session is already confirmed, so the shell
 * shows at once; /api/v1/me is still fetched on mount, which sets the CSRF token. With null the app
 * boots as in the browser: session check first.
 */
export function renderApp(path = "/inbox", me: Me | null = ME) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (me) {
    client.setQueryData(["me"], me);
    void client.invalidateQueries({ queryKey: ["me"], refetchType: "none" });
  }
  const utils = render(
    <ThemeProvider>
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </ThemeProvider>,
  );
  return { ...utils, router, client };
}
