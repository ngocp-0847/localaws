import * as React from "react";
import { Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input, Textarea } from "@/components/ui/form";
import { Dialog, DialogContent } from "@/components/ui/overlay";
import { cn, tryJSON } from "@/lib/utils";

/** JSON text area that says whether it parses, with Format */
export function JsonEditor({ value, onChange, rows = 12, className }: { value: string; onChange: (v: string) => void; rows?: number; className?: string }) {
  const r = value.trim() ? tryJSON(value) : { ok: true as const, value: null };
  return (
    <div className={cn("space-y-1", className)}>
      <Textarea rows={rows} value={value} onChange={(e) => onChange(e.target.value)} className={cn(!r.ok && "border-danger")} />
      <div className="flex items-center justify-between text-xs">
        <span className={r.ok ? "text-success" : "text-danger font-bold"}>{r.ok ? "Valid JSON" : `Invalid JSON: ${r.error}`}</span>
        <Button variant="link" size="sm" disabled={!r.ok || !value.trim()} onClick={() => r.ok && onChange(JSON.stringify(r.value, null, 2))}>Format</Button>
      </div>
    </div>
  );
}

/** delete confirmation the way the console does it: type the name (or "delete") */
export function ConfirmDelete({ open, onOpenChange, what, name, confirmWord, onConfirm, children }: {
  open: boolean; onOpenChange: (v: boolean) => void; what: string; name: string; confirmWord?: string; onConfirm: () => Promise<unknown>; children?: React.ReactNode;
}) {
  const word = confirmWord ?? name;
  const [typed, setTyped] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => { if (open) setTyped(""); }, [open]);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title={`Delete ${what}`} description={<>Delete <b className="break-all">{name}</b>? This cannot be undone.</>}
        footer={<>
          <Button variant="link" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" disabled={typed !== word || busy} onClick={async () => { setBusy(true); await onConfirm(); setBusy(false); onOpenChange(false); }}>Delete</Button>
        </>}>
        {children}
        <div className="text-sm">To confirm, type <b className="font-mono break-all">{word}</b></div>
        <Input value={typed} onChange={(e) => setTyped(e.target.value)} placeholder={word} autoFocus />
      </DialogContent>
    </Dialog>
  );
}

export function TextFilter({ value, onChange, placeholder, count }: { value: string; onChange: (v: string) => void; placeholder: string; count?: number }) {
  return (
    <div className="flex items-center gap-3 mb-3">
      <div className="relative w-full max-w-md">
        <Search className="size-4 absolute left-2.5 top-2 text-muted-foreground" />
        <Input value={value} onChange={(e) => onChange(e.target.value)} placeholder={placeholder} className="pl-8" />
      </div>
      {value && count !== undefined && <span className="text-sm text-muted-foreground whitespace-nowrap">{count} match{count === 1 ? "" : "es"}</span>}
    </div>
  );
}

export function useFilter<T>(rows: T[] | undefined, text: (r: T) => string) {
  const [q, setQ] = React.useState("");
  const out = React.useMemo(() => (rows ?? []).filter((r) => !q || text(r).toLowerCase().includes(q.toLowerCase())), [rows, q, text]);
  return { q, setQ, rows: out };
}
