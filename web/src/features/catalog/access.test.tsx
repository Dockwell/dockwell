// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { EntrySchema, InputType, type ProposeChangeRequest, Role, type WatchServersResponse } from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, ENV, places, SERVER } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const gitea = create(EntrySchema, {
  id: "gitea",
  name: "Gitea",
  summary: "Git hosting",
  category: "developer",
  version: "1.25",
  appLicense: "MIT",
  web: { service: "gitea" },
  firstVisitorOwns: true,
  signups: { service: "gitea", variable: "GITEA__service__DISABLE_REGISTRATION", closed: "true" },
  addressVariables: [{ service: "gitea", variable: "GITEA__server__ROOT_URL", value: "https://{domain}/" }],
  notes: ["Git over SSH answers on its SSH port, on this server alone."],
  inputs: [{ name: "PORT", label: "Port on this server", type: InputType.PORT, defaultValue: "3000" }],
});

const grafana = create(EntrySchema, {
  id: "grafana",
  name: "Grafana",
  summary: "Dashboards",
  category: "monitoring",
  version: "12",
  appLicense: "AGPL-3.0-only",
  web: { service: "grafana" },
  signIn: { user: "admin", passwordInput: "GRAFANA_PASSWORD", changedIn: "your profile" },
  inputs: [
    { name: "ADMIN_EMAIL", label: "Your email", type: InputType.EMAIL },
    { name: "GRAFANA_PASSWORD", label: "Admin password", type: InputType.SECRET, generate: 32, setOnce: true },
  ],
});

