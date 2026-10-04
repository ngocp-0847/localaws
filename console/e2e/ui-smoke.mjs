// Drives the console the way a QA engineer would, against a running localaws (runner noop is
// enough): create a bucket and upload, create a state machine and run it, register a task
// definition and run a task, create and reveal a parameter, send an event.
//
//   LOCALAWS_URL=http://127.0.0.1:4566 node e2e/ui-smoke.mjs [screenshot-dir]
//
// Uses the Chrome already installed (playwright-core, channel "chrome"): no browser download.
import { chromium } from "playwright-core";
import { mkdirSync } from "node:fs";

const base = (process.env.LOCALAWS_URL ?? "http://127.0.0.1:4566") + "/_localaws/";
const shots = process.argv[2] ?? "e2e-shots";
mkdirSync(shots, { recursive: true });
const id = Date.now().toString(36);
const step = async (label, fn) => {
  process.stdout.write(`… ${label}\r`);
  await fn();
  console.log(`ok  ${label}`);
};

const browser = await chromium.launch({ channel: "chrome", headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 950 } });
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));
const shot = (n) => page.screenshot({ path: `${shots}/${n}.png`, fullPage: true });
const go = (h) => page.goto(base + h);

try {
  await step("home loads and shows the account", async () => {
    await go("#/");
    await page.getByRole("heading", { level: 1, name: "Console Home" }).waitFor();
    await page.getByText("Recent executions").waitFor();
    await shot("01-home");
  });

  await step("service search jumps to a service", async () => {
    await page.fill("#service-search", "step");
    await page.keyboard.press("Enter");
    await page.getByRole("heading", { level: 1, name: "State machines" }).waitFor();
  });

  const bucket = `qa-${id}`;
  await step("S3: create a bucket", async () => {
    await go("#/s3");
    await page.getByRole("button", { name: "Create bucket" }).click();
    await page.getByPlaceholder("my-bucket").fill(bucket);
    await page.getByRole("dialog").getByRole("button", { name: "Create bucket" }).click();
    await page.getByText(`Bucket ${bucket} created`).waitFor();
    await page.getByRole("link", { name: bucket }).click();
    await page.getByRole("heading", { level: 1, name: bucket }).waitFor();
  });

  await step("S3: upload a file and open it", async () => {
    await page.getByRole("button", { name: "Upload" }).click();
    await page.getByPlaceholder("incoming/").fill("incoming/");
    await page.locator('input[type="file"]').setInputFiles({ name: "report.csv", mimeType: "text/csv", buffer: Buffer.from("id,amount\n1,42\n") });
    await page.getByRole("dialog").getByRole("button", { name: /^Upload 1 file/ }).click();
    await page.getByText(/✓ uploaded/).waitFor();
    await page.getByRole("button", { name: "Close" }).first().click();
    await page.getByRole("link", { name: "incoming/" }).click();
    await page.getByRole("link", { name: "report.csv" }).click();
    await page.getByText("Object overview").waitFor();
    await page.getByRole("button", { name: "Preview" }).click();
    await page.getByText("1,42").waitFor();
    await shot("02-s3-object");
  });

  const sm = `hello-${id}`;
  await step("Step Functions: create a state machine (template + live graph)", async () => {
    await go("#/states");
    await page.getByRole("button", { name: "Create state machine" }).click();
    await page.getByPlaceholder("MyStateMachine").fill(sm);
    await page.getByText("Valid JSON").waitFor();
    await shot("03-sfn-create");
    await page.getByRole("dialog").getByRole("button", { name: "Create", exact: true }).click();
    await page.getByRole("heading", { level: 1, name: sm }).waitFor();
  });

  await step("Step Functions: start an execution and watch it succeed", async () => {
    await page.getByRole("button", { name: "Start execution" }).first().click();
    await page.getByRole("dialog").getByRole("button", { name: "Start execution" }).click();
    await page.getByText("Execution details").waitFor();
    await page.getByText("Succeeded", { exact: true }).first().waitFor({ timeout: 30000 });
    await page.getByRole("tab", { name: "Table view" }).click();
    await page.getByRole("cell", { name: "Wait" }).first().waitFor();
    await page.getByRole("tab", { name: "Graph view" }).click();
    await shot("04-sfn-execution");
  });

  await step("Step Functions: a failing input fails at the Fail state", async () => {
    await page.getByRole("button", { name: "New execution" }).click();
    const editor = page.getByRole("dialog").locator("textarea");
    await editor.fill('{ "name": "moon" }');
    await page.getByRole("dialog").getByRole("button", { name: "Start execution" }).click();
    await page.getByText("NotWorld").first().waitFor({ timeout: 30000 });
    await page.getByRole("heading", { level: 2, name: "Fail" }).waitFor(); // the failed state is selected in the panel
    await shot("05-sfn-failed");
  });

  await step("ECS: register a task definition from the template", async () => {
    await go("#/ecs/taskdefs");
    await page.getByRole("button", { name: "Create new task definition" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Create", exact: true }).click();
    await page.getByText(/Registered hello:\d+/).waitFor();
  });

  await step("ECS: run a task with a command override and read its logs", async () => {
    await page.getByRole("button", { name: "Run task" }).click();
    await page.getByRole("dialog").getByRole("textbox").first().waitFor();
    await page.getByRole("dialog").locator("input.font-mono").fill('["sh","-c","echo qa run"]');
    await page.getByRole("dialog").getByRole("button", { name: "Run task" }).click();
    await page.getByText("Task overview").waitFor();
    await page.getByText("Essential container in task exited").waitFor({ timeout: 30000 });
    await page.getByText(/localaws noop runner/).first().waitFor({ timeout: 15000 });
    await shot("06-ecs-task");
  });

  await step("Parameter Store: create a SecureString and reveal it", async () => {
    await go("#/ssm");
    await page.getByRole("button", { name: "Create parameter" }).click();
    const d = page.getByRole("dialog");
    await d.locator("input").first().fill(`/qa/${id}/secret`);
    await d.locator("select").selectOption("SecureString");
    await d.locator("textarea").fill("s3cr3t-value");
    await d.getByRole("button", { name: "Create parameter" }).click();
    await page.getByText(`/qa/${id}/secret saved`).waitFor();
    const row = page.getByRole("row", { name: new RegExp(`/qa/${id}/secret`) });
    await row.locator("button").first().click();
    await row.getByText("s3cr3t-value").waitFor();
    await shot("07-ssm");
  });

  await step("EventBridge: which rules match, then send an event", async () => {
    await go("#/events/send");
    await page.getByRole("button", { name: "Which rules match?" }).click();
    await page.getByText("No rule matches this event.").waitFor();
    await page.getByRole("button", { name: "Send" }).click();
    await page.getByText("Event sent").waitFor();
    await shot("08-events");
  });

  await step("Logs, API activity and dark mode render", async () => {
    await go("#/logs");
    await page.getByRole("link", { name: "/ecs/hello" }).click();
    await page.getByText("Log streams").waitFor();
    await go("#/activity");
    await page.getByText(/RunTask/).first().waitFor();
    await page.getByTitle("Light / dark").click();
    await go("#/ecs");
    await shot("09-dark");
    await page.getByTitle("Light / dark").click();
  });

  if (errors.length) throw new Error("page errors: " + errors.join(" | "));
  console.log("all UI flows passed");
} catch (e) {
  await shot("zz-failure").catch(() => {});
  console.error("FAILED:", e.message);
  process.exitCode = 1;
} finally {
  await browser.close();
}
