import { CircleAlert, CircleCheck, X } from "lucide-react";
import { useCallback, useRef, useState, type ReactNode } from "react";

import { ToastContext, type Toast, type ToastTone } from "@/hooks/toast";
import { cn } from "@/lib/utils";

const TTL_MS = 5000;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const next = useRef(0);
  const dismiss = useCallback((id: number) => setToasts((ts) => ts.filter((t) => t.id !== id)), []);
  const push = useCallback(
    (message: string, tone: ToastTone = "success") => {
      const id = ++next.current;
      setToasts((ts) => [...ts.slice(-3), { id, message, tone }]);
      setTimeout(() => dismiss(id), TTL_MS);
    },
    [dismiss],
  );
  return (
    <ToastContext value={push}>
      {children}
      <div
        role="status"
        aria-live="polite"
        className="pointer-events-none fixed inset-x-3 bottom-3 z-50 flex flex-col items-end gap-2 sm:left-auto sm:w-96"
      >
        {toasts.map((t) => {
          const Icon = t.tone === "error" ? CircleAlert : CircleCheck;
          return (
            <div
              key={t.id}
              className={cn(
                "pointer-events-auto flex w-full items-start gap-2.5 rounded-lg border bg-surface px-3.5 py-3 text-sm shadow-lg shadow-black/10",
                t.tone === "error" ? "border-rose-500/50" : "border-emerald-600/40",
              )}
            >
              <Icon
                aria-hidden
                className={cn(
                  "mt-0.5 size-4 shrink-0",
                  t.tone === "error"
                    ? "text-rose-700 dark:text-rose-300"
                    : "text-emerald-700 dark:text-emerald-300",
                )}
              />
              <p className="flex-1">{t.message}</p>
              <button
                type="button"
                aria-label="Dismiss"
                className="rounded text-muted hover:text-ink"
                onClick={() => dismiss(t.id)}
              >
                <X aria-hidden className="size-4" />
              </button>
            </div>
          );
        })}
      </div>
    </ToastContext>
  );
}
