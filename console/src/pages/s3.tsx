import * as React from "react";
import { Download, Folder, File as FileIcon, Upload, Trash2, FolderPlus } from "lucide-react";
import { S3, type S3Object } from "@/lib/aws";
import { useData } from "@/lib/hooks";
import { act } from "@/store";
import { href, navigate } from "@/router";
import { Button } from "@/components/ui/button";
import { Field, Input, Switch } from "@/components/ui/form";
import { Dialog, DialogContent, Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/overlay";
import { Alert, Badge, Code, Container, Copyable, DataTable, KeyValue, Spinner } from "@/components/ui/data";
import { Breadcrumbs, PageHeader, RefreshButton } from "@/components/layout";
import { ConfirmDelete, TextFilter, useFilter } from "@/components/common";
import { fmtBytes, fmtTime } from "@/lib/utils";

export function Buckets() {
  const { data, error, loading, refresh } = useData(() => S3.listBuckets(), [], { poll: 15000 });
  const f = useFilter(data, (b) => b.name);
  const [sel, setSel] = React.useState<string[]>([]);
  const [create, setCreate] = React.useState(false);
  const [del, setDel] = React.useState(false);
  return (
    <>
      <Breadcrumbs items={[["Amazon S3", "/s3"], ["Buckets"]]} />
      <PageHeader title="Buckets" description="Containers for objects. Buckets with EventBridge notifications send Object Created / Deleted events — that is what starts the pipelines." />
      {error && <Alert title="Could not list buckets">{error}</Alert>}
      <Container title="General purpose buckets" counter={data?.length} flush
        actions={<>
          <RefreshButton onClick={refresh} />
          <Button disabled={sel.length !== 1} onClick={() => setDel(true)}>Delete</Button>
          <Button variant="primary" onClick={() => setCreate(true)}>Create bucket</Button>
        </>}>
        <div className="px-5"><TextFilter value={f.q} onChange={f.setQ} placeholder="Find buckets by name" count={f.rows.length} /></div>
        {loading && !data ? <Spinner /> : (
          <DataTable rows={f.rows} rowKey={(b) => b.name} selected={sel} onSelect={setSel} initialSort={{ key: "name" }}
            empty="No buckets. Create one, or declare it in the config file."
            columns={[
              { key: "name", header: "Name", sort: (b) => b.name, cell: (b) => <a href={href(`/s3/${b.name}`)} onClick={(e) => e.stopPropagation()} className="font-bold">{b.name}</a> },
              { key: "ver", header: "Versioning", sort: (b) => b.versioning, cell: (b) => b.versioning || "Disabled" },
              { key: "eb", header: "EventBridge", cell: (b) => (b.eventBridge ? <Badge tone="blue">On</Badge> : <span className="text-muted-foreground">Off</span>) },
              { key: "created", header: "Creation date", sort: (b) => b.created, cell: (b) => fmtTime(b.created) },
            ]} />
        )}
      </Container>
      <CreateBucket open={create} onOpenChange={setCreate} done={refresh} />
      <ConfirmDelete open={del} onOpenChange={setDel} what="bucket" name={sel[0] ?? ""}
        onConfirm={async () => { await act("Delete bucket", () => S3.deleteBucket(sel[0]), `Bucket ${sel[0]} deleted`); setSel([]); refresh(); }}>
        <Alert kind="warning">The bucket must be empty (every version and delete marker removed).</Alert>
      </ConfirmDelete>
    </>
  );
}

function CreateBucket({ open, onOpenChange, done }: { open: boolean; onOpenChange: (v: boolean) => void; done: () => void }) {
  const [name, setName] = React.useState("");
  const [ver, setVer] = React.useState(true);
  const [eb, setEb] = React.useState(true);
  const valid = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(name);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title="Create bucket"
        footer={<>
          <Button variant="link" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" disabled={!valid} onClick={async () => {
            const ok = await act("Create bucket", async () => {
              await S3.createBucket(name);
              if (ver) await S3.setVersioning(name, true);
              if (eb) await S3.setEventBridge(name, true);
            }, `Bucket ${name} created`);
            if (ok !== undefined) { onOpenChange(false); setName(""); done(); }
          }}>Create bucket</Button>
        </>}>
        <Field label="Bucket name" hint="3–63 characters: lowercase letters, numbers, dots, hyphens." error={name && !valid ? "Not a valid bucket name" : null}>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="my-bucket" autoFocus />
        </Field>
        <label className="flex items-center gap-3 text-sm"><Switch checked={ver} onCheckedChange={setVer} /> Bucket versioning</label>
        <label className="flex items-center gap-3 text-sm"><Switch checked={eb} onCheckedChange={setEb} /> Send notifications to Amazon EventBridge</label>
      </DialogContent>
    </Dialog>
  );
}

