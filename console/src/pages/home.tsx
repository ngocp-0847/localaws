import { useStore } from "@/store";
import { call, type J } from "@/lib/aws";
import { useData } from "@/lib/hooks";
import { Container, DataTable, Status } from "@/components/ui/data";
import { PageHeader } from "@/components/layout";
import { SERVICES } from "@/services";
import { href } from "@/router";
import { arnTail, fmtAgo, fmtDuration, toDate } from "@/lib/utils";

export default function Home() {
  const info = useStore((s) => s.info);
  const recent = useStore((s) => s.recent);
  const execs = useData(async () => {
    const sms: J[] = (await call("states", "ListStateMachines")).stateMachines ?? [];
    const lists = await Promise.all(sms.map((m) => call("states", "ListExecutions", { stateMachineArn: m.stateMachineArn, maxResults: 10 })));
    return lists.flatMap((l) => l.executions as J[]).sort((a, b) => b.startDate - a.startDate).slice(0, 10);
  }, [], { poll: 4000 });
  const counts = info?.counts ?? {};
  const tiles: [string, string, number | undefined][] = [
    ["Buckets", "/s3", counts.buckets], ["State machines", "/states", counts.stateMachines], ["Executions running", "/states", counts.executionsRunning],
    ["Tasks running", "/ecs", counts.tasksRunning], ["Rules", "/events", counts.rules], ["Parameters", "/ssm", counts.parameters],
  ];
  return (
    <div className="space-y-5">
      <PageHeader title="Console Home" description={info ? <>Account <b className="font-mono">{info.account}</b> · {info.region} · runner <b>{info.runner}</b> · data <span className="font-mono">{info.dataDir}</span></> : "Connecting to the emulator…"} />
      <div className="grid gap-4 grid-cols-2 md:grid-cols-3 xl:grid-cols-6">
        {tiles.map(([label, to, n]) => (
          <a key={label} href={href(to)} className="rounded-2xl bg-card shadow-[0_1px_3px_rgba(0,7,22,0.12)] px-5 py-4 no-underline text-foreground hover:ring-2 hover:ring-link/40">
            <div className="text-sm text-muted-foreground font-bold">{label}</div>
            <div className="text-3xl font-light text-link mt-1">{n ?? "–"}</div>
          </a>
        ))}
      </div>
      <div className="grid gap-5 lg:grid-cols-3">
        <Container title="Services" className="lg:col-span-1">
          <ul className="space-y-2.5">
            {[...SERVICES].sort((a, b) => (recent.indexOf(a.id) + 1 || 99) - (recent.indexOf(b.id) + 1 || 99)).map((s) => (
              <li key={s.id}>
                <a href={href(s.path)} className="flex items-center gap-3 no-underline group">
                  <span className="grid place-items-center size-8 rounded-lg bg-nav"><s.icon className="size-4 text-primary" /></span>
                  <span>
                    <span className="font-bold text-link group-hover:underline">{s.name}</span>
                    <span className="block text-xs text-muted-foreground">{s.blurb}</span>
                  </span>
                </a>
              </li>
            ))}
          </ul>
        </Container>
        <Container title="Recent executions" className="lg:col-span-2" flush>
          <DataTable rows={execs.data ?? []} rowKey={(e) => e.executionArn} empty="No executions yet — upload an object to a bucket with EventBridge notifications, or start one."
            columns={[
              { key: "name", header: "Name", cell: (e) => <a href={href("/states/execution", { arn: e.executionArn })} className="break-all">{e.name.length > 48 ? e.name.slice(0, 48) + "…" : e.name}</a> },
              { key: "sm", header: "State machine", cell: (e) => arnTail(e.stateMachineArn) },
              { key: "status", header: "Status", cell: (e) => <Status value={e.status} /> },
              { key: "start", header: "Started", cell: (e) => fmtAgo(e.startDate) },
              { key: "dur", header: "Duration", cell: (e) => fmtDuration(e.stopDate ? (toDate(e.stopDate)!.getTime() - toDate(e.startDate)!.getTime()) : null) },
            ]} />
        </Container>
      </div>
    </div>
  );
}
