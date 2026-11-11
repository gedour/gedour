import { Tooltip as Primitive } from "radix-ui";
import type { ReactNode } from "react";

export const TooltipProvider = Primitive.Provider;

export function Tooltip({
  label,
  side = "right",
  disabled = false,
  children,
}: {
  label: string;
  side?: "right" | "bottom";
  disabled?: boolean;
  children: ReactNode;
}) {
  if (disabled) return children;
  return (
    <Primitive.Root>
      <Primitive.Trigger asChild>{children}</Primitive.Trigger>
      <Primitive.Portal>
        <Primitive.Content
          side={side}
          sideOffset={8}
          className="z-50 max-w-[min(24rem,var(--radix-tooltip-content-available-width))] rounded-md bg-ink px-2 py-1 text-xs font-medium break-words text-bg"
        >
          {label}
        </Primitive.Content>
      </Primitive.Portal>
    </Primitive.Root>
  );
}
