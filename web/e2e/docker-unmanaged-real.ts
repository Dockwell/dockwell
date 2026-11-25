// SPDX-License-Identifier: AGPL-3.0-only

import { randomUUID } from "node:crypto";
import type { Page } from "playwright-core";

import {
  dockerInspect,
  dockerProbe,
  dockerRemove,
  dockerRemoveVolume,
  dockerRun,
  dockerStorageNames,
} from "./docker-probe.ts";
import { ownServerPath } from "./own-server.ts";
import { onTop } from "./sheets.ts";

export async function dockerUnmanagedOnRealDocker(page: Page, origin: string, socket: string) {
  const server = await ownServerPath(page, origin);
  const inventory = `${origin}${server}/docker/containers`;
  const name = `outside-${randomUUID().slice(0, 8)}`;
  const image = "busybox:1.37";
  const engineId = await dockerRun(socket, name, {
    image,
    command: ["sh", "-c", "trap 'exit 0' TERM; while true; do sleep 1; done"],
    anonymousVolume: "/scratch",
  });
  const inspect = () => dockerInspect(socket, engineId);
  const review = onTop(page.getByRole("dialog", { name: "Review plan" }));
  const row = page.getByRole("row").filter({ has: page.getByRole("link", { name, exact: true }) });
  const choose = async (action: string) => {
    await row.getByRole("button", { name: `Actions for ${name}` }).click();
    await page.getByRole("menuitem", { name: action, exact: true }).click();
  };
  const apply = async () => {
    await review.getByRole("button", { name: /^Apply plan/ }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Done", exact: true }).click({ timeout: 45_000 });
  };

  let volume: string | undefined;
  let removed = false;
  try {
    volume = (await inspect()).Mounts.find((mount) => mount.Destination === "/scratch")?.Name;
    if (!volume) throw new Error("Docker did not create the fixture's anonymous volume");
    console.log("e2e: pause, unpause, stop, start, kill and remove an external container on Docker");
    await page.goto(inventory);
    await page.getByRole("textbox", { name: "Search containers" }).fill(name);
    await choose("Pause");
    await apply();
    if (!(await inspect()).State.Paused) throw new Error("Docker did not pause the external container");
    await row.getByText("Paused", { exact: true }).waitFor();
    await choose("Unpause");
    await apply();
    if ((await inspect()).State.Paused) throw new Error("Docker did not unpause the external container");
    await row.getByText("Running", { exact: true }).waitFor();
    await choose("Stop");
    await apply();
    if ((await inspect()).State.Running) throw new Error("Docker did not stop the external container");
    await row.getByText("Stopped", { exact: true }).waitFor();
    await choose("Start");
    await apply();
    if (!(await inspect()).State.Running) throw new Error("Docker did not start the external container");
    await row.getByText("Running", { exact: true }).waitFor();
    await choose("Kill");
    const kill = page.getByRole("dialog", { name: "Kill", exact: true });
    await kill.getByLabel("Signal", { exact: true }).selectOption("SIGKILL");
    await kill.getByRole("button", { name: "Review plan" }).click();
    await apply();
    if ((await inspect()).State.Running) throw new Error("Docker did not kill the external container");
    await row.getByText("Stopped", { exact: true }).waitFor();
    await choose("Remove");
    const remove = page.getByRole("dialog", { name: "Remove", exact: true });
    await remove.getByLabel("Anonymous volumes", { exact: true }).selectOption("remove");
    await remove.getByRole("button", { name: "Review plan" }).click();
    await review.getByText("Applying this asks for your passkey", { exact: true }).waitFor();
    await apply();
    await row.waitFor({ state: "hidden" });
    const exists = await inspect().then(
      () => true,
      (error: unknown) => {
        if (error instanceof Error && error.message.startsWith("Docker answered 404")) return false;
        throw error;
      },
    );
    if (exists || (await dockerStorageNames(socket, "volumes")).includes(volume)) {
      throw new Error("Docker still has the external container or the reviewed anonymous volume");
    }
    removed = true;

    console.log("e2e: paste docker run, apply it and verify the managed app on Docker");
    const app = `imported-${name.slice(8)}`;
    await page.getByRole("button", { name: "Import", exact: true }).click();
    await page.getByRole("menuitem", { name: "Paste docker run", exact: true }).click();
    const paste = page.getByRole("dialog", { name: "Paste docker run", exact: true });
    await paste
      .getByRole("textbox", { name: "Docker run command" })
      .fill(`docker run --name ${app} -e 'TITLE=two words' ${image} sleep 600`);
    await paste.getByRole("button", { name: "Review plan", exact: true }).click();
    await apply();
    await page.waitForURL(/\/apps\/[^/]+\/logs$/);
    const managed = (await dockerProbe(socket)()).find((item) => item.service === app);
    if (!managed?.running || managed.image !== image || !managed.env.includes("TITLE=two words")) {
      throw new Error("Docker does not have the running managed app with its imported settings");
    }
  } finally {
    if (!removed) {
      await dockerRemove(socket, engineId).catch((error: unknown) => {
        if (!(error instanceof Error && error.message.startsWith("Docker answered 404"))) throw error;
      });
      if (volume && (await dockerStorageNames(socket, "volumes")).includes(volume)) {
        await dockerRemoveVolume(socket, volume);
      }
    }
  }
}
