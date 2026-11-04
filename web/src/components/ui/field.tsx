import { useId, type ReactNode } from "react";

import { cn } from "@/lib/utils";

export const inputClass =
  "h-9 w-full rounded-md border border-line bg-surface px-2.5 text-sm text-ink shadow-xs placeholder:text-muted/70 focus-visible:border-beacon aria-[invalid=true]:border-rose-600 disabled:opacity-60 read-only:bg-bg/60 read-only:text-muted";

export interface ControlProps {
  id: string;
  "aria-describedby"?: string;
  "aria-invalid"?: boolean;
}

/** Label, control, hint and error wired together; the control comes from children(props). */
export function Field({
  label,
  hint,
  error,
  className,
  children,
}: {
  label: ReactNode;
  hint?: ReactNode;
  error?: string;
  className?: string;
  children: (props: ControlProps) => ReactNode;
}) {
  const id = useId();
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;
  const describedBy = [error && errorId, hint && hintId].filter(Boolean).join(" ") || undefined;
  return (
    <div className={cn("flex flex-col gap-1.5", className)}>
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      {children({ id, "aria-describedby": describedBy, "aria-invalid": error ? true : undefined })}
      {error && (
        <p id={errorId} className="text-sm text-rose-700 dark:text-rose-300">
          {error}
        </p>
      )}
      {hint && (
        <p id={hintId} className="text-xs text-muted">
          {hint}
        </p>
      )}
    </div>
  );
}
