import * as React from "react";
import { Play, Square, Pencil } from "lucide-react";
import { all, call, type J } from "@/lib/aws";
import { useData } from "@/lib/hooks";
import { act, useStore } from "@/store";
import { href, navigate } from "@/router";
import { Button } from "@/components/ui/button";
import { Field, Input, Select } from "@/components/ui/form";
import { Dialog, DialogContent, Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/overlay";
import { Alert, Badge, Code, Container, Copyable, DataTable, KeyValue, Spinner, Status } from "@/components/ui/data";
import { Breadcrumbs, PageHeader, RefreshButton } from "@/components/layout";
import { ConfirmDelete, JsonEditor, TextFilter, useFilter } from "@/components/common";
import { StateGraph } from "@/components/graph";
import { details, stateRuns, statusMap, type StateRun } from "@/lib/history";
import { arnTail, fmtAgo, fmtDuration, fmtTime, pretty, toDate, tryJSON } from "@/lib/utils";

const HELLO = `{
  "Comment": "A Hello World example",
  "StartAt": "Hello",
  "States": {
    "Hello": { "Type": "Pass", "Result": "Hello", "ResultPath": "$.greeting", "Next": "IsWorld" },
    "IsWorld": {
      "Type": "Choice",
      "Choices": [{ "Variable": "$.name", "StringEquals": "world", "Next": "Wait" }],
      "Default": "Fail"
    },
    "Wait": { "Type": "Wait", "Seconds": 2, "Next": "Done" },
    "Done": { "Type": "Succeed" },
    "Fail": { "Type": "Fail", "Error": "NotWorld", "Cause": "name must be world" }
  }
}`;

const dur = (a: unknown, b: unknown) => {
  const x = toDate(a), y = toDate(b);
  return x && y ? y.getTime() - x.getTime() : null;
};

export function StateMachines() {
  const { data, error, loading, refresh } = useData(async () => {
    const sms: J[] = await all("states", "ListStateMachines", {}, "stateMachines");
    return Promise.all(sms.map(async (m) => {
      const ex: J[] = (await call("states", "ListExecutions", { stateMachineArn: m.stateMachineArn, maxResults: 100 })).executions ?? [];
      return { ...m, running: ex.filter((e) => e.status === "RUNNING").length, last: ex[0] };
    }));
  }, [], { poll: 5000 });
  const f = useFilter(data, (m: J) => m.name);
  const [sel, setSel] = React.useState<string[]>([]);
  const [create, setCreate] = React.useState(false);
  const [del, setDel] = React.useState(false);
  return (
    <>
      <Breadcrumbs items={[["Step Functions", "/states"], ["State machines"]]} />
      <PageHeader title="State machines" description="Workflows written in Amazon States Language. localaws interprets them state by state and writes the same execution history AWS does." />
      {error && <Alert title="Could not list state machines">{error}</Alert>}
      <Container title="State machines" counter={data?.length} flush
        actions={<>
          <RefreshButton onClick={refresh} />
          <Button disabled={sel.length !== 1} onClick={() => setDel(true)}>Delete</Button>
          <Button disabled={sel.length !== 1} onClick={() => navigate(`/states/sm/${sel[0]}`)}>View details</Button>
          <Button variant="primary" onClick={() => setCreate(true)}>Create state machine</Button>
        </>}>
        <div className="px-5"><TextFilter value={f.q} onChange={f.setQ} placeholder="Search for state machines" count={f.rows.length} /></div>
        {loading && !data ? <Spinner /> : (
          <DataTable rows={f.rows} rowKey={(m: J) => m.name} selected={sel} onSelect={setSel} initialSort={{ key: "name" }}
            columns={[
              { key: "name", header: "Name", sort: (m: J) => m.name, cell: (m: J) => <a href={href(`/states/sm/${m.name}`)} onClick={(e) => e.stopPropagation()} className="font-bold">{m.name}</a> },
              { key: "type", header: "Type", cell: (m: J) => (m.type === "EXPRESS" ? "Express" : "Standard") },
              { key: "running", header: "Running", sort: (m: J) => m.running, cell: (m: J) => (m.running ? <Status value="RUNNING" label={String(m.running)} /> : "0") },
              { key: "last", header: "Last execution", cell: (m: J) => (m.last ? <span className="inline-flex gap-2"><Status value={m.last.status} /> <span className="text-muted-foreground">{fmtAgo(m.last.startDate)}</span></span> : "–") },
              { key: "created", header: "Creation date", sort: (m: J) => m.creationDate, cell: (m: J) => fmtTime(m.creationDate) },
            ]} />
        )}
      </Container>
      <EditMachine open={create} onOpenChange={setCreate} done={(name) => navigate(`/states/sm/${name}`)} />
      <ConfirmDelete open={del} onOpenChange={setDel} what="state machine" name={sel[0] ?? ""}
        onConfirm={async () => { const m = data?.find((x: J) => x.name === sel[0]); await act("Delete state machine", () => call("states", "DeleteStateMachine", { stateMachineArn: m.stateMachineArn }), `${sel[0]} deleted`); setSel([]); refresh(); }} />
    </>
  );
}

/** create (no `existing`) or edit a state machine, with a live graph of the definition */
function EditMachine({ open, onOpenChange, existing, done }: { open: boolean; onOpenChange: (v: boolean) => void; existing?: J; done: (name: string) => void }) {
  const info = useStore((s) => s.info);
  const [name, setName] = React.useState("");
  const [type, setType] = React.useState("STANDARD");
  const [role, setRole] = React.useState("");
  const [def, setDef] = React.useState(HELLO);
  const [diag, setDiag] = React.useState<J[] | null>(null);
  React.useEffect(() => {
    if (!open) return;
    setName(existing?.name ?? "");
    setType(existing?.type ?? "STANDARD");
    setRole(existing?.roleArn ?? `arn:aws:iam::${info?.account ?? "000000000000"}:role/StepFunctionsRole`);
    setDef(existing ? pretty(existing.definition) : HELLO);
    setDiag(null);
  }, [open, existing, info]);
  React.useEffect(() => {
    const t = setTimeout(async () => {
      if (!tryJSON(def).ok) return setDiag(null);
      const r = await call("states", "ValidateStateMachineDefinition", { definition: def }).catch(() => null);
      setDiag(r?.diagnostics ?? null);
    }, 400);
    return () => clearTimeout(t);
  }, [def]);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent wide title={existing ? `Edit ${existing.name}` : "Create state machine"} className="max-w-6xl"
        footer={<>
          <Button variant="link" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" disabled={!name || !tryJSON(def).ok || (diag?.length ?? 0) > 0} onClick={async () => {
            const r = existing
              ? await act("Update state machine", () => call("states", "UpdateStateMachine", { stateMachineArn: existing.stateMachineArn, definition: def, roleArn: role }), `${name} updated`)
              : await act("Create state machine", () => call("states", "CreateStateMachine", { name, type, roleArn: role, definition: def }), `${name} created`);
            if (r) { onOpenChange(false); done(name); }
          }}>{existing ? "Save" : "Create"}</Button>
        </>}>
        {!existing && (
          <div className="grid gap-4 sm:grid-cols-3">
            <Field label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="MyStateMachine" autoFocus /></Field>
            <Field label="Type"><Select value={type} onChange={(e) => setType(e.target.value)} className="w-full"><option value="STANDARD">Standard</option><option value="EXPRESS">Express</option></Select></Field>
            <Field label="Execution role ARN"><Input value={role} onChange={(e) => setRole(e.target.value)} /></Field>
          </div>
        )}
        <div className="grid gap-4 lg:grid-cols-2">
          <Field label="Definition (Amazon States Language)">
            <JsonEditor value={def} onChange={setDef} rows={22} />
          </Field>
          <div>
            <div className="text-sm font-bold mb-1">Graph</div>
            <StateGraph definition={def} className="max-h-[468px]" />
          </div>
        </div>
        {diag && diag.length > 0 && <Alert title="Definition has errors">{diag.map((d: J, i: number) => <div key={i} className="font-mono text-xs">{d.code}: {d.message}</div>)}</Alert>}
      </DialogContent>
    </Dialog>
  );
}

