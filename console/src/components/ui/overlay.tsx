import * as React from "react";
import * as DialogPrimitive from "@radix-ui/react-dialog";
import * as TabsPrimitive from "@radix-ui/react-tabs";
import * as DropdownPrimitive from "@radix-ui/react-dropdown-menu";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";

// shadcn/ui Dialog (the console's modal), Tabs (underlined), DropdownMenu ("Actions ▾").

export const Dialog = DialogPrimitive.Root;
export const DialogTrigger = DialogPrimitive.Trigger;
export const DialogClose = DialogPrimitive.Close;

export function DialogContent({ className, children, title, description, footer, wide, ...props }:
  React.ComponentProps<typeof DialogPrimitive.Content> & { title: string; description?: React.ReactNode; footer?: React.ReactNode; wide?: boolean }) {
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-[#16191f]/60" />
      <DialogPrimitive.Content
        className={cn(
          "fixed left-1/2 top-[8vh] z-50 -translate-x-1/2 w-[calc(100%-2rem)] max-h-[84vh] flex flex-col rounded-2xl bg-card shadow-2xl border",
          wide ? "max-w-4xl" : "max-w-xl",
          className,
        )}
        {...props}
      >
        <div className="flex items-start justify-between gap-4 px-5 pt-4 pb-3 border-b">
          <div>
            <DialogPrimitive.Title className="text-lg font-bold">{title}</DialogPrimitive.Title>
            {description && <DialogPrimitive.Description className="text-sm text-muted-foreground mt-0.5">{description}</DialogPrimitive.Description>}
          </div>
          <DialogPrimitive.Close className="text-muted-foreground hover:text-foreground cursor-pointer" aria-label="Close">
            <X className="size-5" />
          </DialogPrimitive.Close>
        </div>
        <div className="px-5 py-4 overflow-auto space-y-4">{children}</div>
        {footer && <div className="flex justify-end gap-2 px-5 py-3 border-t">{footer}</div>}
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  );
}

export const Tabs = TabsPrimitive.Root;

export function TabsList({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.List>) {
  return <TabsPrimitive.List className={cn("flex gap-1 border-b overflow-x-auto", className)} {...props} />;
}

export function TabsTrigger({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      className={cn(
        "px-4 py-2 text-sm font-bold text-muted-foreground border-b-[3px] border-transparent -mb-px whitespace-nowrap cursor-pointer hover:text-foreground data-[state=active]:text-link data-[state=active]:border-link",
        className,
      )}
      {...props}
    />
  );
}

export function TabsContent({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.Content>) {
  return <TabsPrimitive.Content className={cn("pt-4 focus-visible:outline-none", className)} {...props} />;
}

export const DropdownMenu = DropdownPrimitive.Root;
export const DropdownMenuTrigger = DropdownPrimitive.Trigger;

export function DropdownMenuContent({ className, ...props }: React.ComponentProps<typeof DropdownPrimitive.Content>) {
  return (
    <DropdownPrimitive.Portal>
      <DropdownPrimitive.Content
        align="end"
        sideOffset={4}
        className={cn("z-50 min-w-44 rounded-lg border bg-card p-1 shadow-lg", className)}
        {...props}
      />
    </DropdownPrimitive.Portal>
  );
}

export function DropdownMenuItem({ className, danger, ...props }: React.ComponentProps<typeof DropdownPrimitive.Item> & { danger?: boolean }) {
  return (
    <DropdownPrimitive.Item
      className={cn(
        "flex cursor-pointer select-none items-center gap-2 rounded-md px-3 py-1.5 text-sm outline-none data-[highlighted]:bg-accent data-[disabled]:opacity-40 data-[disabled]:pointer-events-none",
        danger && "text-danger",
        className,
      )}
      {...props}
    />
  );
}

export const DropdownMenuSeparator = () => <DropdownPrimitive.Separator className="my-1 h-px bg-border" />;
