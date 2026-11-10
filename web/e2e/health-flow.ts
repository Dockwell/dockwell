// SPDX-License-Identifier: AGPL-3.0-only

// What the panel says of what runs, against what really happens to it: the same steps
// on scripts/fakedocker in make e2e, and on this machine's Docker in make
// docker-panel-e2e. A person stops, pauses and kills containers from outside Dockwell,
// and an agent dies, and the list and the overview have to say so, from what the server
// reports. Nothing here reloads to see it: the panel reads it again on its own.

import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { Browser, BrowserContext, Page } from "playwright-core";

import { type DockerProbe, dockerAct, dockerExec, dockerHealth } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";

export interface HealthFlowOptions {
  origin: string;
  step: (what: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
  docker: DockerProbe;
  socket: string;
  /** A fake engine has no checks to run, so it's told how they went. A real one runs them. */
  fake: boolean;
  bin: string;
  data: string;
  env: NodeJS.ProcessEnv;
  browser: Browser;
  context: BrowserContext;
  /** Makes Docker stop answering, as a hung daemon does, and returns what makes it answer again. Only a fake can. */
  hangDocker?: () => () => void;
  /** Freezes the control plane with its connections open, and returns what thaws it. */
  freezeControlPlane?: () => () => void;
}

function fail(message: string): never {
  throw new Error(message);
}

/** The longest a change takes to reach the page: an agent's scan and the panel's own read, with room. */
const SEEN_WITHIN = 40_000;

const CHECKED = `name: checked
services:
  checked:
    image: nginx:1.27-alpine
    restart: unless-stopped
    healthcheck:
      test: ["CMD-SHELL", "test ! -f /tmp/sick"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 30s
`;

/** Reads the plan sheet, applies it, and leaves the sheet open on how it ended. */
async function apply(page: Page) {
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: /Apply plan/ }).click();
}

