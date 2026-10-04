import * as React from "react";
import { Send, Plus } from "lucide-react";
import { all, call, type J } from "@/lib/aws";
import { useData } from "@/lib/hooks";
import { act, useStore } from "@/store";
import { href, navigate } from "@/router";
import { Button } from "@/components/ui/button";
import { Field, Input, Select, Switch } from "@/components/ui/form";
import { Dialog, DialogContent } from "@/components/ui/overlay";
import { Alert, Badge, Code, Container, Copyable, DataTable, KeyValue, Spinner, Status } from "@/components/ui/data";
import { Breadcrumbs, PageHeader, RefreshButton } from "@/components/layout";
import { ConfirmDelete, JsonEditor, TextFilter, useFilter } from "@/components/common";
import { arnTail, pretty, tryJSON } from "@/lib/utils";

function targetLink(arn: string) {
  if (arn.includes(":stateMachine:")) return <a href={href(`/states/sm/${arnTail(arn)}`)}>{arnTail(arn)} <Badge tone="grey">Step Functions</Badge></a>;
  if (arn.includes(":log-group:")) {
    const g = arn.split(":log-group:")[1].replace(/:\*$/, "");
    return <a href={href("/logs/group", { name: g })}>{g} <Badge tone="grey">Logs</Badge></a>;
  }
  return <span className="font-mono text-xs break-all">{arn}</span>;
}

export function Rules() {
  const { data, error, refresh } = useData(async () => {
    const rules: J[] = await all("events", "ListRules", {}, "Rules", "NextToken");
    return Promise.all(rules.map(async (r) => ({ ...r, targets: (await call("events", "ListTargetsByRule", { Rule: r.Name })).Targets ?? [] })));
  }, [], { poll: 10000 });
  const f = useFilter(data, (r: J) => `${r.Name} ${r.EventPattern ?? ""}`);
  const [sel, setSel] = React.useState<string[]>([]);
  const [create, setCreate] = React.useState(false);
  const [del, setDel] = React.useState(false);
  const rule = data?.find((r: J) => r.Name === sel[0]);
  return (
    <>
      <Breadcrumbs items={[["Amazon EventBridge", "/events"], ["Rules"]]} />
      <PageHeader title="Rules" description="On the default event bus. S3 buckets with EventBridge notifications, PutEvents and rate() schedules are all matched here." />
      {error && <Alert>{error}</Alert>}
      <Container title="Rules" counter={data?.length} flush actions={<>
        <RefreshButton onClick={refresh} />
        <Button disabled={!rule} onClick={async () => { await act(rule.State === "ENABLED" ? "Disable rule" : "Enable rule", () => call("events", rule.State === "ENABLED" ? "DisableRule" : "EnableRule", { Name: rule.Name }), `${rule.Name} ${rule.State === "ENABLED" ? "disabled" : "enabled"}`); refresh(); }}>
          {rule?.State === "ENABLED" ? "Disable" : "Enable"}</Button>
        <Button disabled={!rule} onClick={() => setDel(true)}>Delete</Button>
        <Button variant="primary" onClick={() => setCreate(true)}><Plus /> Create rule</Button>
      </>}>
        <div className="px-5"><TextFilter value={f.q} onChange={f.setQ} placeholder="Find rules by name or pattern" count={f.rows.length} /></div>
        <DataTable rows={f.rows} rowKey={(r: J) => r.Name} selected={sel} onSelect={setSel} initialSort={{ key: "n" }} columns={[
          { key: "n", header: "Name", sort: (r: J) => r.Name, cell: (r: J) => <a href={href(`/events/rule/${r.Name}`)} onClick={(e) => e.stopPropagation()} className="font-bold">{r.Name}</a> },
          { key: "s", header: "Status", cell: (r: J) => <Status value={r.State} /> },
          { key: "t", header: "Type", cell: (r: J) => (r.ScheduleExpression ? <>Schedule <span className="font-mono text-xs">{r.ScheduleExpression}</span></> : "Standard") },
          { key: "tg", header: "Targets", cell: (r: J) => <div className="space-y-0.5">{r.targets.map((t: J) => <div key={t.Id}>{targetLink(t.Arn)}</div>)}</div> },
        ]} />
      </Container>
      <CreateRule open={create} onOpenChange={setCreate} done={(n) => navigate(`/events/rule/${n}`)} />
      <ConfirmDelete open={del} onOpenChange={setDel} what="rule" name={sel[0] ?? ""}
        onConfirm={async () => { await act("Delete rule", () => call("events", "DeleteRule", { Name: sel[0], Force: true }), `${sel[0]} deleted`); setSel([]); refresh(); }} />
    </>
  );
}

