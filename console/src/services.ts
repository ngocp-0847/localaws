import { Archive, Activity, Box, FileText, KeyRound, Network, Workflow, Zap } from "lucide-react";
import type { ComponentType } from "react";

export type ServiceDef = {
  id: string;
  name: string;
  full: string;
  blurb: string;
  keywords: string;
  path: string;
  icon: ComponentType<{ className?: string }>;
  nav: { label: string; path: string }[];
};

export const SERVICES: ServiceDef[] = [
  { id: "s3", name: "S3", full: "Amazon S3", blurb: "Buckets, objects, versions", keywords: "storage bucket object upload", path: "/s3", icon: Archive,
    nav: [{ label: "Buckets", path: "/s3" }] },
  { id: "states", name: "Step Functions", full: "Step Functions", blurb: "State machines and executions", keywords: "sfn workflow state machine execution asl", path: "/states", icon: Workflow,
    nav: [{ label: "State machines", path: "/states" }] },
  { id: "ecs", name: "ECS", full: "Amazon Elastic Container Service", blurb: "Clusters, tasks, task definitions", keywords: "container task fargate docker cluster service", path: "/ecs", icon: Box,
    nav: [{ label: "Clusters", path: "/ecs" }, { label: "Task definitions", path: "/ecs/taskdefs" }] },
  { id: "events", name: "EventBridge", full: "Amazon EventBridge", blurb: "Rules, targets, send events", keywords: "events rule pattern bus schedule cloudwatch events", path: "/events", icon: Zap,
    nav: [{ label: "Rules", path: "/events" }, { label: "Send events", path: "/events/send" }] },
  { id: "logs", name: "CloudWatch Logs", full: "CloudWatch Logs", blurb: "Log groups, streams, search", keywords: "cloudwatch log stream tail search", path: "/logs", icon: FileText,
    nav: [{ label: "Log groups", path: "/logs" }] },
  { id: "ssm", name: "Systems Manager", full: "Systems Manager", blurb: "Parameter Store", keywords: "ssm parameter store secret config", path: "/ssm", icon: KeyRound,
    nav: [{ label: "Parameter Store", path: "/ssm" }] },
  { id: "ec2", name: "VPC", full: "VPC (networks)", blurb: "Subnets and security groups", keywords: "ec2 vpc subnet security group network", path: "/vpc", icon: Network,
    nav: [{ label: "Subnets & security groups", path: "/vpc" }] },
  { id: "activity", name: "API activity", full: "API activity", blurb: "Every call the emulator received", keywords: "cloudtrail api calls requests audit", path: "/activity", icon: Activity,
    nav: [{ label: "Recent calls", path: "/activity" }] },
];

export function serviceFor(path: string): ServiceDef | undefined {
  return SERVICES.find((s) => path === s.path || path.startsWith(s.path + "/"));
}
