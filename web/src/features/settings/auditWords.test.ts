// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { AuditEventSchema, EventKind } from "@dockwell/api";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { renderHookWithApi } from "../../test/render";
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
        // A health event is said by its kind, which events.FromAudit always sets for it.
        const kind = action === "service.health" ? EventKind.HEALTH : EventKind.ACCESS;
        const said = say(create(AuditEventSchema, { action, event: { action, kind } })).title;
        return said === action || said.includes(action);
      });
      expect(bare).toEqual([]);
      expect(actions.length).toBeGreaterThan(40);
    });
  }
});
