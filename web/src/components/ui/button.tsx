import { cva, type VariantProps } from "class-variance-authority";
import { Slot } from "radix-ui";
import type { ComponentProps } from "react";

import { cn } from "@/lib/utils";

const buttonVariants = cva(
  "inline-flex shrink-0 items-center justify-center gap-2 rounded-md text-sm font-medium transition-colors disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        ghost: "text-muted hover:bg-line/60 hover:text-ink",
        outline: "border border-line bg-surface text-ink hover:bg-line/40",
        harbor: "text-harbor-muted hover:bg-harbor-hover hover:text-harbor-ink",
        primary: "bg-ink text-bg shadow-sm hover:bg-ink/85",
        danger: "bg-rose-700 text-white shadow-sm hover:bg-rose-800",
      },
      size: {
        default: "h-9 px-3",
        icon: "size-9",
        sm: "h-8 px-2.5 text-xs",
      },
    },
    defaultVariants: { variant: "ghost", size: "default" },
  },
);

export interface ButtonProps extends ComponentProps<"button">, VariantProps<typeof buttonVariants> {
  asChild?: boolean;
}

export function Button({ className, variant, size, asChild = false, ...props }: ButtonProps) {
  const Comp = asChild ? Slot.Root : "button";
  return <Comp className={cn(buttonVariants({ variant, size }), className)} {...props} />;
}
