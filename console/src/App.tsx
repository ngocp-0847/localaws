import * as React from "react";
import { useStore } from "@/store";
import { match, useRoute } from "@/router";
import { serviceFor } from "@/services";
import { Flashbar, SideNav, TopBar } from "@/components/layout";
import { Alert } from "@/components/ui/data";
import Home from "@/pages/home";
import { BucketPage, Buckets, ObjectPage } from "@/pages/s3";
import { ExecutionPage, StateMachinePage, StateMachines } from "@/pages/states";
import { ClusterPage, Clusters, TaskDefPage, TaskDefs, TaskPage } from "@/pages/ecs";
import { LogGroupPage, LogGroups, LogStreamPage } from "@/pages/logs";
import { RulePage, Rules, SendEvents } from "@/pages/events";
import { Activity, Networks, Parameters } from "@/pages/misc";

function Page() {
  const { path, query } = useRoute();
  const q = (k: string) => query.get(k) ?? "";
  let m: Record<string, string> | null;
  if (path === "/") return <Home />;
  if (path === "/s3") return <Buckets />;
  if ((m = match("/s3/:bucket/object", path))) return <ObjectPage key={m.bucket + q("key") + q("version")} bucket={m.bucket} objKey={q("key")} version={q("version") || undefined} />;
  if ((m = match("/s3/:bucket", path))) return <BucketPage bucket={m.bucket} prefix={q("prefix")} />;
  if (path === "/states") return <StateMachines />;
  if (path === "/states/execution") return <ExecutionPage key={q("arn")} arn={q("arn")} />;
  if ((m = match("/states/sm/:name", path))) return <StateMachinePage key={m.name} name={m.name} />;
  if (path === "/ecs") return <Clusters />;
  if (path === "/ecs/taskdefs") return <TaskDefs />;
  if (path === "/ecs/task") return <TaskPage key={q("arn")} arn={q("arn")} />;
  if ((m = match("/ecs/cluster/:name", path))) return <ClusterPage cluster={m.name} />;
  if ((m = match("/ecs/taskdef/:family", path))) return <TaskDefPage family={m.family} rev={q("rev") || undefined} />;
  if (path === "/events") return <Rules />;
  if (path === "/events/send") return <SendEvents />;
  if ((m = match("/events/rule/:name", path))) return <RulePage name={m.name} />;
  if (path === "/logs") return <LogGroups />;
  if (path === "/logs/group") return <LogGroupPage name={q("name")} />;
  if (path === "/logs/stream") return <LogStreamPage key={q("group") + q("stream")} group={q("group")} stream={q("stream")} />;
  if (path === "/ssm") return <Parameters />;
  if (path === "/vpc") return <Networks />;
  if (path === "/activity") return <Activity />;
  return <Alert title="Page not found">No console page at <span className="font-mono">{path}</span>.</Alert>;
}

export default function App() {
  const { path } = useRoute();
  const dark = useStore((s) => s.dark);
  const info = useStore((s) => s.info);
  const loadInfo = useStore((s) => s.loadInfo);
  React.useEffect(() => { document.documentElement.classList.toggle("dark", dark); }, [dark]);
  React.useEffect(() => {
    loadInfo();
    const t = setInterval(loadInfo, 5000);
    const key = (e: KeyboardEvent) => { if (e.altKey && e.key.toLowerCase() === "s") { e.preventDefault(); document.getElementById("service-search")?.focus(); } };
    window.addEventListener("keydown", key);
    return () => { clearInterval(t); window.removeEventListener("keydown", key); };
  }, [loadInfo]);
  React.useEffect(() => { window.scrollTo(0, 0); }, [path]);
  const svc = serviceFor(path);
  return (
    <div className="min-h-full flex flex-col">
      <TopBar />
      <div className="flex flex-1">
        <SideNav service={svc} active={path} />
        <main className="flex-1 min-w-0 px-4 sm:px-8 py-5 max-w-[1600px]">
          {!info && <div className="mb-4"><Alert kind="warning" title="Not connected">The console cannot reach the emulator's API. Is localaws running?</Alert></div>}
          <Flashbar />
          <Page />
        </main>
      </div>
    </div>
  );
}
