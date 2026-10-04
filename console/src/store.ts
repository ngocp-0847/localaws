import { create } from "zustand";
import { persist } from "zustand/middleware";
import { info as fetchInfo, type Info } from "@/lib/aws";

export type Toast = { id: number; kind: "success" | "error" | "info"; title: string; detail?: string };

type State = {
  info: Info | null;
  loadInfo: () => Promise<void>;
  // preferences (persisted)
  dark: boolean;
  toggleDark: () => void;
  autoRefresh: boolean;
  setAutoRefresh: (v: boolean) => void;
  navOpen: boolean;
  setNavOpen: (v: boolean) => void;
  recent: string[];
  visit: (service: string) => void;
  // flash messages (the console's green / red banners)
  toasts: Toast[];
  notify: (kind: Toast["kind"], title: string, detail?: string) => void;
  dismiss: (id: number) => void;
};

let seq = 0;

export const useStore = create<State>()(
  persist(
    (set, get) => ({
      info: null,
      loadInfo: async () => {
        try {
          set({ info: await fetchInfo() });
        } catch {
          /* emulator not reachable — the top bar says so */
        }
      },
      dark: false,
      toggleDark: () => set({ dark: !get().dark }),
      autoRefresh: true,
      setAutoRefresh: (v) => set({ autoRefresh: v }),
      navOpen: true,
      setNavOpen: (v) => set({ navOpen: v }),
      recent: [],
      visit: (s) => set({ recent: [s, ...get().recent.filter((x) => x !== s)].slice(0, 6) }),
      toasts: [],
      notify: (kind, title, detail) => {
        const id = ++seq;
        set({ toasts: [...get().toasts, { id, kind, title, detail }] });
        setTimeout(() => get().dismiss(id), kind === "error" ? 10000 : 3500);
      },
      dismiss: (id) => set({ toasts: get().toasts.filter((t) => t.id !== id) }),
    }),
    {
      name: "localaws-console",
      partialize: (s) => ({ dark: s.dark, autoRefresh: s.autoRefresh, navOpen: s.navOpen, recent: s.recent }),
    },
  ),
);

/** run an action, report success / the AWS error as a flash banner. Resolves to the action's
 * value (or `true` when it has none) on success, `undefined` on failure — so `if (await act(…))`
 * always means "it worked". */
export async function act<T>(title: string, fn: () => Promise<T>, ok?: string): Promise<T | undefined> {
  const { notify } = useStore.getState();
  try {
    const r = await fn();
    if (ok !== "") notify("success", ok ?? title);
    return r ?? (true as unknown as T);
  } catch (e) {
    const err = e as { code?: string; message?: string };
    notify("error", `${title} failed`, [err.code, err.message].filter(Boolean).join(": "));
    return undefined;
  }
}
