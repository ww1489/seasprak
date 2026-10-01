import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { LiveOpenAIProxy, loadOpenAI } from "./live-openai-proxy";

// Browser acceptance always uses the real OpenAI-compatible model configured
// in repository-root .test_env. A loopback proxy observes request counts and
// rechunks/pauses the real provider stream, but never generates or edits model
// output. Credentials are passed to cmd/web only through an environment
// variable and never enter the startup JSON, browser, screenshots or logs.

const here = fileURLToPath(new URL(".", import.meta.url));
const repoRoot = resolve(here, "..", "..");
const shots = resolve(here, "..", "test-results", "visual");
const XSS = `<img src=x onerror="window.__pwned=1"><script>window.__pwned=2</script>`;
const live = loadOpenAI(repoRoot);
const proxy = new LiveOpenAIProxy(live);

let proc: ChildProcess | undefined;
let base = "";
let token = "";
let tmp = "";
let workspace = "";
let stateRoot = "";

const declared = { Status: "declared", Evidence: ["maintainer-approved live browser test"] };
const ref = (node: string, field: string) => ({ ref: { node, field } });

function workflows() {
  const inputSchema = { type: "object", properties: { topic: { type: "string", description: "要原样返回的主题" } }, required: ["topic"], additionalProperties: false };
  return [
    {
      name: "echo-flow",
      version: "v1",
      description: "不调用模型，返回结构化输入",
      source: "trusted-live-e2e",
      formatVersion: "seasprak-workflow/v1",
      inputSchema,
      nodes: [
        { id: "start", type: "start" },
        { id: "end", type: "end", inputs: { answer: ref("start", "topic") } },
      ],
      edges: [{ from: "start", to: "end" }],
    },
    {
      name: "approved-todo-flow",
      version: "v1",
      description: "经审批写入会话 TODO",
      source: "trusted-live-e2e",
      formatVersion: "seasprak-workflow/v1",
      inputSchema,
      nodes: [
        { id: "start", type: "start" },
        {
          id: "todo",
          type: "tool",
          tool: "write_todos",
          inputs: { items: { literal: [{ id: "live-e2e", title: "真实浏览器审批", state: "completed" }] } },
        },
        { id: "end", type: "end", inputs: { result: ref("todo", "result") } },
      ],
      edges: [
        { from: "start", to: "todo" },
        { from: "todo", to: "end" },
      ],
    },
  ];
}

