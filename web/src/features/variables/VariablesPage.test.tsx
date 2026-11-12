// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  ActorKind,
  ChangeSchema,
  ChangeState,
  EnvironmentVariableSchema,
  type ListVariablesRequest,
  PlanAction,
  type ProposeChangeRequest,
  ResourceSchema,
  type RevealSecretRequest,
  StepUpAction,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, stubPasskeys } from "../../test/auth";
import { type Api, onPhone, quietWatch, renderAt } from "../../test/render";
import { REVEALED_FOR } from "./useReveal";

const mira = { kind: ActorKind.USER, id: "usr_1", name: "Mira Chen" };

const production = create(ResourceSchema, {
  id: "env_prod",
  resource: { kind: "environment", scope: "prj_default", name: "production" },
});
const staging = create(ResourceSchema, {
  id: "env_staging",
  resource: { kind: "environment", scope: "prj_default", name: "staging" },
  displayName: "Staging",
});
const project = create(ResourceSchema, { id: "prj_default", resource: { kind: "project", name: "default" } });

const logLevel = create(EnvironmentVariableSchema, {
  id: "var_log",
  name: "LOG_LEVEL",
  value: "info",
  revision: 2n,
  updateTime: timestampFromDate(new Date(Date.now() - 4 * 60_000)),
  updatedBy: mira,
  usedBy: [{ serviceId: "svc_api", service: "api", name: "LOG_LEVEL" }],
});
const dbPassword = create(EnvironmentVariableSchema, {
  id: "sec_db",
  name: "DB_PASSWORD",
  secret: true,
  revision: 3n,
  updateTime: timestampFromDate(new Date(Date.now() - 2 * 86_400_000)),
  updatedBy: mira,
  usedBy: [
    { serviceId: "svc_api", service: "api", name: "DATABASE_PASSWORD" },
    { serviceId: "svc_worker", service: "worker", name: "DB_PASSWORD" },
  ],
});

const planned = create(ChangeSchema, {
  id: "chg_1",
  summary: "Add the secret STRIPE_KEY",
  state: ChangeState.PLANNED,
  proposedBy: mira,
  plan: {
    fingerprint: "0123456789abcdef",
    summary: { create: 1 },
    resources: [
      { id: "res_key", resource: { kind: "secret", scope: "env_prod", name: "STRIPE_KEY" }, action: PlanAction.CREATE },
    ],
  },
});