export function BucketPage({ bucket, prefix }: { bucket: string; prefix: string }) {
  const [versions, setVersions] = React.useState(false);
  const objs = useData(() => S3.list(bucket, prefix), [bucket, prefix], { poll: 10000 });
  const vers = useData(() => (versions ? S3.versions(bucket, prefix) : Promise.resolve([] as S3Object[])), [bucket, prefix, versions]);
  const meta = useData(() => S3.listBuckets().then((bs) => bs.find((b) => b.name === bucket)), [bucket]);
  const [sel, setSel] = React.useState<string[]>([]);
  const [upload, setUpload] = React.useState(false);
  const [folder, setFolder] = React.useState(false);
  const [del, setDel] = React.useState(false);
  const f = useFilter(objs.data?.objects, (o) => o.key);
  const parts = prefix.split("/").filter(Boolean);
  const crumbs: [string, string?][] = [["Amazon S3", "/s3"], ["Buckets", "/s3"], [bucket, `/s3/${bucket}`]];
  parts.forEach((p, i) => crumbs.push([p, `/s3/${bucket}?prefix=${encodeURIComponent(parts.slice(0, i + 1).join("/") + "/")}`]));
  type Row = { kind: "folder"; key: string } | ({ kind: "object" } & S3Object);
  const rows: Row[] = versions
    ? (vers.data ?? []).filter((o) => o.key.slice(prefix.length).indexOf("/") < 0).map((o) => ({ kind: "object" as const, ...o }))
    : [...(objs.data?.folders ?? []).map((k) => ({ kind: "folder" as const, key: k })), ...f.rows.map((o) => ({ kind: "object" as const, ...o }))];
  const rowKey = (r: Row) => (r.kind === "object" && r.versionId ? `${r.key}\u0000${r.versionId}` : r.key);
  const refresh = () => { objs.refresh(); vers.refresh(); };
  return (
    <>
      <Breadcrumbs items={crumbs.map(([l, t]) => [l, t?.split("?")[0] === t ? t : t])} />
      <PageHeader title={bucket} info={meta.data && <>{meta.data.versioning ? <Badge tone="blue">Versioning {meta.data.versioning}</Badge> : null} {meta.data.eventBridge && <Badge tone="orange">EventBridge</Badge>}</>} />
      <Tabs defaultValue="objects">
        <TabsList><TabsTrigger value="objects">Objects</TabsTrigger><TabsTrigger value="properties">Properties</TabsTrigger></TabsList>
        <TabsContent value="objects">
          <Container title="Objects" counter={(objs.data?.objects.length ?? 0) + (objs.data?.folders.length ?? 0)} flush
            description={prefix ? <>Prefix <span className="font-mono">{prefix}</span></> : "Objects are the fundamental entities stored in Amazon S3."}
            actions={<>
              <RefreshButton onClick={refresh} />
              <Button disabled={!sel.length} onClick={() => setDel(true)}><Trash2 /> Delete</Button>
              <Button onClick={() => setFolder(true)}><FolderPlus /> Create folder</Button>
              <Button variant="primary" onClick={() => setUpload(true)}><Upload /> Upload</Button>
            </>}>
            <div className="px-5 flex flex-wrap items-center justify-between gap-3">
              <TextFilter value={f.q} onChange={f.setQ} placeholder="Find objects by prefix" count={f.rows.length} />
              <label className="flex items-center gap-2 text-sm mb-3"><Switch checked={versions} onCheckedChange={(v) => { setVersions(v); setSel([]); }} /> Show versions</label>
            </div>
            {objs.loading && !objs.data ? <Spinner /> : (
              <DataTable<Row> rows={rows} rowKey={rowKey} selected={sel} onSelect={setSel} multi empty="No objects. Upload files, or drop them here."
                columns={[
                  { key: "name", header: "Name", cell: (r) => r.kind === "folder"
                    ? <a href={href(`/s3/${bucket}`, { prefix: r.key })} onClick={(e) => e.stopPropagation()} className="inline-flex items-center gap-1.5 font-bold"><Folder className="size-4 text-muted-foreground" />{r.key.slice(prefix.length)}</a>
                    : <a href={href(`/s3/${bucket}/object`, { key: r.key, version: r.versionId })} onClick={(e) => e.stopPropagation()} className="inline-flex items-center gap-1.5"><FileIcon className="size-4 text-muted-foreground" />{r.key.slice(prefix.length)}</a> },
                  ...(versions ? [{ key: "vid", header: "Version ID", cell: (r: Row) => r.kind === "object" ? <span className="font-mono text-xs">{r.versionId}{r.isLatest && <Badge tone="green" className="ml-2">latest</Badge>}{r.deleteMarker && <Badge className="ml-2">delete marker</Badge>}</span> : null }] : []),
                  { key: "type", header: "Type", cell: (r) => (r.kind === "folder" ? "Folder" : r.key.includes(".") ? r.key.split(".").pop() : "–") },
                  { key: "modified", header: "Last modified", cell: (r) => (r.kind === "object" ? fmtTime(r.modified) : "–") },
                  { key: "size", header: "Size", cell: (r) => (r.kind === "object" && !r.deleteMarker ? fmtBytes(r.size) : "–") },
                ]} />
            )}
          </Container>
        </TabsContent>
        <TabsContent value="properties">
          <BucketProperties bucket={bucket} meta={meta.data} reload={meta.refresh} />
        </TabsContent>
      </Tabs>
      <UploadDialog open={upload} onOpenChange={setUpload} bucket={bucket} prefix={prefix} done={refresh} />
      <Dialog open={folder} onOpenChange={setFolder}>
        <FolderDialog bucket={bucket} prefix={prefix} close={() => { setFolder(false); refresh(); }} />
      </Dialog>
      <ConfirmDelete open={del} onOpenChange={setDel} what={`${sel.length} object(s)`} name={`${sel.length} object(s)`} confirmWord="permanently delete"
        onConfirm={async () => {
          await act("Delete objects", async () => {
            for (const k of sel) {
              const [key, vid] = k.split("\u0000");
              if (key.endsWith("/") && !vid) continue;
              await S3.del(bucket, key, vid);
            }
          }, `${sel.length} object(s) deleted`);
          setSel([]);
          refresh();
        }}>
        <Alert kind="info">{versions ? "Selected versions are removed permanently." : meta.data?.versioning === "Enabled" ? "The bucket is versioned: deleting adds a delete marker; older versions stay (turn on “Show versions” to remove them)." : "Objects are removed permanently."}</Alert>
      </ConfirmDelete>
    </>
  );
}

