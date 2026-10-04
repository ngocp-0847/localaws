import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

// shadcn/ui Button with the console's variants: primary = orange, normal = outlined, link, icon.
const buttonVariants = cva(
  "inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-full text-[13px] font-bold transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-45 [&_svg]:size-4 [&_svg]:shrink-0 cursor-pointer",
  {
    variants: {
      variant: {
        primary: "bg-primary text-primary-foreground hover:bg-[#ec7211]",
        normal: "border-2 border-link text-link bg-card hover:bg-accent",
        danger: "border-2 border-danger text-danger bg-card hover:bg-danger/10",
        ghost: "text-foreground hover:bg-muted",
        link: "text-link hover:underline rounded-none px-0 font-normal",
        icon: "text-muted-foreground hover:text-foreground hover:bg-muted rounded-md",
      },
      size: { default: "h-8 px-4", sm: "h-7 px-3 text-xs", icon: "size-8" },
    },
    defaultVariants: { variant: "normal", size: "default" },
  },
);

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement>, VariantProps<typeof buttonVariants> {
  asChild?: boolean;
}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(({ className, variant, size, asChild = false, ...props }, ref) => {
  const Comp = asChild ? Slot : "button";
  return <Comp className={cn(buttonVariants({ variant, size, className }))} ref={ref} {...props} />;
});
Button.displayName = "Button";
