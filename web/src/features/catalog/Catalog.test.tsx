// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeSchema,
  ChangeState,
  EntrySchema,
  InputType,
  type InstallCatalogEntryRequest,
  PlanAction,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, ENV, places, SERVER } from "../../test/apps";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const postgres = create(EntrySchema, {
  id: "postgres",
  name: "PostgreSQL",
  summary: "Relational database, with Adminer to manage it",
  category: "databases",
  version: "17.11",
  appLicense: "PostgreSQL",
  firstSteps: "Add a domain first, then sign in with the POSTGRES_PASSWORD secret.",
  homepage: "https://www.postgresql.org",
  web: { service: "adminer" },
  inputs: [
    { name: "ADMIN_PORT", label: "Adminer port", type: InputType.PORT, defaultValue: "8081" },
    { name: "POSTGRES_PASSWORD", label: "Database password", type: InputType.SECRET, generate: 32 },
  ],
});

const tunnel = create(EntrySchema, {
  id: "cloudflared",
  name: "Cloudflare Tunnel",
  summary: "Reach apps on this server with no open port",
  category: "networking",
  version: "2026.10.3",
  appLicense: "Apache-2.0",
  hostAccess: "The server's own network, so the tunnel reaches routes on this server's port 80.",
  inputs: [{ name: "TUNNEL_TOKEN", label: "Tunnel token", type: InputType.SECRET }],
});

const proxy = create(EntrySchema, {
  id: "nginx-proxy-manager",
  name: "Nginx Proxy Manager",
  summary: "Reverse proxy with a web console",
  category: "networking",
  version: "2.16.0",
  appLicense: "MIT",
  inputs: [{ name: "ADMIN_PASSWORD", label: "Console password", type: InputType.SECRET, generate: 32, logged: true }],
});

