import { RotateCw } from "lucide-react";

import { Button } from "@/components/ui/button";

export function ErrorState({
  title,
  error,
  onRetry,
}: {
  title: string;
  error: Error;
  onRetry: () => void;
}) {
  return (
    <div
      role="alert"
      className="flex max-w-xl flex-col items-start gap-3 rounded-xl border border-rose-500/40 bg-rose-500/8 px-6 py-5"
    >
      <p className="font-medium">{title}</p>
      <p className="text-sm text-muted">{error.message}</p>
      <Button variant="outline" onClick={onRetry}>
        <RotateCw aria-hidden />
        Retry
      </Button>
    </div>
  );
}
