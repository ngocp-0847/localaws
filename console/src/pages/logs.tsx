import * as React from "react";
import { Pause, Play, Search, WrapText } from "lucide-react";
import { all, call, type J } from "@/lib/aws";
import { useData } from "@/lib/hooks";
import { act, useStore } from "@/store";
import { href, navigate } from "@/router";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { Dialog, DialogContent } from "@/components/ui/overlay";
import { Alert, Container, Copyable, DataTable, KeyValue, Spinner } from "@/components/ui/data";
import { Breadcrumbs, PageHeader, RefreshButton } from "@/components/layout";
import { ConfirmDelete, TextFilter, useFilter } from "@/components/common";
import { cn, fmtAgo, fmtTime } from "@/lib/utils";

type Line = { timestamp: number; message: string; logStreamName?: string };

const LEVEL = /"level"\s*:\s*"(ERROR|FATAL|WARN|WARNING|INFO|DEBUG|TRACE)"|\b(ERROR|FATAL|WARN|INFO|DEBUG)\b/;

function levelOf(m: string) {
  const x = LEVEL.exec(m);
  return (x?.[1] ?? x?.[2] ?? "").replace("WARNING", "WARN");
}

function LogLine({ l, wrap, showStream }: { l: Line; wrap: boolean; showStream?: boolean }) {
  const [open, setOpen] = React.useState(false);
  const lvl = levelOf(l.message);
  let parsed: J = null;
  if (l.message.startsWith("{")) try { parsed = JSON.parse(l.message); } catch { /* text line */ }
  const msg = parsed ? `${parsed.msg ?? parsed.message ?? ""}` : l.message;
  return (
    <div className={cn("border-b border-border/60 px-3 py-0.5 font-mono text-[12px] leading-5 hover:bg-accent/70 cursor-pointer",
      lvl === "ERROR" || lvl === "FATAL" ? "bg-danger/10" : lvl === "WARN" ? "bg-warning/10" : "")} onClick={() => setOpen(!open)}>
      <div className={cn("flex gap-3", !wrap && "whitespace-nowrap")}>
        <span className="text-muted-foreground shrink-0">{fmtTime(l.timestamp, true).slice(11)}</span>
        {showStream && <span className="text-link shrink-0 max-w-48 truncate">{l.logStreamName}</span>}
        {lvl && <span className={cn("shrink-0 w-11 font-bold", lvl === "ERROR" || lvl === "FATAL" ? "text-danger" : lvl === "WARN" ? "text-warning" : "text-muted-foreground")}>{lvl}</span>}
        {parsed?.code && <span className="shrink-0 font-bold text-danger">{parsed.code}</span>}
        <span className={cn(wrap ? "break-all" : "truncate")}>{msg || l.message}</span>
      </div>
      {open && <pre className="mt-1 mb-1 whitespace-pre-wrap break-all rounded bg-muted p-2 cursor-text" onClick={(e) => e.stopPropagation()}>{parsed ? JSON.stringify(parsed, null, 2) : l.message}</pre>}
    </div>
  );
}