const SAMPLE_PATTERN = `{
  "source": ["aws.s3"],
  "detail-type": ["Object Created"],
  "detail": {
    "bucket": { "name": ["my-bucket"] },
    "object": { "key": [{ "prefix": "incoming/" }] }
  }
}`;

function CreateRule({ open, onOpenChange, done }: { open: boolean; onOpenChange: (v: boolean) => void; done: (name: string) => void }) {
  const info = useStore((s) => s.info);
  const sms = useData(() => all("states", "ListStateMachines", {}, "stateMachines"), [open]);
  const groups = useData(() => all("logs", "DescribeLogGroups", {}, "logGroups"), [open]);
  const [name, setName] = React.useState("");
  const [sched, setSched] = React.useState(false);
  const [pattern, setPattern] = React.useState(SAMPLE_PATTERN);
  const [rate, setRate] = React.useState("rate(5 minutes)");
  const [target, setTarget] = React.useState("");
  React.useEffect(() => { if (open) { setName(""); setPattern(SAMPLE_PATTERN); setTarget(""); setSched(false); } }, [open]);
  const opts = [
    ...(sms.data ?? []).map((m: J) => ({ arn: m.stateMachineArn, label: `Step Functions · ${m.name}` })),
    ...(groups.data ?? []).map((g: J) => ({ arn: `arn:aws:logs:${info?.region}:${info?.account}:log-group:${g.logGroupName}`, label: `CloudWatch Logs · ${g.logGroupName}` })),
  ];
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent wide title="Create rule" footer={<><Button variant="link" onClick={() => onOpenChange(false)}>Cancel</Button>
        <Button variant="primary" disabled={!name || (!sched && !tryJSON(pattern).ok)} onClick={async () => {
          const r = await act("Create rule", async () => {
            await call("events", "PutRule", { Name: name, ...(sched ? { ScheduleExpression: rate } : { EventPattern: JSON.stringify(JSON.parse(pattern)) }), State: "ENABLED" });
            if (target) await call("events", "PutTargets", { Rule: name, Targets: [{ Id: "target-1", Arn: target }] });
            return true;
          }, `Rule ${name} created`);
          if (r) { onOpenChange(false); done(name); }
        }}>Create</Button></>}>
        <Field label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} autoFocus /></Field>
        <label className="flex items-center gap-3 text-sm"><Switch checked={sched} onCheckedChange={setSched} /> Schedule (rate expression) instead of an event pattern</label>
        {sched ? <Field label="Schedule expression"><Input value={rate} onChange={(e) => setRate(e.target.value)} className="font-mono" /></Field>
          : <Field label="Event pattern"><JsonEditor value={pattern} onChange={setPattern} rows={12} /></Field>}
        <Field label="Target"><Select value={target} onChange={(e) => setTarget(e.target.value)} className="w-full">
          <option value="">— none (add later) —</option>
          {opts.map((o) => <option key={o.arn} value={o.arn}>{o.label}</option>)}
        </Select></Field>
      </DialogContent>
    </Dialog>
  );
}

export function RulePage({ name }: { name: string }) {
  const rule = useData(() => call("events", "DescribeRule", { Name: name }), [name]);
  const targets = useData(async () => (await call("events", "ListTargetsByRule", { Rule: name })).Targets ?? [], [name]);
  const [ev, setEv] = React.useState(SAMPLE_EVENT);
  const [res, setRes] = React.useState<boolean | null>(null);
  if (rule.error) return <Alert title="Rule not found">{rule.error}</Alert>;
  if (!rule.data) return <Spinner />;
  const r = rule.data;
  return (
    <>
      <Breadcrumbs items={[["Amazon EventBridge", "/events"], ["Rules", "/events"], [name]]} />
      <PageHeader title={name} actions={<Button onClick={async () => { await act("Toggle rule", () => call("events", r.State === "ENABLED" ? "DisableRule" : "EnableRule", { Name: name }), r.State === "ENABLED" ? "Rule disabled" : "Rule enabled"); rule.refresh(); }}>{r.State === "ENABLED" ? "Disable" : "Enable"}</Button>} />
      <div className="space-y-5">
        <Container title="Rule details"><KeyValue cols={3} items={[["Status", <Status value={r.State} />], ["ARN", <Copyable value={r.Arn} />], ["Event bus", r.EventBusName], ["Schedule", r.ScheduleExpression]]} /></Container>
        {r.EventPattern && (
          <div className="grid gap-5 lg:grid-cols-2">
            <Container title="Event pattern"><Code>{pretty(r.EventPattern)}</Code></Container>
            <Container title="Test the pattern" description="TestEventPattern: does this event match?">
              <JsonEditor value={ev} onChange={(v) => { setEv(v); setRes(null); }} rows={12} />
              <div className="flex items-center gap-3 mt-2">
                <Button onClick={async () => { const x = await act("Test pattern", () => call("events", "TestEventPattern", { EventPattern: r.EventPattern, Event: ev }), ""); if (x) setRes(x.Result); }}>Test pattern</Button>
                {res !== null && (res ? <Badge tone="green">Matches</Badge> : <Badge tone="red">Does not match</Badge>)}
              </div>
            </Container>
          </div>
        )}
        <Container title="Targets" counter={targets.data?.length} flush>
          <DataTable rows={targets.data ?? []} rowKey={(t: J) => t.Id} columns={[
            { key: "i", header: "Id", cell: (t: J) => t.Id },
            { key: "a", header: "Target", cell: (t: J) => targetLink(t.Arn) },
            { key: "in", header: "Input", cell: (t: J) => (t.Input ? "Constant" : t.InputPath ? `Path ${t.InputPath}` : t.InputTransformer ? "Input transformer" : "Matched event") },
          ]} />
        </Container>
      </div>
    </>
  );
}

