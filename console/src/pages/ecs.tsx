import * as React from "react";
import { Play, Square, Plus } from "lucide-react";
import { all, call, query, text, type J } from "@/lib/aws";
import { useData } from "@/lib/hooks";
import { act, useStore } from "@/store";
import { href, navigate } from "@/router";
import { Button } from "@/components/ui/button";
import { Field, Input, Select, Textarea } from "@/components/ui/form";
import { Dialog, DialogContent, Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/overlay";
import { Alert, Badge, Code, Container, Copyable, DataTable, KeyValue, Spinner, Status } from "@/components/ui/data";
import { Breadcrumbs, PageHeader, RefreshButton } from "@/components/layout";
import { ConfirmDelete, JsonEditor, TextFilter, useFilter } from "@/components/common";
import { LogViewer } from "@/pages/logs";
import { arnTail, fmtAgo, fmtDuration, fmtTime, pretty, tdName, toDate, tryJSON } from "@/lib/utils";

async function describeTasks(cluster: string, arns: string[]): Promise<J[]> {
  const out: J[] = [];
  for (let i = 0; i < arns.length; i += 100) out.push(...((await call("ecs", "DescribeTasks", { cluster, tasks: arns.slice(i, i + 100) })).tasks ?? []));
  return out;
}

export function Clusters() {
  const { data, error, refresh } = useData(async () => {
    const arns: string[] = (await call("ecs", "ListClusters")).clusterArns ?? [];
    return arns.length ? (await call("ecs", "DescribeClusters", { clusters: arns })).clusters : [];
  }, [], { poll: 5000 });
  const [create, setCreate] = React.useState(false);
  const [name, setName] = React.useState("");
  return (
    <>
      <Breadcrumbs items={[["Amazon ECS", "/ecs"], ["Clusters"]]} />
      <PageHeader title="Clusters" description="Tasks run as real containers (runner docker), host processes (exec) or no-ops, depending on how localaws was started." />
      {error && <Alert title="Could not list clusters">{error}</Alert>}
      <Container title="Clusters" counter={data?.length} flush actions={<><RefreshButton onClick={refresh} /><Button variant="primary" onClick={() => setCreate(true)}>Create cluster</Button></>}>
        <DataTable rows={data ?? []} rowKey={(c: J) => c.clusterArn} onRowClick={(c: J) => navigate(`/ecs/cluster/${c.clusterName}`)}
          columns={[
            { key: "n", header: "Cluster", cell: (c: J) => <a href={href(`/ecs/cluster/${c.clusterName}`)} className="font-bold">{c.clusterName}</a> },
            { key: "s", header: "Services", cell: (c: J) => c.activeServicesCount },
            { key: "r", header: "Running tasks", cell: (c: J) => (c.runningTasksCount ? <Status value="RUNNING" label={String(c.runningTasksCount)} /> : "0") },
            { key: "p", header: "Pending tasks", cell: (c: J) => c.pendingTasksCount },
            { key: "cp", header: "Capacity providers", cell: (c: J) => (c.capacityProviders ?? []).join(", ") },
          ]} />
      </Container>
      <Dialog open={create} onOpenChange={setCreate}>
        <DialogContent title="Create cluster" footer={<><Button variant="link" onClick={() => setCreate(false)}>Cancel</Button>
          <Button variant="primary" disabled={!name} onClick={async () => { await act("Create cluster", () => call("ecs", "CreateCluster", { clusterName: name }), `Cluster ${name} created`); setCreate(false); refresh(); }}>Create</Button></>}>
          <Field label="Cluster name"><Input value={name} onChange={(e) => setName(e.target.value)} autoFocus /></Field>
        </DialogContent>
      </Dialog>
    </>
  );
}

export function ClusterPage({ cluster }: { cluster: string }) {
  const [desired, setDesired] = React.useState("RUNNING");
  const tasks = useData(async () => {
    const arns: string[] = await all("ecs", "ListTasks", { cluster, desiredStatus: desired }, "taskArns");
    return describeTasks(cluster, arns.slice(0, 300));
  }, [cluster, desired], { poll: 3000 });
  const svcs = useData(async () => {
    const arns: string[] = (await call("ecs", "ListServices", { cluster })).serviceArns ?? [];
    return arns.length ? (await call("ecs", "DescribeServices", { cluster, services: arns })).services : [];
  }, [cluster], { poll: 5000 });
  const f = useFilter(tasks.data, (t: J) => `${t.taskArn} ${t.group} ${t.startedBy} ${t.taskDefinitionArn}`);
  const [sel, setSel] = React.useState<string[]>([]);
  const [run, setRun] = React.useState(false);
  const [scale, setScale] = React.useState<J | null>(null);
  return (
    <>
      <Breadcrumbs items={[["Amazon ECS", "/ecs"], ["Clusters", "/ecs"], [cluster]]} />
      <PageHeader title={cluster} actions={<Button variant="primary" onClick={() => setRun(true)}><Play /> Run new task</Button>} />
      <Tabs defaultValue="tasks">
        <TabsList><TabsTrigger value="tasks">Tasks</TabsTrigger><TabsTrigger value="services">Services ({svcs.data?.length ?? 0})</TabsTrigger></TabsList>
        <TabsContent value="tasks">
          <Container title="Tasks" counter={tasks.data?.length} flush
            actions={<>
              <RefreshButton onClick={tasks.refresh} />
              <Button disabled={!sel.length || desired !== "RUNNING"} onClick={async () => {
                await act("Stop tasks", async () => { for (const t of sel) await call("ecs", "StopTask", { cluster, task: t, reason: "Stopped from the localaws console" }); }, `${sel.length} task(s) stopping`);
                setSel([]); tasks.refresh();
              }}><Square /> Stop</Button>
              <Button variant="primary" onClick={() => setRun(true)}>Run new task</Button>
            </>}>
            <div className="px-5 flex flex-wrap gap-3 items-start">
              <div className="flex-1"><TextFilter value={f.q} onChange={f.setQ} placeholder="Filter tasks by id, group, started by, task definition" count={f.rows.length} /></div>
              <Select value={desired} onChange={(e) => { setDesired(e.target.value); setSel([]); }}>
                <option value="RUNNING">Desired status: Running</option><option value="STOPPED">Desired status: Stopped</option>
              </Select>
            </div>
            <DataTable rows={f.rows} rowKey={(t: J) => t.taskArn} selected={sel} onSelect={setSel} multi initialSort={{ key: "created", desc: true }}
              empty={desired === "RUNNING" ? "No running tasks. Run one, start an execution that runs one, or switch to Stopped." : "No stopped tasks"}
              columns={[
                { key: "id", header: "Task", cell: (t: J) => <a href={href("/ecs/task", { arn: t.taskArn })} onClick={(e) => e.stopPropagation()} className="font-mono text-xs">{arnTail(t.taskArn)}</a> },
                { key: "last", header: "Last status", sort: (t: J) => t.lastStatus, cell: (t: J) => <Status value={t.lastStatus} /> },
                { key: "td", header: "Task definition", cell: (t: J) => <a href={href(`/ecs/taskdef/${tdName(t.taskDefinitionArn).split(":")[0]}`, { rev: tdName(t.taskDefinitionArn).split(":")[1] })} onClick={(e) => e.stopPropagation()}>{tdName(t.taskDefinitionArn)}</a> },
                { key: "exit", header: "Exit code", cell: (t: J) => t.containers?.map((c: J) => c.exitCode).filter((x: unknown) => x !== undefined).join(", ") || "–" },
                { key: "stop", header: "Stop code", cell: (t: J) => t.stopCode ?? "–" },
                { key: "by", header: "Started by", cell: (t: J) => t.startedBy || "–" },
                { key: "created", header: "Created", sort: (t: J) => t.createdAt, cell: (t: J) => fmtAgo(t.createdAt) },
                { key: "dur", header: "Run time", cell: (t: J) => fmtDuration(t.startedAt ? ((toDate(t.stoppedAt) ?? new Date()).getTime() - toDate(t.startedAt)!.getTime()) : null) },
              ]} />
          </Container>
        </TabsContent>
        <TabsContent value="services">
          <Container title="Services" counter={svcs.data?.length} flush actions={<RefreshButton onClick={svcs.refresh} />}>
            <DataTable rows={svcs.data ?? []} rowKey={(s: J) => s.serviceArn} empty="No services. Services keep desiredCount tasks running."
              columns={[
                { key: "n", header: "Service name", cell: (s: J) => <b>{s.serviceName}</b> },
                { key: "st", header: "Status", cell: (s: J) => <Status value={s.status} /> },
                { key: "td", header: "Task definition", cell: (s: J) => tdName(s.taskDefinition) },
                { key: "d", header: "Desired", cell: (s: J) => s.desiredCount },
                { key: "r", header: "Running", cell: (s: J) => s.runningCount },
                { key: "p", header: "Pending", cell: (s: J) => s.pendingCount },
                { key: "a", header: "", cell: (s: J) => <Button size="sm" onClick={(e) => { e.stopPropagation(); setScale(s); }}>Update</Button> },
              ]} />
          </Container>
        </TabsContent>
      </Tabs>
      <RunTask open={run} onOpenChange={setRun} cluster={cluster} done={(arn) => navigate("/ecs/task", { arn })} />
      <ScaleService svc={scale} close={() => { setScale(null); svcs.refresh(); }} cluster={cluster} />
    </>
  );
}

function ScaleService({ svc, close, cluster }: { svc: J | null; close: () => void; cluster: string }) {
  const [n, setN] = React.useState("0");
  React.useEffect(() => { if (svc) setN(String(svc.desiredCount)); }, [svc]);
  return (
    <Dialog open={!!svc} onOpenChange={(v) => !v && close()}>
      <DialogContent title={`Update ${svc?.serviceName}`} footer={<><Button variant="link" onClick={close}>Cancel</Button>
        <Button variant="primary" onClick={async () => { await act("Update service", () => call("ecs", "UpdateService", { cluster, service: svc.serviceName, desiredCount: +n }), `Desired count ${n}`); close(); }}>Update</Button></>}>
        <Field label="Desired tasks" hint="The service starts or stops tasks to match within a few seconds."><Input type="number" min={0} value={n} onChange={(e) => setN(e.target.value)} /></Field>
      </DialogContent>
    </Dialog>
  );
}

async function networks() {
  const [s, g] = await Promise.all([query("ec2", "DescribeSubnets"), query("ec2", "DescribeSecurityGroups")]);
  return {
    subnets: Array.from(s.querySelectorAll("subnetSet > item")).map((i) => ({ id: text(i, "subnetId"), vpc: text(i, "vpcId"), az: text(i, "availabilityZone") })),
    groups: Array.from(g.querySelectorAll("securityGroupInfo > item")).map((i) => ({ id: text(i, "groupId"), name: text(i, "groupName") })),
  };
}

/** the console's "Run task" form */
export function RunTask({ open, onOpenChange, cluster, family, done }: { open: boolean; onOpenChange: (v: boolean) => void; cluster?: string; family?: string; done: (arn: string) => void }) {
  const tds = useData(() => all("ecs", "ListTaskDefinitions", {}, "taskDefinitionArns") as Promise<string[]>, [open]);
  const clusters = useData(async () => ((await call("ecs", "ListClusters")).clusterArns ?? []).map(arnTail) as string[], [open]);
  const net = useData(networks, [open]);
  const [c, setC] = React.useState(cluster ?? "default");
  const [td, setTd] = React.useState("");
  const [spec, setSpec] = React.useState<J>(null);
  const [container, setContainer] = React.useState("");
  const [cmd, setCmd] = React.useState("");
  const [env, setEnv] = React.useState("");
  const [count, setCount] = React.useState("1");
  const [startedBy, setStartedBy] = React.useState("console");
  const [subnets, setSubnets] = React.useState<string[]>([]);
  const [sgs, setSgs] = React.useState<string[]>([]);
  React.useEffect(() => { if (open) { setC(cluster ?? "default"); setCmd(""); setEnv(""); setCount("1"); } }, [open, cluster]);
  React.useEffect(() => {
    if (!tds.data?.length || td) return;
    const pickFam = family ? tds.data.filter((a) => tdName(a).startsWith(family + ":")) : tds.data;
    setTd(tdName(pickFam[pickFam.length - 1] ?? tds.data[0]));
  }, [tds.data, family, td]);
  React.useEffect(() => {
    if (!td) return;
    call("ecs", "DescribeTaskDefinition", { taskDefinition: td }).then((r) => {
      setSpec(r.taskDefinition);
      const first = r.taskDefinition.containerDefinitions?.[0];
      setContainer(first?.name ?? "");
    });
  }, [td]);
  React.useEffect(() => {
    if (net.data && !subnets.length) { setSubnets(net.data.subnets.slice(0, 1).map((s) => s.id)); setSgs(net.data.groups.slice(0, 1).map((g) => g.id)); }
  }, [net.data, subnets.length]);
  const awsvpc = spec?.networkMode === "awsvpc";
  const cmdParsed = cmd.trim().startsWith("[") ? tryJSON(cmd) : { ok: true as const, value: cmd.trim() ? cmd.trim().split(/\s+/) : undefined };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent wide title="Run task" description="RunTask with the same parameters the API takes."
        footer={<>
          <Button variant="link" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" disabled={!td || !cmdParsed.ok} onClick={async () => {
            const override: J = { name: container };
            if (cmdParsed.ok && cmdParsed.value) override.command = cmdParsed.value;
            const envs = env.split("\n").map((l) => l.trim()).filter((l) => l.includes("=")).map((l) => ({ name: l.slice(0, l.indexOf("=")), value: l.slice(l.indexOf("=") + 1) }));
            if (envs.length) override.environment = envs;
            const r = await act("Run task", () => call("ecs", "RunTask", {
              cluster: c, taskDefinition: td, count: +count, launchType: "FARGATE", startedBy,
              ...(awsvpc ? { networkConfiguration: { awsvpcConfiguration: { subnets, securityGroups: sgs, assignPublicIp: "DISABLED" } } } : {}),
              overrides: { containerOverrides: [override] },
            }), `${count} task(s) started`);
            if (r?.tasks?.[0]) { onOpenChange(false); done(r.tasks[0].taskArn); }
          }}><Play /> Run task</Button>
        </>}>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Cluster"><Select value={c} onChange={(e) => setC(e.target.value)} className="w-full">{(clusters.data ?? [c]).map((x) => <option key={x}>{x}</option>)}</Select></Field>
          <Field label="Task definition (family:revision)"><Select value={td} onChange={(e) => setTd(e.target.value)} className="w-full">{(tds.data ?? []).map((a) => <option key={a}>{tdName(a)}</option>)}</Select></Field>
          <Field label="Desired tasks"><Input type="number" min={1} max={10} value={count} onChange={(e) => setCount(e.target.value)} /></Field>
          <Field label="Started by"><Input value={startedBy} onChange={(e) => setStartedBy(e.target.value)} /></Field>
        </div>
        {awsvpc && net.data && (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Subnets" hint="awsvpc: required, validated like ECS does">
              <div className="space-y-1">{net.data.subnets.map((s) => <label key={s.id} className="flex items-center gap-2 text-sm"><input type="checkbox" className="accent-link" checked={subnets.includes(s.id)} onChange={(e) => setSubnets(e.target.checked ? [...subnets, s.id] : subnets.filter((x) => x !== s.id))} /><span className="font-mono text-xs">{s.id}</span> <span className="text-muted-foreground text-xs">{s.az}</span></label>)}
                {!net.data.subnets.length && <span className="text-xs text-muted-foreground">No subnets declared — any id is accepted.</span>}</div>
            </Field>
            <Field label="Security groups">
              <div className="space-y-1">{net.data.groups.map((g) => <label key={g.id} className="flex items-center gap-2 text-sm"><input type="checkbox" className="accent-link" checked={sgs.includes(g.id)} onChange={(e) => setSgs(e.target.checked ? [...sgs, g.id] : sgs.filter((x) => x !== g.id))} /><span className="font-mono text-xs">{g.id}</span> <span className="text-muted-foreground text-xs">{g.name}</span></label>)}</div>
            </Field>
          </div>
        )}
        <div className="rounded-xl border p-4 space-y-3">
          <div className="text-sm font-bold">Container overrides</div>
          <Field label="Container"><Select value={container} onChange={(e) => setContainer(e.target.value)}>{(spec?.containerDefinitions ?? []).map((cd: J) => <option key={cd.name}>{cd.name}</option>)}</Select></Field>
          <Field label="Command override" hint={<>Space separated, or a JSON array: <span className="font-mono">["node","dist/main.js","--job=x"]</span>. Empty = the definition's command{spec?.containerDefinitions?.find((x: J) => x.name === container)?.command ? <> (<span className="font-mono">{JSON.stringify(spec.containerDefinitions.find((x: J) => x.name === container).command)}</span>)</> : ""}.</>}
            error={cmdParsed.ok ? null : "Invalid JSON array"}>
            <Input value={cmd} onChange={(e) => setCmd(e.target.value)} className="font-mono" />
          </Field>
          <Field label="Environment overrides" hint="KEY=value, one per line"><Textarea rows={3} value={env} onChange={(e) => setEnv(e.target.value)} /></Field>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function TaskPage({ arn }: { arn: string }) {
  const cluster = arn.split(":task/")[1]?.split("/")[0] ?? "default";
  const t = useData(async () => (await describeTasks(cluster, [arn]))[0], [arn], { poll: 2000, live: (d: J) => d?.lastStatus !== "STOPPED" });
  const td = useData(async () => (t.data ? (await call("ecs", "DescribeTaskDefinition", { taskDefinition: t.data.taskDefinitionArn })).taskDefinition : null), [t.data?.taskDefinitionArn]);
  if (t.error) return <Alert title="Task not found">{t.error}</Alert>;
  if (!t.data) return <Spinner />;
  const task = t.data;
  const id = arnTail(task.taskArn);
  const logs = (td.data?.containerDefinitions ?? []).filter((c: J) => c.logConfiguration?.logDriver === "awslogs").map((c: J) => {
    const o = c.logConfiguration.options ?? {};
    return { container: c.name, group: o["awslogs-group"], stream: o["awslogs-stream-prefix"] ? `${o["awslogs-stream-prefix"]}/${c.name}/${id}` : id };
  });
  return (
    <>
      <Breadcrumbs items={[["Amazon ECS", "/ecs"], ["Clusters", "/ecs"], [cluster, `/ecs/cluster/${cluster}`], ["Tasks", `/ecs/cluster/${cluster}`], [id]]} />
      <PageHeader title={<span className="font-mono text-2xl">{id}</span>}
        actions={task.lastStatus !== "STOPPED" && <Button variant="danger" onClick={() => act("Stop task", () => call("ecs", "StopTask", { cluster, task: task.taskArn, reason: "Stopped from the localaws console" }), "Task stopping").then(() => t.refresh())}><Square /> Stop</Button>} />
      <div className="space-y-5">
        <Container title="Task overview">
          <KeyValue cols={4} items={[
            ["Last status", <Status value={task.lastStatus} />], ["Desired status", task.desiredStatus],
            ["Task definition", <a href={href(`/ecs/taskdef/${tdName(task.taskDefinitionArn).split(":")[0]}`, { rev: tdName(task.taskDefinitionArn).split(":")[1] })}>{tdName(task.taskDefinitionArn)}</a>],
            ["Started by", task.startedBy], ["Group", task.group], ["Launch type", task.launchType],
            ["Created", fmtTime(task.createdAt, true)], ["Started", fmtTime(task.startedAt, true)], ["Stopped", fmtTime(task.stoppedAt, true)],
            ["Run time", fmtDuration(task.startedAt ? ((toDate(task.stoppedAt) ?? new Date()).getTime() - toDate(task.startedAt)!.getTime()) : null)],
            ["Stop code", task.stopCode], ["CPU / memory", `${task.cpu ?? "–"} / ${task.memory ?? "–"}`],
            ["ARN", <Copyable value={task.taskArn} />],
          ]} />
          {task.stoppedReason && <div className="mt-4"><Alert kind={task.stopCode === "EssentialContainerExited" ? "info" : "error"} title="Stopped reason">{task.stoppedReason}</Alert></div>}
        </Container>
        <Container title="Containers" counter={task.containers?.length} flush>
          <DataTable rows={task.containers ?? []} rowKey={(c: J) => c.name} columns={[
            { key: "n", header: "Name", cell: (c: J) => <b>{c.name}</b> },
            { key: "s", header: "Status", cell: (c: J) => <Status value={c.lastStatus} /> },
            { key: "e", header: "Exit code", cell: (c: J) => c.exitCode === undefined ? "–" : <Badge tone={c.exitCode === 0 ? "green" : "red"}>{c.exitCode}</Badge> },
            { key: "i", header: "Image", cell: (c: J) => <span className="font-mono text-xs break-all">{c.image}</span> },
            { key: "r", header: "Reason", cell: (c: J) => c.reason ?? "–" },
          ]} />
        </Container>
        <Tabs defaultValue="logs">
          <TabsList><TabsTrigger value="logs">Logs</TabsTrigger><TabsTrigger value="overrides">Overrides</TabsTrigger><TabsTrigger value="json">JSON</TabsTrigger></TabsList>
          <TabsContent value="logs" className="space-y-5">
            {logs.length === 0 && <Alert kind="info">No container uses the awslogs driver.</Alert>}
            {logs.map((l: J) => (
              <Container key={l.container} title={`Logs · ${l.container}`} description={<><a href={href("/logs/stream", { group: l.group, stream: l.stream })}>{l.group} / {l.stream}</a></>}>
                <LogViewer group={l.group} stream={l.stream} live={task.lastStatus !== "STOPPED"} height="h-[420px]" />
              </Container>
            ))}
          </TabsContent>
          <TabsContent value="overrides"><Container title="Overrides"><Code>{pretty(task.overrides)}</Code></Container></TabsContent>
          <TabsContent value="json"><Container title="DescribeTasks"><Code>{pretty(task)}</Code></Container></TabsContent>
        </Tabs>
      </div>
    </>
  );
}

const READONLY = ["taskDefinitionArn", "revision", "status", "registeredAt", "compatibilities", "requiresAttributes", "registeredBy", "deregisteredAt"];

export function TaskDefs() {
  const { data, error, refresh } = useData(async () => {
    const arns: string[] = await all("ecs", "ListTaskDefinitions", { status: "ALL" }, "taskDefinitionArns");
    const fam = new Map<string, { family: string; latest: number; active: number; total: number }>();
    for (const a of arns) {
      const [f, r] = tdName(a).split(":");
      const x = fam.get(f) ?? { family: f, latest: 0, active: 0, total: 0 };
      x.latest = Math.max(x.latest, +r);
      x.total++;
      fam.set(f, x);
    }
    const active: string[] = await all("ecs", "ListTaskDefinitions", {}, "taskDefinitionArns");
    for (const a of active) { const f = tdName(a).split(":")[0]; const x = fam.get(f); if (x) x.active++; }
    return [...fam.values()];
  }, []);
  const [create, setCreate] = React.useState(false);
  const f = useFilter(data, (x) => x.family);
  return (
    <>
      <Breadcrumbs items={[["Amazon ECS", "/ecs"], ["Task definitions"]]} />
      <PageHeader title="Task definitions" description="Families and their revisions. A registered JSON exported from a real account works unchanged." />
      {error && <Alert>{error}</Alert>}
      <Container title="Task definitions" counter={data?.length} flush actions={<><RefreshButton onClick={refresh} /><Button variant="primary" onClick={() => setCreate(true)}><Plus /> Create new task definition</Button></>}>
        <div className="px-5"><TextFilter value={f.q} onChange={f.setQ} placeholder="Filter by family" count={f.rows.length} /></div>
        <DataTable rows={f.rows} rowKey={(x) => x.family} onRowClick={(x) => navigate(`/ecs/taskdef/${x.family}`)} columns={[
          { key: "f", header: "Task definition", cell: (x) => <a href={href(`/ecs/taskdef/${x.family}`)} className="font-bold">{x.family}</a> },
          { key: "l", header: "Latest revision", cell: (x) => x.latest },
          { key: "a", header: "Active revisions", cell: (x) => x.active },
        ]} />
      </Container>
      <RegisterTaskDef open={create} onOpenChange={setCreate} done={(fam) => navigate(`/ecs/taskdef/${fam}`)} />
    </>
  );
}

function RegisterTaskDef({ open, onOpenChange, base, done }: { open: boolean; onOpenChange: (v: boolean) => void; base?: J; done: (family: string) => void }) {
  const [json, setJson] = React.useState("");
  React.useEffect(() => {
    if (!open) return;
    if (base) {
      const b = { ...base };
      READONLY.forEach((k) => delete b[k]);
      setJson(JSON.stringify(b, null, 2));
    } else {
      setJson(JSON.stringify({
        family: "hello", networkMode: "awsvpc", requiresCompatibilities: ["FARGATE"], cpu: "256", memory: "512",
        containerDefinitions: [{ name: "app", image: "alpine:3", essential: true, command: ["sh", "-c", "echo hello from $HOSTNAME; sleep 5"],
          environment: [{ name: "GREETING", value: "hello" }],
          logConfiguration: { logDriver: "awslogs", options: { "awslogs-group": "/ecs/hello", "awslogs-stream-prefix": "hello", "awslogs-create-group": "true" } } }],
      }, null, 2));
    }
  }, [open, base]);
  const p = tryJSON(json);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent wide title={base ? `Create new revision of ${base.family}` : "Create new task definition"} description="RegisterTaskDefinition input (JSON)."
        footer={<><Button variant="link" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" disabled={!p.ok} onClick={async () => {
            const r = await act("Register task definition", () => call("ecs", "RegisterTaskDefinition", p.ok ? (p.value as J) : {}), "");
            if (r) { useStore.getState().notify("success", `Registered ${r.taskDefinition.family}:${r.taskDefinition.revision}`); onOpenChange(false); done(r.taskDefinition.family); }
          }}>Create</Button></>}>
        <JsonEditor value={json} onChange={setJson} rows={24} />
      </DialogContent>
    </Dialog>
  );
}

