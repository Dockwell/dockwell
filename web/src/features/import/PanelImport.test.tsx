// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ChangeSchema,
  ChangeState,
  type CheckPanelDomainsRequest,
  CheckPanelDomainsResponseSchema,
  ContainerState,
  GuessableSecret,
  ImportAction,
  ImportCoolifyResponseSchema,
  ImportKind,
  ImportWaitsFor,
  ListContainersResponseSchema,
  PanelKind,
  PlanAction,
  type PlanPanelDomainsRequest,
  ServerContainerSchema,
  Surface,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

const coolifyRuns = () =>
  create(ListContainersResponseSchema, {
    containers: [
      create(ServerContainerSchema, {
        name: "coolify",
        image: "ghcr.io/coollabsio/coolify:4.7.5",
        state: ContainerState.RUNNING,
      }),
    ],
  });

function api(more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  return { servers: { watchServers: watch.stream }, containers: { listContainers: coolifyRuns }, ...more };
}

const found = create(ImportCoolifyResponseSchema, {
  found: { kind: PanelKind.COOLIFY, version: "4.7.5", knownVersion: true, folder: "/data/coolify" },
  steps: [
    {
      kind: ImportKind.APP,
      name: "blog",
      summary: "a service from Coolify's templates, in personal / production",
      action: ImportAction.DONE,
      becomes: "the project blog, taken on already",
    },
    {
      kind: ImportKind.DOMAIN,
      name: "blog.example.com",
      summary: "a domain of blog, sent to ghost-k4s9",
      action: ImportAction.MAKE,
      becomes: "a route to ghost on port 2368",
      waitsFor: ImportWaitsFor.PORTS,
    },
    {
      kind: ImportKind.PANEL,
      name: "Coolify",
      summary: "Coolify itself 4.7.5: coolify, coolify-proxy",
      action: ImportAction.LEAVE,
      reason: "It goes on running beside Dockwell, as nothing of it is changed.",
    },
  ],
});

const host = { kind: "server.host", scope: "srv_1", name: "host" };
const route = { kind: "route", scope: "env_1", name: "blog-example-com" };
const proxy = {
  kind: "coolify",
  proxy: true,
  parts: [{ name: "coolify-proxy", wasRunning: true, wasRestart: "always" }],
  domains: ["blog.example.com"],
  evidence: "0123456789abcdef0123456789abcdef",
};
const handover = create(ChangeSchema, {
  id: "chg_domains",
  summary: "Hand over Coolify's domains on hz-1",
  state: ChangeState.PLANNED,
  proposedVia: Surface.PANEL,
  createTime: timestampFromDate(new Date()),
  plan: {
    fingerprint: "1a2b3c4d5e6f708192a3b4c5d6e7f809",
    hostAccess: true,
    notes: [{ reason: "panel_domains_unguarded", message: "", names: ["blog.example.com"] }],
    resources: [
      { id: "res_host", resource: host, action: PlanAction.UPDATE, servers: ["srv_1"], panel: proxy },
      { id: "res_route", resource: route, action: PlanAction.CREATE, servers: ["srv_1"] },
    ],
    servers: [
      {
        serverId: "srv_1",
        serverName: "hz-1",
        steps: [
          { resourceId: "res_host", resource: host, action: PlanAction.UPDATE, panel: proxy },
          { resourceId: "res_route", resource: route, action: PlanAction.CREATE },
        ],
      },
    ],
  },
});

