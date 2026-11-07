import { CircleAlert, CircleCheck, TriangleAlert, X } from "lucide-react";
import { useCallback, useRef, useState, type ReactNode } from "react";

import { ToastContext, type Toast, type ToastTone } from "@/hooks/toast";
import { cn } from "@/lib/utils";

const TTL_MS = 5000;

const TONES = {
  success: {
    icon: CircleCheck,
    border: "border-emerald-600/40",
    text: "text-emerald-700 dark:text-emerald-300",
  },
  warning: {
    icon: TriangleAlert,
    border: "border-amber-500/50",
    text: "text-amber-700 dark:text-amber-300",
  },
  error: {
    icon: CircleAlert,
    border: "border-rose-500/50",
    text: "text-rose-700 dark:text-rose-300",
  },
} as const;

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
          const Icon = TONES[t.tone].icon;
          return (
            <div
              key={t.id}
              className={cn(
                "pointer-events-auto flex w-full items-start gap-2.5 rounded-lg border bg-surface px-3.5 py-3 text-sm shadow-lg shadow-black/10",
                TONES[t.tone].border,
              )}
            >
              <Icon aria-hidden className={cn("mt-0.5 size-4 shrink-0", TONES[t.tone].text)} />
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