export function TaskDefPage({ family, rev }: { family: string; rev?: string }) {
  const revs = useData(async () => (await all("ecs", "ListTaskDefinitions", { familyPrefix: family, status: "ALL" }, "taskDefinitionArns") as string[])
    .filter((a) => tdName(a).split(":")[0] === family).reverse(), [family]);
  const current = rev ?? revs.data?.[0]?.split(":").pop();
  const td = useData(async () => (current ? (await call("ecs", "DescribeTaskDefinition", { taskDefinition: `${family}:${current}` })).taskDefinition : null), [family, current]);
  const [create, setCreate] = React.useState(false);
  const [run, setRun] = React.useState(false);
  const [dereg, setDereg] = React.useState(false);
  const d = td.data;
  return (
    <>
      <Breadcrumbs items={[["Amazon ECS", "/ecs"], ["Task definitions", "/ecs/taskdefs"], [family, `/ecs/taskdef/${family}`], ...(current ? [[`Revision ${current}`] as [string]] : [])]} />
      <PageHeader title={`${family}${current ? `:${current}` : ""}`}
        actions={d && <>
          <Button disabled={d.status !== "ACTIVE"} onClick={() => setDereg(true)}>Deregister</Button>
          <Button onClick={() => setCreate(true)}>Create new revision</Button>
          <Button variant="primary" disabled={d.status !== "ACTIVE"} onClick={() => setRun(true)}><Play /> Run task</Button>
        </>} />
      <div className="grid gap-5 lg:grid-cols-[16rem_minmax(0,1fr)]">
        <Container title="Revisions" counter={revs.data?.length} flush>
          <ul className="pb-2">{(revs.data ?? []).map((a) => { const r = a.split(":").pop(); return (
            <li key={a}><a href={href(`/ecs/taskdef/${family}`, { rev: r })} className={`block px-5 py-1.5 text-sm no-underline ${r === current ? "bg-accent font-bold text-link" : "text-foreground hover:bg-muted"}`}>{family}:{r}</a></li>); })}</ul>
        </Container>
        {!d ? <Spinner /> : (
          <div className="space-y-5 min-w-0">
            <Container title="Overview">
              <KeyValue cols={4} items={[["Status", <Status value={d.status} />], ["Network mode", d.networkMode], ["CPU / memory", `${d.cpu ?? "–"} / ${d.memory ?? "–"}`], ["Registered", fmtTime(d.registeredAt)], ["ARN", <Copyable value={d.taskDefinitionArn} />], ["Compatibilities", (d.requiresCompatibilities ?? []).join(", ")]]} />
            </Container>
            <Container title="Containers" counter={d.containerDefinitions?.length} flush>
              <DataTable rows={d.containerDefinitions ?? []} rowKey={(c: J) => c.name} columns={[
                { key: "n", header: "Name", cell: (c: J) => <b>{c.name}</b> },
                { key: "i", header: "Image", cell: (c: J) => <span className="font-mono text-xs break-all">{c.image}</span> },
                { key: "e", header: "Essential", cell: (c: J) => (c.essential === false ? "No" : "Yes") },
                { key: "env", header: "Environment", cell: (c: J) => `${c.environment?.length ?? 0} variables, ${c.secrets?.length ?? 0} secrets` },
                { key: "l", header: "Logs", cell: (c: J) => c.logConfiguration?.logDriver === "awslogs" ? <a href={href("/logs/group", { name: c.logConfiguration.options?.["awslogs-group"] })}>{c.logConfiguration.options?.["awslogs-group"]}</a> : "–" },
              ]} />
            </Container>
            <Container title="JSON"><Code>{pretty(d)}</Code></Container>
          </div>
        )}
      </div>
      <RegisterTaskDef open={create} onOpenChange={setCreate} base={d} done={() => revs.refresh().then(() => navigate(`/ecs/taskdef/${family}`))} />
      <RunTask open={run} onOpenChange={setRun} family={family} done={(arn) => navigate("/ecs/task", { arn })} />
      <ConfirmDelete open={dereg} onOpenChange={setDereg} what="task definition revision" name={`${family}:${current}`}
        onConfirm={async () => { await act("Deregister", () => call("ecs", "DeregisterTaskDefinition", { taskDefinition: `${family}:${current}` }), `${family}:${current} deregistered`); td.refresh(); }} />
    </>
  );
}