test.beforeAll(async () => {
  await proxy.start();
  tmp = mkdtempSync(join(tmpdir(), "seasprak-live-e2e-"));
  workspace = join(tmp, "workspace");
  stateRoot = join(tmp, "state");
  mkdirSync(workspace);
  const config = join(tmp, "config.json");
  writeFileSync(
    config,
    JSON.stringify({
      generationFingerprint: "live-e2e-v1",
      profile: "memory",
      tools: ["write_todos"],
      approvalTools: ["write_todos"],
      agents: [{ name: "reviewer", version: "v1", description: "真实模型审阅 Agent", instruction: "LIVE_REVIEWER_INSTRUCTION: Follow the user request precisely." }],
      workflows: workflows(),
      model: {
        Provider: "openai",
        Protocol: "openai-chat",
        Model: live.model,
        Endpoint: proxy.url,
        CredentialRef: "env:SEASPRAK_WEB_E2E_API_KEY",
        Version: "live-e2e-v1",
        AccountScope: "maintainer-live-e2e",
        NoCredentials: false,
        Parameters: { PolicyVersion: "live-e2e-v1", ConservativeContextWindow: 32768, MaxOutputTokens: 2048 },
        Capabilities: {
          ContextWindowTokens: 32768,
          MaxOutputTokens: 2048,
          Items: {
            text: declared,
            text_stream: declared,
            tools: declared,
            multiple_tools: declared,
            context_window: declared,
            output_limit: declared,
            physical_request_metering: declared,
          },
        },
      },
    }),
  );
  const bin = join(tmp, process.platform === "win32" ? "web.exe" : "web");
  const built = spawnSync("go", ["build", "-o", bin, "./cmd/web"], { cwd: repoRoot, stdio: "inherit" });
  if (built.status !== 0) throw new Error("go build ./cmd/web failed");
  proc = spawn(bin, ["--web", "--workspace", workspace, "--state-root", stateRoot, "--config", config, "--listen", "127.0.0.1:0"], {
    cwd: repoRoot,
    env: { ...process.env, SEASPRAK_WEB_E2E_API_KEY: live.key },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let out = "";
  await new Promise<void>((ok, fail) => {
    const timer = setTimeout(() => fail(new Error("cmd/web did not start")), 150_000);
    proc!.stdout!.on("data", (b: Buffer) => {
      out += b.toString("utf8");
      const url = /Listening: (http:\/\/127\.0\.0\.1:\d+)/.exec(out);
      const file = /Bearer file: (.+)\r?\n/.exec(out);
      if (url && file) {
        clearTimeout(timer);
        base = url[1];
        token = readFileSync(file[1].trim(), "utf8").trim();
        ok();
      }
    });
    proc!.on("exit", (code) => fail(new Error("cmd/web exited " + code)));
  });
});

test.afterAll(async () => {
  if (proc && proc.exitCode === null) {
    const exited = new Promise((resolveExit) => proc!.once("exit", resolveExit));
    proc.kill();
    await Promise.race([exited, new Promise((resolveWait) => setTimeout(resolveWait, 10_000))]);
  }
  await proxy.stop();
  console.log(`Live model requests: ${proxy.count()}; upstream responses: ${proxy.statuses.length}; successful responses: ${proxy.statuses.filter((status) => status >= 200 && status < 300).length}`);
  console.log(`Live upstream HTTP status codes: ${proxy.statuses.join(",")}`);
  if (tmp) rmSync(tmp, { recursive: true, force: true, maxRetries: 5 });
});

async function guard(page: Page) {
  const external: string[] = [];
  const errors: string[] = [];
  await page.route("**/*", (route) => {
    const u = new URL(route.request().url());
    if (u.protocol === "data:" || (u.hostname === "127.0.0.1" && u.origin === base)) return route.continue();
    external.push(u.origin);
    return route.abort();
  });
  page.on("pageerror", (error) => errors.push(error.message));
  return { external, errors };
}

async function login(page: Page) {
  await page.goto(base + "/");
  await page.getByRole("textbox", { name: "令牌", exact: true }).fill(token);
  await page.getByRole("button", { name: "连接" }).click();
  await expect(page.getByRole("heading", { name: "会话" })).toBeVisible();
}

async function newSession(page: Page): Promise<string> {
  const list = page.getByRole("list", { name: "会话列表" });
  const before = await list.getByRole("button").count();
  await page.getByLabel("工作区绝对路径").fill(workspace);
  await page.getByRole("button", { name: "新建会话" }).click();
  await expect(list.getByRole("button")).toHaveCount(before + 1);
  const sid = (await page.locator("main h2").first().textContent())!.trim();
  expect(sid).toMatch(/^[A-Za-z0-9_-]+$/);
  await expect(page.getByText("实时", { exact: true })).toBeVisible();
  return sid;
}

async function openSession(page: Page, sid: string) {
  await page.getByRole("list", { name: "会话列表" }).getByRole("button", { name: sid }).click();
  await expect(page.getByRole("heading", { name: sid })).toBeVisible();
}

async function prompt(page: Page, text: string) {
  await page.getByRole("textbox", { name: "输入消息" }).fill(text);
  await page.getByRole("button", { name: "发送" }).click();
}

const auth = () => ({ Authorization: "Bearer " + token });
type Trace = { traceId: string; state: string; settled: boolean; canResume: boolean };
type Snap = { revision: number; traces: Trace[] };

async function snapshot(request: APIRequestContext, sid: string): Promise<Snap> {
  const response = await request.get(`${base}/v1/sessions/${sid}/snapshot`, { headers: auth() });
  expect(response.status()).toBe(200);
  return (await response.json()) as Snap;
}

async function idle(request: APIRequestContext, sid: string) {
  await expect.poll(async () => (await snapshot(request, sid)).traces.every((trace) => ["completed", "failed", "cancelled"].includes(trace.state)), { timeout: 180_000 }).toBe(true);
}

function marker(name: string) {
  return `LIVE_${name}_${Date.now()}_${Math.random().toString(16).slice(2)}`;
}

const exactPrompt = (value: string, extra = "") => `只输出下面 ASCII 标记，不要添加引号、代码块或其他文字：\n${value}\n${extra}`;
const conversation = (page: Page) => page.getByRole("region", { name: "对话" });
const assistantText = (page: Page) => conversation(page).locator("p.text-\\[13px\\]");

function journalRecordCount(sid: string, recordType: string): number {
  const lines = readFileSync(join(stateRoot, "sessions", sid, "journal.jsonl"), "utf8").trim().split(/\r?\n/).slice(1);
  let count = 0;
  for (const line of lines) {
    const commit = JSON.parse(line) as { controlRecords?: { type?: string }[] };
    count += (commit.controlRecords ?? []).filter((record) => record.type === recordType).length;
  }
  return count;
}

test("live page: authentication, session creation and registered capabilities", async ({ page }) => {
  const g = await guard(page);
  const response = await page.goto(base + "/");
  expect(response?.status()).toBe(200);
  expect(await response!.text()).not.toContain(token);
  expect((await page.request.get(base + "/v1/sessions")).status()).toBe(401);
  const input = page.getByRole("textbox", { name: "令牌", exact: true });
  await input.fill("wrong-token");
  await page.getByRole("button", { name: "连接" }).click();
  await expect(page.getByRole("alert")).toContainText("令牌无效");
  await input.fill(token);
  await page.getByRole("button", { name: "连接" }).click();
  await expect(page.getByRole("heading", { name: "会话" })).toBeVisible();
  expect(await page.evaluate(() => localStorage.length + sessionStorage.length)).toBe(0);
  expect(page.url()).not.toContain(token);
  await newSession(page);
  const caps = page.getByRole("region", { name: "能力" });
  await expect(caps).toContainText("reviewer");
  await expect(caps).toContainText("echo-flow");
  await expect(caps).toContainText("write_todos");
  expect(g.external).toEqual([]);
  expect(g.errors).toEqual([]);
});

test("static routes keep MIME, traversal and API authentication boundaries", async ({ request }) => {
  const index = await request.get(base + "/index.html");
  expect(index.status()).toBe(200);
  const script = /src="(\/assets\/[^"]+\.js)"/.exec(await index.text());
  expect(script).not.toBeNull();
  const asset = await request.get(base + script![1]);
  expect(asset.status()).toBe(200);
  expect(asset.headers()["content-type"]).toContain("text/javascript");
  for (const path of ["/assets/..%2Fserver.go", "/assets/%2e%2e/server.go", "/static/index.html", "/server.go"]) {
    expect((await request.get(base + path)).status(), path).not.toBe(200);
  }
  expect((await request.post(base + "/")).status()).toBe(401);
});

