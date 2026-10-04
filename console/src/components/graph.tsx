import * as React from "react";
import { cn } from "@/lib/utils";

// State machine graph, drawn as SVG with a small layered layout (no graph library): states are
// placed on rows by their longest distance from StartAt, back edges (loops) curve round the
// right side, Catch transitions are dashed. Colours follow the execution, like the console's
// Graph view: green succeeded, red failed, blue in progress, orange caught, grey not reached.

type Def = { StartAt?: string; States?: Record<string, Record<string, unknown>> };
export type NodeStatus = "SUCCEEDED" | "FAILED" | "RUNNING" | "CAUGHT" | "ABORTED" | "NOT_STARTED";

type Edge = { from: string; to: string; kind: "next" | "choice" | "default" | "catch"; back?: boolean };

const W = 200, H = 44, GX = 36, GY = 70;

function edges(def: Def): Edge[] {
  const out: Edge[] = [];
  const states = def.States ?? {};
  out.push({ from: "__start", to: def.StartAt ?? "", kind: "next" });
  for (const [name, st] of Object.entries(states)) {
    if (typeof st.Next === "string") out.push({ from: name, to: st.Next, kind: "next" });
    if (typeof st.Default === "string") out.push({ from: name, to: st.Default, kind: "default" });
    for (const c of (st.Choices as { Next?: string }[]) ?? []) if (c.Next) out.push({ from: name, to: c.Next, kind: "choice" });
    for (const c of (st.Catch as { Next?: string }[]) ?? []) if (c.Next) out.push({ from: name, to: c.Next, kind: "catch" });
    if (st.End === true || st.Type === "Succeed" || st.Type === "Fail") out.push({ from: name, to: "__end", kind: "next" });
  }
  // dedupe choice edges to the same target
  const seen = new Set<string>();
  return out.filter((e) => {
    const k = `${e.from}>${e.to}>${e.kind === "catch" ? "c" : "n"}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
}

function layout(def: Def) {
  const es = edges(def);
  const adj = new Map<string, string[]>();
  for (const e of es) adj.set(e.from, [...(adj.get(e.from) ?? []), e.to]);
  // DFS from start: discovery order + back edges
  const order: string[] = [];
  const state = new Map<string, number>(); // 1 on stack, 2 done
  const back = new Set<string>();
  const dfs = (n: string) => {
    state.set(n, 1);
    order.push(n);
    for (const m of adj.get(n) ?? []) {
      if (state.get(m) === 1) back.add(`${n}>${m}`);
      else if (!state.has(m)) dfs(m);
    }
    state.set(n, 2);
  };
  dfs("__start");
  for (const n of Object.keys(def.States ?? {})) if (!state.has(n)) dfs(n); // unreachable states still drawn
  if (!state.has("__end")) order.push("__end");
  for (const e of es) e.back = back.has(`${e.from}>${e.to}`);
  // longest-path layering on the DAG
  const level = new Map<string, number>([["__start", 0]]);
  for (let pass = 0; pass < order.length; pass++) {
    let changed = false;
    for (const e of es) {
      if (e.back) continue;
      const l = (level.get(e.from) ?? 0) + 1;
      if (l > (level.get(e.to) ?? -1)) { level.set(e.to, l); changed = true; }
    }
    if (!changed) break;
  }
  const maxL = Math.max(...order.filter((n) => n !== "__end").map((n) => level.get(n) ?? 0));
  level.set("__end", maxL + 1);
  const rows = new Map<number, string[]>();
  for (const n of order) rows.set(level.get(n) ?? 0, [...(rows.get(level.get(n) ?? 0) ?? []), n]);
  const widest = Math.max(...[...rows.values()].map((r) => r.length));
  const width = Math.max(widest * (W + GX) - GX, W) + 80;
  const pos = new Map<string, { x: number; y: number }>();
  for (const [l, ns] of rows) {
    const rowW = ns.length * (W + GX) - GX;
    ns.forEach((n, i) => pos.set(n, { x: (width - rowW) / 2 + i * (W + GX), y: 20 + l * (H + GY) }));
  }
  return { es, pos, width, height: 20 + (maxL + 2) * (H + GY) - GY + 20 };
}

const FILL: Record<NodeStatus, string> = {
  SUCCEEDED: "fill-[#e9f6e6] stroke-success dark:fill-[#17321a]",
  FAILED: "fill-[#fde9e9] stroke-danger dark:fill-[#3a1a1c]",
  RUNNING: "fill-[#e7f2fc] stroke-info dark:fill-[#152a40]",
  CAUGHT: "fill-[#fff3e0] stroke-[#c45500] dark:fill-[#3a2a12]",
  ABORTED: "fill-[#f2f3f3] stroke-danger dark:fill-[#2a2f36]",
  NOT_STARTED: "fill-card stroke-border",
};

export function StateGraph({ definition, status, selected, onSelect, className }: {
  definition: string | Def; status?: Record<string, NodeStatus>; selected?: string; onSelect?: (name: string) => void; className?: string;
}) {
  const def: Def = React.useMemo(() => {
    if (typeof definition !== "string") return definition;
    try { return JSON.parse(definition); } catch { return {}; }
  }, [definition]);
  const { es, pos, width, height } = React.useMemo(() => layout(def), [def]);
  const [zoom, setZoom] = React.useState(1);
  if (!def.States) return <div className="text-sm text-muted-foreground p-4">Definition is not valid JSON.</div>;
  const anchor = (n: string, side: "top" | "bottom") => {
    const p = pos.get(n)!;
    const round = n.startsWith("__");
    const cx = p.x + W / 2;
    return { x: cx, y: side === "top" ? p.y + (round ? H / 2 - 14 : 0) : p.y + (round ? H / 2 + 14 : H) };
  };
  return (
    <div className={cn("relative rounded-xl border bg-muted overflow-auto", className)}
      style={{ backgroundImage: "radial-gradient(var(--border) 1px, transparent 1px)", backgroundSize: "18px 18px" }}>
      <div className="absolute right-2 top-2 z-10 flex gap-1">
        {[["−", 0.8], ["100%", 0], ["+", 1.25]].map(([l, f]) => (
          <button key={String(l)} onClick={() => setZoom((z) => (f === 0 ? 1 : Math.min(2, Math.max(0.4, z * (f as number)))))}
            className="rounded-md border bg-card px-2 text-xs font-bold cursor-pointer hover:bg-accent">{l}</button>
        ))}
      </div>
      <svg width={width * zoom} height={height * zoom} viewBox={`0 0 ${width} ${height}`} className="mx-auto block">
        <defs>
          <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
            <path d="M 0 0 L 10 5 L 0 10 z" className="fill-[#7d8998]" />
          </marker>
        </defs>
        {es.map((e, i) => {
          if (!pos.has(e.from) || !pos.has(e.to)) return null;
          const a = anchor(e.from, "bottom"), b = anchor(e.to, "top");
          let d: string;
          if (e.back) {
            const pa = pos.get(e.from)!, pb = pos.get(e.to)!;
            const x = Math.max(pa.x, pb.x) + W + 24;
            d = `M ${pa.x + W} ${pa.y + H / 2} C ${x} ${pa.y + H / 2}, ${x} ${pb.y + H / 2}, ${pb.x + W} ${pb.y + H / 2}`;
          } else {
            const my = (a.y + b.y) / 2;
            d = `M ${a.x} ${a.y} C ${a.x} ${my}, ${b.x} ${my}, ${b.x} ${b.y - 2}`;
          }
          return <path key={i} d={d} fill="none" strokeWidth={1.6} markerEnd="url(#arrow)"
            className={e.kind === "catch" ? "stroke-[#c45500]" : "stroke-[#7d8998]"} strokeDasharray={e.kind === "catch" ? "5 4" : e.kind === "default" ? "2 3" : undefined} />;
        })}
        {[...pos.entries()].map(([n, p]) => {
          if (n === "__start" || n === "__end") {
            const reached = (s?: NodeStatus) => !!s && s !== "NOT_STARTED" && s !== "RUNNING";
            const done = n === "__start"
              ? Object.keys(status ?? {}).length > 0
              : es.some((e) => e.to === "__end" && reached(status?.[e.from]));
            return (
              <g key={n}>
                <circle cx={p.x + W / 2} cy={p.y + H / 2} r={14} className={cn(done ? "fill-[#414d5c]" : "fill-card stroke-[#7d8998]")} strokeWidth={1.5} />
                <text x={p.x + W / 2 + 22} y={p.y + H / 2 + 4} className="fill-muted-foreground text-[11px] font-bold">{n === "__start" ? "Start" : "End"}</text>
              </g>
            );
          }
          const st = (def.States?.[n] ?? {}) as Record<string, unknown>;
          const s: NodeStatus = status?.[n] ?? "NOT_STARTED";
          const typ = String(st.Type ?? "");
          const res = typeof st.Resource === "string" ? String(st.Resource).replace("arn:aws:states:::", "") : "";
          return (
            <g key={n} onClick={() => onSelect?.(n)} className={onSelect ? "cursor-pointer" : ""}>
              <rect x={p.x} y={p.y} width={W} height={H} rx={typ === "Choice" ? 22 : 8} strokeWidth={selected === n ? 3 : 1.6}
                className={cn(FILL[s], selected === n && "stroke-link")} />
              {s === "RUNNING" && <rect x={p.x} y={p.y} width={W} height={H} rx={8} fill="none" strokeWidth={3} className="stroke-info animate-pulse" />}
              <text x={p.x + W / 2} y={p.y + 18} textAnchor="middle" className="fill-foreground text-[12.5px] font-bold">{n.length > 26 ? n.slice(0, 25) + "…" : n}</text>
              <text x={p.x + W / 2} y={p.y + 34} textAnchor="middle" className="fill-muted-foreground text-[10.5px]">
                {res ? (res.length > 32 ? res.slice(0, 31) + "…" : res) : typ}
                {typ === "Map" ? " · Map" : typ === "Parallel" ? ` · ${(st.Branches as unknown[])?.length ?? 0} branches` : ""}
              </text>
            </g>
          );
        })}
      </svg>
      <div className="flex flex-wrap gap-3 px-3 pb-2 text-[11px] text-muted-foreground">
        {(["SUCCEEDED", "FAILED", "RUNNING", "CAUGHT", "NOT_STARTED"] as NodeStatus[]).map((s) => (
          <span key={s} className="inline-flex items-center gap-1"><svg width="12" height="12"><rect x="1" y="1" width="10" height="10" rx="2" className={FILL[s]} strokeWidth="1.5" /></svg>{s.replace("_", " ").toLowerCase()}</span>
        ))}
        <span className="inline-flex items-center gap-1"><svg width="22" height="8"><path d="M1 4 H21" className="stroke-[#c45500]" strokeDasharray="4 3" strokeWidth="1.6" /></svg>catch</span>
      </div>
    </div>
  );
}
