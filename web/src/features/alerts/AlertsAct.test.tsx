// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  type AlertChannelInfo,
  AlertChannelInfoSchema,
  AlertRuleInfoSchema,
  AlertState,
  ChangeSchema,
  ChangeState,
  AlertRule_Condition as Condition,
  type ProposeChangeRequest,
  AlertRule_Severity as Severity,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, ENV } from "../../test/apps";
import { type Api, feed, onPhone, renderAt } from "../../test/render";

const NOW = new Date();

function aChannel(init: MessageInitShape<typeof AlertChannelInfoSchema>): AlertChannelInfo {
  return create(AlertChannelInfoSchema, init);
}

/** The only channel, whose every message fails. */
const failing = aChannel({
  id: "ach_ops",
  name: "ops-email",
  type: "email",
  target: "ops@example.com",
  rules: 3,
  channel: { to: { case: "email", value: { host: "smtp.example.com", port: 587, to: ["ops@example.com"] } } },
  last: {
    time: timestampFromDate(NOW),
    ok: false,
    said: "dial tcp: no such host",
    about: "web is down",
    channel: "ops-email",
  },
});

const working = aChannel({
  id: "ach_hook",
  name: "ops-hook",
  type: "webhook",
  target: "hooks.example.com",
  rules: 3,
  channel: { to: { case: "webhook", value: {} } },
  last: { time: timestampFromDate(NOW), ok: true, said: "202 Accepted", about: "web is down", channel: "ops-hook" },
});

const rules = [
  create(AlertRuleInfoSchema, {
    id: "alr_disk",
    name: "disk-full",
    title: "Disk above 90%",
    expression: "disk.percent > 90",
    spec: { condition: Condition.DISK_FULL, threshold: 90, severity: Severity.WARNING },
  }),
  create(AlertRuleInfoSchema, {
    id: "alr_disk2",
    name: "disk-nearly-full",
    title: "Disk above 90%",
    expression: "disk.percent > 90",
    spec: { condition: Condition.DISK_FULL, threshold: 90, severity: Severity.CRITICAL },
  }),
  create(AlertRuleInfoSchema, {
    id: "alr_down",
    name: "app-down",
    title: "App down",
    expression: "app.health == down",
    spec: { condition: Condition.SERVICE_DOWN, duration: "5m", severity: Severity.CRITICAL },
  }),
];

function api(channels: AlertChannelInfo[], more: Api = {}): Api {
  return {
    ...more,
    alerts: {
      listAlertChannels: () => ({ channels }),
      listAlertRules: () => ({ rules }),
      listAlerts: () => ({}),
      listAlertDeliveries: () => ({ deliveries: [] }),
      ...more.alerts,
    },
  };
}

function planned() {
  return vi.fn((req: ProposeChangeRequest) => {
    void req;
    return { change: create(ChangeSchema, { id: "chg_1", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }) };
  });
}

describe("alerts that reach no one (#794)", () => {
  it("says so once, above every tab, with a test and the way to the channel", async () => {
    const user = userEvent.setup();
    const tested: string[] = [];
    const { container } = renderAt(
      "/alerts",
      api([failing], {
        alerts: {
          testAlertChannel: (req) => {
            tested.push(req.name);
            return { delivery: { ok: false, said: "dial tcp: no such host", channel: req.name } };
          },
        },
      }),
    );
    expect(await screen.findByText("Alerts aren't reaching anyone")).toBeVisible();
    expect(screen.getByText("ops-email failed: dial tcp: no such host")).toBeVisible();
    // The page's lead says it too, not "sent to 1 channel".
    expect(container.querySelector(".page-lead")).toHaveTextContent("reaching no channel");
    await user.click(screen.getByRole("button", { name: "Send test" }));
    await waitFor(() => expect(tested).toEqual(["ops-email"]));
    expect(screen.getByRole("link", { name: "Edit channel" })).toHaveAttribute(
      "href",
      "/alerts/channels?edit=ops-email",
    );
    await expectAccessible(container);
  });

  it("says nothing where a channel still takes them, and counts the one failing", async () => {
    const { container } = renderAt("/alerts", api([failing, working]));
    await waitFor(() => expect(container.querySelector(".page-lead")).toHaveTextContent("1 channel failing"));
    expect(screen.queryByText("Alerts aren't reaching anyone")).toBeNull();
  });

  it("opens the channel a link names", async () => {
    renderAt("/alerts/channels?edit=ops-email", api([failing]));
    expect(await screen.findByRole("dialog", { name: "Change ops-email" })).toBeVisible();
  });

  it("closes a channel's sheet with Cancel", async () => {
    const user = userEvent.setup();
    renderAt("/alerts/channels?edit=ops-email", api([failing]));
    const sheet = await screen.findByRole("dialog", { name: "Change ops-email" });
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Change ops-email" })).toBeNull());
  });
});

