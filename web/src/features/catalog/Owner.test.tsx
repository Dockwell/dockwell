// SPDX-License-Identifier: AGPL-3.0-only

import { clone, create } from "@bufbuild/protobuf";
import { CatalogOwnerState, EntrySchema, InputType, SignupsSchema, type WatchServersResponse } from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, ENV, places, SERVER } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

// Gitea with an owner Dockwell makes, as ADR 0105 has it.
const gitea = create(EntrySchema, {
  id: "gitea",
  name: "Gitea",
  summary: "Git hosting",
  category: "developer",
  version: "28.3.0",
  appLicense: "MIT",
  web: { service: "gitea" },
  owner: { service: "gitea", emailInput: "OWNER_EMAIL", userInput: "OWNER_USER", passwordInput: "OWNER_PASSWORD" },
  inputs: [
    { name: "PORT", label: "Port on this server", type: InputType.PORT, defaultValue: "3000" },
    { name: "OWNER_EMAIL", label: "Owner's email", type: InputType.EMAIL },
    { name: "OWNER_PASSWORD", label: "Owner's password", type: InputType.SECRET, generate: 24 },
  ],
});

/** The app page of a Gitea install, with what its install says of its owner. */
function api(install: Record<string, unknown>, more: Api = {}, held = false): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return {
    servers: { watchServers: watch.stream },
    resources: places(),
    auth: { getAuthState: () => authState({ user: { ...mira, email: "mira@example.com" } }) },
    services: { listServices: () => ({ services: [anApp("gitea", "gitea/gitea:28.3.0")] }) },
    domains: {
      listDomains: () => ({
        domains: held
          ? [
              {
                name: "git.example.com",
                environmentId: ENV,
                routes: [{ id: "rte_1", name: "www", service: "gitea", held: true, tls: "" }],
              },
            ]
          : [],
      }),
    },
    catalog: {
      listCatalogEntries: () => ({ entries: [gitea] }),
      getCatalogEntry: () => ({ entry: gitea, project: "gitea", values: {}, parts: [] }),
      listCatalogInstalls: () => ({
        installs: [
          {
            project: "gitea",
            projectId: "prj_1",
            environmentId: ENV,
            pin: { entry: "gitea", version: "28.3.0", release: 5, values: {} },
            webServiceId: "svc_gitea",
            name: "Gitea",
            ...install,
          },
        ],
      }),
      ...(more.catalog ?? {}),
    },
    ...Object.fromEntries(Object.entries(more).filter(([k]) => k !== "catalog")),
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("An install's owner, on its app's page (ADR 0105)", () => {
  it("once made, is a card with who, and the password a passkey reveals", async () => {
    const user = userEvent.setup();
    const revealed: string[] = [];
    const { container } = renderAt(
      "/apps/svc_gitea",
      api(
        { owner: { email: "mira@example.com", user: "mira", state: CatalogOwnerState.MADE } },
        {
          variables: {
            listVariables: () => ({
              environmentId: ENV,
              variables: [{ id: "sec_o", name: "OWNER_PASSWORD", secret: true, revision: 1n }],
            }),
            revealSecret: (req: { id: string }) => {
              revealed.push(req.id);
              return { value: "Ow9Kx7Vq2mRt9Lw4zBpN8cT3", revision: 1n };
            },
          },
        },
      ),
    );
    const card = await screen.findByRole("region", { name: "Owner" });
    expect(within(card).getByText("mira@example.com")).toBeVisible();
    expect(within(card).getByText("mira")).toBeVisible();
    await user.click(await within(card).findByRole("button", { name: /Reveal/ }));
    expect(await within(card).findByText("Ow9Kx7Vq2mRt9Lw4zBpN8cT3")).toBeVisible();
    expect(revealed).toEqual(["sec_o"]);
    expect(screen.queryByText(/Held until/)).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("while it's made, says as whom and that it's held until then", async () => {
    renderAt(
      "/apps/svc_gitea",
      api({ held: true, owner: { email: "mira@example.com", state: CatalogOwnerState.WAITING } }),
    );
    expect(await screen.findByText("Making Gitea's owner account")).toBeVisible();
    expect(screen.getByText(/As mira@example.com, once gitea runs/)).toBeVisible();
    expect(screen.queryByRole("region", { name: "Owner" })).not.toBeInTheDocument();
  });

  it("where it failed, says what the app said, and tries again or lets you set it up yourself", async () => {
    const user = userEvent.setup();
    const retried: string[] = [];
    renderAt(
      "/apps/svc_gitea",
      api(
        { held: true, owner: { email: "mira@example.com", state: CatalogOwnerState.FAILED, error: "it answered 500" } },
        {
          catalog: {
            retryCatalogOwner: (req: { id: string }) => {
              retried.push(req.id);
              return { owner: { state: CatalogOwnerState.WAITING } };
            },
          },
        },
        true,
      ),
    );
    const title = await screen.findByText("Couldn't make Gitea's owner");
    const callout = title.closest(".dw-callout") as HTMLElement;
    expect(within(callout).getByText("it answered 500")).toBeVisible();
    await user.click(within(callout).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(retried).toEqual(["prj_1"]));
    await user.click(within(callout).getByRole("button", { name: "Set it up yourself" }));
    expect(await screen.findByText("Held until its owner account exists")).toBeVisible();
    expect(screen.getByRole("button", { name: "Set it up at git.example.com" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Open it to everyone" })).toBeVisible();
  });

  it("waiting for an email, makes it as yours, or another you type", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_gitea", api({ held: true, owner: { state: CatalogOwnerState.WAITING_EMAIL } }));
    expect(await screen.findByText("Gitea waits for its owner's email")).toBeVisible();
    expect(screen.getByRole("button", { name: "Make it as mira@example.com" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Another email" }));
    expect(screen.getByRole("textbox", { name: "Owner's email" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Make the owner" })).toBeDisabled();
  });

  it("held, makes a setup link to its domain and opens it in a tab of its own", async () => {
    const user = userEvent.setup();
    const asked: { id: string; domain: string }[] = [];
    const tab = { opener: {} as unknown, location: { href: "" }, close: vi.fn() };
    vi.stubGlobal(
      "open",
      vi.fn(() => tab),
    );
    renderAt(
      "/apps/svc_gitea",
      api(
        { held: true },
        {
          catalog: {
            setUpCatalogInstall: (req: { id: string; domain: string }) => {
              asked.push({ id: req.id, domain: req.domain });
              return {
                url: "https://git.example.com/.well-known/dockwell-setup?key=k",
                until: { seconds: BigInt(Math.floor(Date.now() / 1000) + 3600), nanos: 0 },
              };
            },
          },
        },
        true,
      ),
    );
    expect(await screen.findByText("Held until you set it up")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Set it up at git.example.com" }));
    await waitFor(() => expect(asked).toEqual([{ id: "prj_1", domain: "git.example.com" }]), { timeout: 5_000 });
    expect(tab.location.href).toBe("https://git.example.com/.well-known/dockwell-setup?key=k");
    expect(tab.opener).toBeNull();
    expect(
      await screen.findByText(/Your setup link to git.example.com works for 60 min/, undefined, { timeout: 5_000 }),
    ).toBeVisible();
  });

  it("counts a new link down from when it was made, not from when the page opened", async () => {
    const user = userEvent.setup();
    let clock = Date.UTC(2026, 10, 17, 12);
    vi.spyOn(Date, "now").mockImplementation(() => clock);
    vi.stubGlobal(
      "open",
      vi.fn(() => ({ opener: {}, location: { href: "" }, close: vi.fn() })),
    );
    renderAt(
      "/apps/svc_gitea",
      api(
        { held: true },
        {
          catalog: {
            setUpCatalogInstall: () => ({
              url: "https://git.example.com/.well-known/dockwell-setup?key=k",
              until: { seconds: BigInt(Math.floor(clock / 1000) + 3600), nanos: 0 },
            }),
          },
        },
        true,
      ),
    );
    expect(await screen.findByText("Held until you set it up")).toBeVisible();
    // Two minutes on the page before the link is made.
    clock += 120_000;
    await user.click(screen.getByRole("button", { name: "Set it up at git.example.com" }));
    expect(
      await screen.findByText(/Your setup link to git.example.com works for 60 min/, undefined, { timeout: 5_000 }),
    ).toBeVisible();
  });

  it("set up through its link, says so, with the way to open it to everyone", async () => {
    renderAt(
      "/apps/svc_gitea",
      api({ held: true, owner: { state: CatalogOwnerState.FAILED, error: "it answered 500", setUp: true } }, {}, true),
    );
    expect(await screen.findByText("Gitea is set up")).toBeVisible();
    expect(screen.getByRole("button", { name: "Open it to everyone" })).toBeVisible();
  });
});

describe("A setup link, as the panel offers it (the review's M6 and L5)", () => {
  /** The app page of a held Gitea whose domains' routes are as given. */
  function heldAt(routes: Record<string, unknown>[], names = ["git.example.com"]): Api {
    return {
      ...api({ held: true }),
      domains: {
        listDomains: () => ({
          domains: names.map((name) => ({
            name,
            environmentId: ENV,
            routes: routes.map((r) => ({ id: `rte_${name}`, name: "www", service: "gitea", held: true, ...r })),
          })),
        }),
      },
    };
  }

  it("offers no link to a domain only this server reaches, and opens the app through the panel instead", async () => {
    renderAt("/apps/svc_gitea", heldAt([{ tls: "off", plainForAnyone: false, trustedProxies: [] }]));
    expect(await screen.findByText("Held until you set it up")).toBeVisible();
    expect(screen.queryByRole("button", { name: /Set it up at/ })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open Gitea" })).toBeVisible();
  });

  it("offers it behind a proxy the route names", async () => {
    renderAt("/apps/svc_gitea", heldAt([{ tls: "off", plainForAnyone: false, trustedProxies: ["127.0.0.1"] }]));
    expect(await screen.findByRole("button", { name: "Set it up at git.example.com" })).toBeVisible();
  });

  it("counts a live link down on any page, as the domain's routes say when it ends", async () => {
    const until = BigInt(Math.floor(Date.now() / 1000) + 30 * 60);
    renderAt("/apps/svc_gitea", heldAt([{ tls: "", setupUntil: until }]));
    expect(await screen.findByText(/Your setup link to git.example.com works for 30 min/)).toBeVisible();
    expect(screen.getByRole("button", { name: "New setup link" })).toBeVisible();
  });

  it("names the first of several domains, and how many more", async () => {
    renderAt("/apps/svc_gitea", heldAt([{ tls: "" }], ["git.example.com", "code.example.com"]));
    expect(
      await screen.findByText(
        "git.example.com and 1 more answer that it isn't open yet, so only people in this project reach it.",
      ),
    ).toBeVisible();
  });

  it("says a long failure's step and last words, with all of it a step away", async () => {
    const user = userEvent.setup();
    const error =
      'Step 1: OCI runtime exec failed: \\"sh -c gitea admin user create --admin --username \\"$OWNER_USER\\" --random-password\\": executable file not found in $PATH';
    renderAt(
      "/apps/svc_gitea",
      api({ held: true, owner: { email: "mira@example.com", state: CatalogOwnerState.FAILED, error } }),
    );
    expect(await screen.findByText("Step 1: executable file not found in $PATH")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "All of it" }));
    expect(screen.getByText(/--username "\$OWNER_USER" --random-password/)).toBeVisible();
  });
});

describe("Sign-ups an install closed (ADR 0105)", () => {
  it("read closed as the service runs them, with nothing to do", async () => {
    const closedGitea = clone(EntrySchema, gitea);
    closedGitea.signups = create(SignupsSchema, {
      service: "gitea",
      variable: "GITEA__service__DISABLE_REGISTRATION",
      closed: "true",
    });
    renderAt("/apps/svc_gitea", {
      ...api({ owner: { email: "mira@example.com", state: CatalogOwnerState.MADE } }),
      services: {
        listServices: () => ({
          services: [anApp("gitea", "gitea/gitea:28.3.0", { env: { GITEA__service__DISABLE_REGISTRATION: "true" } })],
        }),
      },
      catalog: {
        listCatalogEntries: () => ({ entries: [closedGitea] }),
        getCatalogEntry: () => ({ entry: closedGitea, project: "gitea", values: {}, parts: [] }),
        listCatalogInstalls: () => ({
          installs: [
            {
              project: "gitea",
              projectId: "prj_1",
              environmentId: ENV,
              pin: { entry: "gitea", version: "28.3.0", release: 5, values: {} },
              webServiceId: "svc_gitea",
              name: "Gitea",
              owner: { email: "mira@example.com", state: CatalogOwnerState.MADE },
            },
          ],
        }),
      },
    });
    const panel = await screen.findByRole("region", { name: "From the catalog" });
    expect(await within(panel).findByText("Closed")).toBeVisible();
    expect(within(panel).queryByRole("button", { name: "Close sign-ups" })).not.toBeInTheDocument();
  });
});

describe("An install's owner, in Catalog", () => {
  it("marks a held install on its card, and its row on the app's page", async () => {
    renderAt("/catalog", api({ held: true, owner: { state: CatalogOwnerState.FAILED } }));
    expect(await screen.findByText("Owner not made")).toBeVisible();
  });
});