test("real model streams an ASCII marker through UTF-8-rechunked transport exactly once", async ({ page }) => {
  const g = await guard(page);
  await login(page);
  const sid = await newSession(page);
  const value = marker("STREAM");
  const before = proxy.count();
  await prompt(page, exactPrompt(value, "标记前后各写一个中文字符和一个 emoji。"));
  await expect(assistantText(page).getByText(value, { exact: false })).toHaveCount(1, { timeout: 180_000 });
  await idle(page.request, sid);
  expect(proxy.count() - before).toBe(1);
  expect(g.external).toEqual([]);
  expect(g.errors).toEqual([]);
});

test("reload during a real provider stream reconnects without another model request", async ({ page }) => {
  await guard(page);
  await login(page);
  const sid = await newSession(page);
  const value = marker("STREAM_RELOAD");
  const hold = proxy.gateNextContentFrame();
  const before = proxy.count();
  await prompt(page, exactPrompt(value));
  await Promise.race([
    hold.reached,
    new Promise<never>((_, reject) => setTimeout(() => reject(new Error(`live stream gate not reached: ${JSON.stringify(proxy.shapes.at(-1) ?? { requests: proxy.count() - before })}`)), 30_000)),
  ]);
  await expect(conversation(page).getByText("生成中", { exact: true })).toBeVisible({ timeout: 30_000 });
  await page.reload();
  hold.open();
  await login(page);
  await openSession(page, sid);
  await idle(page.request, sid);
  await expect(conversation(page).getByText("生成中", { exact: true })).toHaveCount(0);
  await expect(assistantText(page).getByText(value, { exact: false })).toHaveCount(1);
  expect(proxy.count() - before).toBe(1);
});

