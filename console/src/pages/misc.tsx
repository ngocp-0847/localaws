import * as React from "react";
import { Eye, EyeOff, Plus } from "lucide-react";
import { all, apiCalls, call, query, text, type J } from "@/lib/aws";
import { useData } from "@/lib/hooks";
import { act } from "@/store";
import { Button } from "@/components/ui/button";
import { Field, Input, Select, Textarea } from "@/components/ui/form";
import { Dialog, DialogContent } from "@/components/ui/overlay";
import { Alert, Badge, Container, DataTable } from "@/components/ui/data";
import { Breadcrumbs, PageHeader, RefreshButton } from "@/components/layout";
import { ConfirmDelete, TextFilter, useFilter } from "@/components/common";
import { fmtTime } from "@/lib/utils";

// ── Systems Manager · Parameter Store ─────────────────────────────────────
export function Parameters() {
  const { data, error, refresh } = useData(() => all("ssm", "DescribeParameters", { MaxResults: 50 }, "Parameters", "NextToken"), [], { poll: 15000 });
  const f = useFilter(data, (p: J) => p.Name);
  const [sel, setSel] = React.useState<string[]>([]);
  const [edit, setEdit] = React.useState<{ name?: string } | null>(null);
  const [del, setDel] = React.useState(false);
  const [shown, setShown] = React.useState<Record<string, string>>({});
  return (
    <>
      <Breadcrumbs items={[["Systems Manager", "/ssm"], ["Parameter Store"]]} />
      <PageHeader title="Parameter Store" description="ECS task definitions' `secrets` (valueFrom: parameter name or ARN) are resolved from here when a task starts." />
      {error && <Alert>{error}</Alert>}
      <Container title="My parameters" counter={data?.length} flush actions={<>
        <RefreshButton onClick={refresh} />
        <Button disabled={!sel.length} onClick={() => setDel(true)}>Delete</Button>
        <Button disabled={sel.length !== 1} onClick={() => setEdit({ name: sel[0] })}>Edit</Button>
        <Button variant="primary" onClick={() => setEdit({})}><Plus /> Create parameter</Button>
      </>}>
        <div className="px-5"><TextFilter value={f.q} onChange={f.setQ} placeholder="Filter by name or path" count={f.rows.length} /></div>
        <DataTable rows={f.rows} rowKey={(p: J) => p.Name} selected={sel} onSelect={setSel} multi initialSort={{ key: "n" }} columns={[
          { key: "n", header: "Name", sort: (p: J) => p.Name, cell: (p: J) => <span className="font-mono text-xs break-all">{p.Name}</span> },
          { key: "t", header: "Type", cell: (p: J) => (p.Type === "SecureString" ? <Badge tone="orange">SecureString</Badge> : p.Type) },
          { key: "v", header: "Value", cell: (p: J) => (
            <span className="inline-flex items-start gap-1.5">
              <button className="text-muted-foreground hover:text-foreground cursor-pointer" onClick={async (e) => {
                e.stopPropagation();
                if (shown[p.Name] !== undefined) return setShown(({ [p.Name]: _, ...rest }) => rest);
                const r = await call("ssm", "GetParameter", { Name: p.Name, WithDecryption: true });
                setShown((s) => ({ ...s, [p.Name]: r.Parameter.Value }));
              }}>{shown[p.Name] !== undefined ? <EyeOff className="size-4" /> : <Eye className="size-4" />}</button>
              <span className="font-mono text-xs break-all">{shown[p.Name] ?? "••••••"}</span>
            </span>) },
          { key: "ver", header: "Version", cell: (p: J) => p.Version },
          { key: "m", header: "Last modified", sort: (p: J) => p.LastModifiedDate, cell: (p: J) => fmtTime(p.LastModifiedDate) },
        ]} />
      </Container>
      <EditParam target={edit} close={() => { setEdit(null); refresh(); setShown({}); }} />
      <ConfirmDelete open={del} onOpenChange={setDel} what={`${sel.length} parameter(s)`} name={sel.join(", ")} confirmWord="delete"
        onConfirm={async () => { await act("Delete parameters", () => call("ssm", "DeleteParameters", { Names: sel }), `${sel.length} parameter(s) deleted`); setSel([]); refresh(); }} />
    </>
  );
}

function EditParam({ target, close }: { target: { name?: string } | null; close: () => void }) {
  const [name, setName] = React.useState("");
  const [type, setType] = React.useState("String");
  const [value, setValue] = React.useState("");
  React.useEffect(() => {
    if (!target) return;
    setName(target.name ?? "");
    setType("String");
    setValue("");
    if (target.name) call("ssm", "GetParameter", { Name: target.name, WithDecryption: true }).then((r) => { setType(r.Parameter.Type); setValue(r.Parameter.Value); });
  }, [target]);
  return (
    <Dialog open={!!target} onOpenChange={(v) => !v && close()}>
      <DialogContent title={target?.name ? "Edit parameter" : "Create parameter"} footer={<><Button variant="link" onClick={close}>Cancel</Button>
        <Button variant="primary" disabled={!name} onClick={async () => { const r = await act("Save parameter", () => call("ssm", "PutParameter", { Name: name, Type: type, Value: value, Overwrite: !!target?.name }), `${name} saved`); if (r) close(); }}>{target?.name ? "Save changes" : "Create parameter"}</Button></>}>
        <Field label="Name" hint="Hierarchy with slashes, e.g. /app/prod/db/password"><Input value={name} onChange={(e) => setName(e.target.value)} disabled={!!target?.name} className="font-mono" /></Field>
        <Field label="Type"><Select value={type} onChange={(e) => setType(e.target.value)} className="w-full"><option>String</option><option>StringList</option><option>SecureString</option></Select></Field>
        <Field label="Value"><Textarea rows={4} value={value} onChange={(e) => setValue(e.target.value)} /></Field>
      </DialogContent>
    </Dialog>
  );
}

