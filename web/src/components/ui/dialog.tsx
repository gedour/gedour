import { useEffect, useId, useRef, type ReactNode } from "react";

import { cn } from "@/lib/utils";

// Native <dialog> + showModal(): focus trap, Escape and inert background with no injected <style>,
// which the CSP would block (Radix modal mode adds one).
export function Dialog({
  open,
  title,
  onClose,
  children,
  className,
  dismissible = true,
}: {
  open: boolean;
  title: string;
  onClose: () => void;
  children: ReactNode;
  className?: string;
  /** False: Escape does nothing; only the content's own buttons close it. */
  dismissible?: boolean;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    if (!open && d.open) d.close();
  }, [open]);
  return (
    <dialog
      ref={ref}
      aria-labelledby={titleId}
      // React bubbles cancel and close up the component tree: a nested dialog's must not close this one.
      onCancel={(e) => {
        if (e.target === e.currentTarget && !dismissible) e.preventDefault();
      }}
      onClose={(e) => {
        if (e.target !== e.currentTarget) return;
        // A browser may close it despite the cancelled Escape: put it back while it must stay.
        if (!dismissible && open) ref.current?.showModal();
        else onClose();
      }}
      className={cn(
        "m-auto w-[min(28rem,calc(100vw-2rem))] rounded-xl border border-line bg-surface p-0 text-ink shadow-2xl backdrop:bg-black/50",
        className,
      )}
    >
      {open && (
        <div className="flex flex-col gap-4 p-6">
          <h2 id={titleId} className="font-display text-lg font-semibold tracking-tight">
            {title}
          </h2>
          {children}
        </div>
      )}
    </dialog>
  );
}

export function DialogActions({ children }: { children: ReactNode }) {
  return <div className="flex flex-wrap justify-end gap-2 pt-2">{children}</div>;
}
