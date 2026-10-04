import * as React from "react";
import { Bell, ChevronRight, CircleCheck, CircleX, Info, Menu, Moon, RefreshCw, RotateCcw, Search, Sun, User, X, PanelLeftClose } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/form";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/overlay";
import { act, useStore } from "@/store";
import { href, navigate } from "@/router";
import { reset } from "@/lib/aws";
import { cn, fmtAgo } from "@/lib/utils";
import { SERVICES, type ServiceDef } from "@/services";

export function TopBar() {
  const info = useStore((s) => s.info);
  const dark = useStore((s) => s.dark);
  const toggleDark = useStore((s) => s.toggleDark);
  const auto = useStore((s) => s.autoRefresh);
  const setAuto = useStore((s) => s.setAutoRefresh);
  const visit = useStore((s) => s.visit);
  const [q, setQ] = React.useState("");
  const [open, setOpen] = React.useState(false);
  const hits = SERVICES.filter((s) => (s.name + s.full + s.keywords).toLowerCase().includes(q.toLowerCase()));
  const go = (s: ServiceDef) => {
    visit(s.id);
    navigate(s.path);
    setQ("");
    setOpen(false);
  };
  return (
    <header className="h-11 bg-nav text-white flex items-center gap-3 px-3 sticky top-0 z-40 shadow">
      <a href={href("/")} className="flex items-center gap-2 font-bold text-[15px] text-white no-underline shrink-0">
        <span className="grid place-items-center size-7 rounded-md bg-primary text-nav text-xs font-black">la</span>
        <span className="hidden sm:inline">localaws</span>
      </a>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button className="text-sm font-bold px-2 py-1 rounded hover:bg-white/10 cursor-pointer">Services</button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="w-72">
          {SERVICES.map((s) => (
            <DropdownMenuItem key={s.id} onSelect={() => go(s)}>
              <s.icon className="size-4 text-primary" />
              <div>
                <div className="font-bold">{s.name}</div>
                <div className="text-xs text-muted-foreground">{s.blurb}</div>
              </div>
            </DropdownMenuItem>
          ))}
        </DropdownMenuContent>
      </DropdownMenu>
      <div className="relative flex-1 max-w-xl">
        <Search className="size-4 absolute left-2.5 top-2 text-[#8d99a8]" />
        <input value={q} onChange={(e) => { setQ(e.target.value); setOpen(true); }} onFocus={() => setOpen(true)} onBlur={() => setTimeout(() => setOpen(false), 150)}
          onKeyDown={(e) => { if (e.key === "Enter" && hits[0]) go(hits[0]); if (e.key === "Escape") setOpen(false); }}
          placeholder="Search services  [Alt+S]" id="service-search"
          className="w-full h-7 rounded-md bg-nav-2 border border-[#414d5c] pl-8 pr-2 text-sm text-white placeholder:text-[#8d99a8] focus:outline-none focus:border-[#539fe5]" />
        {open && q && (
          <div className="absolute top-8 left-0 right-0 rounded-lg bg-card text-foreground shadow-xl border p-1 z-50">
            {hits.length === 0 && <div className="px-3 py-2 text-sm text-muted-foreground">No services match “{q}”</div>}
            {hits.map((s) => (
              <button key={s.id} onMouseDown={() => go(s)} className="w-full text-left flex items-center gap-2 px-3 py-1.5 rounded-md hover:bg-accent cursor-pointer">
                <s.icon className="size-4 text-primary" /> <span className="font-bold text-sm">{s.name}</span>
                <span className="text-xs text-muted-foreground truncate">{s.blurb}</span>
              </button>
            ))}
          </div>
        )}
      </div>
      <div className="ml-auto flex items-center gap-1 text-sm">
        <span className="hidden md:inline-flex items-center gap-1 px-2 py-1 rounded hover:bg-white/10" title="auto-refresh lists and running executions">
          <RefreshCw className={cn("size-3.5", auto && "text-primary")} />
          <Switch checked={auto} onCheckedChange={setAuto} className="scale-75" />
        </span>
        <button onClick={toggleDark} className="p-1.5 rounded hover:bg-white/10 cursor-pointer" title="Light / dark">
          {dark ? <Sun className="size-4" /> : <Moon className="size-4" />}
        </button>
        <span className="hidden sm:inline px-2 py-1 font-bold">{info?.region ?? "…"}</span>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button className="flex items-center gap-1 px-2 py-1 rounded hover:bg-white/10 font-bold cursor-pointer max-w-56">
              <User className="size-4 shrink-0" /> <span className="truncate">{info ? info.identity.split("/").pop() : "not connected"}</span>
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent className="w-80">
            <div className="px-3 py-2 text-xs space-y-1">
              <div><span className="text-muted-foreground">Account</span> <span className="font-mono font-bold">{info?.account}</span></div>
              <div className="break-all"><span className="text-muted-foreground">Identity</span> <span className="font-mono">{info?.identity}</span></div>
              <div><span className="text-muted-foreground">Endpoint</span> <span className="font-mono">{info?.endpoint}</span></div>
              <div><span className="text-muted-foreground">Runner</span> <b>{info?.runner}</b> · v{info?.version} · up {info ? fmtAgo(info.started).replace(" ago", "") : ""}</div>
            </div>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={() => navigate("/activity")}><Bell className="size-4" /> API activity</DropdownMenuItem>
            <DropdownMenuItem danger onSelect={async () => {
              if (!window.confirm("Reset the emulator? Every bucket, object, execution, task, log and parameter is deleted, then the config file is applied again.")) return;
              await act("Reset", reset, "Emulator reset — the config was applied again");
              window.location.reload();
            }}><RotateCcw className="size-4" /> Reset emulator…</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  );
}