/** a stream's events, followed while live (forward-token polling, like `aws logs tail -f`) */
export function LogViewer({ group, stream, live, height = "h-[60vh]" }: { group: string; stream: string; live?: boolean; height?: string }) {
  const [lines, setLines] = React.useState<Line[]>([]);
  const [err, setErr] = React.useState<string | null>(null);
  const [follow, setFollow] = React.useState(true);
  const [wrap, setWrap] = React.useState(true);
  const [q, setQ] = React.useState("");
  const [lvl, setLvl] = React.useState("");
  const token = React.useRef<string | undefined>(undefined);
  const box = React.useRef<HTMLDivElement>(null);
  const auto = useStore((s) => s.autoRefresh);
  const pull = React.useCallback(async () => {
    try {
      for (let i = 0; i < 20; i++) {
        const r = await call("logs", "GetLogEvents", { logGroupName: group, logStreamName: stream, startFromHead: true, ...(token.current ? { nextToken: token.current } : {}) });
        const ev: Line[] = r.events ?? [];
        if (ev.length) setLines((l) => [...l, ...ev]);
        const same = r.nextForwardToken === token.current;
        token.current = r.nextForwardToken;
        if (same || !ev.length) break;
      }
      setErr(null);
    } catch (e) {
      setErr((e as Error).message);
    }
  }, [group, stream]);
  React.useEffect(() => { setLines([]); token.current = undefined; pull(); }, [pull]);
  React.useEffect(() => {
    if (!live || !auto) return;
    const t = setInterval(pull, 1500);
    return () => clearInterval(t);
  }, [live, auto, pull]);
  React.useEffect(() => { if (follow && box.current) box.current.scrollTop = box.current.scrollHeight; }, [lines, follow]);
  const shown = lines.filter((l) => (!q || l.message.toLowerCase().includes(q.toLowerCase())) && (!lvl || levelOf(l.message) === lvl));
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative flex-1 min-w-48 max-w-md">
          <Search className="size-4 absolute left-2.5 top-2 text-muted-foreground" />
          <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Filter events" className="pl-8" />
        </div>
        {["", "ERROR", "WARN", "INFO", "DEBUG"].map((x) => (
          <Button key={x} size="sm" variant={lvl === x ? "primary" : "normal"} onClick={() => setLvl(x)}>{x || "All"}</Button>
        ))}
        <Button size="sm" variant="icon" title="Wrap lines" onClick={() => setWrap(!wrap)}><WrapText className={wrap ? "text-link" : ""} /></Button>
        {live && <Button size="sm" onClick={() => setFollow(!follow)}>{follow ? <><Pause /> Following</> : <><Play /> Follow</>}</Button>}
        <span className="text-xs text-muted-foreground ml-auto">{shown.length}/{lines.length} events{live && auto ? " · live" : ""}</span>
      </div>
      {err && <Alert>{err}</Alert>}
      <div ref={box} className={cn("rounded-xl border bg-card overflow-auto", height)} onScroll={(e) => {
        const el = e.currentTarget;
        setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 30);
      }}>
        {shown.map((l, i) => <LogLine key={i} l={l} wrap={wrap} />)}
        {!lines.length && <div className="p-6 text-center text-sm text-muted-foreground">{live ? "Waiting for events…" : "No events"}</div>}
      </div>
    </div>
  );
}

export function LogGroups() {
  const { data, error, refresh } = useData(() => all("logs", "DescribeLogGroups", {}, "logGroups"), [], { poll: 10000 });
  const f = useFilter(data, (g: J) => g.logGroupName);
  const [create, setCreate] = React.useState(false);
  const [name, setName] = React.useState("");
  const [sel, setSel] = React.useState<string[]>([]);
  const [del, setDel] = React.useState(false);
  return (
    <>
      <Breadcrumbs items={[["CloudWatch", "/logs"], ["Log groups"]]} />
      <PageHeader title="Log groups" description="ECS tasks with the awslogs driver write one stream per container: <prefix>/<container>/<task id>." />
      {error && <Alert>{error}</Alert>}
      <Container title="Log groups" counter={data?.length} flush actions={<>
        <RefreshButton onClick={refresh} />
        <Button disabled={sel.length !== 1} onClick={() => setDel(true)}>Delete</Button>
        <Button variant="primary" onClick={() => setCreate(true)}>Create log group</Button>
      </>}>
        <div className="px-5"><TextFilter value={f.q} onChange={f.setQ} placeholder="Filter log groups" count={f.rows.length} /></div>
        <DataTable rows={f.rows} rowKey={(g: J) => g.logGroupName} selected={sel} onSelect={setSel} columns={[
          { key: "n", header: "Log group", cell: (g: J) => <a href={href("/logs/group", { name: g.logGroupName })} onClick={(e) => e.stopPropagation()} className="font-bold">{g.logGroupName}</a> },
          { key: "r", header: "Retention", cell: (g: J) => (g.retentionInDays ? `${g.retentionInDays} days` : "Never expire") },
          { key: "c", header: "Creation time", cell: (g: J) => fmtTime(g.creationTime) },
        ]} />
      </Container>
      <Dialog open={create} onOpenChange={setCreate}>
        <DialogContent title="Create log group" footer={<><Button variant="link" onClick={() => setCreate(false)}>Cancel</Button>
          <Button variant="primary" disabled={!name} onClick={async () => { await act("Create log group", () => call("logs", "CreateLogGroup", { logGroupName: name }), `${name} created`); setCreate(false); refresh(); }}>Create</Button></>}>
          <Field label="Log group name"><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="/ecs/my-service" autoFocus /></Field>
        </DialogContent>
      </Dialog>
      <ConfirmDelete open={del} onOpenChange={setDel} what="log group" name={sel[0] ?? ""}
        onConfirm={async () => { await act("Delete log group", () => call("logs", "DeleteLogGroup", { logGroupName: sel[0] }), `${sel[0]} deleted`); setSel([]); refresh(); }} />
    </>
  );
}