test("same idempotency key creates one trace and one real model request", async ({ page }) => {
  await guard(page);
  await login(page);
  const sid = await newSession(page);
  const value = marker("IDEMPOTENT");
  const beforeRequests = proxy.count();
  const beforeTraces = (await snapshot(page.request, sid)).traces.length;
  const send = () =>
    page.request.post(`${base}/v1/sessions/${sid}/inputs`, {
      headers: { ...auth(), "Idempotency-Key": "live-e2e-retry-key", "Content-Type": "application/json" },
      data: { kind: "prompt", content: [{ type: "text", text: exactPrompt(value) }] },
    });
  const first = await send();
  const second = await send();
  expect(first.status()).toBe(202);
  expect(second.status()).toBe(202);
  expect((await second.json()).traceId).toBe((await first.json()).traceId);
  await idle(page.request, sid);
  await expect(assistantText(page).getByText(value, { exact: false })).toHaveCount(1, { timeout: 180_000 });
  expect((await snapshot(page.request, sid)).traces.length - beforeTraces).toBe(1);
  expect(proxy.count() - beforeRequests).toBe(1);
});

test("switching sessions discards late frames from a real provider stream", async ({ page }) => {
  await guard(page);
  await login(page);
  const firstSession = await newSession(page);
  const value = marker("OLD_SESSION");
  const hold = proxy.gateNextContentFrame();
  const before = proxy.count();
  await prompt(page, exactPrompt(value));
  await Promise.race([
    hold.reached,
    new Promise<never>((_, reject) => setTimeout(() => reject(new Error(`live stream gate not reached: ${JSON.stringify(proxy.shapes.at(-1) ?? { requests: proxy.count() - before })}`)), 30_000)),
  ]);
  await expect(conversation(page).getByText("生成中", { exact: true })).toBeVisible({ timeout: 30_000 });
  const secondSession = await newSession(page);
  expect(secondSession).not.toBe(firstSession);
  hold.open();
  await idle(page.request, firstSession);
  await page.waitForTimeout(500);
  await expect(conversation(page).getByText("生成中", { exact: true })).toHaveCount(0);
  await expect(assistantText(page).getByText(value, { exact: false })).toHaveCount(0);
  await openSession(page, firstSession);
  await expect(assistantText(page).getByText(value, { exact: false })).toHaveCount(1, { timeout: 30_000 });
  expect(proxy.count() - before).toBe(1);
});

test("selected registered Agent executes through the real model", async ({ page }) => {
  await guard(page);
  await login(page);
  const sid = await newSession(page);
  const value = marker("REVIEWER");
  await page.getByLabel("目标 Agent").selectOption("reviewer");
  const before = proxy.count();
  await prompt(page, exactPrompt(value));
  await expect(assistantText(page).getByText(value, { exact: false })).toBeVisible({ timeout: 180_000 });
  await idle(page.request, sid);
  expect(proxy.count() - before).toBe(1);
  const request = proxy.requests[proxy.requests.length - 1];
  expect(JSON.stringify(request.body).includes("LIVE_REVIEWER_INSTRUCTION")).toBe(true);
});

test("real model markup output remains inert React text", async ({ page }) => {
  const g = await guard(page);
  await login(page);
  const sid = await newSession(page);
  const value = marker("XSS");
  await prompt(page, `原样输出下面两行，不要使用代码块：\n${value}\n${XSS}`);
  await expect(assistantText(page).getByText(value, { exact: false })).toBeVisible({ timeout: 180_000 });
  await idle(page.request, sid);
  expect(await page.locator("main img, main script").count()).toBe(0);
  expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
  expect(g.external).toEqual([]);
  expect(g.errors).toEqual([]);
});

