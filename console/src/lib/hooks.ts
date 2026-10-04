import { useCallback, useEffect, useRef, useState } from "react";
import { useStore } from "@/store";

/** load data, optionally re-poll while `live` (honours the global auto-refresh switch) */
export function useData<T>(load: () => Promise<T>, deps: unknown[], opts: { poll?: number; live?: (d: T) => boolean } = {}) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const auto = useStore((s) => s.autoRefresh);
  const loadRef = useRef(load);
  loadRef.current = load;

  const refresh = useCallback(async () => {
    try {
      const d = await loadRef.current();
      setData(d);
      setError(null);
    } catch (e) {
      const err = e as { code?: string; message?: string };
      setError([err.code, err.message].filter(Boolean).join(": ") || String(e));
    } finally {
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  useEffect(() => {
    setLoading(true);
    refresh();
  }, [refresh]);

  useEffect(() => {
    if (!opts.poll || !auto) return;
    if (data !== undefined && opts.live && !opts.live(data)) return;
    const t = setInterval(refresh, opts.poll);
    return () => clearInterval(t);
  }, [refresh, opts.poll, auto, data, opts.live]);

  return { data, error, loading, refresh, setData };
}
