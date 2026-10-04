import type { J } from "@/lib/aws";
import type { NodeStatus } from "@/components/graph";
import { toDate } from "@/lib/utils";

// Execution history → one record per state (what the console's Table view shows). Events are
// attributed to a state by walking previousEventId back to its StateEntered event.

export type StateRun = {
  name: string;
  type: string;
  status: NodeStatus;
  entered?: Date;
  exited?: Date;
  input?: string;
  output?: string;
  resource?: string;
  parameters?: string;
  error?: string;
  cause?: string;
  attempts: number;
  taskArns: string[];
  events: J[];
};

export function stateRuns(events: J[], execStatus?: string): StateRun[] {
  const byId = new Map<number, J>(events.map((e) => [e.id, e]));
  const owner = (e: J): string | undefined => {
    let cur: J | undefined = e;
    for (let i = 0; cur && i < 10000; i++) {
      if (cur.stateEnteredEventDetails) return cur.stateEnteredEventDetails.name;
      if (cur.stateExitedEventDetails) return cur.stateExitedEventDetails.name;
      cur = byId.get(cur.previousEventId);
    }
    return undefined;
  };
  const runs = new Map<string, StateRun>();
  const get = (n: string) => {
    let r = runs.get(n);
    if (!r) runs.set(n, (r = { name: n, type: "", status: "RUNNING", attempts: 0, taskArns: [], events: [] }));
    return r;
  };
  for (const e of events) {
    const t: string = e.type;
    if (e.stateEnteredEventDetails) {
      const r = get(e.stateEnteredEventDetails.name);
      r.type = t.replace("StateEntered", "");
      r.status = "RUNNING";
      r.entered = toDate(e.timestamp) ?? undefined;
      r.input = e.stateEnteredEventDetails.input;
      r.events.push(e);
      continue;
    }
    if (e.stateExitedEventDetails) {
      const r = get(e.stateExitedEventDetails.name);
      r.status = r.error ? "CAUGHT" : "SUCCEEDED";
      r.exited = toDate(e.timestamp) ?? undefined;
      r.output = e.stateExitedEventDetails.output;
      r.events.push(e);
      continue;
    }
    const n = owner(e);
    if (!n) continue;
    const r = get(n);
    r.events.push(e);
    const d = e.taskScheduledEventDetails ?? e.taskSubmittedEventDetails ?? e.taskSucceededEventDetails ?? e.taskFailedEventDetails ?? e.taskTimedOutEventDetails;
    if (t === "TaskScheduled") {
      r.attempts++;
      r.resource = `${d.resourceType}:${d.resource}`;
      r.parameters = d.parameters;
    }
    if ((t === "TaskSubmitted" || t === "TaskSucceeded") && d?.output) {
      try {
        const o = JSON.parse(d.output);
        const arn = o.TaskArn ?? o.Tasks?.[0]?.TaskArn ?? o.ExecutionArn;
        if (arn && !r.taskArns.includes(arn)) r.taskArns.push(arn);
      } catch { /* not JSON */ }
    }
    if (t === "TaskFailed" || t === "TaskTimedOut") {
      r.error = d.error;
      r.cause = d.cause;
      r.status = "FAILED";
    }
    if (t === "TaskSucceeded") { r.error = undefined; r.cause = undefined; }
  }
  const out = [...runs.values()];
  // the execution ended while a state was still open: that state is where it stopped
  if (execStatus && execStatus !== "RUNNING" && execStatus !== "SUCCEEDED") {
    const end = events.find((e) => e.executionFailedEventDetails || e.executionAbortedEventDetails || e.executionTimedOutEventDetails);
    const det = end?.executionFailedEventDetails ?? end?.executionAbortedEventDetails ?? end?.executionTimedOutEventDetails;
    for (const r of out) {
      if (r.status === "RUNNING" || (r.status === "FAILED" && !r.exited)) {
        r.status = execStatus === "ABORTED" ? "ABORTED" : "FAILED";
        r.error ??= det?.error;
        r.cause ??= det?.cause;
      }
    }
  }
  return out;
}

export function statusMap(runs: StateRun[]): Record<string, NodeStatus> {
  return Object.fromEntries(runs.map((r) => [r.name, r.status]));
}

/** the event's details object, whatever its type */
export function details(e: J): J {
  const k = Object.keys(e).find((x) => x.endsWith("EventDetails"));
  return k ? e[k] : {};
}
