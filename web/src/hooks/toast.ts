import { createContext, useContext } from "react";

export type ToastTone = "success" | "error";

export interface Toast {
  id: number;
  message: string;
  tone: ToastTone;
}

export const ToastContext = createContext<(message: string, tone?: ToastTone) => void>(() => {});

/** Shows a short-lived message in the shell's live region. */
export function useToast() {
  return useContext(ToastContext);
}
