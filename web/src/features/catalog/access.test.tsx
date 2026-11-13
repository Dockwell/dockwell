// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import {
  EntrySchema,
  InputType,
  type ProposeChangeRequest,
  Role,
  StepUpAction,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, ENV, places, SERVER } from "../../test/apps";
import { authError, authState, mira, stubPasskeys } from "../../test/auth";
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
    expect(within(known).getByText("Its links follow the domain you add: GITEA__server__ROOT_URL")).toBeVisible();
    expect(within(known).getByText("Git over SSH answers on its SSH port, on this server alone.")).toBeVisible();
    expect(screen.queryByText("What to do first")).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("asks for an email starting from yours, with nothing to do first", async () => {
    renderAt("/catalog/grafana/install", api(grafana));
    const sheet = await screen.findByRole("dialog", { name: "Install Grafana" });
    expect(await within(sheet).findByRole("group", { name: "Your email" })).toHaveTextContent("mira@example.com");
    expect(within(sheet).getByRole("button", { name: "Advanced" })).toHaveAccessibleDescription(/1 secret generated/);
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
    // The setting is the app's own, shown as it is (ADR 0081), not a variable every app shares.
    const signups = proposed[0]?.edits ?? [];
    expect(signups.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual(["service gitea"]);
    const closing = signups[0]?.spec?.kind;
    expect(closing?.case === "service" && closing.value.runs.case === "app" && closing.value.runs.value.env).toEqual({
      GITEA__service__DISABLE_REGISTRATION: "true",
    });
    expect(closing?.case === "service" && closing.value.shownEnv).toContain("GITEA__service__DISABLE_REGISTRATION");
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
        // A panel at its domain, which doesn't open apps yet (ADR 0061, slice 1).
        auth: { getAuthState: () => authState({ rpId: "panel.example.com" }) },
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

  /** Gitea, published on loopback alone, which opens through the panel, with a passkey. */
  function openable(
    open: () => unknown,
    more: { ports?: { hostIp: string; hostPort: number; containerPort: number }[]; domains?: string[] } = {},
  ) {
    const asked: { id: string; port: number; proof: string }[] = [];
    const steps: unknown[] = [];
    return {
      asked,
      steps,
      api: api(gitea, {
        auth: {
          getAuthState: () => authState({ user: { ...mira, email: "mira@example.com" }, rpId: "localhost" }),
          beginStepUp: (req) => {
            steps.push(req);
            return { ceremonyId: "cer_1", optionsJson: JSON.stringify({ challenge: "q83v", rpId: "localhost" }) };
          },
          finishStepUp: () => ({ proof: "proof_1" }),
        },
        services: {
          listServices: () => ({
            services: [
              anApp("gitea", "gitea/gitea:1.25", {
                ports: more.ports ?? [{ hostIp: "127.0.0.1", hostPort: 3000, containerPort: 3000 }],
              }),
            ],
          }),
          openService: (req: { id: string; port: number; proof: string }) => {
            asked.push({ id: req.id, port: req.port, proof: req.proof });
            // As the control plane: a passkey, once nothing else refuses.
            if (req.proof === "") throw authError(Code.PermissionDenied, "needs_step_up");
            open();
            return {
              url: "http://abcde23456.localhost:7600/.dockwell/open?ticket=t",
              origin: "http://abcde23456.localhost:7600",
            };
          },
        },
        domains: { listDomains: () => ({ domains: (more.domains ?? []).map((name) => ({ name })) }) },
      }),
    };
  }

  it("opens it through the panel on localhost with a passkey, in a tab of its own", async () => {
    const user = userEvent.setup();
    const passkeys = stubPasskeys();
    const tab = { opener: {} as unknown, location: { href: "" }, close: vi.fn() };
    vi.stubGlobal(
      "open",
      vi.fn(() => tab),
    );
    const stand = openable(() => {});
    renderAt("/apps/svc_gitea", stand.api);
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    await user.click(await within(panel).findByRole("button", { name: "Open Gitea" }));
    await waitFor(() => expect(tab.location.href).toBe("http://abcde23456.localhost:7600/.dockwell/open?ticket=t"));
    expect(passkeys.get).toHaveBeenCalledOnce();
    expect(stand.steps).toEqual([expect.objectContaining({ action: StepUpAction.OPEN_SERVICE, target: "svc_gitea" })]);
    expect(stand.asked).toEqual([
      { id: "svc_gitea", port: 3000, proof: "" },
      { id: "svc_gitea", port: 3000, proof: "proof_1" },
    ]);
    expect(tab.opener).toBeNull();
  });

  it("keeps the tunnel beside Open on localhost, for an app that takes only its own address", async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    renderAt("/apps/svc_gitea", openable(() => {}).api);
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    const local = (await within(panel).findByText("Answers on this server alone, on port 3000, until it has a domain"))
      .parentElement as HTMLElement;
    expect(within(local).getByRole("button", { name: "Open Gitea" })).toBeVisible();
    await user.click(within(local).getByRole("button", { name: "Copy the SSH tunnel command" }));
    expect(writeText).toHaveBeenCalledWith("ssh -N -L 3000:127.0.0.1:3000 203.0.113.7");
  });

  it("gives the address as a link to press when the browser holds the tab back", async () => {
    const user = userEvent.setup();
    stubPasskeys();
    vi.stubGlobal(
      "open",
      vi.fn(() => null),
    );
    renderAt("/apps/svc_gitea", openable(() => {}).api);
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    await user.click(await within(panel).findByRole("button", { name: "Open Gitea" }));
    const link = await within(panel).findByRole("link", { name: "Open Gitea in a new tab" });
    expect(link).toHaveAttribute("href", "http://abcde23456.localhost:7600/.dockwell/open?ticket=t");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
  });

  it("opens it on localhost from any port it publishes, with a domain or without", async () => {
    renderAt(
      "/apps/svc_gitea",
      openable(() => {}, { ports: [{ hostIp: "", hostPort: 3000, containerPort: 3000 }], domains: ["git.example.com"] })
        .api,
    );
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    expect(await within(panel).findByText("Published on port 3000")).toBeVisible();
    expect(within(panel).getByRole("button", { name: "Open Gitea" })).toBeVisible();
  });

  it("leaves Close sign-ups and the sign-in's Reveal in place for a viewer, off, with why", async () => {
    renderAt(
      "/apps/svc_gitea",
      api(gitea, {
        auth: { getAuthState: () => authState({ user: { ...mira, role: Role.VIEWER } }) },
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
    const close = await within(panel).findByRole("button", { name: "Close sign-ups" });
    await waitFor(() => expect(close).toHaveAttribute("aria-disabled", "true"));
    expect(close).toHaveAccessibleDescription(/developer or more/);
    const open = await within(panel).findByRole("button", { name: "Open Gitea" });
    expect(open).toHaveAttribute("aria-disabled", "true");
    expect(open).toHaveAccessibleDescription(/developer or more/);
  });
});
