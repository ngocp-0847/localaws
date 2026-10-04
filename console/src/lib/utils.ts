import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/** epoch seconds (JSON protocol) or ISO string → Date */
export function toDate(v: unknown): Date | null {
  if (v == null || v === "") return null;
  if (typeof v === "number") return new Date(v > 1e11 ? v : v * 1000);
  const d = new Date(String(v));
  return isNaN(d.getTime()) ? null : d;
}

export function fmtTime(v: unknown, withMs = false): string {
  const d = toDate(v);
  if (!d) return "–";
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  const s = `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  return withMs ? `${s}.${p(d.getMilliseconds(), 3)}` : s;
}

export function fmtAgo(v: unknown): string {
  const d = toDate(v);
  if (!d) return "–";
  const s = Math.round((Date.now() - d.getTime()) / 1000);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export function fmtDuration(ms: number | null | undefined): string {
  if (ms == null || ms < 0) return "–";
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)} s`;
  const m = Math.floor(s / 60);
  return `${m}m ${Math.round(s % 60)}s`;
}

export function fmtBytes(n: number | null | undefined): string {
  if (n == null) return "–";
  if (n < 1024) return `${n} B`;
  const u = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${u[i]}`;
}

export function arnTail(arn: string): string {
  const i = Math.max(arn.lastIndexOf(":"), arn.lastIndexOf("/"));
  return i >= 0 ? arn.slice(i + 1) : arn;
}

/** task definition ARN → "family:revision" (arnTail would stop at the revision) */
export function tdName(arn: string): string {
  const i = arn.indexOf("task-definition/");
  return i >= 0 ? arn.slice(i + "task-definition/".length) : arn;
}

export function pretty(v: unknown): string {
  if (typeof v === "string") {
    try {
      return JSON.stringify(JSON.parse(v), null, 2);
    } catch {
      return v;
    }
  }
  return JSON.stringify(v, null, 2);
}

export function tryJSON(s: string): { ok: true; value: unknown } | { ok: false; error: string } {
  try {
    return { ok: true, value: JSON.parse(s) };
  } catch (e) {
    return { ok: false, error: (e as Error).message };
  }
}
