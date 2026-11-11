import { Popover as Primitive } from "radix-ui";
import type { ComponentProps } from "react";

import { cn } from "@/lib/utils";

// Modal mode injects a <style> (react-remove-scroll) that the CSP blocks; forced off, not overridable.
export function Popover(props: Omit<ComponentProps<typeof Primitive.Root>, "modal">) {
  return <Primitive.Root {...props} modal={false} />;
}

export const PopoverTrigger = Primitive.Trigger;

export function PopoverContent({
  className,
  sideOffset = 6,
  ...props
}: ComponentProps<typeof Primitive.Content>) {
  return (
    <Primitive.Portal>
      <Primitive.Content
        sideOffset={sideOffset}
        collisionPadding={8}
        className={cn(
          "z-50 max-w-[min(24rem,var(--radix-popover-content-available-width))] rounded-lg border border-line bg-surface p-1 text-ink shadow-lg shadow-black/10",
          className,
        )}
        {...props}
      />
    </Primitive.Portal>
  );
}