function StartExecution({ open, onOpenChange, sm, input: initial }: { open: boolean; onOpenChange: (v: boolean) => void; sm: J; input?: string }) {
  const [name, setName] = React.useState("");
  const [input, setInput] = React.useState("{}");
  React.useEffect(() => { if (open) { setName(crypto.randomUUID()); setInput(initial ? pretty(initial) : `{\n  "name": "world"\n}`); } }, [open, initial]);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title="Start execution" description={sm?.name}
        footer={<>
          <Button variant="link" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" disabled={!tryJSON(input).ok} onClick={async () => {
            const r = await act("Start execution", () => call("states", "StartExecution", { stateMachineArn: sm.stateMachineArn, name, input }), "Execution started");
            if (r) { onOpenChange(false); navigate("/states/execution", { arn: r.executionArn }); }
          }}><Play /> Start execution</Button>
        </>}>
        <Field label="Name" hint="Unique per state machine"><Input value={name} onChange={(e) => setName(e.target.value)} /></Field>
        <Field label="Input"><JsonEditor value={input} onChange={setInput} rows={10} /></Field>
      </DialogContent>
    </Dialog>
  );
}

export function StateMachinePage({ name }: { name: string }) {
  const info = useStore((s) => s.info);
  const arn = `arn:aws:states:${info?.region}:${info?.account}:stateMachine:${name}`;
  const sm = useData(() => call("states", "DescribeStateMachine", { stateMachineArn: name }), [name]);
  const [statusF, setStatusF] = React.useState("");
  const ex = useData(() => all("states", "ListExecutions", { stateMachineArn: sm.data?.stateMachineArn ?? arn, ...(statusF ? { statusFilter: statusF } : {}) }, "executions"),
    [name, statusF, sm.data?.stateMachineArn], { poll: 3000 });
  const f = useFilter(ex.data, (e: J) => e.name);
  const [start, setStart] = React.useState(false);
  const [edit, setEdit] = React.useState(false);
  const [del, setDel] = React.useState(false);
  if (sm.error) return <Alert title="State machine not found">{sm.error}</Alert>;
  if (!sm.data) return <Spinner />;
  const m = sm.data;
  return (
    <>
      <Breadcrumbs items={[["Step Functions", "/states"], ["State machines", "/states"], [name]]} />
      <PageHeader title={name}
        actions={<>
          <Button variant="normal" onClick={() => setDel(true)}>Delete</Button>
          <Button onClick={() => setEdit(true)}><Pencil /> Edit</Button>
          <Button variant="primary" onClick={() => setStart(true)}><Play /> Start execution</Button>
        </>} />
      <div className="space-y-5">
        <Container title="Details">
          <KeyValue cols={4} items={[
            ["ARN", <Copyable value={m.stateMachineArn} />], ["Type", m.type === "EXPRESS" ? "Express" : "Standard"],
            ["IAM role ARN", <Copyable value={m.roleArn} />], ["Creation date", fmtTime(m.creationDate)],
          ]} />
        </Container>
        <Tabs defaultValue="executions">
          <TabsList><TabsTrigger value="executions">Executions</TabsTrigger><TabsTrigger value="definition">Definition</TabsTrigger></TabsList>
          <TabsContent value="executions">
            <Container title="Executions" counter={ex.data?.length} flush actions={<RefreshButton onClick={ex.refresh} />}>
              <div className="px-5 flex flex-wrap gap-3 items-start">
                <div className="flex-1"><TextFilter value={f.q} onChange={f.setQ} placeholder="Search executions by name" count={f.rows.length} /></div>
                <Select value={statusF} onChange={(e) => setStatusF(e.target.value)}>
                  <option value="">Any status</option>
                  {["RUNNING", "SUCCEEDED", "FAILED", "TIMED_OUT", "ABORTED"].map((s) => <option key={s} value={s}>{s.toLowerCase()}</option>)}
                </Select>
              </div>
              <DataTable rows={f.rows} rowKey={(e: J) => e.executionArn} initialSort={{ key: "start", desc: true }} empty="No executions"
                onRowClick={(e: J) => navigate("/states/execution", { arn: e.executionArn })}
                columns={[
                  { key: "name", header: "Name", cell: (e: J) => <a href={href("/states/execution", { arn: e.executionArn })} className="break-all">{e.name}</a> },
                  { key: "status", header: "Status", sort: (e: J) => e.status, cell: (e: J) => <Status value={e.status} /> },
                  { key: "start", header: "Started", sort: (e: J) => e.startDate, cell: (e: J) => fmtTime(e.startDate) },
                  { key: "end", header: "End time", cell: (e: J) => (e.stopDate ? fmtTime(e.stopDate) : "–") },
                  { key: "dur", header: "Duration", sort: (e: J) => dur(e.startDate, e.stopDate ?? Date.now() / 1000) ?? 0, cell: (e: J) => fmtDuration(dur(e.startDate, e.stopDate ?? Date.now() / 1000)) },
                ]} />
            </Container>
          </TabsContent>
          <TabsContent value="definition">
            <div className="grid gap-5 xl:grid-cols-2">
              <Container title="Graph"><StateGraph definition={m.definition} className="max-h-[640px]" /></Container>
              <Container title="Definition" actions={<Button onClick={() => setEdit(true)}><Pencil /> Edit</Button>}><Code className="h-[560px] max-h-none">{pretty(m.definition)}</Code></Container>
            </div>
          </TabsContent>
        </Tabs>
      </div>
      <StartExecution open={start} onOpenChange={setStart} sm={m} />
      <EditMachine open={edit} onOpenChange={setEdit} existing={m} done={() => sm.refresh()} />
      <ConfirmDelete open={del} onOpenChange={setDel} what="state machine" name={name}
        onConfirm={async () => { await act("Delete state machine", () => call("states", "DeleteStateMachine", { stateMachineArn: m.stateMachineArn }), `${name} deleted`); navigate("/states"); }} />
    </>
  );
}

