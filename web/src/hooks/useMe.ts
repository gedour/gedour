import { useQuery } from "@tanstack/react-query";

import { getMe } from "@/api/client";

// The signed-in user; react-query's cache is the shared current-user state.
export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: getMe, staleTime: Infinity, retry: false });
}