test("uploaded attachment reaches the real model request without entering browser storage", async ({ page }) => {
  await guard(page);
  await login(page);
  const sid = await newSession(page);
  const value = marker("ATTACHMENT");
  await page.getByLabel("添加附件").setInputFiles({ name: "说明.txt", mimeType: "text/plain", buffer: Buffer.from(`附件正文：${value}`, "utf8") });
  await expect(page.getByRole("list", { name: "待发送附件" })).toContainText("说明.txt");
  const before = proxy.count();
  await prompt(page, `阅读附件，并在回答中包含标记 ${value}`);
  await idle(page.request, sid);
  await expect(assistantText(page).getByText(value, { exact: false })).toBeVisible({ timeout: 180_000 });
  expect(proxy.count() - before).toBe(1);
  expect(proxy.requests[proxy.requests.length - 1].text).toContain(value);
  expect(await page.evaluate(() => localStorage.length + sessionStorage.length)).toBe(0);
});

test("trusted workflow form executes without routing through the main model", async ({ page }) => {
  await guard(page);
  await login(page);
  const sid = await newSession(page);
  const value = marker("WORKFLOW");
  await page.getByLabel("目标 Agent").selectOption("echo-flow");
  await page.getByLabel(/topic/).fill(value);
  const before = proxy.count();
  await page.getByRole("button", { name: "启动工作流" }).click();
  await expect(assistantText(page).getByText(value, { exact: false })).toBeVisible({ timeout: 60_000 });
  await idle(page.request, sid);
  expect(proxy.count() - before).toBe(0);
});

test("workflow tool approval requires explicit response and resume, then commits once", async ({ page }) => {
  await guard(page);
  await login(page);
  const sid = await newSession(page);
  await page.getByLabel("目标 Agent").selectOption("approved-todo-flow");
  await page.getByLabel(/topic/).fill(marker("APPROVAL"));
  const beforeRequests = proxy.count();
  await page.getByRole("button", { name: "启动工作流" }).click();
  const card = page.getByRole("region", { name: "待审批操作" });
  await expect(card).toBeVisible({ timeout: 60_000 });
  expect(journalRecordCount(sid, "todo_update")).toBe(0);
  await card.getByRole("button", { name: "批准一次" }).click();
  await expect(page.getByRole("button", { name: "恢复" })).toBeVisible({ timeout: 60_000 });
  await page.getByRole("button", { name: "恢复" }).click();
  await idle(page.request, sid);
  await expect(card).toHaveCount(0, { timeout: 60_000 });
  expect(journalRecordCount(sid, "todo_update")).toBe(1);
  expect(proxy.count() - beforeRequests).toBe(0);
});

test("branch fork and navigation re-render history created with the real model", async ({ page }) => {
  await guard(page);
  await login(page);
  const sid = await newSession(page);
  const first = marker("BRANCH_FIRST");
  const second = marker("BRANCH_SECOND");
  await prompt(page, exactPrompt(first));
  await idle(page.request, sid);
  await prompt(page, exactPrompt(second));
  await idle(page.request, sid);
  await page.getByText("分支与压缩").click();
  await page.getByLabel("新分支名").fill("alt-live");
  const from = page.getByLabel("分叉起点消息");
  const firstID = await from.locator("option").nth(1).getAttribute("value");
  await from.selectOption(firstID!);
  await page.getByRole("button", { name: "创建分支" }).click();
  await expect(conversation(page).getByText(exactPrompt(second), { exact: true })).toHaveCount(0);
  const branches = page.getByRole("list", { name: "分支列表" });
  await expect(branches).toContainText("alt-live");
  await branches.getByRole("listitem").filter({ hasText: "main" }).getByRole("button", { name: "切换" }).click();
  await expect(conversation(page).getByText(exactPrompt(second), { exact: true })).toBeVisible();
});

