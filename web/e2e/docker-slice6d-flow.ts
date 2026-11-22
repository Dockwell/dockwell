// SPDX-License-Identifier: AGPL-3.0-only

import { BinaryReader } from "@bufbuild/protobuf/wire";
import type { Route } from "playwright-core";

import { dockerPull, dockerRemoveImage } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop, pastTheEdge } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

const image = "fixture/push-example:local";
const registry = "registry.example.test";
const repo = "fixture/push-example";
const manifest = `sha256:${"b".repeat(64)}`;
const login = "fixture-registry";
const changeID = "chg_push_fixture";
const fingerprint = "fixture-push-fingerprint";
const approvalTarget = `${changeID}:${fingerprint}`;
const rpc = (name: string) => new RegExp(`/dockwell\\.v1\\.\\w+Service/${name}(\\?|$)`);
const message = (...parts: Buffer[]) => Buffer.concat(parts);

function field(body: Uint8Array, want: number): Buffer {
  const reader = new BinaryReader(body);
  while (reader.pos < reader.len) {
    const [tag, type] = reader.tag();
    if (tag === want) return Buffer.from(reader.bytes());
    reader.skip(type);
  }
  return Buffer.alloc(0);
}

function respond(route: Route, body: Buffer = Buffer.alloc(0)) {
  return route.fulfill({ status: 200, body, contentType: "application/proto" });
}

function planReply(server: string, push: Buffer, destination: string, outcome: 0 | 1 | 6) {
  const descriptor = message(bytes(1, `${registry}/${repo}`), bytes(3, manifest), bytes(4, "linux/arm64"));
  const action = message(
    bytes(1, message(bytes(1, destination), number(4, 1))),
    bytes(2, descriptor),
    bytes(5, message(bytes(1, login), bytes(2, login), bytes(3, registry), number(4, 1))),
    bytes(15, push),
  );
  const maintenance = bytes(4, action);
  const resource = message(bytes(1, "host"), bytes(2, server), bytes(3, "host"));
  const planned = message(
    bytes(1, "res_push_fixture"),
    bytes(2, resource),
    number(3, 3),
    bytes(8, message(number(1, 401), bytes(2, destination))),
    bytes(400, maintenance),
  );
  const step = message(bytes(1, "res_push_fixture"), bytes(2, resource), number(3, 3), bytes(400, maintenance));
  const summary = message(
    bytes(1, fingerprint),
    bytes(2, number(2, 1)),
    bytes(3, planned),
    bytes(4, message(bytes(1, server), bytes(2, "example-server"), bytes(6, step))),
    number(7, 1),
  );
  const progress =
    outcome === 0
      ? Buffer.alloc(0)
      : bytes(
          15,
          message(
            bytes(1, server),
            bytes(2, "res_push_fixture"),
            bytes(3, resource),
            number(4, 3),
            number(5, outcome === 6 ? 4 : 3),
            bytes(
              401,
              message(number(1, outcome), bytes(3, field(push, 1)), bytes(4, destination), bytes(5, manifest)),
            ),
          ),
        );
  return bytes(
    1,
    message(
      bytes(1, changeID),
      bytes(2, "Push Docker image"),
      number(3, outcome === 0 ? 1 : outcome === 6 ? 4 : 3),
      bytes(7, summary),
      progress,
      number(24, 1),
    ),
  );
}

