// The console speaks the real AWS wire protocols to the emulator — the same calls the SDKs
// make — so what QA does here is exactly what an application could do. No SDK in the bundle:
// JSON-protocol services are one fetch, S3 is REST + XML.

export class AwsError extends Error {
  constructor(public code: string, message: string, public status: number) {
    super(message || code);
  }
}

const TARGET = {
  states: "AWSStepFunctions",
  ecs: "AmazonEC2ContainerServiceV20141113",
  logs: "Logs_20140328",
  ssm: "AmazonSSM",
  events: "AWSEvents",
} as const;

export type Service = keyof typeof TARGET;
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type J = any;

export async function call<T = J>(service: Service, op: string, input: Record<string, unknown> = {}): Promise<T> {
  const r = await fetch("/", {
    method: "POST",
    headers: { "Content-Type": "application/x-amz-json-1.1", "X-Amz-Target": `${TARGET[service]}.${op}` },
    body: JSON.stringify(input),
  });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new AwsError(body.__type ?? `HTTP${r.status}`, body.message ?? body.Message ?? "", r.status);
  return body as T;
}

/** follow nextToken until done (list operations) */
export async function all<T = J>(service: Service, op: string, input: Record<string, unknown>, key: string, token = "nextToken"): Promise<T[]> {
  const out: T[] = [];
  let next: string | undefined;
  for (let i = 0; i < 50; i++) {
    const r = await call(service, op, { ...input, ...(next ? { [token]: next } : {}) });
    out.push(...((r[key] as T[]) ?? []));
    next = r[token === "NextToken" ? "NextToken" : "nextToken"];
    if (!next) break;
  }
  return out;
}

// ── Query protocol (STS, EC2) ──────────────────────────────────────────────
export async function query(service: "sts" | "ec2" | "sns", action: string, params: Record<string, string> = {}): Promise<Document> {
  const body = new URLSearchParams({ Action: action, Version: service === "ec2" ? "2016-11-15" : "2011-06-15", ...params });
  const r = await fetch("/", {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      Authorization: `AWS4-HMAC-SHA256 Credential=console/20260101/us-east-1/${service}/aws4_request`,
    },
    body,
  });
  const doc = new DOMParser().parseFromString(await r.text(), "application/xml");
  if (!r.ok) throw new AwsError(text(doc, "Code"), text(doc, "Message"), r.status);
  return doc;
}

// ── S3 (REST + XML) ───────────────────────────────────────────────────────
export function text(n: ParentNode, tag: string): string {
  return n.querySelector(tag)?.textContent ?? "";
}

function enc(key: string) {
  return key.split("/").map(encodeURIComponent).join("/");
}

async function s3(method: string, path: string, init: RequestInit = {}): Promise<Response> {
  const r = await fetch(path, { method, ...init });
  if (!r.ok && r.status !== 304) {
    const doc = new DOMParser().parseFromString(await r.text(), "application/xml");
    throw new AwsError(text(doc, "Code") || `HTTP${r.status}`, text(doc, "Message"), r.status);
  }
  return r;
}

async function xml(method: string, path: string, init: RequestInit = {}): Promise<Document> {
  const r = await s3(method, path, init);
  return new DOMParser().parseFromString(await r.text(), "application/xml");
}

export type Bucket = { name: string; created: string; versioning: string; eventBridge: boolean };
export type S3Object = { key: string; size: number; etag: string; modified: string; versionId?: string; isLatest?: boolean; deleteMarker?: boolean };