test("branch fork with summary makes one metered real request and preserves the main history", async ({ page }) => {
  const g = await guard(page);
  await login(page);
  const sid = await newSession(page);
  const first = marker("SUMMARY_SHARED");
  const second = marker("SUMMARY_ABANDONED");
  await prompt(page, exactPrompt(first));
  await idle(page.request, sid);
  await expect(assistantText(page).filter({ hasText: first })).toBeVisible();
  await prompt(page, exactPrompt(second));
  await idle(page.request, sid);
  await expect(assistantText(page).filter({ hasText: second })).toBeVisible();
  const before = proxy.count();
  await page.getByText("分支与压缩").click();
  await page.getByLabel("新分支名").fill("summary-live");
  const from = page.getByLabel("分叉起点消息");
  // Keep both messages of the shared first round; only round two is abandoned.
  const firstAssistantID = await from.locator("option").nth(2).getAttribute("value");
  await from.selectOption(firstAssistantID!);
  await page.getByLabel("为离开的分支生成摘要").check();
  await page.getByRole("button", { name: "创建分支" }).click();
  const branches = page.getByRole("list", { name: "分支列表" });
  await expect(branches).toContainText("summary-live", { timeout: 180_000 });
  await expect(conversation(page).getByText("摘要", { exact: true })).toBeVisible();
  await expect(conversation(page).getByText(exactPrompt(second), { exact: true })).toHaveCount(0);
  expect(proxy.count() - before).toBe(1);
  expect(proxy.statuses.slice(before)).toEqual([200]);
  const summaryRequest = proxy.requests[before];
  expect(summaryRequest.stream).toBe(false);
  expect(summaryRequest.text.includes(second)).toBe(true);
  expect(summaryRequest.text.includes(first)).toBe(false);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await branches.getByRole("listitem").filter({ hasText: "main" }).getByRole("button", { name: "切换" }).click();
  await expect(conversation(page).getByText(exactPrompt(second), { exact: true })).toBeVisible();
  await expect(conversation(page).getByText("摘要", { exact: true })).toHaveCount(0);
  expect(proxy.count() - before).toBe(1);
  expect(g.external).toEqual([]);
  expect(g.errors).toEqual([]);
});

test("manual compaction calls the real model and activates its validated summary", async ({ page }) => {
  await guard(page);
  await login(page);
  const sid = await newSession(page);
  for (let i = 1; i <= 3; i++) {
    await prompt(page, `用一句话确认收到第 ${i} 条压缩测试消息。`);
    await idle(page.request, sid);
  }
  const before = proxy.count();
  await page.getByText("分支与压缩").click();
  await page.getByRole("button", { name: "手动压缩上下文" }).click();
  await expect(conversation(page).getByText("摘要", { exact: true })).toBeVisible({ timeout: 180_000 });
  expect(proxy.count() - before).toBe(1);
  expect(proxy.requests.slice(before).some((request) => request.text.includes("<history>"))).toBe(true);
  await expect(page.getByRole("alert")).toHaveCount(0);
});

for (const [width, height] of [
  [1280, 800],
  [390, 844],
] as const) {
  for (const scheme of ["light", "dark"] as const) {
    test(`live visual ${width}x${height} ${scheme}: no overflow and keyboard reaches composer`, async ({ page }) => {
      const g = await guard(page);
      await page.setViewportSize({ width, height });
      await page.emulateMedia({ colorScheme: scheme });
      await login(page);
      const sid = await newSession(page);
      const value = marker("VISUAL");
      await prompt(page, exactPrompt(value));
      await idle(page.request, sid);
      await expect(assistantText(page).getByText(value, { exact: false })).toBeVisible({ timeout: 180_000 });
      await page.getByRole("textbox", { name: "输入消息" }).fill("待发送");
      expect(await page.evaluate(() => document.scrollingElement!.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(0);
      await page.screenshot({ path: join(shots, `live-session-${width}x${height}-${scheme}.png`), fullPage: true });
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      const seen: string[] = [];
      for (let i = 0; i < 60 && !seen.includes("发送"); i++) {
        await page.keyboard.press("Tab");
        seen.push(await page.evaluate(() => document.activeElement?.getAttribute("aria-label") ?? ""));
      }
      const input = seen.indexOf("输入消息");
      expect(input).toBeGreaterThanOrEqual(0);
      expect(seen.indexOf("发送")).toBeGreaterThan(input);
      expect(g.external).toEqual([]);
      expect(g.errors).toEqual([]);
    });
  }
}