export const flow: Flow = {
  order: 1.7,
  async run(page, { origin, socket, step, shoot }) {
    const serverPath = await ownServerPath(page, origin);
    const server = serverPath.split("/").at(-1) ?? "";
    let push: Buffer = Buffer.alloc(0);
    let destination = "";
    let checks = 0;
    // The browser stand's registry can't publish via HTTPS. These replies
    // exercise the panel. The real Engine test exercises the publisher.
    const routes: [string, (route: Route) => Promise<void>][] = [
      ["GetDockerOverview", (route) => respond(route, bytes(1, message(bytes(1, server), bytes(17, number(7, 1)))))],
      [
        "ListRegistryLogins",
        (route) =>
          respond(route, bytes(1, message(bytes(1, login), bytes(2, registry), bytes(7, login), number(8, 1)))),
      ],
      [
        "PlanDockerMaintenance",
        (route) => {
          const input = field(route.request().postDataBuffer() ?? Buffer.alloc(0), 15);
          const target = field(input, 2).toString();
          if (!field(input, 1).length) {
            checks++;
            return respond(route, bytes(2, bytes(1, bytes(3, manifest))));
          }
          if (target.endsWith(":occupied")) {
            return route.fulfill({
              status: 400,
              contentType: "application/json",
              headers: { "Dockwell-Error": "docker_push_conflict" },
              json: { code: "failed_precondition", message: "This destination serves a different digest" },
            });
          }
          if (!/^sha256:[a-f0-9]{64}$/.test(field(field(input, 1), 2).toString())) {
            throw new Error("Push review did not submit a full image ID");
          }
          push = input;
          destination = target;
          return respond(route, planReply(server, push, destination, 0));
        },
      ],
      ["DiscardChange", (route) => respond(route)],
      [
        "BeginStepUp",
        (route) => {
          const request = route.request().postDataBuffer() ?? Buffer.alloc(0);
          if (field(request, 2).toString() !== approvalTarget) {
            throw new Error("Passkey approval wasn't bound to the change and fingerprint");
          }
          return route.continue();
        },
      ],
      [
        "ApplyChange",
        (route) => {
          const request = route.request().postDataBuffer() ?? Buffer.alloc(0);
          if (field(request, 2).toString() !== fingerprint) throw new Error("Apply dropped the reviewed fingerprint");
          return field(request, 3).length
            ? respond(route, planReply(server, push, destination, destination.endsWith(":unknown") ? 6 : 1))
            : route.fulfill({
                status: 403,
                contentType: "application/json",
                headers: { "Dockwell-Error": "needs_step_up" },
                json: {
                  code: "permission_denied",
                  message: "Confirm with a passkey",
                  details: [
                    {
                      type: "dockwell.v1.StepUpNeeded",
                      value: message(number(1, 114), bytes(2, approvalTarget)).toString("base64"),
                    },
                  ],
                },
              });
        },
      ],
    ];
    await dockerPull(socket, image);
    for (const [name, handler] of routes) await page.route(rpc(name), handler);
    const top = (name: string) => onTop(page.getByRole("dialog", { name, exact: true }));
    const sheet = top("Push image");
    const review = top("Review plan");
    const layout = async () => {
      for (const colorScheme of ["light", "dark"] as const) {
        await page.emulateMedia({ colorScheme });
        for (const width of [1440, 1024, 390, 320]) {
          await atWidth(page, { width, height: 900 });
          const edge = await pastTheEdge(page);
          if (edge.wider > 1) throw new Error(`Push sheet exceeds ${width}px in ${colorScheme}: ${edge.what}`);
        }
      }
      await atWidth(page, { width: 1440, height: 900 });
      await page.emulateMedia({ colorScheme: "light" });
    };
    const open = async () => {
      await page.getByRole("button", { name: `Actions for ${image}` }).click();
      await page.getByRole("menuitem", { name: "Push image" }).click();
      await sheet.getByRole("combobox", { name: "Registry login" }).waitFor();
    };
    const apply = async (receipt: string) => {
      const passkey = page.waitForResponse((response) => rpc("FinishStepUp").test(response.url()) && response.ok());
      await review.getByRole("button", { name: /^Apply plan/ }).click();
      await passkey;
      await top(receipt).waitFor();
    };
    try {
      step("Docker push: review the exact destination, refuse a conflict and confirm publication with a passkey");
      await page.goto(`${origin}${serverPath}/docker/images`);
      await open();
      await shoot(page, "docker-slice6d-destination");
      await sheet.getByRole("textbox", { name: "Tag" }).fill("occupied");
      await sheet.getByRole("button", { name: "Review push" }).click();
      await sheet.getByText("Choose another tag", { exact: true }).waitFor();
      await shoot(page, "docker-slice6d-conflict");
      await sheet.getByRole("textbox", { name: "Tag" }).fill("published");
      await sheet.getByRole("button", { name: "Review push" }).click();
      await review.getByText(manifest, { exact: true }).waitFor();
      await review.getByText("Another writer can claim this tag").waitFor();
      await layout();
      await shoot(page, "docker-slice6d-review");
      await review.getByText("Applying this asks for your passkey", { exact: true }).waitFor();
      await shoot(page, "docker-slice6d-passkey");
      await apply("Image published");
      await top("Image published").getByText("Verified", { exact: true }).waitFor();
      await shoot(page, "docker-slice6d-success");
      await top("Image published").getByRole("button", { name: "Done" }).click();

      step("Docker push: check an uncertain result without issuing another publication");
      await page.getByRole("link", { name: image, exact: true }).click();
      await open();
      await sheet.getByRole("textbox", { name: "Tag" }).fill("unknown");
      await sheet.getByRole("button", { name: "Review push" }).click();
      await apply("Result unknown");
      const result = top("Result unknown");
      await result.getByRole("button", { name: "Check result" }).waitFor();
      await layout();
      await shoot(page, "docker-slice6d-unknown");
      await result.getByRole("button", { name: "Check result" }).click();
      await result.getByText("Registry serves the reviewed digest").waitFor();
      if (checks !== 1) throw new Error(`Expected one read-only check, got ${checks}`);
      await result.getByRole("button", { name: "Review again" }).click();
      await sheet.getByRole("switch", { name: "Publish by digest" }).click();
      if (await sheet.getByRole("textbox", { name: "Tag" }).count())
        throw new Error("Digest publishing showed a tag input");
      await sheet.getByRole("button", { name: "Cancel" }).click();
      await result.getByRole("button", { name: "Done" }).click();
    } finally {
      for (const [name, handler] of routes) await page.unroute(rpc(name), handler);
      await dockerRemoveImage(socket, image).catch(() => undefined);
    }
  },
};