export function SideNav({ service, active }: { service: ServiceDef | undefined; active: string }) {
  const open = useStore((s) => s.navOpen);
  const setOpen = useStore((s) => s.setNavOpen);
  if (!service) return null;
  if (!open)
    return (
      <button onClick={() => setOpen(true)} className="fixed left-0 top-14 z-30 rounded-r-lg bg-card shadow px-1.5 py-2 cursor-pointer" title="Open navigation">
        <Menu className="size-4" />
      </button>
    );
  return (
    <nav className="w-60 shrink-0 bg-card border-r min-h-[calc(100vh-2.75rem)] hidden md:block">
      <div className="flex items-center justify-between px-5 pt-4 pb-3 border-b">
        <a href={href(service.path)} className="text-lg font-bold text-foreground no-underline">{service.full}</a>
        <button onClick={() => setOpen(false)} className="text-muted-foreground hover:text-foreground cursor-pointer" title="Close navigation">
          <PanelLeftClose className="size-4" />
        </button>
      </div>
      <ul className="py-3">
        {service.nav.map((n) => (
          <li key={n.path}>
            <a href={href(n.path)} className={cn("block px-5 py-1 text-sm no-underline", active.startsWith(n.path) ? "font-bold text-link" : "text-foreground hover:text-link")}>
              {n.label}
            </a>
          </li>
        ))}
      </ul>
      <div className="border-t mx-5 pt-3 text-xs text-muted-foreground">
        All services
        <ul className="mt-2 space-y-1">
          {SERVICES.filter((s) => s.id !== service.id).map((s) => (
            <li key={s.id}><a href={href(s.path)} className="text-foreground hover:text-link no-underline">{s.name}</a></li>
          ))}
        </ul>
      </div>
    </nav>
  );
}

export function Breadcrumbs({ items }: { items: [string, string?][] }) {
  return (
    <nav className="flex flex-wrap items-center gap-1 text-sm mb-3">
      {items.map(([label, to], i) => (
        <React.Fragment key={i}>
          {i > 0 && <ChevronRight className="size-3.5 text-muted-foreground" />}
          {to && i < items.length - 1 ? <a href={href(to)} className="hover:underline">{label}</a> : <span className="text-muted-foreground break-all">{label}</span>}
        </React.Fragment>
      ))}
    </nav>
  );
}

export function PageHeader({ title, description, actions, info }: { title: React.ReactNode; description?: React.ReactNode; actions?: React.ReactNode; info?: React.ReactNode }) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-3 mb-4">
      <div className="min-w-0">
        <h1 className="text-[26px] font-bold leading-8 break-all">
          {title} {info && <span className="text-sm font-normal text-link align-middle ml-1">{info}</span>}
        </h1>
        {description && <p className="text-sm text-muted-foreground mt-1 max-w-3xl">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
    </div>
  );
}

/** the console's flashbar: stacked banners above the content */
export function Flashbar() {
  const toasts = useStore((s) => s.toasts);
  const dismiss = useStore((s) => s.dismiss);
  if (!toasts.length) return null;
  const shown = toasts.slice(-3);
  return (
    <div className="space-y-2 mb-4">
      {toasts.length > shown.length && <div className="text-xs text-muted-foreground">+{toasts.length - shown.length} earlier notification(s)</div>}
      {shown.map((t) => {
        const c = { success: "bg-success", error: "bg-danger", info: "bg-link" }[t.kind];
        const Icon = { success: CircleCheck, error: CircleX, info: Info }[t.kind];
        return (
          <div key={t.id} className={cn("flex items-start gap-3 rounded-xl px-4 py-3 text-white shadow", c)}>
            <Icon className="size-5 shrink-0 mt-px" />
            <div className="flex-1 text-sm">
              <div className="font-bold">{t.title}</div>
              {t.detail && <div className="mt-0.5 break-all opacity-95">{t.detail}</div>}
            </div>
            <button onClick={() => dismiss(t.id)} className="opacity-80 hover:opacity-100 cursor-pointer"><X className="size-4" /></button>
          </div>
        );
      })}
    </div>
  );
}

export function RefreshButton({ onClick }: { onClick: () => void }) {
  const [spin, setSpin] = React.useState(false);
  return (
    <Button variant="normal" size="icon" title="Refresh" onClick={async () => { setSpin(true); await onClick(); setTimeout(() => setSpin(false), 400); }}>
      <RefreshCw className={cn(spin && "animate-spin")} />
    </Button>
  );
}