// ── VPC (the networks ECS validates against) ──────────────────────────────
export function Networks() {
  const { data, error, refresh } = useData(async () => {
    const [s, g] = await Promise.all([query("ec2", "DescribeSubnets"), query("ec2", "DescribeSecurityGroups")]);
    const tags = (i: Element) => Array.from(i.querySelectorAll("tagSet > item")).map((t) => `${text(t, "key")}=${text(t, "value")}`).join(", ");
    return {
      subnets: Array.from(s.querySelectorAll("subnetSet > item")).map((i) => ({ id: text(i, "subnetId"), vpc: text(i, "vpcId"), cidr: text(i, "cidrBlock"), az: text(i, "availabilityZone"), tags: tags(i) })),
      groups: Array.from(g.querySelectorAll("securityGroupInfo > item")).map((i) => ({ id: text(i, "groupId"), name: text(i, "groupName"), vpc: text(i, "vpcId") })),
    };
  }, []);
  return (
    <>
      <Breadcrumbs items={[["VPC", "/vpc"], ["Subnets & security groups"]]} />
      <PageHeader title="Subnets & security groups" description="Declared in the emulator's config file (resources.ec2). awsvpc RunTask calls are validated against these subnets." />
      {error && <Alert>{error}</Alert>}
      <div className="space-y-5">
        <Container title="Subnets" counter={data?.subnets.length} flush actions={<RefreshButton onClick={refresh} />}>
          <DataTable rows={data?.subnets ?? []} rowKey={(s) => s.id} empty="No subnets declared — RunTask accepts any subnet id." columns={[
            { key: "i", header: "Subnet ID", cell: (s) => <span className="font-mono text-xs">{s.id}</span> },
            { key: "v", header: "VPC", cell: (s) => s.vpc }, { key: "c", header: "IPv4 CIDR", cell: (s) => s.cidr },
            { key: "a", header: "Availability Zone", cell: (s) => s.az }, { key: "t", header: "Tags", cell: (s) => <span className="text-xs">{s.tags}</span> },
          ]} />
        </Container>
        <Container title="Security groups" counter={data?.groups.length} flush>
          <DataTable rows={data?.groups ?? []} rowKey={(g) => g.id} empty="No security groups declared." columns={[
            { key: "i", header: "Security group ID", cell: (g) => <span className="font-mono text-xs">{g.id}</span> },
            { key: "n", header: "Name", cell: (g) => g.name }, { key: "v", header: "VPC", cell: (g) => g.vpc },
          ]} />
        </Container>
      </div>
    </>
  );
}

// ── API activity ──────────────────────────────────────────────────────────
export function Activity() {
  const { data, refresh } = useData(apiCalls, [], { poll: 2000 });
  const rows = React.useMemo(() => [...(data ?? [])].reverse(), [data]);
  const f = useFilter(rows, (c) => c.What);
  const [onlyErr, setOnlyErr] = React.useState(false);
  const shown = f.rows.filter((c) => !onlyErr || c.Status >= 400);
  return (
    <>
      <Breadcrumbs items={[["API activity"]]} />
      <PageHeader title="API activity" description="The last 1000 calls the emulator received, newest first — what your application (or this console) actually asked for." />
      <Container title="Calls" counter={shown.length} flush actions={<><label className="flex items-center gap-2 text-sm"><input type="checkbox" className="accent-link" checked={onlyErr} onChange={(e) => setOnlyErr(e.target.checked)} /> Errors only</label><RefreshButton onClick={refresh} /></>}>
        <div className="px-5"><TextFilter value={f.q} onChange={f.setQ} placeholder="Filter by operation (e.g. RunTask, s3 PUT)" count={shown.length} /></div>
        <DataTable rows={shown.slice(0, 500)} rowKey={(c) => `${c.At}-${c.What}-${Math.random()}`} columns={[
          { key: "t", header: "Time", cell: (c) => <span className="font-mono text-xs">{fmtTime(c.At, true)}</span> },
          { key: "w", header: "Operation", cell: (c) => <span className="font-mono text-xs break-all">{c.What}</span> },
          { key: "s", header: "Status", cell: (c) => <Badge tone={c.Status >= 500 ? "red" : c.Status >= 400 ? "orange" : "green"}>{c.Status}</Badge> },
          { key: "m", header: "Duration", cell: (c) => `${c.Ms} ms` },
        ]} />
      </Container>
    </>
  );
}
