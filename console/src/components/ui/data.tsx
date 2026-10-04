import * as React from "react";
import { Check, CircleX, Clock, Copy, Info, Loader2, CircleSlash, CircleDot, ChevronUp, ChevronDown } from "lucide-react";
import { cn } from "@/lib/utils";

// Containers, tables, status indicators, key/value grids, JSON blocks — the building blocks of
// every console page (shadcn/ui Card + Table + Badge, Cloudscape look).

export function Container({ title, counter, description, actions, children, className, flush }:
  { title?: React.ReactNode; counter?: number | string; description?: React.ReactNode; actions?: React.ReactNode; children?: React.ReactNode; className?: string; flush?: boolean }) {
  return (
    <section className={cn("rounded-2xl bg-card shadow-[0_1px_3px_rgba(0,7,22,0.12),0_0_1px_rgba(0,7,22,0.25)]", className)}>
      {(title || actions) && (
        <header className="flex flex-wrap items-start justify-between gap-3 px-5 pt-4 pb-3">
          <div className="min-w-0">
            <h2 className="text-lg font-bold leading-6">
              {title}
              {counter !== undefined && <span className="ml-1.5 font-normal text-muted-foreground">({counter})</span>}
            </h2>
            {description && <p className="text-sm text-muted-foreground mt-0.5">{description}</p>}
          </div>
          {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className={cn(flush ? "" : "px-5 pb-5", !title && !actions && !flush && "pt-5")}>{children}</div>
    </section>
  );
}

export type Column<T> = {
  key: string;
  header: React.ReactNode;
  cell: (row: T) => React.ReactNode;
  sort?: (row: T) => string | number;
  className?: string;
};

export function DataTable<T>({ rows, columns, rowKey, selected, onSelect, multi, empty, initialSort, onRowClick }: {
  rows: T[];
  columns: Column<T>[];
  rowKey: (r: T) => string;
  selected?: string[];
  onSelect?: (keys: string[]) => void;
  multi?: boolean;
  empty?: React.ReactNode;
  initialSort?: { key: string; desc?: boolean };
  onRowClick?: (r: T) => void;
}) {
  const [sort, setSort] = React.useState(initialSort);
  const sorted = React.useMemo(() => {
    const col = columns.find((c) => c.key === sort?.key);
    if (!col?.sort) return rows;
    const s = [...rows].sort((a, b) => {
      const x = col.sort!(a), y = col.sort!(b);
      return x < y ? -1 : x > y ? 1 : 0;
    });
    return sort?.desc ? s.reverse() : s;
  }, [rows, columns, sort]);
  const sel = new Set(selected ?? []);
  const toggle = (k: string) => {
    if (!onSelect) return;
    if (multi) onSelect(sel.has(k) ? [...sel].filter((x) => x !== k) : [...sel, k]);
    else onSelect(sel.has(k) ? [] : [k]);
  };
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm border-collapse">
        <thead>
          <tr className="border-b-2">
            {onSelect && (
              <th className="w-9 px-3 py-2">
                {multi && (
                  <input type="checkbox" className="accent-link cursor-pointer" checked={rows.length > 0 && sel.size === rows.length}
                    onChange={(e) => onSelect(e.target.checked ? rows.map(rowKey) : [])} />
                )}
              </th>
            )}
            {columns.map((c) => (
              <th key={c.key} className={cn("px-3 py-2 text-left font-bold text-[13px] whitespace-nowrap", c.sort && "cursor-pointer select-none", c.className)}
                onClick={() => c.sort && setSort({ key: c.key, desc: sort?.key === c.key ? !sort.desc : false })}>
                <span className="inline-flex items-center gap-1">
                  {c.header}
                  {c.sort && sort?.key === c.key && (sort.desc ? <ChevronDown className="size-3.5" /> : <ChevronUp className="size-3.5" />)}
                </span>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sorted.map((r) => {
            const k = rowKey(r);
            return (
              <tr key={k} className={cn("border-b last:border-b-0 hover:bg-accent/60", sel.has(k) && "bg-accent", (onSelect || onRowClick) && "cursor-pointer")}
                onClick={() => (onRowClick ? onRowClick(r) : toggle(k))}>
                {onSelect && (
                  <td className="px-3 py-2" onClick={(e) => { e.stopPropagation(); toggle(k); }}>
                    <input type={multi ? "checkbox" : "radio"} className="accent-link cursor-pointer" checked={sel.has(k)} readOnly />
                  </td>
                )}
                {columns.map((c) => (
                  <td key={c.key} className={cn("px-3 py-2 align-top", c.className)}>{c.cell(r)}</td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
      {rows.length === 0 && <div className="py-10 text-center text-muted-foreground">{empty ?? "No resources"}</div>}
    </div>
  );
}

const STATUS: Record<string, { cls: string; icon: React.ComponentType<{ className?: string }>; spin?: boolean }> = {
  SUCCEEDED: { cls: "text-success", icon: Check },
  ACTIVE: { cls: "text-success", icon: Check },
  ENABLED: { cls: "text-success", icon: Check },
  RUNNING: { cls: "text-info", icon: Loader2, spin: true },
  PENDING: { cls: "text-info", icon: Clock },
  PROVISIONING: { cls: "text-info", icon: Clock },
  ENTERED: { cls: "text-info", icon: Loader2, spin: true },
  STOPPED: { cls: "text-muted-foreground", icon: CircleSlash },
  FAILED: { cls: "text-danger", icon: CircleX },
  TIMED_OUT: { cls: "text-danger", icon: CircleX },
  ABORTED: { cls: "text-danger", icon: CircleSlash },
  CAUGHT: { cls: "text-warning", icon: CircleDot },
  DISABLED: { cls: "text-muted-foreground", icon: CircleSlash },
  INACTIVE: { cls: "text-muted-foreground", icon: CircleSlash },
  DRAINING: { cls: "text-warning", icon: Clock },
};

/** the console's status indicator: icon + word, coloured by meaning */
export function Status({ value, label }: { value: string; label?: string }) {
  const s = STATUS[value] ?? { cls: "text-muted-foreground", icon: Info };
  const Icon = s.icon;
  const text = label ?? value.charAt(0) + value.slice(1).toLowerCase().replace(/_/g, " ");
  return (
    <span className={cn("inline-flex items-center gap-1 font-bold text-[13px] whitespace-nowrap", s.cls)}>
      <Icon className={cn("size-4", s.spin && "animate-spin")} />
      {text}
    </span>
  );
}

export function Badge({ children, tone = "grey", className }: { children: React.ReactNode; tone?: "grey" | "blue" | "green" | "red" | "orange"; className?: string }) {
  const t = {
    grey: "bg-[#414d5c] text-white",
    blue: "bg-link text-white",
    green: "bg-success text-white",
    red: "bg-danger text-white",
    orange: "bg-primary text-[#16191f]",
  }[tone];
  return <span className={cn("inline-block rounded-full px-2 py-px text-xs font-bold whitespace-nowrap", t, className)}>{children}</span>;
}

export function KeyValue({ items, cols = 3 }: { items: [React.ReactNode, React.ReactNode][]; cols?: 1 | 2 | 3 | 4 }) {
  const grid = { 1: "grid-cols-1", 2: "sm:grid-cols-2", 3: "sm:grid-cols-2 lg:grid-cols-3", 4: "sm:grid-cols-2 lg:grid-cols-4" }[cols];
  return (
    <dl className={cn("grid gap-x-8 gap-y-4", grid)}>
      {items.map(([k, v], i) => (
        <div key={i} className="min-w-0">
          <dt className="text-sm font-bold text-muted-foreground">{k}</dt>
          <dd className="text-sm mt-0.5 break-words">{v === "" || v == null ? "–" : v}</dd>
        </div>
      ))}
    </dl>
  );
}

export function Copyable({ value, children, mono = true }: { value: string; children?: React.ReactNode; mono?: boolean }) {
  const [done, setDone] = React.useState(false);
  return (
    <span className={cn("inline-flex items-start gap-1 break-all", mono && "font-mono text-[12.5px]")}>
      <button className="text-muted-foreground hover:text-foreground mt-0.5 cursor-pointer shrink-0" title="Copy"
        onClick={(e) => { e.stopPropagation(); navigator.clipboard?.writeText(value); setDone(true); setTimeout(() => setDone(false), 1200); }}>
        {done ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />}
      </button>
      <span>{children ?? value}</span>
    </span>
  );
}

export function Code({ children, className, max = true }: { children: React.ReactNode; className?: string; max?: boolean }) {
  return (
    <pre className={cn("rounded-lg bg-muted border p-3 text-[12.5px] leading-5 overflow-auto whitespace-pre-wrap break-all", max && "max-h-[60vh]", className)}>
      {children}
    </pre>
  );
}

export function Spinner({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 py-8 justify-center text-muted-foreground">
      <Loader2 className="size-5 animate-spin" /> {label}
    </div>
  );
}

export function Alert({ kind = "error", title, children }: { kind?: "error" | "info" | "success" | "warning"; title?: string; children?: React.ReactNode }) {
  const c = { error: "border-danger bg-danger/10", info: "border-link bg-accent", success: "border-success bg-success/10", warning: "border-warning bg-warning/10" }[kind];
  return (
    <div className={cn("rounded-xl border-2 px-4 py-3 text-sm", c)}>
      {title && <div className="font-bold">{title}</div>}
      {children}
    </div>
  );
}
