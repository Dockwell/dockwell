// SPDX-License-Identifier: AGPL-3.0-only

import type { Flow } from "./flows.ts";
import { pastTheEdge } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

export const flow: Flow = {
  order: 56,
  run: async (_page, { context, origin, shoot, step }) => {
    step("ClickHouse: users, user actions and backups fit at 390 px");
    const phone = await context.newPage();
    try {
      await phone.setViewportSize({ width: 390, height: 844 });
      const account = Buffer.concat([
        bytes(1, "events_user"),
        number(4, 1),
        bytes(5, "events"),
        bytes(7, "EVENTS_PASSWORD"),
        bytes(8, "fixture-app"),
        bytes(9, "dbu_clickhouse_fixture"),
      ]);
      const reading = Buffer.concat([bytes(5, "26.3.62.3"), bytes(13, account), number(21, 1)]);
      const db = Buffer.concat([
        bytes(1, "svc_clickhouse_fixture"),
        bytes(2, Buffer.concat([bytes(1, "service"), bytes(2, "env_clickhouse_fixture"), bytes(3, "clickhouse")])),
        number(3, 7),
        bytes(4, "clickhouse/clickhouse-server:26.3.62.3-alpine"),
        bytes(5, "26.3.62.3"),
        bytes(6, "data"),
        bytes(7, "vol_clickhouse_fixture"),
        number(9, 9000),
        bytes(11, "CLICKHOUSE_PASSWORD"),
        bytes(12, reading),
        number(18, 8123),
      ]);
      await phone.route(/\/GetDatabase(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: bytes(1, db) }),
      );
      await phone.goto(`${origin}/databases/svc_clickhouse_fixture/users`);
      await phone.getByText("events_user", { exact: true }).waitFor();
      await phone.getByText(/fixture-app/).waitFor();
      await phone.getByRole("button", { name: "Actions for events_user" }).click();
      await phone.getByRole("menuitem", { name: "Change password" }).waitFor();
      await phone.getByRole("menuitem", { name: "Remove", exact: true }).waitFor();
      await phone.keyboard.press("Escape");
      await phone.getByRole("button", { name: "Add a user" }).click();
      const user = phone.getByRole("dialog", { name: /^Add a user to / });
      await user.waitFor();
      const usersEdge = await pastTheEdge(phone);
      if (usersEdge.wider > 0) throw new Error(`ClickHouse users scroll sideways: ${usersEdge.what}`);
      await shoot(phone, "clickhouse-users-390");
      await user.getByRole("button", { name: "Cancel" }).click();
      await phone.goto(`${origin}/databases/svc_clickhouse_fixture/backups`);
      await phone.getByRole("button", { name: "Back up now" }).first().waitFor();
      const backupEdge = await pastTheEdge(phone);
      if (backupEdge.wider > 0) throw new Error(`ClickHouse backups scroll sideways: ${backupEdge.what}`);
      await shoot(phone, "clickhouse-backups-390");
    } finally {
      await phone.close();
    }
  },
};
