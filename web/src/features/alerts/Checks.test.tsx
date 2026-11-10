// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { durationFromMs, type Timestamp, timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  AlertChannelInfoSchema,
  type AlertCheckInfo,
  AlertCheckInfoSchema,
  type AlertCheckRun,
  AlertCheckRunSchema,
  AlertCheckState,
  AlertRuleInfoSchema,
  ChangeSchema,
  ChangeState,
  AlertRule_Condition as Condition,
  type ListAlertChecksRequest,
  type ProposeChangeRequest,
  Role,
  AlertRule_Severity as Severity,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { authState, mira } from "../../test/auth";
import { watched } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { nameFor } from "./checks";

const NOW = new Date();

const ops = create(AlertChannelInfoSchema, {
  id: "ach_ops",
  name: "ops-email",
  type: "email",
  target: "ops@example.com",
});

const checkDown = create(AlertRuleInfoSchema, {
  id: "alr_chk",
  name: "check-down",
  title: "Address down",
  expression: "check.state == down",
  spec: { condition: Condition.CHECK_DOWN, severity: Severity.CRITICAL, repeat: "4h" },
});

function aCheck(
  name: string,
  url: string,
  more: { id?: string; state?: AlertCheckState; since?: Timestamp; runs?: AlertCheckRun[] } = {},
): AlertCheckInfo {
  return create(AlertCheckInfoSchema, {
    id: `chk_${name}`,
    name,
    spec: { address: url, every: "1m", timeout: "10s", downAfter: 2, expectFrom: 200, expectTo: 399 },
    state: AlertCheckState.UP,
    from: "the control server",
    ...more,
  });
}

const run = (ok: boolean, status: number, said = "", ago = 0) =>
  create(AlertCheckRunSchema, {
    time: timestampFromDate(new Date(NOW.getTime() - ago * 60_000)),
    ok,
    status,
    took: durationFromMs(142),
    said,
  });

function api(checks: () => AlertCheckInfo[], more: Api = {}): Api {
  return {
    ...more,
    alerts: {
      listAlertChannels: () => ({ channels: [ops] }),
      listAlertRules: () => ({ rules: [checkDown] }),
      listAlerts: () => ({}),
      listAlertChecks: (_req: ListAlertChecksRequest) => ({ checks: checks() }),
      ...more.alerts,
    },
  };
}