export function ExecutionPage({ arn }: { arn: string }) {
  const ex = useData(() => call("states", "DescribeExecution", { executionArn: arn }), [arn], { poll: 1500, live: (d: J) => d.status === "RUNNING" });
  const hist = useData(() => all("states", "GetExecutionHistory", { executionArn: arn, maxResults: 1000 }, "events"), [arn],
    { poll: 1500, live: () => ex.data?.status === "RUNNING" });
  const smName = arnTail(arn.split(":execution:")[1]?.split(":")[0] ?? "") || (arn.split(":")[6] ?? "");
  const sm = useData(() => call("states", "DescribeStateMachineForExecution", { executionArn: arn }), [arn]);
  const [sel, setSel] = React.useState<string>();
  const [start, setStart] = React.useState(false);
  const runs = React.useMemo(() => stateRuns(hist.data ?? [], ex.data?.status), [hist.data, ex.data?.status]);
  const status = statusMap(runs);
  React.useEffect(() => {
    if (!sel && runs.length) setSel((runs.find((r) => r.status === "FAILED") ?? runs.find((r) => r.status === "RUNNING") ?? runs[runs.length - 1]).name);
  }, [runs, sel]);
  if (ex.error) return <Alert title="Execution not found">{ex.error}</Alert>;
  if (!ex.data) return <Spinner />;
  const e = ex.data;
  const selected = runs.find((r) => r.name === sel);
  return (
    <>
      <Breadcrumbs items={[["Step Functions", "/states"], ["State machines", "/states"], [smName, `/states/sm/${smName}`], [e.name]]} />
      <PageHeader title={e.name}
        actions={<>
          {e.status === "RUNNING" && <Button variant="danger" onClick={() => act("Stop execution", () => call("states", "StopExecution", { executionArn: arn, error: "UserStopped", cause: "Stopped from the localaws console" }), "Execution stopped").then(() => ex.refresh())}><Square /> Stop execution</Button>}
          {sm.data && <Button onClick={() => setStart(true)}><Play /> New execution</Button>}
        </>} />
      <div className="space-y-5">
        <Container title="Execution details">
          <KeyValue cols={4} items={[
            ["Status", <Status value={e.status} />], ["Execution ARN", <Copyable value={e.executionArn} />],
            ["State machine", <a href={href(`/states/sm/${smName}`)}>{smName}</a>], ["Duration", fmtDuration(dur(e.startDate, e.stopDate ?? Date.now() / 1000))],
            ["Start time", fmtTime(e.startDate, true)], ["End time", e.stopDate ? fmtTime(e.stopDate, true) : "–"],
            ["Error", e.error ? <span className="text-danger font-bold">{e.error}</span> : "–"], ["Events", hist.data?.length ?? "…"],
          ]} />
          {e.cause && <div className="mt-4"><div className="text-sm font-bold text-muted-foreground mb-1">Cause</div><Code className="max-h-48">{pretty(e.cause)}</Code></div>}
        </Container>
        <Tabs defaultValue="graph">
          <TabsList>
            <TabsTrigger value="graph">Graph view</TabsTrigger>
            <TabsTrigger value="table">Table view</TabsTrigger>
            <TabsTrigger value="events">Events ({hist.data?.length ?? 0})</TabsTrigger>
            <TabsTrigger value="io">Execution input &amp; output</TabsTrigger>
          </TabsList>
          <TabsContent value="graph">
            <div className="grid gap-5 xl:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)]">
              <Container title="Graph view" flush><div className="p-3">{sm.data ? <StateGraph definition={sm.data.definition} status={status} selected={sel} onSelect={setSel} className="max-h-[680px]" /> : <Spinner />}</div></Container>
              <StatePanel run={selected} />
            </div>
          </TabsContent>
          <TabsContent value="table">
            <Container title="States" counter={runs.length} flush>
              <DataTable rows={runs} rowKey={(r) => r.name} onRowClick={(r) => setSel(r.name)} selected={sel ? [sel] : []}
                columns={[
                  { key: "name", header: "Name", cell: (r) => <b>{r.name}</b> },
                  { key: "type", header: "Type", cell: (r) => r.type },
                  { key: "status", header: "Status", cell: (r) => <Status value={r.status === "NOT_STARTED" ? "PENDING" : r.status} /> },
                  { key: "resource", header: "Resource", cell: (r) => r.resource ?? "–" },
                  { key: "attempts", header: "Attempts", cell: (r) => r.attempts || "–" },
                  { key: "start", header: "Started after", cell: (r) => fmtDuration(dur(e.startDate, r.entered)) },
                  { key: "dur", header: "Duration", cell: (r) => fmtDuration(r.entered && (r.exited ?? (e.stopDate ? toDate(e.stopDate) : new Date())) ? ((r.exited ?? toDate(e.stopDate) ?? new Date()).getTime() - r.entered.getTime()) : null) },
                ]} />
            </Container>
            <div className="mt-5"><StatePanel run={selected} /></div>
          </TabsContent>
          <TabsContent value="events">
            <Container title="Event history" counter={hist.data?.length} flush actions={<RefreshButton onClick={hist.refresh} />}>
              <EventTable events={hist.data ?? []} start={e.startDate} />
            </Container>
          </TabsContent>
          <TabsContent value="io">
            <div className="grid gap-5 lg:grid-cols-2">
              <Container title="Input"><Code>{pretty(e.input)}</Code></Container>
              <Container title="Output">{e.output ? <Code>{pretty(e.output)}</Code> : <span className="text-muted-foreground text-sm">{e.status === "RUNNING" ? "Still running" : "No output"}</span>}</Container>
            </div>
          </TabsContent>
        </Tabs>
      </div>
      {sm.data && <StartExecution open={start} onOpenChange={setStart} sm={sm.data} input={e.input} />}
    </>
  );
}

