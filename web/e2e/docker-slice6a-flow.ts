// SPDX-License-Identifier: AGPL-3.0-only

import type { Page, Route } from "playwright-core";
import { dockerTag } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop, pastTheEdge } from "./sheets.ts";

const repository = "example/dockwell-pull";
const pulled = `${repository}:1.0`;
const alias = "example/abcdefghijklmnopqrstuvwxyz/another-repository-segment/a-long-local-alias:review";

async function hold(page: Page, rpc: string): Promise<() => Promise<void>> {
  const pattern = new RegExp(`/dockwell\\.v1\\.${rpc.replace(".", "/")}(\\?|$)`);
  let release = () => {};
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  const handler = async (route: Route) => {
    await gate;
    await route.continue().catch(() => undefined);
  };
  await page.route(pattern, handler);
  return async () => {
    release();
    await page.unroute(pattern, handler);
  };
}

async function checkLayout(page: Page, selector: string) {
  const viewport = page.viewportSize();
  try {
    for (const colorScheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme });
      for (const width of [1440, 1024, 390, 320]) {
        await atWidth(page, { width, height: 900 });
        const outside = await pastTheEdge(page);
        if (outside.wider > 1) throw new Error(`Image workspace overflow at ${width}px: ${outside.what}`);
        const overflow = await page
          .locator(selector)
          .first()
          .evaluate((root) => {
            const bounds = root.getBoundingClientRect();
            const nodes = root.querySelectorAll(
              "button, input, .plan-image-evidence dd, .docker-image-row, .docker-row-link, .docker-image-table th, .docker-image-platform",
            );
            return [...nodes]
              .filter((node) => {
                if (node.closest(".dw-tabs")) return false;
                if (node.matches(".docker-image-table th") && !node.clientWidth) return true;
                if (
                  node.matches(".docker-image-row .docker-row-link, .docker-image-platform") &&
                  node.scrollWidth > node.clientWidth + 2
                )
                  return true;
                const item = node.getBoundingClientRect();
                return item.width > 0 && (item.left < bounds.left - 2 || item.right > bounds.right + 2);
              })
              .map(
                (node) => node.textContent?.trim().slice(0, 65) || node.getAttribute("aria-label") || node.className,
              );
          });
        if (overflow.length)
          throw new Error(`Image workspace overflow at ${width}px, ${colorScheme}: ${overflow.join(", ")}`);
      }
    }
  } finally {
    await page.emulateMedia({ colorScheme: "light" });
    if (viewport) await atWidth(page, viewport);
  }
}