function api(more: Api["catalog"] = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return {
    servers: { watchServers: watch.stream },
    catalog: {
      listCatalogEntries: () => ({ entries: [tunnel, postgres] }),
      getCatalogEntry: (req) => {
        const values: Record<string, string> = req.id === "postgres" ? { ADMIN_PORT: "8085" } : {};
        const entry = req.id === "postgres" ? postgres : req.id === proxy.id ? proxy : tunnel;
        const parts =
          req.id === "postgres"
            ? [
                { service: "adminer", image: "adminer:6.2.0", opens: true },
                { service: "db", image: "postgres:17.11", database: true, volumes: ["data"] },
              ]
            : [];
        return { entry, project: req.id, values, parts };
      },
      ...more,
    },
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("The catalog", () => {
  it("lists what it has, and filters by word and by category", async () => {
    const user = userEvent.setup();
    const { container } = renderAt("/catalog", api());
    await screen.findByRole("heading", { level: 1, name: "Catalog" });
    expect(await screen.findByRole("link", { name: "Install PostgreSQL" })).toHaveAttribute(
      "href",
      "/catalog/postgres",
    );
    const chips = screen.getByRole("group", { name: "Categories" });
    await user.click(within(chips).getByRole("button", { name: "Networking" }));
    await waitFor(() => expect(screen.queryByRole("link", { name: "Install PostgreSQL" })).not.toBeInTheDocument());
    expect(screen.getByRole("link", { name: "Install Cloudflare Tunnel" })).toBeVisible();
    await user.click(within(chips).getByRole("button", { name: "All" }));
    await user.type(screen.getByRole("textbox", { name: "Search apps" }), "zzz");
    expect(await screen.findByText(/No app matches that/)).toBeVisible();
    await expectAccessible(container);
  });

  it("leads to the app once it's installed, and says how many are", async () => {
    renderAt(
      "/catalog",
      api({
        listCatalogInstalls: () => ({
          installs: [
            { project: "postgres", pin: { entry: "postgres", version: "17.11" }, webServiceId: "svc_adminer" },
          ],
        }),
      }),
    );
    const card = await screen.findByRole("link", { name: "Open PostgreSQL" });
    expect(card).toHaveAttribute("href", "/apps/svc_adminer");
    expect(within(card).getByText("Installed")).toBeVisible();
    expect(screen.getByRole("link", { name: "1 installed" })).toHaveAttribute("href", "/apps");
  });

  it("says when it can't read the catalog", async () => {
    renderAt(
      "/catalog",
      api({
        listCatalogEntries: () => {
          throw new ConnectError("the control plane couldn't complete the request", Code.Internal);
        },
      }),
    );
    expect(await screen.findByText("Couldn't read the catalog")).toBeVisible();
  });
});

describe("A newer release", () => {
  const behind = {
    installs: [
      {
        project: "postgres",
        projectId: "prj_1",
        environmentId: ENV,
        pin: { entry: "postgres", version: "17.11", release: 1 },
        webServiceId: "svc_adminer",
        name: "PostgreSQL",
        upgrade: { version: "17.13", release: 2 },
      },
    ],
  };

  it("reads as one in the catalog and on the rail, which leads to the app", async () => {
    renderAt("/catalog", api({ listCatalogInstalls: () => behind }));
    const card = await screen.findByRole("link", { name: "Open PostgreSQL" });
    expect(await within(card).findByText("Upgrade to 17.11")).toBeVisible();
    // What it runs, not what's out.
    expect(within(card).getByText("17.11", { exact: true })).toBeVisible();
    expect(
      await within(screen.getByRole("navigation", { name: "Main" })).findByRole("link", { name: "Catalog 1" }),
    ).toBeVisible();
  });

  it("words a change at the same version as what it is", async () => {
    const sameVersion = {
      installs: [{ ...behind.installs[0], upgrade: { version: "17.11", release: 2 } }],
    };
    renderAt("/apps/svc_adminer", {
      ...api({ listCatalogInstalls: () => sameVersion }),
      resources: places(),
      services: { listServices: () => ({ services: [anApp("adminer", "adminer:6.2.0")] }) },
    });
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    expect(await within(panel).findByText("A change to how 17.11 runs is out.")).toBeVisible();
    expect(within(panel).getByRole("button", { name: "Review the change" })).toBeVisible();
    expect(within(panel).queryByText(/Version 17.11 is out/)).not.toBeInTheDocument();
  });

  it("offers its plan on the app's page, by the project", async () => {
    const user = userEvent.setup();
    let asked = "";
    renderAt("/apps/svc_adminer", {
      ...api({
        listCatalogInstalls: () => behind,
        upgradeCatalogInstall: (req) => {
          asked = req.id;
          throw new ConnectError("nothing more to do here", Code.FailedPrecondition);
        },
      }),
      resources: places(),
      services: { listServices: () => ({ services: [anApp("adminer", "adminer:6.2.0")] }) },
    });
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    expect(await within(panel).findByText("Version 17.13 is out.")).toBeVisible();
    await user.click(within(panel).getByRole("button", { name: "Upgrade to 17.13" }));
    await waitFor(() => expect(asked).toBe("prj_1"));
  });
});

describe("Installing from the catalog", () => {
  it("says what it installs before the plan: the app you open, then its database, each by image", async () => {
    const { container } = renderAt("/catalog/postgres", api());
    const parts = await screen.findByRole("region", { name: "What it installs" });
    expect(within(parts).getByText("Each image pinned by digest")).toBeVisible();
    const text = parts.textContent ?? "";
    expect(text.indexOf("adminer")).toBeLessThan(text.indexOf("postgres:17.11"));
    for (const word of ["App you open", "adminer:6.2.0", "Database", "postgres:17.11", "data"]) {
      expect(within(parts).getByText(word)).toBeVisible();
    }
    await expectAccessible(container);
  });

  it("starts from what's free, asks nothing it can generate, and plans it", async () => {
    const user = userEvent.setup();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, {
      id: "chg_install",
      summary: "Install PostgreSQL 17.11 as postgres",
      state: ChangeState.PLANNED,
      plan: {
        fingerprint: "feedfacefeedfacefeedfacefeedface",
        summary: { create: 2 },
        resources: [
          {
            id: "svc_adminer",
            resource: { kind: "service", scope: "env_1", name: "adminer" },
            action: PlanAction.CREATE,
          },
        ],
      },
    });
    const { container } = renderAt("/catalog/postgres", {
      ...api({
        installCatalogEntry: (req) => {
          asked = req;
          return { change };
        },
      }),
      changes: {
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "Install PostgreSQL" });
    expect(await within(sheet).findByLabelText("Adminer port")).toHaveValue("8085");
    expect(within(sheet).getByLabelText("Name")).toHaveValue("postgres");
    // A password no one types has no field: it's made and sealed where it runs.
    expect(within(sheet).queryByLabelText("Database password")).not.toBeInTheDocument();
    expect(within(sheet).getByText(/Dockwell generates Database password and keeps it sealed/)).toBeVisible();
    // What to do first is here, before anyone reviews a plan, let alone applies it.
    expect(within(sheet).getByText(/Add a domain first, then sign in/)).toBeVisible();
    await expectAccessible(container);

    await user.clear(within(sheet).getByLabelText("Adminer port"));
    await user.type(within(sheet).getByLabelText("Adminer port"), "9090");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.id).toBe("postgres");
    expect(asked?.project).toBe("postgres");
    expect(asked?.values).toEqual({ ADMIN_PORT: "9090" });
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
  });

  it("needs what only a person has, keeps it out of sight, and says what the app reaches", async () => {
    const user = userEvent.setup();
    renderAt("/catalog/cloudflared", api());
    const sheet = await screen.findByRole("dialog", { name: "Install Cloudflare Tunnel" });
    const token = await within(sheet).findByLabelText("Tunnel token");
    expect(token).toHaveAttribute("type", "password");
    expect(within(sheet).getByText(/It reaches past its containers/)).toBeVisible();
    const review = within(sheet).getByRole("button", { name: /Review plan/ });
    expect(review).toBeDisabled();
    expect(within(sheet).getByText("Give Tunnel token a value to review the plan")).toBeVisible();
    await user.type(token, "a-token");
    expect(review).toBeEnabled();
  });

  it("doesn't call a password sealed when the app writes it in its log, and says to change it", async () => {
    renderAt("/catalog/nginx-proxy-manager", api());
    const sheet = await screen.findByRole("dialog", { name: "Install Nginx Proxy Manager" });
    expect(await within(sheet).findByText(/writes it in its log/)).toBeVisible();
    expect(within(sheet).getByText(/Change it after you first sign in/)).toBeVisible();
    expect(within(sheet).queryByText(/sealed/)).not.toBeInTheDocument();
  });

  it("shows a taken name at its field", async () => {
    const user = userEvent.setup();
    renderAt(
      "/catalog/postgres",
      api({
        installCatalogEntry: () => {
          const err = new ConnectError("a project named postgres is here already.", Code.AlreadyExists);
          err.metadata.set("Dockwell-Error", "project_exists");
          throw err;
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install PostgreSQL" });
    await within(sheet).findByLabelText("Adminer port");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await within(sheet).findByText(/a project named postgres is here already/i)).toBeVisible();
    expect(within(sheet).getByLabelText("Name")).toHaveAttribute("aria-invalid", "true");
    expect(within(sheet).queryByText("Can't make a plan")).not.toBeInTheDocument();
  });
});