describe("a new rule (#794)", () => {
  it("plans a rule from what it watches for, on its condition's defaults, under a free name", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    renderAt(
      "/alerts/rules",
      api([working], { changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream } }),
    );
    await user.click(await screen.findByRole("button", { name: "New rule" }));
    const sheet = await screen.findByRole("dialog", { name: "New rule" });
    await user.selectOptions(within(sheet).getByLabelText("Watch for"), "Memory runs high");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource?.name).toBe("memory-high");
    expect(edit?.spec?.kind.case === "alertRule" && edit.spec.kind.value).toMatchObject({
      condition: Condition.MEMORY_HIGH,
      threshold: 90,
      disabled: false,
    });
  });

  it("says when a rule watches for what another does, as a hint that never blocks", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    renderAt(
      "/alerts/rules",
      api([working], { changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream } }),
    );
    // The list marks the pair, each with the other's name.
    const table = await screen.findByRole("table", { name: "Alert rules" });
    expect(within(table).getByText("Same as disk-nearly-full")).toBeVisible();
    expect(within(table).getByText("Same as disk-full")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "New rule" }));
    const sheet = await screen.findByRole("dialog", { name: "New rule" });
    await user.selectOptions(within(sheet).getByLabelText("Watch for"), "A disk fills up");
    expect(within(sheet).getByRole("status")).toHaveTextContent("Same as disk-full, which watches for this already");
    // A free name beside the one taken, and the plan goes ahead.
    expect(sheet).toHaveTextContent("disk-full-2");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed.mock.calls[0]?.[0].edits[0]?.resource?.name).toBe("disk-full-2"));
  });

  it("offers New rule in a phone's header", async () => {
    onPhone();
    renderAt("/alerts/rules", api([working]));
    expect(await screen.findByRole("button", { name: "New rule" })).toHaveClass("mobile-header-button");
  });
});

describe("a new uptime check (#794)", () => {
  it("offers the routed domains no check watches, with another address a step away", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    renderAt(
      "/alerts/checks",
      api([working], {
        alerts: {
          listAlertChecks: () => ({
            checks: [{ id: "achk_1", name: "docs-example-com", spec: { address: "https://docs.example.com/" } }],
          }),
        },
        domains: {
          listDomains: () => ({
            domains: [
              { name: "shop.example.com", url: "https://shop.example.com", environmentId: "env_prod", routes: [] },
              { name: "docs.example.com", url: "https://docs.example.com", environmentId: "env_prod", routes: [] },
            ],
          }),
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "New uptime check" }));
    const sheet = await screen.findByRole("dialog", { name: "New uptime check" });
    const address = await within(sheet).findByRole("combobox", { name: "Address" });
    expect(address).toHaveValue("https://shop.example.com");
    // The domain already watched isn't offered.
    expect(within(address).queryByRole("option", { name: "https://docs.example.com" })).toBeNull();
    expect(within(sheet).queryByRole("textbox", { name: "Another address" })).toBeNull();
    await user.selectOptions(address, "Another address");
    expect(within(sheet).getByRole("textbox", { name: "Another address" })).toBeVisible();
    await user.selectOptions(address, "https://shop.example.com");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.spec?.kind.case === "alertCheck" && edit.spec.kind.value.address).toBe("https://shop.example.com");
  });
});

describe("what an alert asks you to do (#794)", () => {
  const firing = (more: object) => ({
    id: "alt_1",
    ruleName: "app-down",
    severity: Severity.CRITICAL,
    state: AlertState.FIRING,
    openTime: timestampFromDate(NOW),
    sentTime: timestampFromDate(NOW),
    ...more,
  });
  const site = {
    domains: {
      listDomains: () => ({
        domains: [
          {
            name: "shop.example.com",
            url: "https://shop.example.com",
            environmentId: ENV,
            routes: [{ name: "shop-example-com", service: "web", path: "" }],
          },
        ],
      }),
    },
    services: { listServices: () => ({ services: [anApp("web", "nginx:1.27")] }) },
  };

  it("opens an app's site and its logs where it's down, and says when nothing reached anyone", async () => {
    renderAt(
      "/alerts",
      api([working], {
        ...site,
        alerts: {
          listAlerts: () => ({
            firing: 1,
            alerts: [
              firing({
                condition: Condition.SERVICE_DOWN,
                subjectId: "svc_web",
                subjectName: "web",
                detail: "Check what serves it, then its logs.",
                undelivered: true,
              }),
            ],
          }),
        },
      }),
    );
    const card = within(await screen.findByRole("region", { name: "Open" })).getByRole("article");
    expect(await within(card).findByRole("link", { name: "Open the site" })).toHaveAttribute(
      "href",
      "https://shop.example.com",
    );
    expect(within(card).getByRole("link", { name: "Its logs" })).toHaveAttribute("href", "/apps/svc_web/logs");
    expect(card).not.toHaveTextContent("Check what serves it");
    expect(card).toHaveTextContent("Failed to send");
  });

  it("opens the site an uptime check watches, the logs of the app behind it, and the check", async () => {
    renderAt(
      "/alerts",
      api([working], {
        ...site,
        alerts: {
          listAlertChecks: () => ({
            checks: [{ id: "achk_shop", name: "shop-example-com", spec: { address: "https://shop.example.com/" } }],
          }),
          listAlerts: () => ({
            firing: 1,
            alerts: [firing({ condition: Condition.CHECK_DOWN, subjectId: "achk_shop", where: "shop.example.com" })],
          }),
        },
      }),
    );
    const card = within(await screen.findByRole("region", { name: "Open" })).getByRole("article");
    expect(await within(card).findByRole("link", { name: "Its logs" })).toHaveAttribute("href", "/apps/svc_web/logs");
    expect(within(card).getByRole("link", { name: "Open the site" })).toHaveAttribute(
      "href",
      "https://shop.example.com/",
    );
    expect(within(card).getByRole("link", { name: "Its uptime check" })).toHaveAttribute("href", "/alerts/checks");
  });
});
