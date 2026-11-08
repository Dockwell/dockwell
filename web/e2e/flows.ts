// SPDX-License-Identifier: AGPL-3.0-only

// The panel's flows that sign-in.ts runs once it's signed in, on the fake Docker
// Engine: every *-flow.ts in this folder that exports a flow, in the order each
// one's order puts it. A new flow is a new file, so pull requests that each add
// one don't edit a list they share, and don't conflict there
// (docs/adr/0051-files-that-merge-without-conflicts.md).

import { readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import type { Browser, BrowserContext, Page } from "playwright-core";

import type { DockerProbe } from "./docker-probe.ts";

/** What sign-in.ts gives every flow: each takes what it needs. */
export interface FlowRun {
  /** The panel's address. */
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
  browser: Browser;
  context: BrowserContext;
  /** Says what the fake Docker Engine runs. */
  docker: DockerProbe;
  /** The fake Docker Engine's socket. */
  socket: string;
  /** The dockwell binary, and the data directory it runs on. */
  bin: string;
  data: string;
  /** The environment the binary runs with. */
  env: NodeJS.ProcessEnv;
  /** The directory the control plane reads more catalog entries from. */
  catalogDir: string;
  /** Stops a container on the fake engine, as someone at the server would. */
  stopByHand: (id: string) => Promise<void>;
  /** Makes Docker stop answering, and returns what makes it answer again. */
  hangDocker: () => () => void;
  /** Freezes the control plane with its connections open, and returns what thaws it. */
  freezeControlPlane: () => () => void;
}

/** What a *-flow.ts exports for sign-in.ts to run. */
export interface Flow {
  /** Where it runs among the others, lowest first; flows with the same order run by file name. */
  order: number;
  run: (page: Page, run: FlowRun) => Promise<void>;
}

/** Runs every flow in this folder, in order. */
export async function runFlows(page: Page, run: FlowRun): Promise<void> {
  const dir = dirname(fileURLToPath(import.meta.url));
  const flows: { file: string; flow: Flow }[] = [];
  for (const file of readdirSync(dir).filter((name) => name.endsWith("-flow.ts"))) {
    const module = (await import(pathToFileURL(join(dir, file)).href)) as { flow?: Flow };
    if (!module.flow) {
      throw new Error(`${file} exports no flow, so sign-in.ts can't run it. Export one, as settings-flow.ts does.`);
    }
    flows.push({ file, flow: module.flow });
  }
  flows.sort((a, b) => a.flow.order - b.flow.order || a.file.localeCompare(b.file));
  run.step(`flows: ${flows.map(({ file }) => file.replace(/-flow\.ts$/, "")).join(", ")}`);
  for (const { flow } of flows) await flow.run(page, run);
}