function api(entry = gitea, more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(
    changed(
      aServer({ id: SERVER, name: "hz-1", inventory: { osName: "Ubuntu 24.04.1 LTS", addresses: ["203.0.113.7"] } }),
    ),
    snapshotComplete,
  );
  return {
    servers: { watchServers: watch.stream },
    resources: places(),
    auth: { getAuthState: () => authState({ user: { ...mira, email: "mira@example.com" } }) },
    catalog: {
      listCatalogEntries: () => ({ entries: [entry] }),
      getCatalogEntry: () => ({ entry, project: entry.id, values: {}, parts: [] }),
      listCatalogInstalls: () => ({
        installs: [
          {
            project: entry.id,
            projectId: "prj_1",
            environmentId: ENV,
            pin: { entry: entry.id, version: entry.version, release: 1, values: {} },
            webServiceId: `svc_${entry.web?.service}`,
            name: entry.name,
          },
        ],
      }),
    },
    ...more,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("What an entry says of itself", () => {
  it("reads on its store page as state, not steps", async () => {
    const { container } = renderAt("/catalog/gitea", api());
    const known = await screen.findByRole("region", { name: "Good to know" });
    expect(within(known).getByText("Its first account is its owner")).toBeVisible();
    expect(within(known).getByText(/Anyone who reaches it can make an account/)).toBeVisible();
    expect(within(known).getByText("GITEA__server__ROOT_URL follow the domain you add")).toBeVisible();
    expect(within(known).getByText("Git over SSH answers on its SSH port, on this server alone.")).toBeVisible();
    expect(screen.queryByText("What to do first")).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("asks for an email starting from yours, and says how it signs in in a line", async () => {
    renderAt("/catalog/grafana/install", api(grafana));
    const sheet = await screen.findByRole("dialog", { name: "Install Grafana" });
    expect(await within(sheet).findByLabelText("Your email")).toHaveValue("mira@example.com");
    expect(within(sheet).getByText("Generated and sealed, for your passkey to reveal: Admin password")).toBeVisible();
    expect(within(sheet).queryByText("What to do first")).not.toBeInTheDocument();
  });
});

describe("What an installed app says of itself", () => {
  it("shows how to sign in, with the password to reveal and where it changes", async () => {
    const user = userEvent.setup();
    const revealed: string[] = [];
    renderAt(
      "/apps/svc_grafana",
      api(grafana, {
        services: { listServices: () => ({ services: [anApp("grafana", "grafana/grafana:12")] }) },
        variables: {
          listVariables: () => ({
            environmentId: ENV,
            variables: [{ id: "sec_g", name: "GRAFANA_PASSWORD", secret: true, revision: 1n }],
          }),
          revealSecret: (req: { id: string }) => {
            revealed.push(req.id);
            return { value: "generated-for-grafana", revision: 1n };
          },
        },
      }),
    );
    const card = await screen.findByRole("region", { name: "Sign in" });
    expect(within(card).getByText("admin")).toBeVisible();
    expect(within(card).getByText("Set as it first started: change it in Grafana, in your profile")).toBeVisible();
    await user.click(await within(card).findByRole("button", { name: "Reveal admin password" }));
    expect(await within(card).findByText("generated-for-grafana")).toBeVisible();
    expect(revealed).toEqual(["sec_g"]);
  });

  it("closes sign-ups, and points what follows its domain at the one it has, each through a plan", async () => {
    const user = userEvent.setup();
    const proposed: ProposeChangeRequest[] = [];
    renderAt(
      "/apps/svc_gitea",
      api(gitea, {
        services: {
          listServices: () => ({
            services: [anApp("gitea", "gitea/gitea:1.25", { env: { GITEA__server__ROOT_URL: "" } })],
          }),
        },
        domains: {
          listDomains: () => ({
            domains: [{ name: "git.example.com", environmentId: ENV, route: { service: "gitea", port: 3000 } }],
          }),
        },
        changes: {
          proposeChange: (req: ProposeChangeRequest) => {
            proposed.push(req);
            return { change: { id: "chg_1", summary: req.summary, state: 1 } };
          },
        },
      }),
    );
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    await user.click(await within(panel).findByRole("button", { name: "Close sign-ups" }));
    await waitFor(() => expect(proposed).toHaveLength(1));
    const signups = proposed[0]?.edits ?? [];
    expect(signups.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual([
      "variable GITEA__service__DISABLE_REGISTRATION",
      "service gitea",
    ]);
    await user.keyboard("{Escape}");
    await user.click(await within(panel).findByRole("button", { name: "Set to git.example.com" }));
    await waitFor(() => expect(proposed).toHaveLength(2));
    const address = proposed[1]?.edits ?? [];
    expect(address).toHaveLength(1);
    const service = address[0]?.spec?.kind;
    expect(service?.case === "service" && service.value.runs.case === "app" && service.value.runs.value.env).toEqual({
      GITEA__server__ROOT_URL: "https://git.example.com/",
    });
  });

  it("answers only on this server until it has a domain, with the tunnel command to copy", async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    renderAt(
      "/apps/svc_gitea",
      api(gitea, {
        services: {
          listServices: () => ({
            services: [
              anApp("gitea", "gitea/gitea:1.25", {
                ports: [{ hostIp: "127.0.0.1", hostPort: 3000, containerPort: 3000 }],
              }),
            ],
          }),
        },
        domains: { listDomains: () => ({ domains: [] }) },
      }),
    );
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    expect(
      await within(panel).findByText("Answers on this server alone, on port 3000, until it has a domain"),
    ).toBeVisible();
    await user.click(within(panel).getByRole("button", { name: "Copy the SSH tunnel command" }));
    expect(writeText).toHaveBeenCalledWith("ssh -N -L 3000:127.0.0.1:3000 203.0.113.7");
    expect(within(panel).getByText("Its first account is its owner: make yours before it has a domain")).toBeVisible();
  });

  it("leaves Close sign-ups and the sign-in's Reveal in place for a viewer, off, with why", async () => {
    renderAt(
      "/apps/svc_gitea",
      api(gitea, {
        auth: { getAuthState: () => authState({ user: { ...mira, role: Role.VIEWER } }) },
        services: { listServices: () => ({ services: [anApp("gitea", "gitea/gitea:1.25")] }) },
        domains: { listDomains: () => ({ domains: [] }) },
      }),
    );
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    const close = await within(panel).findByRole("button", { name: "Close sign-ups" });
    await waitFor(() => expect(close).toHaveAttribute("aria-disabled", "true"));
    expect(close).toHaveAccessibleDescription(/developer or more/);
  });
});