function api(environments = [production], more: Api = {}): Api & { proposals: ProposeChangeRequest[] } {
  const proposals: ProposeChangeRequest[] = [];
  return {
    proposals,
    resources: {
      listResources: (req) =>
        req.kind === "environment"
          ? { resources: environments }
          : req.kind === "project"
            ? { resources: [project] }
            : {},
    },
    variables: {
      listVariables: (req: ListVariablesRequest) =>
        req.environment === "env_prod"
          ? { environmentId: "env_prod", variables: [dbPassword, logLevel] }
          : { environmentId: req.environment },
      ...more.variables,
    },
    changes: {
      proposeChange: (req) => {
        proposals.push(req);
        return { change: planned };
      },
      watchChange: quietWatch,
      discardChange: () => ({ change: planned }),
    },
    auth: more.auth ?? {},
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("the variables and secrets page", () => {
  it("lists an environment's variables with their values, and its secrets hidden", async () => {
    const { container } = renderAt("/variables", api());
    // The first render in this file loads the route, which takes a while on a busy machine.
    const table = await screen.findByRole("table", { name: "Variables and secrets in production" }, { timeout: 5000 });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([
      expect.stringContaining("DB_PASSWORD, secret"),
      expect.stringContaining("LOG_LEVEL"),
    ]);
    const [secret, variable] = rows as [HTMLElement, HTMLElement];
    expect(within(variable).getByText("info")).toBeVisible();
    expect(within(secret).getByRole("img", { name: "Hidden" })).toBeVisible();
    expect(within(secret).getByRole("button", { name: "Reveal DB_PASSWORD" })).toBeVisible();
    expect(secret).toHaveTextContent("apias DATABASE_PASSWORD");
    expect(secret).toHaveTextContent("worker");
    expect(variable).toHaveTextContent("Mira Chen4 min ago");
    expect(screen.getByText("1 variable · 1 secret")).toBeVisible();
    // With one environment there is nothing to switch to.
    expect(screen.queryByRole("radiogroup", { name: "Environment" })).toBeNull();
    await expectAccessible(container);
  });

  it("reads every page of a long list", async () => {
    const asked: ListVariablesRequest[] = [];
    renderAt(
      "/variables",
      api([production], {
        variables: {
          listVariables: (req) => {
            asked.push(req);
            return req.pageToken === ""
              ? { environmentId: "env_prod", variables: [dbPassword], nextPageToken: "page-2" }
              : { environmentId: "env_prod", variables: [logLevel] };
          },
        },
      }),
    );
    const table = await screen.findByRole("table", { name: "Variables and secrets in production" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(3));
    expect(asked.map((r) => r.pageToken)).toEqual(["", "page-2"]);
    expect(screen.getByText("1 variable · 1 secret")).toBeVisible();
  });

  it("shows each name a service uses a value under", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const twice = create(EnvironmentVariableSchema, {
      id: "sec_db",
      name: "DB_PASSWORD",
      secret: true,
      revision: 3n,
      updatedBy: mira,
      usedBy: [
        { serviceId: "svc_api", service: "api", name: "DATABASE_PASSWORD" },
        { serviceId: "svc_api", service: "api", name: "REPLICA_PASSWORD" },
      ],
    });
    renderAt(
      "/variables",
      api([production], { variables: { listVariables: () => ({ environmentId: "env_prod", variables: [twice] }) } }),
    );
    const table = await screen.findByRole("table", { name: "Variables and secrets in production" });
    const secret = within(table).getAllByRole("row")[1] as HTMLElement;
    expect(secret).toHaveTextContent("as DATABASE_PASSWORD");
    expect(secret).toHaveTextContent("as REPLICA_PASSWORD");
    expect(errors.mock.calls.flat().join(" ")).not.toMatch(/same key/);
    errors.mockRestore();
  });

  it("switches environments once there are two, defaulting to production", async () => {
    const user = userEvent.setup();
    const asked: string[] = [];
    const { router } = renderAt(
      "/variables",
      api([production, staging], {
        variables: {
          listVariables: (req) => {
            asked.push(req.environment);
            return { environmentId: req.environment };
          },
        },
      }),
    );
    const envs = await screen.findByRole("radiogroup", { name: "Environment" });
    await waitFor(() => expect(asked).toContain("env_prod"));
    await user.click(within(envs).getByRole("radio", { name: "Staging" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ environment: "env_staging" }));
    await waitFor(() => expect(asked).toContain("env_staging"));
    expect(await screen.findByText(/No variables or secrets in Staging yet/)).toBeVisible();
  });

  it("says an environment has nothing yet, and puts you in the key's field to add the first", async () => {
    const user = userEvent.setup();
    renderAt("/variables?environment=env_staging", api([production, staging]));
    const empty = await screen.findByRole("heading", { name: "No variables or secrets in Staging yet" });
    expect(empty).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Add the first" }));
    expect(screen.getByRole("textbox", { name: "Key" })).toHaveFocus();
  });

  it("clears a filter that matches nothing, from where it says so", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/variables?environment=env_prod&q=NOPE", api([production]));
    expect(await screen.findByRole("heading", { name: "Nothing in production is named like that" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Clear the filter" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ environment: "env_prod" }));
    expect(await screen.findByText("DB_PASSWORD")).toBeVisible();
  });

  it("reveals a secret with a passkey, copies it, and hides it again after a minute", async () => {
    const user = userEvent.setup({ advanceTimers: (ms) => vi.advanceTimersByTime(ms) });
    const passkeys = stubPasskeys();
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const reveals: RevealSecretRequest[] = [];
    const steps: unknown[] = [];
    renderAt(
      "/variables",
      api([production], {
        auth: {
          beginStepUp: (req) => {
            steps.push(req);
            return { ceremonyId: "cer_1", optionsJson: JSON.stringify({ challenge: "q83v", rpId: "localhost" }) };
          },
          finishStepUp: () => ({ proof: "proof_1" }),
        },
        variables: {
          revealSecret: (req) => {
            reveals.push(req);
            // As the control plane: a passkey, once the secret is there.
            if (req.proof === "") throw authError(Code.PermissionDenied, "needs_step_up");
            return { value: "hunter2-hunter2", revision: 3n };
          },
        },
      }),
    );
    const reveal = await screen.findByRole("button", { name: "Reveal DB_PASSWORD" });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    await user.click(reveal);
    expect(await screen.findByText("hunter2-hunter2")).toBeVisible();
    expect(passkeys.get).toHaveBeenCalledOnce();
    expect(steps).toEqual([expect.objectContaining({ action: StepUpAction.REVEAL_SECRET, target: "sec_db" })]);
    expect(reveals).toEqual([
      expect.objectContaining({ id: "sec_db", proof: "" }),
      expect.objectContaining({ id: "sec_db", proof: "proof_1" }),
    ]);

    await user.click(screen.getByRole("button", { name: "Copy DB_PASSWORD" }));
    expect(writeText).toHaveBeenCalledWith("hunter2-hunter2");
    // What is announced names the secret, never its value.
    expect(await screen.findByText("Copied DB_PASSWORD", { selector: "[aria-live]" })).toBeInTheDocument();

    act(() => vi.advanceTimersByTime(REVEALED_FOR));
    await waitFor(() => expect(screen.queryByText("hunter2-hunter2")).toBeNull());
    expect(screen.getByRole("button", { name: "Reveal DB_PASSWORD" })).toBeVisible();
  });

  it("says why a reveal was refused", async () => {
    const user = userEvent.setup();
    stubPasskeys();
    renderAt(
      "/variables",
      api([production], {
        auth: {
          beginStepUp: () => {
            throw authError(Code.FailedPrecondition, "no_passkey_here");
          },
        },
        variables: {
          revealSecret: () => {
            throw authError(Code.PermissionDenied, "needs_step_up");
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Reveal DB_PASSWORD" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "That takes a passkey, and there's none for this address yet.",
    );
  });

  it("asks for no passkey when the secret isn't there to reveal", async () => {
    const user = userEvent.setup();
    const passkeys = stubPasskeys();
    renderAt(
      "/variables",
      api([production], {
        variables: {
          revealSecret: () => {
            throw authError(
              Code.NotFound,
              "not_found",
              "there's no secret sec_db. Check its ID with dockwell variables list",
            );
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Reveal DB_PASSWORD" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("There's no secret sec_db.");
    expect(passkeys.get).not.toHaveBeenCalled();
  });

  it("drops a value that arrives after the page is gone", async () => {
    const user = userEvent.setup();
    stubPasskeys();
    let answer: (v: { value: string; revision: bigint }) => void = () => {};
    let signal: AbortSignal | undefined;
    const { unmount } = renderAt(
      "/variables",
      api([production], {
        variables: {
          revealSecret: (_req, ctx) => {
            signal = ctx.signal;
            return new Promise((resolve) => {
              answer = resolve;
            });
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Reveal DB_PASSWORD" }));
    await waitFor(() => expect(signal).toBeDefined());
    const timers = vi.spyOn(globalThis, "setTimeout");
    unmount();
    expect(signal?.aborted).toBe(true);
    answer({ value: "hunter2-hunter2", revision: 3n });
    await new Promise((resolve) => setTimeout(resolve, 20));
    // No timer holds the value until it would have hidden it.
    expect(timers.mock.calls.filter(([, ms]) => ms === REVEALED_FOR)).toEqual([]);
    timers.mockRestore();
  });

  it("adds a secret, changes a variable into one, and removes one, each through the plan sheet", async () => {
    const user = userEvent.setup();
    const backend = api();
    renderAt("/variables", backend);
    const form = await screen.findByRole("form", { name: "Add a variable or secret to production" });
    await user.type(within(form).getByRole("textbox", { name: "Key" }), "STRIPE_KEY");
    await user.click(within(form).getByRole("switch", { name: "Secret" }));
    const value = within(form).getByLabelText("Value");
    expect(value).toHaveAttribute("type", "password");
    await user.type(value, "sk_live_123");
    await user.click(within(form).getByRole("button", { name: "Add" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(backend.proposals[0]).toMatchObject({
      summary: "Add the secret STRIPE_KEY",
      edits: [
        {
          resource: { kind: "secret", scope: "env_prod", name: "STRIPE_KEY" },
          spec: { kind: { case: "secret", value: { value: "sk_live_123" } } },
        },
      ],
    });
    // The value doesn't stay on the page once the plan has it.
    expect(within(form).getByLabelText("Value")).toHaveValue("");
    await user.click(within(sheet).getByRole("button", { name: "Close" }));

    await user.click(screen.getByRole("button", { name: "Change LOG_LEVEL" }));
    const change = screen.getByRole("form", { name: "Change LOG_LEVEL" });
    expect(within(change).getByLabelText("Value")).toHaveValue("info");
    await user.click(within(change).getByRole("switch", { name: "Secret" }));
    // Its value as a variable isn't carried over, and the panel says why.
    expect(within(change).getByLabelText("Value")).toHaveValue("");
    expect(within(change).getByText("The old value stays in this variable's history")).toBeVisible();
    await user.click(within(change).getByRole("button", { name: "Review plan" }));
    expect(within(change).getByText("A secret needs a value.")).toBeVisible();
    expect(backend.proposals).toHaveLength(1);
    await user.type(within(change).getByLabelText("Value"), "debug");
    await user.click(within(change).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(backend.proposals[1]).toMatchObject({
      summary: "Make LOG_LEVEL a secret",
      edits: [
        { resource: { kind: "variable", scope: "env_prod", name: "LOG_LEVEL" }, delete: true },
        {
          resource: { kind: "secret", name: "LOG_LEVEL" },
          spec: { kind: { case: "secret", value: { value: "debug" } } },
        },
      ],
    });
    await user.click(screen.getByRole("button", { name: "Close" }));

    await user.click(screen.getByRole("button", { name: "Remove DB_PASSWORD" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(backend.proposals[2]).toMatchObject({
      summary: "Remove the secret DB_PASSWORD",
      edits: [{ resource: { kind: "secret", scope: "env_prod", name: "DB_PASSWORD" }, delete: true }],
    });
  });

  it("checks a name before planning anything", async () => {
    const user = userEvent.setup();
    const backend = api();
    renderAt("/variables", backend);
    const form = await screen.findByRole("form", { name: "Add a variable or secret to production" });
    await user.type(within(form).getByRole("textbox", { name: "Key" }), "db-url");
    await user.click(within(form).getByRole("button", { name: "Add" }));
    expect(within(form).getByText(/Use letters, digits and underscores/)).toBeVisible();
    expect(backend.proposals).toEqual([]);
  });

  it("stacks each one on a phone, with Reveal in words", async () => {
    onPhone();
    const { container } = renderAt("/variables", api());
    const list = await screen.findByRole("list", { name: "Variables and secrets in production" });
    const items = within(list)
      .getAllByRole("listitem", { name: undefined })
      .filter((li) => li.parentElement === list);
    expect(items).toHaveLength(2);
    expect(within(items[0] as HTMLElement).getByRole("button", { name: "Reveal DB_PASSWORD" })).toHaveTextContent(
      "Reveal",
    );
    expect(screen.getByRole("heading", { level: 1, name: "Variables" })).toBeVisible();
    await expectAccessible(container);
  });

  it("speaks Chinese", async () => {
    await activateLocale("zh-CN");
    try {
      const { container } = renderAt("/variables", api());
      expect(await screen.findByRole("table", { name: "production 中的变量和密钥" })).toBeVisible();
      expect(screen.getByRole("heading", { level: 1, name: "变量和密钥" })).toBeVisible();
      expect(screen.getByRole("button", { name: "显示 DB_PASSWORD" })).toBeVisible();
      expect(screen.getByRole("form", { name: "向 production 添加变量或密钥" })).toBeVisible();
      expect(screen.getByText("1 个变量 · 1 个密钥")).toBeVisible();
      await expectAccessible(container);
    } finally {
      await activateLocale("en");
    }
  });
});

describe("the variables page, once there are several projects", () => {
  const projects = [
    create(ResourceSchema, { id: "prj_default", resource: { kind: "project", scope: "org_1", name: "default" } }),
    create(ResourceSchema, {
      id: "prj_blog",
      resource: { kind: "project", scope: "org_1", name: "blog" },
      displayName: "Blog",
    }),
    create(ResourceSchema, { id: "prj_old", resource: { kind: "project", scope: "org_1", name: "old-shop" } }),
  ];
  const envs = [
    production,
    create(ResourceSchema, {
      id: "env_blog",
      resource: { kind: "environment", scope: "prj_blog", name: "production" },
    }),
    create(ResourceSchema, { id: "env_old", resource: { kind: "environment", scope: "prj_old", name: "production" } }),
  ];
  const inside = [
    create(ResourceSchema, { id: "svc_api", resource: { kind: "service", scope: "env_prod", name: "api" } }),
    create(ResourceSchema, { id: "sec_db", resource: { kind: "secret", scope: "env_prod", name: "DB_PASSWORD" } }),
    create(ResourceSchema, { id: "var_log", resource: { kind: "variable", scope: "env_prod", name: "LOG_LEVEL" } }),
    create(ResourceSchema, { id: "svc_ghost", resource: { kind: "service", scope: "env_blog", name: "ghost" } }),
    create(ResourceSchema, { id: "sec_mail", resource: { kind: "secret", scope: "env_blog", name: "MAIL_PASSWORD" } }),
    create(ResourceSchema, { id: "sec_stripe", resource: { kind: "secret", scope: "env_old", name: "STRIPE_KEY" } }),
  ];
  const mail = create(EnvironmentVariableSchema, { id: "sec_mail", name: "MAIL_PASSWORD", secret: true, revision: 1n });
  const stripe = create(EnvironmentVariableSchema, {
    id: "sec_stripe",
    name: "STRIPE_KEY",
    secret: true,
    revision: 1n,
  });

  function several(): Api & { proposals: ProposeChangeRequest[] } {
    const base = api(envs);
    return {
      ...base,
      resources: {
        listResources: (req) => ({
          resources:
            req.kind === "project"
              ? projects
              : req.kind === "environment"
                ? envs
                : req.scope
                  ? inside.filter((r) => r.resource?.scope === req.scope)
                  : req.kind === ""
                    ? [...projects, ...envs, ...inside]
                    : [],
        }),
      },
      variables: {
        listVariables: (req: ListVariablesRequest) =>
          req.environment === "env_prod"
            ? { environmentId: "env_prod", variables: [dbPassword, logLevel] }
            : req.environment === "env_blog"
              ? { environmentId: "env_blog", variables: [mail] }
              : { environmentId: req.environment, variables: [stripe] },
      },
    };
  }

  afterEach(() => {
    window.localStorage.clear();
  });

  it("chooses a project from a list with each one's counts, in place of a switch, and remembers it", async () => {
    const user = userEvent.setup();
    const { router, unmount } = renderAt("/variables", several());
    const picker = await screen.findByRole("button", { name: /Project default/ });
    expect(screen.queryByRole("radiogroup", { name: "Environment" })).toBeNull();
    await user.click(picker);
    const list = await screen.findByRole("dialog", { name: "Choose a project" });
    expect(within(list).getByRole("button", { name: /Blog 0 variables · 1 secret/ })).toBeVisible();
    // A project with nothing running stands apart, under its own heading.
    expect(within(within(list).getByRole("region", { name: "Nothing running" })).getByRole("button")).toHaveTextContent(
      "old-shop",
    );
    await expectAccessible(document.body);
    await user.click(within(list).getByRole("button", { name: /Blog/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({ environment: "env_blog" }));
    expect(await screen.findByText("MAIL_PASSWORD")).toBeVisible();
    expect(screen.queryByRole("dialog", { name: "Choose a project" })).toBeNull();
    unmount();

    // The next visit with none named opens on it.
    renderAt("/variables", several());
    expect(await screen.findByRole("button", { name: /Project Blog/ })).toBeVisible();
  });

  it("finds a name in every project from the same box, and opens it there, filtered", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/variables", several());
    await user.click(await screen.findByRole("button", { name: /Project default/ }));
    await user.type(screen.getByRole("searchbox", { name: "Find a project, or a variable by name" }), "stripe");
    const found = screen.getByRole("region", { name: "Found by name" });
    await user.click(within(found).getByRole("button", { name: /STRIPE_KEY old-shop/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({ environment: "env_old", q: "STRIPE_KEY" }));
    expect(await screen.findByText("STRIPE_KEY")).toBeVisible();
    expect(screen.getByRole("searchbox", { name: "Filter by name" })).toHaveValue("STRIPE_KEY");
  });

  it("offers a project with nothing running its Delete, with everything in it", async () => {
    const user = userEvent.setup();
    const base = several();
    renderAt("/variables?environment=env_old", base);
    expect(await screen.findByText("Nothing running in old-shop")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Delete the project" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(base.proposals[0]?.edits.map((e) => [e.resource?.kind, e.resource?.name, e.delete])).toEqual([
      ["secret", "STRIPE_KEY", true],
      ["project", "old-shop", true],
    ]);
  });

  it("names the environment by its project in the form", async () => {
    renderAt("/variables?environment=env_blog", several());
    expect(await screen.findByRole("form", { name: "Add a variable or secret to Blog" })).toBeVisible();
  });
});