function StatePanel({ run }: { run?: StateRun }) {
  if (!run) return <Container title="Details"><span className="text-sm text-muted-foreground">Select a state in the graph.</span></Container>;
  return (
    <Container title={run.name} description={run.type}>
      <Tabs defaultValue="details">
        <TabsList><TabsTrigger value="details">Details</TabsTrigger><TabsTrigger value="input">Input</TabsTrigger><TabsTrigger value="output">Output</TabsTrigger><TabsTrigger value="events">Events</TabsTrigger></TabsList>
        <TabsContent value="details" className="space-y-4">
          <KeyValue cols={2} items={[
            ["Status", <Status value={run.status === "NOT_STARTED" ? "PENDING" : run.status} />], ["Resource", run.resource],
            ["Started", fmtTime(run.entered, true)], ["Duration", fmtDuration(run.entered && run.exited ? run.exited.getTime() - run.entered.getTime() : null)],
            ["Attempts", run.attempts || "–"],
            ["Resources started", run.taskArns.length ? <div className="space-y-1">{run.taskArns.map((a) => (
              a.includes(":task/") ? <a key={a} href={href("/ecs/task", { arn: a })} className="block font-mono text-xs">task {arnTail(a)}</a>
                : <a key={a} href={href("/states/execution", { arn: a })} className="block font-mono text-xs">{arnTail(a)}</a>))}</div> : "–"],
          ]} />
          {run.error && <Alert title={run.error}><Code className="mt-2 max-h-56 bg-card">{pretty(run.cause ?? "")}</Code></Alert>}
          {run.parameters && <div><div className="text-sm font-bold text-muted-foreground mb-1">Parameters sent</div><Code className="max-h-64">{pretty(run.parameters)}</Code></div>}
        </TabsContent>
        <TabsContent value="input"><Code>{run.input ? pretty(run.input) : "–"}</Code></TabsContent>
        <TabsContent value="output"><Code>{run.output ? pretty(run.output) : "–"}</Code></TabsContent>
        <TabsContent value="events"><EventTable events={run.events} /></TabsContent>
      </Tabs>
    </Container>
  );
}