export const S3 = {
  async listBuckets(): Promise<Bucket[]> {
    const doc = await xml("GET", "/");
    const out: Bucket[] = [];
    for (const b of Array.from(doc.querySelectorAll("Bucket"))) {
      const name = text(b, "Name");
      const [v, n] = await Promise.all([xml("GET", `/${name}?versioning`), xml("GET", `/${name}?notification`)]);
      out.push({ name, created: text(b, "CreationDate"), versioning: text(v, "Status"), eventBridge: !!n.querySelector("EventBridgeConfiguration") });
    }
    return out;
  },
  createBucket: (name: string) => s3("PUT", `/${name}`),
  deleteBucket: (name: string) => s3("DELETE", `/${name}`),
  setVersioning: (name: string, on: boolean) =>
    s3("PUT", `/${name}?versioning`, { body: `<VersioningConfiguration><Status>${on ? "Enabled" : "Suspended"}</Status></VersioningConfiguration>` }),
  setEventBridge: (name: string, on: boolean) =>
    s3("PUT", `/${name}?notification`, { body: `<NotificationConfiguration>${on ? "<EventBridgeConfiguration/>" : ""}</NotificationConfiguration>` }),
  async list(bucket: string, prefix: string): Promise<{ folders: string[]; objects: S3Object[] }> {
    const folders: string[] = [];
    const objects: S3Object[] = [];
    let token = "";
    for (let i = 0; i < 20; i++) {
      const q = new URLSearchParams({ "list-type": "2", delimiter: "/", prefix });
      if (token) q.set("continuation-token", token);
      const doc = await xml("GET", `/${bucket}?${q}`);
      doc.querySelectorAll("CommonPrefixes > Prefix").forEach((p) => folders.push(p.textContent ?? ""));
      doc.querySelectorAll("Contents").forEach((c) =>
        objects.push({ key: text(c, "Key"), size: +text(c, "Size"), etag: text(c, "ETag").replace(/"/g, ""), modified: text(c, "LastModified") }),
      );
      token = text(doc, "NextContinuationToken");
      if (!token) break;
    }
    return { folders, objects };
  },
  async versions(bucket: string, prefix: string): Promise<S3Object[]> {
    const doc = await xml("GET", `/${bucket}?versions&prefix=${encodeURIComponent(prefix)}`);
    return Array.from(doc.querySelectorAll("Version, DeleteMarker")).map((v) => ({
      key: text(v, "Key"),
      versionId: text(v, "VersionId"),
      isLatest: text(v, "IsLatest") === "true",
      modified: text(v, "LastModified"),
      size: +text(v, "Size"),
      etag: text(v, "ETag").replace(/"/g, ""),
      deleteMarker: v.tagName === "DeleteMarker",
    }));
  },
  async head(bucket: string, key: string, versionId?: string) {
    const r = await s3("HEAD", `/${bucket}/${enc(key)}${versionId ? `?versionId=${encodeURIComponent(versionId)}` : ""}`);
    const meta: Record<string, string> = {};
    r.headers.forEach((v, k) => (meta[k] = v));
    return meta;
  },
  async getText(bucket: string, key: string, max = 256 * 1024): Promise<string> {
    const r = await s3("GET", `/${bucket}/${enc(key)}`, { headers: { Range: `bytes=0-${max - 1}` } });
    return r.text();
  },
  url: (bucket: string, key: string, versionId?: string) => `/${bucket}/${enc(key)}${versionId ? `?versionId=${encodeURIComponent(versionId)}` : ""}`,
  put: (bucket: string, key: string, body: Blob | string, contentType?: string) =>
    s3("PUT", `/${bucket}/${enc(key)}`, { body, headers: contentType ? { "Content-Type": contentType } : {} }),
  del: (bucket: string, key: string, versionId?: string) =>
    s3("DELETE", `/${bucket}/${enc(key)}${versionId ? `?versionId=${encodeURIComponent(versionId)}` : ""}`),
};

// ── emulator admin endpoints ──────────────────────────────────────────────
export type Info = {
  version: string;
  account: string;
  region: string;
  identity: string;
  runner: string;
  started: number;
  endpoint: string;
  dataDir: string;
  counts: Record<string, number>;
};

export async function info(): Promise<Info> {
  return (await fetch("/_localaws/api/info")).json();
}

export async function apiCalls(): Promise<{ At: number; What: string; Status: number; Ms: number }[]> {
  return (await fetch("/_localaws/api/calls")).json();
}

export async function reset(): Promise<void> {
  const r = await fetch("/_localaws/reset", { method: "POST" });
  if (!r.ok) throw new AwsError("ResetFailed", await r.text(), r.status);
}
