import { useEffect, useState } from "react";

// A 40-line hash router: the console is served under /_localaws/ by the emulator, so hash
// routes need no server rewrites. Routes: "#/s3/my-bucket?prefix=a/".

function parse() {
  const h = window.location.hash.replace(/^#/, "") || "/";
  const [path, qs = ""] = h.split("?");
  return { path, query: new URLSearchParams(qs) };
}

export function useRoute() {
  const [r, setR] = useState(parse);
  useEffect(() => {
    const on = () => setR(parse());
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  return r;
}

export function href(path: string, query?: Record<string, string | undefined>) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(query ?? {})) if (v !== undefined && v !== "") q.set(k, v);
  const s = q.toString();
  return `#${path}${s ? `?${s}` : ""}`;
}

export function navigate(path: string, query?: Record<string, string | undefined>) {
  window.location.hash = href(path, query).slice(1);
}

/** match "/s3/:bucket" against a path → params, or null */
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split("/").filter(Boolean);
  const s = path.split("/").filter(Boolean);
  if (p.length !== s.length) return null;
  const out: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    if (p[i].startsWith(":")) out[p[i].slice(1)] = decodeURIComponent(s[i]);
    else if (p[i] !== s[i]) return null;
  }
  return out;
}