export const flow: Flow = {
  order: 1.5,
  async run(page, { origin, socket, step, shoot }) {
    const server = await ownServerPath(page, origin);
    const images = `${origin}${server}/docker/images`;
    const pull = page.getByRole("button", { name: "Pull image", exact: true });
    const picker = onTop(page.getByRole("dialog", { name: "Pull image" }));
    const review = onTop(page.getByRole("dialog", { name: "Review plan" }));
    const firstReceipt = onTop(page.getByRole("dialog", { name: "Image pulled" }));
    const tagReceipt = onTop(page.getByRole("dialog", { name: "Image tag set" }));

    step("Docker images: the table and its controls hold their places while inventory loads");
    const showInventory = await hold(page, "DockerService.ListDockerObjects");
    try {
      await page.goto(images);
      await pull.waitFor();
      await page.locator(".docker-workspace [aria-busy=true] .dw-skeleton").first().waitFor();
      await shoot(page, "docker-slice6a-images-loading");
    } finally {
      await showInventory();
    }

    step("Docker images: browse a registry and review a digest-pinned pull");
    for (const colorScheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme });
      if (
        !(await pull.evaluate(
          (node) =>
            node.classList.contains("dw-btn-primary") && getComputedStyle(node).backgroundColor !== "rgba(0, 0, 0, 0)",
        ))
      ) {
        throw new Error(`Pull image doesn't use a filled primary button in ${colorScheme}`);
      }
    }
    await page.emulateMedia({ colorScheme: "light" });
    await pull.click();
    const registry = picker.getByRole("combobox", { name: "Registry", exact: true });
    if ((await registry.inputValue()) !== "docker.io") {
      throw new Error("Pull image did not select the Engine's registry");
    }
    await picker.getByText("Public", { exact: true }).waitFor();
    const registryLink = picker.getByRole("link", { name: "Manage logins" });
    if ((await registryLink.evaluate((node) => getComputedStyle(node).textDecorationLine)) !== "none") {
      throw new Error("Manage logins did not use the panel's button link style");
    }
    await checkLayout(page, ".dw-sheet-view:not(.is-away)");
    await shoot(page, "docker-slice6a-pull");
    await registry.selectOption({ label: "Other registry…" });
    await picker.getByRole("textbox", { name: "Registry address" }).fill("registry.example.test:5443");
    await shoot(page, "docker-slice6a-other-registry");
    await registry.selectOption("docker.io");
    if (await picker.getByRole("textbox", { name: "Registry address" }).count())
      throw new Error("Other registry didn't close its address field");
    const showRepositories = await hold(page, "ImageService.ListRegistryRepositories");
    try {
      await picker.getByRole("button", { name: "Browse registry" }).click();
      await picker.locator("[aria-busy=true] .dw-skeleton").first().waitFor();
      await shoot(page, "docker-slice6a-repositories-loading");
    } finally {
      await showRepositories();
    }
    await picker.getByRole("button", { name: repository, exact: true }).waitFor();
    await shoot(page, "docker-slice6a-repositories");
    await picker.getByRole("button", { name: repository, exact: true }).click();
    const imageField = picker.getByRole("textbox", { name: "Image", exact: true });
    const tags = picker.getByRole("region", { name: "Repository tags" });
    const tagRow = tags.getByRole("button", { name: /^1\.0(?: |$)/ });
    await tagRow.getByText("Not on this server", { exact: true }).waitFor();
    const ordered = await tags.locator(".docker-pull-tag-name code").allTextContents();
    if (ordered.join(",") !== "1.0,latest,1.1")
      throw new Error("The repository tags didn't keep their newest-first order");
    if (!(await tagRow.getByText(/^sha256:[0-9a-f]{64}$/).count()))
      throw new Error("The tag row doesn't show its digest");
    await checkLayout(page, ".dw-sheet-view:not(.is-away)");
    await shoot(page, "docker-slice6a-tags");
    await tagRow.click();
    if ((await imageField.inputValue()) !== pulled)
      throw new Error("Picking a repository and tag didn't set the image");
    const showPlan = await hold(page, "DockerService.PlanDockerMaintenance");
    try {
      await picker.getByRole("button", { name: "Review pull" }).click();
      await picker.locator(".docker-image-compare[aria-busy=true] .dw-skeleton").first().waitFor();
      await shoot(page, "docker-slice6a-compare-loading");
    } finally {
      await showPlan();
    }
    await review.getByText("Platform manifest digest", { exact: true }).waitFor();
    await review.getByText("Image config digest", { exact: true }).waitFor();
    await review.getByText("linux/amd64", { exact: true }).waitFor();
    await checkLayout(page, ".dw-sheet-view:not(.is-away)");
    await shoot(page, "docker-slice6a-pull-review");
    await review.getByRole("button", { name: /^Apply plan/ }).click();
    await firstReceipt.getByText("Verified image ID", { exact: true }).waitFor();
    await shoot(page, "docker-slice6a-pull-receipt");
    await firstReceipt.getByRole("button", { name: "Done" }).click();

    step("Docker images: pull again shows Up to date on its row without an apply");
    const imageLink = page.getByRole("link", { name: pulled, exact: true });
    const row = page.getByRole("row").or(page.getByRole("listitem")).filter({ has: imageLink });
    await row.getByRole("button", { name: `Actions for ${pulled}` }).click();
    const pullAgain = page.getByRole("menuitem", { name: "Pull again" });
    const showAgain = await hold(page, "DockerService.PlanDockerMaintenance");
    try {
      await pullAgain.click();
      await row.locator("[aria-busy=true] .dw-skeleton").waitFor();
      await shoot(page, "docker-slice6a-pull-again-loading");
    } finally {
      await showAgain();
    }
    await row.getByRole("status").locator(".dw-tone-healthy").getByText("Up to date", { exact: true }).waitFor();
    if (await review.isVisible()) throw new Error("an unchanged digest opened a plan");
    await checkLayout(page, ".docker-workspace");
    await shoot(page, "docker-slice6a-images");
    await imageLink.click();
    await page.getByRole("heading", { name: pulled, exact: true }).waitFor();
    const original = page.url();
    await shoot(page, "docker-slice6a-detail");

    const tag = async (name: string, move: boolean) => {
      const kind = move ? "Move tag" : "Add tag";
      await page.locator(".docker-detail-body .docker-image-menu").getByRole("button").click();
      await page.getByRole("menuitem", { name: kind, exact: true }).click();
      const editor = onTop(page.getByRole("dialog", { name: kind }));
      const input = editor.getByRole("textbox", { name: move ? "Tag to move" : "New tag" });
      if (!move && (await input.inputValue()) !== `${repository}:`) {
        throw new Error("Add tag didn't start with the selected image's repository");
      }
      await input.fill(name);
      await checkLayout(page, ".dw-sheet-view:not(.is-away)");
      await shoot(page, move ? "docker-slice6a-move-tag" : "docker-slice6a-add-tag");
      await editor.getByRole("button", { name: "Review plan" }).click();
      await review.getByText(move ? "New image ID" : "Image ID", { exact: true }).waitFor();
    };

    step("Docker images: add a tag and move it with a passkey while keeping both image IDs");
    await tag(alias, false);
    await shoot(page, "docker-slice6a-add-review");
    await review.getByRole("button", { name: /^Apply plan/ }).click();
    await tagReceipt.getByRole("button", { name: "Done" }).click();

    const other = `${repository}:1.1`;
    await page.goto(images);
    await pull.click();
    await picker.getByRole("textbox", { name: "Image", exact: true }).fill(other);
    await picker.getByRole("button", { name: "Review pull" }).click();
    await review.getByRole("button", { name: /^Apply plan/ }).click();
    await firstReceipt.getByRole("button", { name: "Done" }).click();
    await page.getByRole("link", { name: other, exact: true }).click();
    await page.getByRole("heading", { name: other, exact: true }).waitFor();
    const replacement = page.url();

    await tag(alias, true);
    await review.getByText("Current image ID", { exact: true }).waitFor();
    await review.locator(".plan-risk.dw-tone-warning").getByText("This moves the tag", { exact: true }).waitFor();
    const ids = await review.locator(".plan-image-evidence dd code").allTextContents();
    const idOf = (url: string) => decodeURIComponent(new URL(url).pathname.split("/").at(-1) ?? "");
    if (ids[0] !== idOf(original) || ids[1] !== idOf(replacement) || ids[0] === ids[1]) {
      throw new Error("Move tag didn't show the exact current and replacement IDs");
    }
    await checkLayout(page, ".dw-sheet-view:not(.is-away)");
    await shoot(page, "docker-slice6a-move-review");
    const refused = page.waitForResponse(
      (res) => res.url().endsWith("/dockwell.v1.ChangeService/ApplyChange") && res.status() === 403,
    );
    await review.getByRole("button", { name: /^Apply plan/ }).click();
    await refused;
    await tagReceipt.getByText("Verified image ID", { exact: true }).waitFor();
    await shoot(page, "docker-slice6a-move-receipt");
    await tagReceipt.getByRole("button", { name: "Done" }).click();

    await page.goto(replacement);
    await page.getByText("Tags and digests", { exact: true }).click();
    await page.locator(".docker-aliases").getByText(alias, { exact: true }).waitFor();
    await page.goto(original);
    await page.getByRole("heading", { name: pulled, exact: true }).waitFor();

    step("Docker images: checking an installed tag reviews current evidence before the registry update");
    await dockerTag(socket, idOf(replacement), repository, "1.0");
    await page.goto(images);
    await pull.click();
    await imageField.fill(pulled);
    await tagRow.getByText("On this server", { exact: true }).waitFor();
    await shoot(page, "docker-slice6a-installed-tags");
    await picker.getByRole("button", { name: "Check for updates" }).click();
    const evidence = review.locator(".plan-image-evidence");
    await evidence.getByText("New image config digest", { exact: true }).waitFor();
    const values = await evidence.locator("dt, dd").allTextContents();
    if (
      values.slice(0, 4).join("|") !==
        ["Current image ID", idOf(replacement), "Current image config digest", idOf(replacement)].join("|") ||
      values[values.indexOf("New image config digest") + 1] !== idOf(original)
    ) {
      throw new Error("The pull review didn't put the current image and digest before the replacement");
    }
    await checkLayout(page, ".dw-sheet-view:not(.is-away)");
    await shoot(page, "docker-slice6a-replacement-review");
    await review.getByRole("button", { name: "Cancel" }).click();
  },
};
