// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { AuditEventSchema, EventKind, EventSchema } from "@dockwell/api";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { renderHookWithApi } from "../../test/render";
import { useDescribe } from "../activity/describe";
import actions from "./auditActions.json";
import { useAuditWords } from "./auditWords";

afterEach(async () => {
  await activateLocale("en");
});

// The control plane's own test keeps auditActions.json to every action it records
// (internal/control/audit/actions_test.go), so a new action can't reach the audit
// log saying only its machine name.
describe("every recorded action", () => {
  for (const locale of ["en", "zh-CN"] as const) {
    it(`has a sentence in ${locale}`, async () => {
      await activateLocale(locale);
      const say = renderHookWithApi(useAuditWords, {}).result.current;
      const bare = actions.filter((action) => {
        // Health and alert events are described by kind, as events.FromAudit sets them.
        const kind =
          action === "service.health"
            ? EventKind.HEALTH
            : action.startsWith("alert.")
              ? EventKind.ALERT
              : EventKind.ACCESS;
        const said = say(create(AuditEventSchema, { action, event: { action, kind } })).title;
        return said === action || said.includes(action);
      });
      expect(bare).toEqual([]);
      expect(actions.length).toBeGreaterThan(40);
    });
  }
});

describe("reading logs", () => {
  it("names the service, or the server of a container Dockwell didn't make", async () => {
    const say = renderHookWithApi(useAuditWords, {}).result.current;
    const read = (id: string, name: string) =>
      say(create(AuditEventSchema, { action: "logs.read", target: { id, name }, event: { action: "logs.read" } }))
        .title;
    expect(read("svc_1", "web")).toBe("Read the logs of web");
    expect(read("srv_1", "web-1")).toBe("Read a container's logs on web-1");
    await activateLocale("zh-CN");
    const zh = renderHookWithApi(useAuditWords, {}).result.current;
    expect(
      zh(
        create(AuditEventSchema, {
          action: "logs.read",
          target: { id: "srv_1", name: "web-1" },
          event: { action: "logs.read" },
        }),
      ).title,
    ).toBe("读取了 web-1 上某个容器的日志");
  });
});

describe("build cancellation", () => {
  it.each([
    ["en", "Asked to cancel a build on web-1", "Asked to cancel a build"],
    ["zh-CN", "请求取消 web-1 上的构建", "请求取消构建"],
  ] as const)("names the server, with a sentence when it is unknown, in %s", async (locale, named, bare) => {
    await activateLocale(locale);
    const say = renderHookWithApi(useAuditWords, {}).result.current;
    const event = create(AuditEventSchema, {
      action: "build.cancel",
      target: { id: "srv_1", name: "web-1" },
      subject: "bld_1",
      event: { action: "build.cancel", subject: { id: "bld_1" } },
    });
    expect(say(event)).toEqual({ title: named });
    event.target = undefined;
    expect(say(event)).toEqual({ title: bare });
  });
});

describe("scheduled task runs", () => {
  it.each([
    ["en", "Nightly backup failed", "Nightly backup succeeded after a failure", "Asked to run Nightly backup"],
    ["zh-CN", "Nightly backup 失败了", "Nightly backup 在失败后成功了", "请求运行 Nightly backup"],
  ] as const)("names failures, recoveries and manual requests in %s", async (locale, failed, recovered, run) => {
    await activateLocale(locale);
    const say = renderHookWithApi(useAuditWords, {}).result.current;
    const describe = renderHookWithApi(useDescribe, {}).result.current;
    for (const [action, title, tone] of [
      ["task.failed", failed, "critical"],
      ["task.recovered", recovered, "cobalt"],
      ["task.run", run, undefined],
    ] as const) {
      const event = create(EventSchema, {
        action,
        kind: EventKind.TASK,
        subject: { name: "Stored task" },
        taskRun: { task: "nightly", displayName: "Nightly backup" },
      });
      const expected = { title, ...(tone ? { tone } : {}) };
      expect(say(create(AuditEventSchema, { action, event }))).toEqual(expected);
      expect(describe(event, new Map())).toEqual({ ...expected, icon: "clock" });
    }
  });

  it("uses the task name, then the subject, and gives unnamed tasks a sentence", () => {
    const describe = renderHookWithApi(useDescribe, {}).result.current;
    const event = create(EventSchema, {
      action: "task.failed",
      kind: EventKind.TASK,
      subject: { name: "Stored task" },
      taskRun: { task: "nightly" },
    });
    expect(describe(event, new Map()).title).toBe("nightly failed");
    event.taskRun = undefined;
    expect(describe(event, new Map()).title).toBe("Stored task failed");
    event.subject = undefined;
    for (const [action, title] of [
      ["task.failed", "A scheduled task failed"],
      ["task.recovered", "A scheduled task succeeded after a failure"],
      ["task.run", "Asked to run a scheduled task"],
    ] as const) {
      event.action = action;
      expect(describe(event, new Map()).title).toBe(title);
    }
  });
});

describe("setting an app's updates aside", () => {
  const event = (action: string, detail: string) =>
    create(AuditEventSchema, { action, subject: "svc_1", detail, event: { action } });

  it("says the app and the version skipped, or when a snooze ends, from what was recorded", async () => {
    const say = renderHookWithApi(useAuditWords, {}).result.current;
    expect(say(event("service.update_skip", '{"service":"db","version":"17.2"}')).title).toBe("Skipped 17.2 of db");
    expect(say(event("service.update_unskip", '{"service":"db","version":"17.2"}')).title).toBe(
      "Showed 17.2 of db again",
    );
    expect(say(event("service.update_snooze", '{"service":"db","until":"2026-11-19T10:00:00Z"}')).title).toMatch(
      /^Snoozed the updates of db until .+/,
    );
    expect(say(event("service.update_unsnooze", '{"service":"db"}')).title).toBe(
      "Ended the snooze of the updates of db",
    );
    // What it can't read, it leaves out, and names the app by its ID.
    expect(say(event("service.update_skip", "")).title).toBe("Skipped an update of svc_1");
    await activateLocale("zh-CN");
    const zh = renderHookWithApi(useAuditWords, {}).result.current;
    expect(zh(event("service.update_skip", '{"service":"db","version":"17.2"}')).title).toBe("跳过了 db 的 17.2");
  });
});