function BucketProperties({ bucket, meta, reload }: { bucket: string; meta?: { versioning: string; eventBridge: boolean; created: string }; reload: () => void }) {
  if (!meta) return <Spinner />;
  return (
    <div className="space-y-5">
      <Container title="Bucket overview">
        <KeyValue items={[["Name", <Copyable value={bucket} />], ["ARN", <Copyable value={`arn:aws:s3:::${bucket}`} />], ["Creation date", fmtTime(meta.created)]]} />
      </Container>
      <Container title="Bucket versioning" description="Keep every version of every object; deletes add delete markers.">
        <label className="flex items-center gap-3 text-sm">
          <Switch checked={meta.versioning === "Enabled"} onCheckedChange={async (v) => { await act("Edit versioning", () => S3.setVersioning(bucket, v), `Versioning ${v ? "enabled" : "suspended"}`); reload(); }} />
          {meta.versioning || "Disabled"}
        </label>
      </Container>
      <Container title="Amazon EventBridge" description="Send Object Created / Object Deleted events for this bucket to EventBridge.">
        <label className="flex items-center gap-3 text-sm">
          <Switch checked={meta.eventBridge} onCheckedChange={async (v) => { await act("Edit event notifications", () => S3.setEventBridge(bucket, v), `EventBridge notifications ${v ? "on" : "off"}`); reload(); }} />
          {meta.eventBridge ? "On" : "Off"}
        </label>
      </Container>
    </div>
  );
}