export function LogGroupPage({ name }: { name: string }) {
  const streams = useData(() => all("logs", "DescribeLogStreams", { logGroupName: name, orderBy: "LastEventTime", descending: true }, "logStreams"), [name], { poll: 5000 });
  const f = useFilter(streams.data, (s: J) => s.logStreamName);
  const [pattern, setPattern] = React.useState("");
  const [hits, setHits] = React.useState<Line[] | null>(null);
  return (
    <>
      <Breadcrumbs items={[["CloudWatch", "/logs"], ["Log groups", "/logs"], [name]]} />
      <PageHeader title={name} />
      <div className="space-y-5">
        <Container title="Search log group" description={<>FilterLogEvents across every stream: terms, <span className="font-mono">"exact phrase"</span>, <span className="font-mono">?any ?of</span>, <span className="font-mono">{"{ $.level = \"ERROR\" }"}</span></>}>
          <form className="flex gap-2" onSubmit={async (e) => {
            e.preventDefault();
            const r = await act("Search", () => call("logs", "FilterLogEvents", { logGroupName: name, filterPattern: pattern, limit: 500 }), "");
            setHits(r?.events ?? []);
          }}>
            <Input value={pattern} onChange={(e) => setPattern(e.target.value)} placeholder='e.g. ERROR  or  { $.code = "MB_*" }' className="font-mono" />
            <Button variant="primary" type="submit"><Search /> Search</Button>
          </form>
          {hits && (
            <div className="mt-3 rounded-xl border max-h-[50vh] overflow-auto">
              {hits.map((l, i) => <div key={i}><LogLine l={l} wrap showStream /></div>)}
              {!hits.length && <div className="p-4 text-sm text-muted-foreground">No matching events</div>}
            </div>
          )}
        </Container>
        <Container title="Log streams" counter={streams.data?.length} flush actions={<RefreshButton onClick={streams.refresh} />}>
          <div className="px-5"><TextFilter value={f.q} onChange={f.setQ} placeholder="Filter log streams (task id, container…)" count={f.rows.length} /></div>
          {!streams.data ? <Spinner /> : (
            <DataTable rows={f.rows} rowKey={(s: J) => s.logStreamName} onRowClick={(s: J) => navigate("/logs/stream", { group: name, stream: s.logStreamName })} columns={[
              { key: "n", header: "Log stream", cell: (s: J) => <a href={href("/logs/stream", { group: name, stream: s.logStreamName })} className="font-mono text-xs">{s.logStreamName}</a> },
              { key: "l", header: "Last event time", cell: (s: J) => (s.lastEventTimestamp ? <>{fmtTime(s.lastEventTimestamp)} <span className="text-muted-foreground">({fmtAgo(s.lastEventTimestamp)})</span></> : "–") },
              { key: "c", header: "Created", cell: (s: J) => fmtTime(s.creationTime) },
            ]} />
          )}
        </Container>
      </div>
    </>
  );
}

export function LogStreamPage({ group, stream }: { group: string; stream: string }) {
  const meta = useData(async () => ((await call("logs", "DescribeLogStreams", { logGroupName: group, logStreamNamePrefix: stream })).logStreams ?? []).find((s: J) => s.logStreamName === stream), [group, stream]);
  const taskId = stream.split("/").pop() ?? "";
  return (
    <>
      <Breadcrumbs items={[["CloudWatch", "/logs"], ["Log groups", "/logs"], [group, `/logs/group?name=${encodeURIComponent(group)}`], [stream]]} />
      <PageHeader title={<span className="font-mono text-xl">{stream}</span>} />
      <div className="space-y-5">
        <Container>
          <KeyValue cols={4} items={[["Log group", <a href={href("/logs/group", { name: group })}>{group}</a>], ["Stream", <Copyable value={stream} />],
            ["Last event", meta.data?.lastEventTimestamp ? fmtTime(meta.data.lastEventTimestamp) : "–"],
            ["Task", /^[0-9a-f]{32}$/.test(taskId) ? <a href={href("/ecs/task", { arn: `task/${taskId}` })} onClick={(e) => { e.preventDefault(); findTask(taskId); }}>{taskId}</a> : "–"]]} />
        </Container>
        <Container title="Log events"><LogViewer group={group} stream={stream} live /></Container>
      </div>
    </>
  );
}

async function findTask(id: string) {
  const clusters: string[] = (await call("ecs", "ListClusters")).clusterArns ?? [];
  for (const c of clusters) {
    const r = await call("ecs", "DescribeTasks", { cluster: c, tasks: [id] });
    if (r.tasks?.length) return navigate("/ecs/task", { arn: r.tasks[0].taskArn });
  }
}

