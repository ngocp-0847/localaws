import * as React from "react";
import * as SwitchPrimitive from "@radix-ui/react-switch";
import * as CheckboxPrimitive from "@radix-ui/react-checkbox";
import { Check } from "lucide-react";
import { cn } from "@/lib/utils";

// shadcn/ui Input, Textarea, Label, Select (native), Switch, Checkbox — console styling.

export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(({ className, ...props }, ref) => (
  <input
    ref={ref}
    className={cn(
      "h-8 w-full rounded-lg border-2 border-input bg-card px-2.5 text-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:border-link disabled:opacity-50",
      className,
    )}
    {...props}
  />
));
Input.displayName = "Input";

export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(({ className, ...props }, ref) => (
  <textarea
    ref={ref}
    spellCheck={false}
    className={cn(
      "w-full rounded-lg border-2 border-input bg-card px-2.5 py-2 text-[13px] font-mono leading-5 focus-visible:outline-none focus-visible:border-link",
      className,
    )}
    {...props}
  />
));
Textarea.displayName = "Textarea";

export function Label({ className, ...props }: React.LabelHTMLAttributes<HTMLLabelElement>) {
  return <label className={cn("text-sm font-bold block mb-1", className)} {...props} />;
}

export function Field({ label, hint, error, children }: { label: string; hint?: React.ReactNode; error?: string | null; children: React.ReactNode }) {
  return (
    <div className="space-y-1">
      <Label>{label}</Label>
      {hint && <div className="text-xs text-muted-foreground -mt-0.5 mb-1">{hint}</div>}
      {children}
      {error && <div className="text-xs text-danger font-bold">{error}</div>}
    </div>
  );
}

export const Select = React.forwardRef<HTMLSelectElement, React.SelectHTMLAttributes<HTMLSelectElement>>(({ className, ...props }, ref) => (
  <select
    ref={ref}
    className={cn("h-8 rounded-lg border-2 border-input bg-card px-2 text-sm focus-visible:outline-none focus-visible:border-link", className)}
    {...props}
  />
));
Select.displayName = "Select";

export function Switch({ className, ...props }: React.ComponentProps<typeof SwitchPrimitive.Root>) {
  return (
    <SwitchPrimitive.Root
      className={cn(
        "peer inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors data-[state=checked]:bg-link data-[state=unchecked]:bg-[#8c8c94]",
        className,
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb className="pointer-events-none block size-4 rounded-full bg-white shadow transition-transform data-[state=checked]:translate-x-4 data-[state=unchecked]:translate-x-0" />
    </SwitchPrimitive.Root>
  );
}

export function Checkbox({ className, ...props }: React.ComponentProps<typeof CheckboxPrimitive.Root>) {
  return (
    <CheckboxPrimitive.Root
      className={cn(
        "size-4 shrink-0 rounded-[3px] border-2 border-[#7d8998] data-[state=checked]:bg-link data-[state=checked]:border-link text-white cursor-pointer",
        className,
      )}
      {...props}
    >
      <CheckboxPrimitive.Indicator className="flex items-center justify-center">
        <Check className="size-3" strokeWidth={4} />
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Root>
  );
}
