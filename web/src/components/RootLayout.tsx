import { Outlet } from "react-router";

import { useNotificationLinks } from "@/hooks/usePush";

/** Above every route, the gate and the signed-out page included: a notification click routes any tab. */
export function RootLayout() {
  useNotificationLinks();
  return <Outlet />;
}