describe("Checks of a URL", () => {
  it("lists each check with its state and its last answers, from the control server", async () => {
    const shop = aCheck("shop-example-com", "https://shop.example.com/", {
      runs: [run(true, 200, "", 0), run(true, 200, "", 1)],
    });
    const blog = aCheck("blog-example-com", "https://blog.example.com/", {
      state: AlertCheckState.DOWN,
      since: timestampFromDate(new Date(NOW.getTime() - 120_000)),
      runs: [run(false, 502, "502, outside 200 to 399", 0), run(false, 502, "502, outside 200 to 399", 1)],
    });
    const { container } = renderAt(
      "/alerts/checks",
      api(() => [blog, shop]),
    );
    const table = await screen.findByRole("table", { name: "Checks" });
    const up = within(table).getByRole("row", { name: /shop\.example\.com/ });
    expect(up).toHaveTextContent("Up");
    expect(up).toHaveTextContent("200 in 142 ms");
    expect(up).toHaveTextContent("Every minute from the control server");
    const down = within(table).getByRole("row", { name: /blog\.example\.com/ });
    expect(down).toHaveTextContent("Down");
    expect(down).toHaveTextContent(/since/);
    expect(down).toHaveTextContent("502, outside 200 to 399");
    expect(within(down).getByRole("img", { name: "Last 2 runs" })).toBeVisible();
    expect(screen.getByRole("link", { name: /Checks/ })).toHaveAttribute("aria-current", "page");
    await expectAccessible(container);
  });

  it("offers Watch an address in the phone header", async () => {
    onPhone();
    const user = userEvent.setup();
    renderAt(
      "/alerts/checks",
      api(() => [aCheck("shop-example-com", "https://shop.example.com/")]),
    );
    expect(await screen.findByRole("list", { name: "Checks" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Watch an address" }));
    expect(await screen.findByRole("dialog", { name: "Watch this address" })).toBeVisible();
  });

  it("says nothing is watched yet, and how to start", async () => {
    renderAt(
      "/alerts/checks",
      api(() => []),
    );
    expect(await screen.findByText("No address is watched yet")).toBeVisible();
    expect(screen.getByRole("button", { name: "Watch an address" })).toBeVisible();
  });

  it("watches an address on its defaults, with a plan, and shows its first answer", async () => {
    const user = userEvent.setup();
    let checks: AlertCheckInfo[] = [];
    const proposed: ProposeChangeRequest[] = [];
    const watch = feed<WatchChangeResponse>();
    const plannedChange = create(ChangeSchema, {
      id: "chg_watch",
      summary: "Watch shop.example.com",
      state: ChangeState.PLANNED,
      plan: {
        fingerprint: "abc",
        resources: [{ id: "chk_new", resource: { kind: "alert.check", name: "shop-example-com" } }],
      },
    });
    renderAt(
      "/alerts/checks",
      api(() => checks, {
        changes: {
          proposeChange: (req) => {
            proposed.push(req);
            return { change: plannedChange };
          },
          applyChange: () => ({ change: { ...plannedChange, state: ChangeState.APPLYING } }),
          watchChange: watch.stream,
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Watch an address" }));
    const sheet = await screen.findByRole("dialog", { name: "Watch this address" });
    // The defaults in one line, with Change one step away.
    expect(within(sheet).getByText("Every minute · down after 2 failures · alerts every channel")).toBeVisible();
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    const field = within(sheet).getByLabelText("Address");
    await user.type(field, "shop.example.com");
    await user.tab();
    expect(within(sheet).getByText(/Use an http or https address/)).toBeVisible();
    await user.clear(field);
    await user.type(field, "https://shop.example.com/");
    expect(review).toBeEnabled();
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveLength(1));
    const edit = proposed[0]?.edits[0];
    expect(edit?.resource).toMatchObject({ kind: "alert.check", name: "shop-example-com" });
    expect(edit?.spec?.kind).toMatchObject({
      case: "alertCheck",
      value: { address: "https://shop.example.com/", every: "1m", downAfter: 2 },
    });
    await user.click(await screen.findByRole("button", { name: /Apply plan/ }));
    watch.push(watched({ ...plannedChange, state: ChangeState.APPLIED }));
    await user.click(await screen.findByRole("button", { name: /Done/ }, { timeout: 5000 }));
    // Waiting, then its first answer, as the control plane gets it.
    const first = await screen.findByRole("dialog", { name: "Watching shop.example.com" });
    expect(within(first).getByText("Asking shop.example.com from the control server…")).toBeVisible();
    checks = [aCheck("shop-example-com", "https://shop.example.com/", { id: "chk_new", runs: [run(true, 200)] })];
    expect(await within(first).findByText("It answered 200 in 142 ms.", undefined, { timeout: 5000 })).toBeVisible();
    expect(within(first).getByRole("link", { name: "See every check" })).toHaveAttribute("href", "/alerts/checks");
  });

  it("changes how often and after how many failures, and says so in the line", async () => {
    const user = userEvent.setup();
    const proposed: ProposeChangeRequest[] = [];
    const shop = aCheck("shop-example-com", "https://shop.example.com/", { runs: [run(true, 200)] });
    renderAt(
      "/alerts/checks",
      api(() => [shop], {
        alerts: {
          listAlertRules: () => ({
            rules: [
              create(AlertRuleInfoSchema, {
                id: "alr_chk",
                name: "check-down",
                spec: { condition: Condition.CHECK_DOWN, severity: Severity.CRITICAL, channels: ["ops-email"] },
              }),
            ],
          }),
        },
        changes: {
          proposeChange: vi.fn((req: ProposeChangeRequest) => {
            proposed.push(req);
            return {
              change: create(ChangeSchema, { id: "chg_1", state: ChangeState.PLANNED, plan: { fingerprint: "a" } }),
            };
          }),
          watchChange: feed<WatchChangeResponse>().stream,
        },
      }),
    );
    const table = await screen.findByRole("table", { name: "Checks" });
    await user.click(within(table).getByRole("row", { name: /shop\.example\.com/ }));
    const sheet = await screen.findByRole("dialog", { name: "shop-example-com" });
    await user.click(within(sheet).getByRole("button", { name: /Change/ }));
    await user.selectOptions(within(sheet).getByLabelText("How often"), "5m");
    await user.selectOptions(within(sheet).getByLabelText("Down after"), "3");
    await user.click(within(sheet).getByRole("button", { name: /Change/ }));
    expect(within(sheet).getByText("Every 5 minutes · down after 3 failures · alerts ops-email")).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveLength(1));
    expect(proposed[0]?.edits[0]?.spec?.kind).toMatchObject({
      case: "alertCheck",
      // What the sheet doesn't show stays as the check has it.
      value: { address: "https://shop.example.com/", every: "5m", downAfter: 3, timeout: "10s", expectTo: 399 },
    });
  });

  it("watches an address inside the network only when you say so, and the plan and the line say it", async () => {
    const user = userEvent.setup();
    const proposed: ProposeChangeRequest[] = [];
    renderAt(
      "/alerts/checks",
      api(() => [], {
        changes: {
          proposeChange: (req) => {
            proposed.push(req);
            return {
              change: create(ChangeSchema, { id: "chg_1", state: ChangeState.PLANNED, plan: { fingerprint: "a" } }),
            };
          },
          watchChange: feed<WatchChangeResponse>().stream,
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Watch an address" }));
    const sheet = await screen.findByRole("dialog", { name: "Watch this address" });
    expect(within(sheet).getByText(/leave tokens and passwords out of it/)).toBeVisible();
    await user.type(within(sheet).getByLabelText("Address"), "http://intranet.example.com/");
    await user.click(within(sheet).getByRole("button", { name: /Change/ }));
    const inside = within(sheet).getByRole("switch", { name: "Inside the network" });
    expect(inside).not.toBeChecked();
    await user.click(inside);
    await user.click(within(sheet).getByRole("button", { name: /Change/ }));
    expect(within(sheet).getByText(/inside the network · alerts every channel/)).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveLength(1));
    expect(proposed[0]?.summary).toBe("Watch intranet.example.com, inside the network");
    expect(proposed[0]?.edits[0]?.spec?.kind).toMatchObject({ case: "alertCheck", value: { insideNetwork: true } });
  });

  it("says a check is inside the network, why one can't run, and a run that was skipped", async () => {
    const intranet = aCheck("intranet-example-com", "http://intranet.example.com/", {
      runs: [
        create(AlertCheckRunSchema, {
          time: timestampFromDate(NOW),
          skipped: true,
          said: "skipped: all 16 runs at once were taken",
        }),
      ],
    });
    if (intranet.spec) intranet.spec.insideNetwork = true;
    const lost = aCheck("lost-example-com", "https://lost.example.com/", {
      state: AlertCheckState.DOWN,
      runs: [run(false, 502)],
    });
    lost.problem = "Dockwell can't read this check's spec, so it doesn't run.";
    renderAt(
      "/alerts/checks",
      api(() => [intranet, lost]),
    );
    const table = await screen.findByRole("table", { name: "Checks" });
    const inside = within(table).getByRole("row", { name: /intranet\.example\.com/ });
    expect(inside).toHaveTextContent("Inside the network");
    expect(inside).toHaveTextContent("skipped: all 16 runs at once were taken");
    expect(inside.querySelector(".alerts-run.is-skipped")).not.toBeNull();
    expect(within(table).getByRole("row", { name: /lost\.example\.com/ })).toHaveTextContent(
      "can't read this check's spec",
    );
  });

  it("stops watching an address with a plan that deletes the check", async () => {
    const user = userEvent.setup();
    const proposed: ProposeChangeRequest[] = [];
    renderAt(
      "/alerts/checks",
      api(() => [aCheck("shop-example-com", "https://shop.example.com/")], {
        changes: {
          proposeChange: (req) => {
            proposed.push(req);
            return {
              change: create(ChangeSchema, { id: "chg_1", state: ChangeState.PLANNED, plan: { fingerprint: "a" } }),
            };
          },
          watchChange: feed<WatchChangeResponse>().stream,
        },
      }),
    );
    const table = await screen.findByRole("table", { name: "Checks" });
    await user.click(within(table).getByRole("row", { name: /shop\.example\.com/ }));
    const sheet = await screen.findByRole("dialog", { name: "shop-example-com" });
    await user.click(within(sheet).getByRole("button", { name: "Stop watching" }));
    await waitFor(() => expect(proposed).toHaveLength(1));
    expect(proposed[0]?.summary).toBe("Stop watching shop.example.com");
    expect(proposed[0]?.edits[0]).toMatchObject({
      resource: { kind: "alert.check", name: "shop-example-com" },
      delete: true,
    });
  });

  it("leaves the Checks tab out, and asks for no check, for someone below an admin", async () => {
    const listAlertChecks = vi.fn(() => ({ checks: [] }));
    renderAt(
      "/alerts/rules",
      api(() => [], {
        alerts: { listAlertChecks },
        auth: {
          getAuthState: () =>
            authState({
              user: { ...mira, role: Role.UNSPECIFIED },
              grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
            }),
        },
      }),
    );
    expect(await screen.findByRole("link", { name: /Rules/ })).toBeVisible();
    expect(screen.queryByRole("link", { name: /Checks/ })).toBeNull();
    expect(listAlertChecks).not.toHaveBeenCalled();
  });
});

describe("a check's name", () => {
  it("comes from the address's host, and is one no check has", () => {
    expect(nameFor("https://shop.example.com/cart", [])).toBe("shop-example-com");
    expect(nameFor("https://shop.example.com/", ["shop-example-com"])).toBe("shop-example-com-2");
    expect(nameFor("https://shop.example.com/", ["shop-example-com", "shop-example-com-2"])).toBe("shop-example-com-3");
    expect(nameFor("http://192.0.2.10:8080/", [])).toBe("check-192-0-2-10-8080");
    expect(nameFor(`https://${"a".repeat(80)}.example.com/`, [])).toHaveLength(56);
  });
});
