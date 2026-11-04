import { useId, type ReactNode } from "react";

export function EmptyState({
  title,
  body,
  action,
}: {
  title: string;
  body: string;
  action?: ReactNode;
}) {
  const titleId = useId();
  return (
    <section
      aria-labelledby={titleId}
      className="flex max-w-xl flex-col items-start gap-4 rounded-xl border border-dashed border-line px-6 py-10 sm:px-10"
    >
      <svg viewBox="0 0 64 40" className="h-10 w-16 text-muted" aria-hidden>
        <circle cx="10" cy="30" r="5" className="fill-beacon" />
        <path
          d="M10 18a12 12 0 0 1 12 12M10 8a22 22 0 0 1 22 22M10-2a32 32 0 0 1 32 32"
          fill="none"
          stroke="currentColor"
          strokeWidth="2.5"
          strokeLinecap="round"
          strokeDasharray="2 5"
        />
      </svg>
      <h2 id={titleId} className="font-display text-xl font-semibold tracking-tight">
        {title}
      </h2>
      <p className="max-w-prose text-muted">{body}</p>
      {action}
    </section>
  );
}
