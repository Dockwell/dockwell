// SPDX-License-Identifier: AGPL-3.0-only

import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { renderAt } from "../../test/render";
import { recoveryPhraseStrength } from "./recoveryPhrase";

it("finds the newest copy while recovering, even after the original bundles expire", async () => {
  renderAt("/settings/backups", {
    controlBackup: {
      getControlBackup: () => ({
        install: "ins_fixture",
        address: "https://example.com",
        offsite: { file: "pinned-offsite.dwcb" },
        storage: { endpoint: "https://s3.example.com", bucket: "archive", prefix: "backups/current/" },
        servers: [
          { name: "hz-1", server: "server_fixture", path: "/var/lib/dockwell/copies/", file: "pinned-agent.dwcb" },
        ],
      }),
    },
  });
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Recovery sheet" }));
  const text = (await screen.findByRole("dialog", { name: "Recovery sheet" })).textContent ?? "";
  expect(text).not.toMatch(/pinned-|control backup status/);
  expect(text).toContain("${newest:");
  expect(text).toContain("ssh -T");
  expect(text).toContain("Open on the machine with the full Dockwell build");
  expect(text).toContain("dockwell open-backup ./control-backup.dwcb");
});

it("shows whether a custom passphrase meets the rule before asking for a passkey", async () => {
  renderAt("/settings/backups", { controlBackup: { getControlBackup: () => ({}) } });
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Set passphrase" }));
  const sheet = within(await screen.findByRole("dialog", { name: "Set recovery passphrase" }));
  await user.selectOptions(sheet.getByLabelText("Passphrase", { exact: true }), "custom");
  const phrase = sheet.getByLabelText("New passphrase");
  await user.type(phrase, "passwordpasswordpassword");
  expect(sheet.getByText("Strength: too easy to guess")).toBeVisible();
  expect(sheet.getByRole("button", { name: "Set passphrase" })).toBeDisabled();
  await user.clear(phrase);
  await user.type(phrase, "cQ8#nRsW4@kH9zE2tM7w");
  expect(sheet.getByText("Strength: meets the requirement")).toBeVisible();
  expect(sheet.getByRole("button", { name: "Set passphrase" })).toBeEnabled();
  expect(recoveryPhraseStrength("12345678901234567890")).toBe("weak");
});