export async function healthFlow(page: Page, options: HealthFlowOptions) {
  const {
    origin,
    step,
    shoot,
    docker,
    socket,
    fake,
    bin,
    data,
    env,
    browser,
    context,
    hangDocker,
    freezeControlPlane,
  } = options;
  const cleanUp: (() => void)[] = [];
  const only = async (service: string) => {
    const all = (await docker()).filter((c) => c.service === service);
    return all[0] ?? fail(`Docker has no ${service}: ${JSON.stringify(await docker())}`);
  };
  const row = (name: string) => page.getByRole("row", { name: new RegExp(`\\b${name}\\b`) });
  /** The list says word of an app, in its status column, on its own. */
  const listSays = async (name: string, word: string) => {
    await row(name).locator(".apps-status").getByText(word, { exact: true }).waitFor({ timeout: SEEN_WITHIN });
  };
  /** The page says word of an app, in its head and in its panel, on its own. */
  const pageSays = async (word: string) => {
    const panel = page.getByRole("region", { name: "What runs" });
    await panel.locator(".app-runs-row").getByText(word, { exact: true }).waitFor({ timeout: SEEN_WITHIN });
    // A phone has no head of this kind, and every flow here is on a wide screen.
    await page.locator(".page-title-row").getByText(word, { exact: true }).waitFor();
  };
  const goToList = () => page.goto(`${origin}/apps`);
  const goToApp = async (name: string) => {
    await goToList();
    await page.getByRole("link", { name, exact: true }).first().click();
    await page.getByRole("heading", { level: 1, name, exact: true }).waitFor();
  };
  /** Says what the list and the page say of it, without either of them being told. */
  const everywhere = async (name: string, word: string, shot: string) => {
    await goToList();
    await listSays(name, word);
    await shoot(page, `health-${shot}-list`);
    await goToApp(name);
    await pageSays(word);
    await shoot(page, `health-${shot}`);
  };

  step("health: a first deploy says how it runs from what its server says, and only a passed check is healthy");
  await page.goto(`${origin}/apps/new?source=compose`);
  const form = page.getByRole("dialog", { name: "New app" });
  await form.getByLabel("Compose file").fill(CHECKED);
  await form.getByRole("region", { name: "What this file makes" }).getByText("This makes 1 app").waitFor();
  await form.getByRole("button", { name: /Review plan/ }).click();
  await apply(page);
  await page.getByRole("heading", { name: "checked is healthy" }).waitFor({ timeout: 90_000 });
  // The sheet's own: the list behind it may offer to show what needs a look first.
  await page
    .locator(".dw-sheet-layer")
    .getByRole("button", { name: /^Open checked|^Done|^Show/ })
    .first()
    .click();
  const checked = await only("checked");
  await everywhere("checked", "Healthy", "healthy");
  const config = page.getByRole("region", { name: "Health check" });
  await config.getByText("test ! -f /tmp/sick").waitFor();

  step("health: its check starts failing, and it reads unhealthy, with what the check said");
  if (fake) await dockerHealth(socket, checked.id, "unhealthy", "the database refuses connections");
  else await dockerExec(socket, checked.id, ["touch", "/tmp/sick"]);
  await pageSays("Unhealthy");
  await page.locator(".dw-callout").getByText("checked fails its health check").waitFor();
  // The check's own panel says so too, by how the server reads, as the head does.
  await page.getByRole("region", { name: "Health check" }).getByText("Failing", { exact: true }).waitFor();
  await shoot(page, "health-unhealthy");
  await goToList();
  await listSays("checked", "Unhealthy");
  await shoot(page, "health-unhealthy-list");

  step("health: it starts again, and reads starting until its check passes");
  if (fake) await dockerHealth(socket, checked.id, "starting", "");
  else await dockerAct(socket, checked.id, "restart");
  await listSays("checked", "Starting");
  await shoot(page, "health-starting-list");
  await goToApp("checked");
  await pageSays("Starting");
  await shoot(page, "health-starting");
  if (fake) await dockerHealth(socket, checked.id, "healthy", "ok");
  else await dockerExec(socket, checked.id, ["rm", "-f", "/tmp/sick"]);
  await pageSays("Healthy");

  step("health: stopped by hand, it reads as not running, and nothing says it runs");
  await dockerAct(socket, checked.id, "stop");
  await pageSays("Not running");
  await page.locator(".dw-callout").getByText("checked isn't running").waitFor();
  await shoot(page, "health-not-running");
  await goToList();
  await listSays("checked", "Not running");
  await shoot(page, "health-not-running-list");
  // What was deployed is on record as it was: it isn't the deploy that went wrong.
  if ((await row("checked").innerText()).includes("Failed")) fail("stopping it reads as a deploy that failed");
  const lead = await page.locator(".page-lead").innerText();
  if (!/needs? a look/.test(lead)) fail(`the list's lead says nothing needs a look: ${lead}`);

  step("health: Restart is a plan that says what it does and that it can't be undone, and it comes back");
  await goToApp("checked");
  await page.getByRole("button", { name: "Restart", exact: true }).first().click();
  const restart = page.getByRole("dialog", { name: "Review plan" });
  await restart.waitFor({ timeout: 30_000 });
  await restart.getByText("A restart has no undo, as it changes nothing to go back to.").waitFor();
  // What it restarts counts as that, not as a replace.
  await restart.locator(".dw-plan-counts").getByText("to restart").waitFor();
  if ((await restart.innerText()).includes("to replace")) fail("a restart's plan counts it as a replace");
  await shoot(page, "health-restart-plan");
  await restart.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("button", { name: /^Done/ }).click({ timeout: 90_000 });
  await pageSays("Healthy");

  step("health: docker pause reads as paused, on the list and the page, and comes back");
  const restarted = await only("checked");
  await dockerAct(socket, restarted.id, "pause");
  await pageSays("Paused");
  await page.locator(".dw-callout").getByText("checked is paused").waitFor();
  await shoot(page, "health-paused");
  await goToList();
  await listSays("checked", "Paused");
  await shoot(page, "health-paused-list");
  await dockerAct(socket, restarted.id, "unpause");
  await listSays("checked", "Healthy");

  step("health: with nothing checking it, it reads as running, and says what that comes to");
  await page.goto(`${origin}/apps/new?source=compose`);
  const plainForm = page.getByRole("dialog", { name: "New app" });
  // Somewhere a server path may be, which isn't Dockwell's own data directory.
  const dir = mkdtempSync(join(tmpdir(), "dockwell-e2e-file-"));
  cleanUp.push(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(join(dir, "conf"));
  await plainForm.getByLabel("Compose file").fill(`name: plain
services:
  plain:
    image: nginx:\${TAG}
    labels:
      greeting: \${GREETING}
    volumes:
      - ./conf:/conf:ro
`);
  // The file reads what's beside it, and each refusal is on the field it's about.
  await plainForm.getByLabel(/Directory on the server/).waitFor({ timeout: 15_000 });
  await plainForm.getByLabel(/Directory on the server/).fill("not/absolute");
  await plainForm.getByText("isn't an absolute path").waitFor({ timeout: 15_000 });
  await plainForm.getByLabel(/Directory on the server/).fill(dir);
  await plainForm.getByRole("button", { name: "Add an .env file" }).click();
  const secret = "hello-this-is-not-a-name";
  await plainForm.getByLabel(".env file").fill(`TAG=1.27-alpine\nGREETING=$oops`);
  await plainForm.getByText(/Line 2 of the \.env file/).waitFor({ timeout: 15_000 });
  await shoot(page, "health-env-refused");
  await plainForm.getByLabel(".env file").fill(`TAG=1.27-alpine\nGREETING=${secret}`);
  await plainForm
    .getByRole("region", { name: "What this file makes" })
    .getByText("This makes 1 app")
    .waitFor({ timeout: 15_000 });
  const stored = await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }));
  if (stored.includes(secret)) fail("an .env's value is in this browser's storage");
  if (page.url().includes(secret)) fail("an .env's value is in the address");
  await plainForm.getByRole("button", { name: /Review plan/ }).click();
  const plainPlan = page.getByRole("dialog", { name: "Review plan" });
  await plainPlan.waitFor({ timeout: 30_000 });
  // A value that lands in plain sight says so, in plain words, as what the plan is about.
  await plainPlan.getByText(/plain's labels hold the value of GREETING from your \.env/).waitFor();
  await shoot(page, "health-env-note");
  await plainPlan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "plain is running" }).waitFor({ timeout: 90_000 });
  await page
    .locator(".dw-sheet-layer")
    .getByRole("button", { name: /^Open plain|^Done|^Show/ })
    .first()
    .click();
  await everywhere("plain", "Running", "running");
  await page
    .getByRole("region", { name: "What runs" })
    .getByText("Nothing checks it, so this only means it started.")
    .waitFor();
  await page
    .getByRole("region", { name: "Health check" })
    .getByText(/^None\./)
    .waitFor();

  step("health: killed, it reads as not running, and says how it went");
  const plain = await only("plain");
  await dockerAct(socket, plain.id, "kill");
  await pageSays("Not running");
  await page
    .getByRole("region", { name: "What runs" })
    .getByText(/exit code 137/)
    .waitFor();
  // What follows is the restart policy's: plain has none, so nothing starts it again.
  await page.locator(".dw-callout").getByText("Nothing starts it again on its own. Restart it, or deploy.").waitFor();

  step("health: it reads in Chinese too");
  const zh = await browser.newContext({
    storageState: await context.storageState(),
    locale: "zh-CN",
    viewport: { width: 1440, height: 900 },
  });
  const zhPage = await zh.newPage();
  await zhPage.goto(`${origin}/apps`);
  await zhPage
    .getByRole("row", { name: /plain/ })
    .getByText("没有运行", { exact: true })
    .waitFor({ timeout: SEEN_WITHIN });
  await shoot(zhPage, "health-zh-list");
  await zhPage.getByRole("link", { name: "plain", exact: true }).first().click();
  await zhPage.getByRole("region", { name: "运行内容" }).waitFor();
  await shoot(zhPage, "health-zh");
  await zh.close();

  step("health: its agent dies, and nobody can say: it isn't green, and it isn't not running");
  const cli = (...args: string[]) =>
    execFileSync(bin, [...args, "--data-dir", data, "--log-format", "json"], {
      encoding: "utf8",
      env: {
        ...process.env,
        ...env,
        USER: "mira",
        DOCKWELL_URL: "",
        DOCKWELL_TOKEN: "",
        XDG_CONFIG_HOME: join(data, "cli-config"),
      },
    });
  cli("login", "--local");
  // The second server's ticket comes from the panel, as a person adds one: Add server,
  // a passkey, and the command with the ticket in place.
  await page.goto(`${origin}/servers`);
  await page.getByRole("button", { name: "Add server" }).first().click();
  const adding = page.getByRole("dialog", { name: "Add a server" });
  await adding.getByRole("button", { name: "Make the command" }).click();
  const command =
    (await adding.locator(".servers-add-command code").first().textContent({ timeout: SEEN_WITHIN })) ?? "";
  const ticket =
    /(?:--join|dockwell join) (\S+)/.exec(command)?.[1] ?? fail(`the sheet's command has no ticket: ${command}`);
  const agentData = join(data, "second-agent");
  const clean = { ...process.env, ...env, DOCKWELL_DATA_DIR: "", DOCKWELL_URL: "", DOCKWELL_TOKEN: "" };
  execFileSync(bin, ["join", "--data-dir", agentData, "--name", "second", ticket], { encoding: "utf8", env: clean });
  const startAgent = (): ChildProcess =>
    spawn(bin, ["run", "--data-dir", agentData, "--log-format", "json"], { stdio: "ignore", env: clean });
  let agent = startAgent();
  await adding.getByText("second joined", { exact: true }).waitFor({ timeout: SEEN_WITHIN });
  await shoot(page, "add-server-joined");
  await adding.getByRole("button", { name: "Done" }).click();
  try {
    await page.goto(`${origin}/apps`);
    const servers = JSON.parse(cli("servers", "list", "--json")) as {
      servers?: { id?: string; name?: string; connection?: string }[];
    };
    const second =
      servers.servers?.find((s) => s.name === "second")?.id ??
      fail(`the second server isn't listed: ${JSON.stringify(servers)}`);
    for (let i = 0; i < 100; i++) {
      const now = JSON.parse(cli("servers", "get", second, "--json")) as { server?: { connection?: string } };
      if (now.server?.connection === "CONNECTION_CONNECTED") break;
      await page.waitForTimeout(300);
    }
    cli(
      "services",
      "create",
      "afar",
      "--project",
      "default",
      "--on",
      second,
      "--app-image",
      "nginx:1.27-alpine",
      "--yes",
    );
    await goToList();
    await listSays("afar", "Running");
    await goToApp("afar");
    await pageSays("Running");
    agent.kill("SIGKILL");
    await new Promise((done) => agent.once("exit", done));
    await pageSays("Unknown");
    // What it last read may be said, as that: only what it reads as now counts.
    const reads = await page.getByRole("region", { name: "What runs" }).locator(".dw-status-label").allInnerTexts();
    if (reads.join() !== "Unknown") fail(`with its agent gone, the page reads: ${reads.join()}`);
    await page
      .getByRole("region", { name: "What runs" })
      .getByText(/The agent on second isn't connected, so it can't say how this runs now\. It last read Running/)
      .waitFor({ timeout: SEEN_WITHIN });
    await page
      .locator(".dw-callout")
      .getByText(/It may well be running/)
      .waitFor({ timeout: SEEN_WITHIN });
    await shoot(page, "health-unknown");
    await goToList();
    await listSays("afar", "Unknown");
    if ((await row("afar").locator(".dw-tone-healthy").count()) > 0) fail("with its agent gone, it reads green");
    if ((await row("afar").innerText()).includes("Not running")) fail("with its agent gone, it reads as not running");
    await shoot(page, "health-unknown-list");
    // And it comes back as it was.
    agent = startAgent();
    await listSays("afar", "Running");
    cli("services", "delete", "afar", "--project", "default", "--yes");

    step("health: an agent that starts with Docker down says so from the start, and its server isn't healthy");
    agent.kill("SIGKILL");
    await new Promise((done) => agent.once("exit", done));
    // A socket nothing listens on is Docker down, as after a reboot where it didn't come up.
    agent = spawn(bin, ["run", "--data-dir", agentData, "--log-format", "json"], {
      stdio: "ignore",
      env: { ...clean, DOCKER_HOST: `unix://${join(agentData, "no-docker.sock")}` },
    });
    let said = "";
    for (let i = 0; i < 100; i++) {
      said = cli("servers", "get", second, "--json");
      if (said.includes('"dockerUnreadableSince"') && said.includes("CONNECTION_CONNECTED")) break;
      await page.waitForTimeout(300);
    }
    if (!said.includes('"dockerUnreadableSince"')) fail(`with Docker down from its start, the server says ${said}`);
    await page.goto(`${origin}/servers/${second}`);
    const agentPanel = page.getByRole("region", { name: "Agent" });
    await agentPanel.getByText("Can't read Docker").first().waitFor({ timeout: SEEN_WITHIN });
    await agentPanel.getByRole("button", { name: "Copy the command that restarts Docker" }).waitFor();
    if ((await agentPanel.locator(".dw-tone-healthy").count()) > 0)
      fail("an agent started with Docker down reads green");
    await page
      .locator(".topbar")
      .getByText(/can't read Docker/i)
      .waitFor({ timeout: SEEN_WITHIN });
    await shoot(page, "health-docker-down-at-start");
  } finally {
    agent.kill("SIGKILL");
    for (const undo of cleanUp) undo();
  }

  step("health: a change of health that lasted is on Activity, dated when it happened");
  await page.goto(`${origin}/activity?kind=health`);
  await page
    .getByRole("row", { name: /plain stopped running on/ })
    .first()
    .waitFor({ timeout: SEEN_WITHIN });
  await shoot(page, "health-activity");

  if (hangDocker) {
    step(
      "health: Docker stops answering with its agent connected: it says so, how to check, and the top bar isn't green",
    );
    await goToApp("checked");
    await pageSays("Healthy");
    const answer = hangDocker();
    try {
      // A check the agent makes needs nothing of Docker, so what only Docker can say is what goes.
      await page
        .getByRole("region", { name: "What runs" })
        .getByText(/can't read Docker/)
        .first()
        .waitFor({ timeout: 60_000 });
      const top = page.locator(".topbar");
      await top.getByText(/can't read Docker/i).waitFor({ timeout: SEEN_WITHIN });
      if ((await top.innerText()).includes("All healthy"))
        fail("with Docker not answering, the top bar says all is healthy");
      if ((await top.locator(".dw-tone-healthy").count()) > 0) fail("with Docker not answering, the top bar is green");
      await shoot(page, "health-docker-gone");
    } finally {
      answer();
    }
    await page
      .locator(".topbar")
      .getByText(/can't read Docker/i)
      .waitFor({ state: "detached", timeout: 60_000 });
  }

  if (freezeControlPlane) {
    step("health: a control plane that stops answering with its streams open: nothing stays green, and it comes back");
    await goToList();
    await listSays("checked", "Healthy");
    // The second server stays down from the steps above, so the top bar says that, not all healthy, either side of this.
    const top = page.locator(".topbar");
    await top.getByText("1 down").waitFor({ timeout: SEEN_WITHIN });
    const thaw = freezeControlPlane();
    try {
      // Past 30 s with nothing heard, not even a heartbeat, what was last said can't stand.
      await top.getByText("Unknown", { exact: true }).waitFor({ timeout: 60_000 });
      if ((await top.getByText("1 down").count()) > 0) fail("with the control plane frozen, the top bar still vouches");
      if ((await top.locator(".dw-tone-healthy").count()) > 0)
        fail("with the control plane frozen, the top bar is green");
      await page
        .locator(".apps-unread")
        .getByText(/The panel can't reach the control plane, so it can't say how your apps run/)
        .waitFor({ timeout: 30_000 });
      const grey = await page.locator(".apps-unread .dw-callout-glyph").evaluate((glyph) => {
        const probe = document.createElement("span");
        probe.style.color = "var(--unknown)";
        document.body.append(probe);
        const want = getComputedStyle(probe).color;
        probe.remove();
        return getComputedStyle(glyph).color === want;
      });
      if (!grey) fail("the glyph of not knowing isn't its own grey in the note");
      if ((await row("checked").locator(".dw-tone-healthy").count()) > 0)
        fail("with the control plane frozen, a row is green");
      await shoot(page, "health-control-plane-frozen");
    } finally {
      thaw();
    }
    await listSays("checked", "Healthy");
    await top.getByText("Unknown", { exact: true }).waitFor({ state: "detached", timeout: 60_000 });
    await top.getByText("1 down").waitFor({ timeout: 60_000 });
  }
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 30, run: (page, run) => healthFlow(page, { ...run, fake: true }) };