describe("moving in from Coolify", () => {
  it("is a tab where Coolify runs, reads what it runs without planning, and hands over the domains a person chooses", async () => {
    const user = userEvent.setup();
    const importCoolify = vi.fn((_req: { serverId: string; skipPlan?: boolean }) => found);
    const checkPanelDomains = vi.fn((_req: CheckPanelDomainsRequest) =>
      create(CheckPanelDomainsResponseSchema, {
        domains: [
          {
            domain: "blog.example.com",
            scanned: "labels on 4 containers and 0 services, and 2 files in the proxy's mounts",
            probes: [{ path: "/", answer: "200", open: true }],
            askedFrom: "203.0.113.10",
          },
          { domain: "wiki.example.com", held: "a request for / through the proxy answered 401" },
          {
            domain: "docs.example.com",
            unsure: "a request for / answered 404, which says nothing of a login",
            probes: [{ path: "/", answer: "404" }],
          },
        ],
      }),
    );
    const planPanelDomains = vi.fn((_req: PlanPanelDomainsRequest) => ({ change: handover }));
    renderAt("/servers/srv_1/import", api({ imports: { importCoolify, checkPanelDomains, planPanelDomains } }));
    expect((await screen.findByRole("link", { name: "Move in" })).getAttribute("href")).toBe("/servers/srv_1/import");
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    await waitFor(() =>
      expect(importCoolify).toHaveBeenCalledWith(expect.objectContaining({ skipPlan: true }), expect.anything()),
    );
    const ports = await screen.findByRole("list", { name: "Later, once Coolify's proxy hands over ports 80 and 443" });
    expect(ports).toHaveTextContent("blog.example.com");
    expect(ports).toHaveTextContent("a route to ghost on port 2368");
    const stays = screen.getByRole("list", { name: "Stays with Coolify" });
    expect(within(stays).getByRole("button", { name: "Stop Coolify" })).toBeTruthy();
    await expectAccessible(document.body);

    await user.click(screen.getByRole("button", { name: "Hand over ports 80 and 443" }));
    const choice = await screen.findByRole("dialog", { name: "Hand over Coolify's domains" });
    const blog = await within(choice).findByRole("checkbox", { name: "blog.example.com" });
    const wiki = within(choice).getByRole("checkbox", { name: "wiki.example.com" });
    const docs = within(choice).getByRole("checkbox", { name: "docs.example.com" });
    expect(checkPanelDomains.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_1", panel: PanelKind.COOLIFY });
    // What nothing was found for is chosen already; what stays can't be.
    expect((blog as HTMLInputElement).checked).toBe(true);
    expect((wiki as HTMLInputElement).disabled).toBe(true);
    // A 404 says nothing either way: it can be chosen, and isn't already.
    expect((docs as HTMLInputElement).checked).toBe(false);
    expect((docs as HTMLInputElement).disabled).toBe(false);
    expect(choice).toHaveTextContent("a request for / answered 404, which says nothing of a login");
    expect(choice).toHaveTextContent(
      "Asked from 203.0.113.10: an address limit that lets in this server or private networks can't be seen.",
    );
    expect(choice).toHaveTextContent("No login or address limit found/ 200");
    expect(choice).toHaveTextContent("a request for / through the proxy answered 401");
    expect(choice).toHaveTextContent(
      "Checked labels on 4 containers and 0 services, and 2 files in the proxy's mounts",
    );
    await expectAccessible(choice);
    await user.click(blog);
    expect((within(choice).getByRole("button", { name: "Review plan" }) as HTMLButtonElement).disabled).toBe(true);
    await user.click(blog);

    await user.click(within(choice).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(planPanelDomains).toHaveBeenCalledTimes(1));
    expect(planPanelDomains.mock.calls[0]?.[0]).toMatchObject({
      serverId: "srv_1",
      panel: PanelKind.COOLIFY,
      domains: ["blog.example.com"],
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await waitFor(() =>
      expect(sheet).toHaveTextContent("Stop coolify-proxy on hz-1, so Dockwell takes ports 80 and 443"),
    );
    expect(sheet).toHaveTextContent("its domains come as the routes below");
    expect(sheet).toHaveTextContent("No login or address limit found on the old proxy");
    expect(sheet).toHaveTextContent(
      "Just before the stop, the agent checks blog.example.com again, and leaves it running if anything changed",
    );
    expect(sheet).toHaveTextContent("Undo for");
  });

  it("hands over a domain the proxy redirects only with the domain it lands on, and says where it goes", async () => {
    const user = userEvent.setup();
    const open = (domain: string, redirectsTo = "") => ({
      domain,
      redirectsTo,
      probes: [
        { path: "/", answer: redirectsTo ? "302 to shop.example.com, as its redirect does" : "200", open: true },
      ],
    });
    const checkPanelDomains = vi.fn((_req: CheckPanelDomainsRequest) =>
      create(CheckPanelDomainsResponseSchema, {
        domains: [
          open("shop.example.com"),
          open("www.shop.example.com", "https://shop.example.com"),
          { domain: "docs.example.com", unsure: "a request for / answered 404, which says nothing of a login" },
          open("www.docs.example.com", "https://docs.example.com"),
        ],
      }),
    );
    const planPanelDomains = vi.fn((_req: PlanPanelDomainsRequest) => ({ change: handover }));
    renderAt(
      "/servers/srv_1/import",
      api({ imports: { importCoolify: () => found, checkPanelDomains, planPanelDomains } }),
    );
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    await user.click(await screen.findByRole("button", { name: "Hand over ports 80 and 443" }));
    const choice = await screen.findByRole("dialog", { name: "Hand over Coolify's domains" });
    const shop = (await within(choice).findByRole("checkbox", { name: "shop.example.com" })) as HTMLInputElement;
    const www = within(choice).getByRole("checkbox", { name: "www.shop.example.com" }) as HTMLInputElement;
    const docsWww = within(choice).getByRole("checkbox", { name: "www.docs.example.com" }) as HTMLInputElement;
    const row = www.closest("li");
    expect(row).toHaveTextContent("Redirects to shop.example.com");
    expect(row).toHaveTextContent("302 to shop.example.com, as its redirect does");
    // Chosen already with the domain it lands on, and not where that one isn't.
    expect(shop.checked && www.checked).toBe(true);
    expect(docsWww.checked).toBe(false);
    // Leaving the domain it lands on leaves it too; choosing it chooses that one.
    await user.click(shop);
    expect(shop.checked || www.checked).toBe(false);
    await user.click(www);
    expect(shop.checked && www.checked).toBe(true);
    await user.click(docsWww);
    expect((within(choice).getByRole("checkbox", { name: "docs.example.com" }) as HTMLInputElement).checked).toBe(true);
    await expectAccessible(choice);
    await user.click(within(choice).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(planPanelDomains).toHaveBeenCalledTimes(1));
    expect(planPanelDomains.mock.calls[0]?.[0]?.domains).toEqual([
      "docs.example.com",
      "shop.example.com",
      "www.docs.example.com",
      "www.shop.example.com",
    ]);
  });

  it("never offers a redirect whose domain stays behind, nor chooses that domain with it", async () => {
    const user = userEvent.setup();
    const checkPanelDomains = vi.fn((_req: CheckPanelDomainsRequest) =>
      create(CheckPanelDomainsResponseSchema, {
        domains: [
          { domain: "shop.example.com", held: "shop stays where it is. Add it from shop's page once it's taken on" },
          {
            domain: "www.shop.example.com",
            redirectsTo: "https://shop.example.com",
            probes: [{ path: "/", answer: "302 to shop.example.com, as its redirect does", open: true }],
          },
        ],
      }),
    );
    const planPanelDomains = vi.fn((_req: PlanPanelDomainsRequest) => ({ change: handover }));
    renderAt(
      "/servers/srv_1/import",
      api({ imports: { importCoolify: () => found, checkPanelDomains, planPanelDomains } }),
    );
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    await user.click(await screen.findByRole("button", { name: "Hand over ports 80 and 443" }));
    const choice = await screen.findByRole("dialog", { name: "Hand over Coolify's domains" });
    const shop = (await within(choice).findByRole("checkbox", { name: "shop.example.com" })) as HTMLInputElement;
    const www = within(choice).getByRole("checkbox", { name: "www.shop.example.com" }) as HTMLInputElement;
    expect(www.disabled && !www.checked).toBe(true);
    expect(shop.checked).toBe(false);
  });

  it("offers the proxy's start where it's stopped and no domain is a route", async () => {
    const user = userEvent.setup();
    const planStartPanel = vi.fn((_req: { serverId: string; panel: PanelKind; proxy: boolean }) => ({
      change: handover,
    }));
    const stopped = () =>
      create(ListContainersResponseSchema, {
        containers: [
          create(ServerContainerSchema, {
            name: "coolify",
            image: "ghcr.io/coollabsio/coolify:4.7.5",
            state: ContainerState.RUNNING,
          }),
          create(ServerContainerSchema, { name: "coolify-proxy", image: "traefik:v3.1", state: ContainerState.EXITED }),
        ],
      });
    renderAt(
      "/servers/srv_1/import",
      api({ containers: { listContainers: stopped }, imports: { importCoolify: () => found, planStartPanel } }),
    );
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    expect(screen.queryByRole("button", { name: "Hand over ports 80 and 443" })).toBeNull();
    await user.click(await screen.findByRole("button", { name: "Start coolify-proxy again" }));
    await waitFor(() => expect(planStartPanel).toHaveBeenCalledTimes(1));
    expect(planStartPanel.mock.calls[0]?.[0]).toMatchObject({
      serverId: "srv_1",
      panel: PanelKind.COOLIFY,
      proxy: true,
    });
  });

  it("leaves an app that needs host access as a state, and takes it on with that once chosen", async () => {
    const user = userEvent.setup();
    const media = (chosen: boolean) =>
      create(ImportCoolifyResponseSchema, {
        found: { kind: PanelKind.COOLIFY, version: "4.7.5", knownVersion: true, folder: "/data/coolify" },
        steps: [
          {
            kind: ImportKind.APP,
            name: "media",
            summary: "a service from Coolify's templates, in personal / production",
            action: chosen ? ImportAction.TAKE_ON : ImportAction.LEAVE,
            becomes: chosen
              ? "the project media, taken on as it runs, with no restart, its volumes where they are"
              : "",
            appId: "j6r2",
            needsHostAccess: ["/dev/dri/renderD128", "all GPUs"],
          },
        ],
      });
    const importCoolify = vi.fn((req: { hostAccess: string[] }) => media(req.hostAccess.includes("j6r2")));
    renderAt("/servers/srv_1/import", api({ imports: { importCoolify } }));
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    const choice = await screen.findByRole("checkbox", { name: "Take it on with /dev/dri/renderD128, all GPUs" });
    expect(screen.getByText("Needs host access")).toBeTruthy();
    expect((choice as HTMLInputElement).checked).toBe(false);
    expect(screen.queryByRole("button", { name: "Review the plan" })).toBeNull();
    await user.click(choice);
    await waitFor(() =>
      expect(importCoolify).toHaveBeenLastCalledWith(
        expect.objectContaining({ hostAccess: ["j6r2"] }),
        expect.anything(),
      ),
    );
    expect(await screen.findAllByRole("button", { name: "Review the plan" })).not.toHaveLength(0);
    expect(
      (screen.getByRole("checkbox", { name: "Take it on with /dev/dri/renderD128, all GPUs" }) as HTMLInputElement)
        .checked,
    ).toBe(true);
    await expectAccessible(document.body);
  });

  it("leaves an app whose database password is one of its own names as a state, and takes it on as it is once chosen", async () => {
    const user = userEvent.setup();
    const choiceName = "Take it on as it is: its password is one of its own names, which anyone can guess.";
    const newsletter = (chosen: boolean) =>
      create(ImportCoolifyResponseSchema, {
        found: { kind: PanelKind.COOLIFY, version: "4.7.5", knownVersion: true, folder: "/data/coolify" },
        steps: [
          {
            kind: ImportKind.APP,
            name: "newsletter",
            summary: "a service from Coolify's templates, in personal / production",
            action: chosen ? ImportAction.TAKE_ON : ImportAction.LEAVE,
            becomes: chosen ? "the project newsletter, taken on as it runs, with no restart" : "",
            appId: "k8q4",
            guessablePassword: true,
          },
        ],
      });
    const importCoolify = vi.fn((req: { guessablePassword: string[] }) =>
      newsletter(req.guessablePassword.includes("k8q4")),
    );
    renderAt("/servers/srv_1/import", api({ imports: { importCoolify } }));
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    const choice = await screen.findByRole("checkbox", { name: choiceName });
    expect(screen.getByText("Guessable password")).toBeTruthy();
    expect(screen.queryByText("Needs host access")).toBeNull();
    expect((choice as HTMLInputElement).checked).toBe(false);
    expect(screen.queryByRole("button", { name: "Review the plan" })).toBeNull();
    await user.click(choice);
    await waitFor(() =>
      expect(importCoolify).toHaveBeenLastCalledWith(
        expect.objectContaining({ guessablePassword: ["k8q4"], hostAccess: [] }),
        expect.anything(),
      ),
    );
    expect(await screen.findAllByRole("button", { name: "Review the plan" })).not.toHaveLength(0);
    expect((screen.getByRole("checkbox", { name: choiceName }) as HTMLInputElement).checked).toBe(true);
    await expectAccessible(document.body);
  });

  it("says a key, where the secret that's one of its own names is a key", async () => {
    const user = userEvent.setup();
    const importCoolify = vi.fn(() =>
      create(ImportCoolifyResponseSchema, {
        found: { kind: PanelKind.COOLIFY, version: "4.7.5", knownVersion: true, folder: "/data/coolify" },
        steps: [
          {
            kind: ImportKind.APP,
            name: "rentals",
            summary: "a service from Coolify's templates",
            action: ImportAction.LEAVE,
            appId: "r2d4",
            guessablePassword: true,
            guessable: GuessableSecret.KEY,
          },
        ],
      }),
    );
    renderAt("/servers/srv_1/import", api({ imports: { importCoolify } }));
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    expect(
      await screen.findByRole("checkbox", {
        name: "Take it on as it is: its key is one of its own names, which anyone can guess.",
      }),
    ).toBeTruthy();
    expect(screen.getByText("Guessable key")).toBeTruthy();
  });
});