function EventTable({ events, start }: { events: J[]; start?: unknown }) {
  const [open, setOpen] = React.useState<number | null>(null);
  return (
    <DataTable rows={events} rowKey={(e: J) => String(e.id)} onRowClick={(e: J) => setOpen(open === e.id ? null : e.id)}
      columns={[
        { key: "id", header: "ID", cell: (e: J) => e.id },
        { key: "type", header: "Type", cell: (e: J) => (
          <div>
            <span className={/Failed|TimedOut|Aborted/.test(e.type) ? "text-danger font-bold" : /Succeeded/.test(e.type) ? "text-success font-bold" : ""}>{e.type}</span>
            {open === e.id && <Code className="mt-2 max-h-80">{pretty(Object.fromEntries(Object.entries(details(e)).map(([k, v]) => [k, typeof v === "string" && /^[[{]/.test(v) ? tryJSON(v).ok ? JSON.parse(v as string) : v : v])))}</Code>}
          </div>
        ) },
        { key: "step", header: "Step", cell: (e: J) => details(e).name ?? details(e).resource ?? "–" },
        { key: "res", header: "Resource", cell: (e: J) => (details(e).resourceType ? <Badge>{details(e).resourceType}</Badge> : "–") },
        { key: "time", header: "Timestamp", cell: (e: J) => fmtTime(e.timestamp, true) },
        ...(start ? [{ key: "elapsed", header: "Elapsed", cell: (e: J) => fmtDuration(dur(start, e.timestamp)) }] : []),
      ]} />
  );
}