function FolderDialog({ bucket, prefix, close }: { bucket: string; prefix: string; close: () => void }) {
  const [name, setName] = React.useState("");
  return (
    <DialogContent title="Create folder" description={<>In <span className="font-mono">s3://{bucket}/{prefix}</span></>}
      footer={<><Button variant="link" onClick={close}>Cancel</Button>
        <Button variant="primary" disabled={!name || name.includes("/")} onClick={async () => { await act("Create folder", () => S3.put(bucket, `${prefix}${name}/`, ""), `Folder ${name}/ created`); close(); }}>Create folder</Button></>}>
      <Field label="Folder name"><Input value={name} onChange={(e) => setName(e.target.value)} autoFocus /></Field>
    </DialogContent>
  );
}

function UploadDialog({ open, onOpenChange, bucket, prefix, done }: { open: boolean; onOpenChange: (v: boolean) => void; bucket: string; prefix: string; done: () => void }) {
  const [files, setFiles] = React.useState<File[]>([]);
  const [dest, setDest] = React.useState(prefix);
  const [progress, setProgress] = React.useState<Record<string, string>>({});
  const [drag, setDrag] = React.useState(false);
  React.useEffect(() => { if (open) { setFiles([]); setProgress({}); setDest(prefix); } }, [open, prefix]);
  const add = (l: FileList | null) => l && setFiles((f) => [...f, ...Array.from(l)]);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title="Upload" wide description="Each file is one PutObject — with EventBridge notifications on, each upload starts the matching rules."
        footer={<>
          <Button variant="link" onClick={() => onOpenChange(false)}>Close</Button>
          <Button variant="primary" disabled={!files.length} onClick={async () => {
            for (const f of files) {
              setProgress((p) => ({ ...p, [f.name]: "uploading…" }));
              const r = await act(`Upload ${f.name}`, () => S3.put(bucket, dest + f.name, f, f.type || undefined), "");
              setProgress((p) => ({ ...p, [f.name]: r ? "✓ uploaded" + (r.headers.get("x-amz-version-id") ? ` · version ${r.headers.get("x-amz-version-id")}` : "") : "✗ failed" }));
            }
            done();
          }}>Upload {files.length ? `${files.length} file(s)` : ""}</Button>
        </>}>
        <Field label="Destination prefix" hint={<>Objects go to <span className="font-mono">s3://{bucket}/{dest}&lt;file name&gt;</span></>}>
          <Input value={dest} onChange={(e) => setDest(e.target.value)} placeholder="incoming/" />
        </Field>
        <label onDragOver={(e) => { e.preventDefault(); setDrag(true); }} onDragLeave={() => setDrag(false)} onDrop={(e) => { e.preventDefault(); setDrag(false); add(e.dataTransfer.files); }}
          className={`flex flex-col items-center justify-center gap-2 rounded-xl border-2 border-dashed py-10 cursor-pointer ${drag ? "border-link bg-accent" : ""}`}>
          <Upload className="size-6 text-muted-foreground" />
          <span className="text-sm">Drag and drop files here, or <span className="text-link font-bold">choose files</span></span>
          <input type="file" multiple className="hidden" onChange={(e) => add(e.target.files)} />
        </label>
        {files.length > 0 && (
          <DataTable rows={files} rowKey={(f) => f.name} columns={[
            { key: "n", header: "Name", cell: (f) => f.name },
            { key: "s", header: "Size", cell: (f) => fmtBytes(f.size) },
            { key: "t", header: "Type", cell: (f) => f.type || "–" },
            { key: "p", header: "Status", cell: (f) => progress[f.name] ?? "pending" },
          ]} />
        )}
      </DialogContent>
    </Dialog>
  );
}

export function ObjectPage({ bucket, objKey, version }: { bucket: string; objKey: string; version?: string }) {
  const head = useData(() => S3.head(bucket, objKey, version), [bucket, objKey, version]);
  const vers = useData(() => S3.versions(bucket, objKey).then((v) => v.filter((x) => x.key === objKey)), [bucket, objKey]);
  const [preview, setPreview] = React.useState<string | null>(null);
  const dir = objKey.includes("/") ? objKey.slice(0, objKey.lastIndexOf("/") + 1) : "";
  const h = head.data ?? {};
  const user = Object.entries(h).filter(([k]) => k.startsWith("x-amz-meta-"));
  return (
    <>
      <Breadcrumbs items={[["Amazon S3", "/s3"], ["Buckets", "/s3"], [bucket, `/s3/${bucket}`], ...(dir ? [[dir, `/s3/${bucket}?prefix=${encodeURIComponent(dir)}`] as [string, string]] : []), [objKey.slice(dir.length)]]} />
      <PageHeader title={objKey.slice(dir.length)}
        actions={<>
          <Button asChild><a href={S3.url(bucket, objKey, version)} download><Download /> Download</a></Button>
          <Button onClick={async () => setPreview(await S3.getText(bucket, objKey))}>Preview</Button>
          <Button variant="normal" onClick={() => navigate(`/s3/${bucket}`, { prefix: dir })}>Back to folder</Button>
        </>} />
      {head.error && <Alert title="Object not found">{head.error}</Alert>}
      <div className="space-y-5">
        <Container title="Object overview">
          <KeyValue items={[
            ["Bucket", <a href={href(`/s3/${bucket}`)}>{bucket}</a>], ["Key", <Copyable value={objKey} />], ["S3 URI", <Copyable value={`s3://${bucket}/${objKey}`} />],
            ["Size", fmtBytes(+(h["content-length"] ?? 0))], ["Type", h["content-type"]], ["Last modified", fmtTime(h["last-modified"])],
            ["ETag", <Copyable value={(h["etag"] ?? "").replace(/"/g, "")} />], ["Version ID", <Copyable value={h["x-amz-version-id"] ?? "null"} />],
            ["Multipart parts", h["x-amz-mp-parts-count"] ?? "–"],
          ]} />
        </Container>
        {user.length > 0 && <Container title="Metadata"><KeyValue items={user.map(([k, v]) => [k.replace("x-amz-meta-", ""), v])} /></Container>}
        {preview !== null && <Container title="Preview" description="first 256 KB"><Code>{preview}</Code></Container>}
        <Container title="Versions" counter={vers.data?.length} flush>
          <DataTable rows={vers.data ?? []} rowKey={(v) => v.versionId ?? v.key} columns={[
            { key: "v", header: "Version ID", cell: (v) => <a href={href(`/s3/${bucket}/object`, { key: objKey, version: v.versionId })} className="font-mono text-xs">{v.versionId}</a> },
            { key: "l", header: "", cell: (v) => <>{v.isLatest && <Badge tone="green">latest</Badge>} {v.deleteMarker && <Badge>delete marker</Badge>}</> },
            { key: "m", header: "Last modified", cell: (v) => fmtTime(v.modified) },
            { key: "s", header: "Size", cell: (v) => (v.deleteMarker ? "–" : fmtBytes(v.size)) },
          ]} />
        </Container>
      </div>
    </>
  );
}