const SAMPLE_EVENT = `{
  "version": "0",
  "id": "6a7e8feb-b491-4cf7-a9f1-bf3703467718",
  "detail-type": "Object Created",
  "source": "aws.s3",
  "account": "000000000000",
  "time": "2026-01-01T00:00:00Z",
  "region": "us-east-1",
  "resources": ["arn:aws:s3:::my-bucket"],
  "detail": {
    "bucket": { "name": "my-bucket" },
    "object": { "key": "incoming/report.csv", "size": 1024 }
  }
}`;

export function SendEvents() {
  const [source, setSource] = React.useState("my.app");
  const [type, setType] = React.useState("Order Placed");
  const [detail, setDetail] = React.useState(`{\n  "orderId": "1234",\n  "amount": 42\n}`);
  const [sent, setSent] = React.useState<J[]>([]);
  const rules = useData(() => all("events", "ListRules", {}, "Rules", "NextToken"), []);
  const [matches, setMatches] = React.useState<string[] | null>(null);
  const event = () => JSON.stringify({ version: "0", id: crypto.randomUUID(), "detail-type": type, source, account: "000000000000", time: new Date().toISOString(), region: "us-east-1", resources: [], detail: tryJSON(detail).ok ? JSON.parse(detail) : {} });
  return (
    <>
      <Breadcrumbs items={[["Amazon EventBridge", "/events"], ["Send events"]]} />
      <PageHeader title="Send events" description="PutEvents to the default bus — every enabled rule whose pattern matches delivers it to its targets." />
      <div className="grid gap-5 lg:grid-cols-2">
        <Container title="Event entry" actions={<>
          <Button onClick={async () => {
            const ev = event();
            const hits: string[] = [];
            for (const r of rules.data ?? []) {
              if (!r.EventPattern) continue;
              const x = await call("events", "TestEventPattern", { EventPattern: r.EventPattern, Event: ev }).catch(() => null);
              if (x?.Result) hits.push(r.Name);
            }
            setMatches(hits);
          }}>Which rules match?</Button>
          <Button variant="primary" disabled={!tryJSON(detail).ok} onClick={async () => {
            const r = await act("Send event", () => call("events", "PutEvents", { Entries: [{ Source: source, DetailType: type, Detail: detail, EventBusName: "default" }] }), "Event sent");
            if (r) setSent((s) => [{ at: new Date(), id: r.Entries?.[0]?.EventId, source, type }, ...s]);
          }}><Send /> Send</Button>
        </>}>
          <div className="space-y-4">
            <Field label="Event source"><Input value={source} onChange={(e) => setSource(e.target.value)} /></Field>
            <Field label="Detail type"><Input value={type} onChange={(e) => setType(e.target.value)} /></Field>
            <Field label="Event detail"><JsonEditor value={detail} onChange={setDetail} rows={10} /></Field>
          </div>
          {matches && <div className="mt-3">{matches.length ? <Alert kind="success" title="Matching rules">{matches.join(", ")}</Alert> : <Alert kind="info">No rule matches this event.</Alert>}</div>}
        </Container>
        <Container title="Sent" counter={sent.length} flush>
          <DataTable rows={sent} rowKey={(s: J) => s.id} empty="Nothing sent yet" columns={[
            { key: "t", header: "Time", cell: (s: J) => s.at.toLocaleTimeString() },
            { key: "s", header: "Source", cell: (s: J) => s.source },
            { key: "d", header: "Detail type", cell: (s: J) => s.type },
            { key: "i", header: "Event id", cell: (s: J) => <span className="font-mono text-xs">{s.id}</span> },
          ]} />
        </Container>
      </div>
    </>
  );
}
