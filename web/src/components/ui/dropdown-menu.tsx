import { Check } from "lucide-react";
import { DropdownMenu as Primitive } from "radix-ui";
import type { ComponentProps } from "react";

import { cn } from "@/lib/utils";

// Modal mode injects a <style> (react-remove-scroll) that the CSP blocks; forced off, not overridable.
export function DropdownMenu(props: Omit<ComponentProps<typeof Primitive.Root>, "modal">) {
  return <Primitive.Root {...props} modal={false} />;
}

export const DropdownMenuTrigger = Primitive.Trigger;
export const DropdownMenuRadioGroup = Primitive.RadioGroup;

export function DropdownMenuContent({
  className,
  sideOffset = 6,
  ...props
}: ComponentProps<typeof Primitive.Content>) {
  return (
    <Primitive.Portal>
      <Primitive.Content
        sideOffset={sideOffset}
        className={cn(
          "z-50 min-w-36 rounded-lg border border-line bg-surface p-1 text-ink shadow-lg shadow-black/10",
          className,
        )}
        {...props}
      />
    </Primitive.Portal>
  );
}

export function DropdownMenuRadioItem({
  className,
  children,
  ...props
}: ComponentProps<typeof Primitive.RadioItem>) {
  return (
    <Primitive.RadioItem
      className={cn(
        "relative flex cursor-default select-none items-center gap-2 rounded-md py-1.5 pr-8 pl-2 text-sm outline-none data-[highlighted]:bg-beacon-soft [&_svg]:size-4",
        className,
      )}
      {...props}
    >
      {children}
      <Primitive.ItemIndicator className="absolute right-2 inline-flex">
        <Check aria-hidden />
      </Primitive.ItemIndicator>
    </Primitive.RadioItem>
  );
}
