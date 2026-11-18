// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  CatalogInstallSchema,
  ChangeSchema,
  ChangeState,
  ConnectionInfoSchema,
  ConnectionReach,
  EntrySchema,
  Kind,
  LinkKind,
  Role,
  type SetCatalogNeedRequest,
  SmtpSecurity,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { ENV, places, SERVER } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const R2 = "0123456789abcdef0123456789abcdef";

/** A check the control plane kept, so many hours ago. */
function kept(hoursAgo: number, init: { passed?: boolean; code?: string; status?: number; server?: string } = {}) {
  return {
    passed: init.passed ?? true,
    code: init.code ?? "",
    status: init.status ?? 0,
    server: init.server ?? "hz-1",
    step: init.passed === false ? "write" : "",
    time: { seconds: BigInt(Math.floor(Date.now() / 1000) - hoursAgo * 3600), nanos: 0 },
  };
}

function aBucket(name: string, init: MessageInitShape<typeof ConnectionInfoSchema> = {}) {
  return create(ConnectionInfoSchema, {
    id: `cnx_${name}`,
    name,
    displayName: name,
    kind: LinkKind.S3,
    endpoint: `https://${R2}.r2.cloudflarestorage.com`,
    region: "auto",
    bucket: name,
    pathStyle: true,
    reach: ConnectionReach.EVERY,
    ...init,
  } as MessageInitShape<typeof ConnectionInfoSchema>);
}

function fleet(): Pick<Api, "servers"> {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return { servers: { watchServers: watch.stream } };
}

describe("a connection's last check (#799)", () => {
  it("lists each connection's last check: passed, failing with why, or not yet", async () => {
    const { container } = renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({
          connections: [
            aBucket("uploads", { checks: [kept(3)] }),
            aBucket("archive", { checks: [kept(1, { passed: false, code: "SignatureDoesNotMatch", status: 403 })] }),
            aBucket("fresh"),
          ],
        }),
      },
    });
    expect(await screen.findByRole("columnheader", { name: "Last check" })).toBeVisible();
    const uploads = screen.getByRole("row", { name: /uploads/ });
    expect(uploads).toHaveTextContent("Passed");
    expect(uploads).toHaveTextContent("hz-1 · 3 h ago");
    const archive = screen.getByRole("row", { name: /archive/ });
    expect(archive).toHaveTextContent("Fails from hz-1");
    expect(archive).toHaveTextContent("Keys refused");
    expect(screen.getByRole("row", { name: /fresh/ })).toHaveTextContent("Not checked yet");
    await expectAccessible(container);
  });

  it("says below an admin whether it passed and when, alone", async () => {
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      auth: {
        getAuthState: () =>
          authState({ user: { ...mira, role: Role.UNSPECIFIED }, grants: [{ projectId: "prj_1", role: Role.VIEWER }] }),
      },
      connections: {
        listConnections: () => ({
          connections: [
            create(ConnectionInfoSchema, {
              id: "cnx_1",
              name: "shared",
              kind: LinkKind.S3,
              checks: [{ passed: false, time: kept(2).time }],
            }),
          ],
        }),
      },
    });
    const row = await screen.findByRole("row", { name: /shared/ });
    expect(row).toHaveTextContent("Failed");
    expect(row).toHaveTextContent("2 h ago");
    expect(row).not.toHaveTextContent("hz-1");
  });

  it("heads a connection's page with its last check, and what a failing one found", async () => {
    renderAt("/connections/archive", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({
          connections: [aBucket("archive", { checks: [kept(1, { passed: false, code: "SignatureDoesNotMatch" })] })],
        }),
      },
    });
    expect(await screen.findByText("Fails from hz-1")).toBeVisible();
    expect(screen.getByText("Keys refused from hz-1")).toBeVisible();
    expect(screen.getByText("Check the access key ID and its secret.")).toBeVisible();
    // Its address style in plain words, and Use in an app once, in the head.
    const details = screen.getByRole("region", { name: "Details" });
    expect(details).toHaveTextContent("Address style");
    expect(details).toHaveTextContent("Bucket in the path");
    expect(details).not.toHaveTextContent("Bucket goes");
    expect(within(screen.getByRole("region", { name: "Used by" })).queryByRole("button")).toBeNull();
  });
});

describe("what a check says (#799)", () => {
  it("says Passed for a read that may read but not list, never green for one that can't confirm, and Keys refused for R2's Unauthorized", async () => {
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({
          connections: [
            aBucket("listless", { checks: [{ ...kept(2), step: "list", code: "NoSuchKey", status: 404 }] }),
            aBucket("locked", {
              checks: [{ ...kept(2, { passed: false, code: "AccessDenied", status: 403 }), step: "list" }],
            }),
            aBucket("revoked", { checks: [kept(1, { passed: false, code: "Unauthorized", status: 401 })] }),
          ],
        }),
      },
    });
    const listless = await screen.findByRole("row", { name: /listless/ });
    expect(listless).toHaveTextContent("Passed");
    expect(listless).not.toHaveTextContent("bucket found");
    // Neither green nor red (#799's review, M3).
    const locked = screen.getByRole("row", { name: /locked/ });
    expect(locked).toHaveTextContent("Can't confirm without writing");
    expect(locked).not.toHaveTextContent("Passed");
    expect(locked).not.toHaveTextContent("Fails from");
    expect(screen.getByRole("row", { name: /revoked/ })).toHaveTextContent("Keys refused");
  });

  it("never says a bucket is found from a read on its page, and offers Check where a read can't confirm", async () => {
    const user = userEvent.setup();
    let checked = 0;
    renderAt("/connections/locked", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({
          connections: [
            aBucket("locked", {
              checks: [{ ...kept(2, { passed: false, code: "AccessDenied", status: 403 }), step: "list" }],
            }),
          ],
        }),
        checkConnection: () => {
          checked++;
          return { passed: true, server: "hz-1" };
        },
      },
    });
    const line = await screen.findByText(/Check writes a test object to confirm them\./);
    const callout = line.closest(".dw-callout") as HTMLElement;
    expect(callout).toHaveTextContent("Can't confirm without writing");
    expect(screen.queryByText(/bucket found/)).toBeNull();
    await user.click(within(callout).getByRole("button", { name: "Check" }));
    await waitFor(() => expect(checked).toBe(1));
  });

  it("says a server's agent needs an update where it can't run the scheduled read, with the way to it", async () => {
    renderAt("/connections/archive", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({
          connections: [
            aBucket("archive", {
              checks: [{ ...kept(1, { passed: false, code: "agent-update" }), step: "list" }],
            }),
          ],
        }),
      },
    });
    const line = await screen.findByText(/The agent on hz-1 can't run the scheduled check\./);
    const callout = line.closest(".dw-callout") as HTMLElement;
    expect(callout).toHaveTextContent("Agent needs an update");
    expect(within(callout).getByRole("link", { name: "Open hz-1's updates" })).toHaveAttribute(
      "href",
      `/servers/${SERVER}/updates`,
    );
    expect(screen.queryByText("Fails from hz-1")).toBeNull();
  });
});

describe("a mail connection no app uses yet (#799)", () => {
  const relay = create(ConnectionInfoSchema, {
    id: "cnx_mail",
    name: "mail",
    displayName: "Mail relay",
    kind: LinkKind.SMTP,
    host: "smtp.example.com",
    port: 587,
    security: SmtpSecurity.STARTTLS,
    sender: "notes@example.com",
    reach: ConnectionReach.EVERY,
  });
  const notes = create(EntrySchema, {
    id: "notes",
    name: "Notes",
    needs: [{ name: "mail", label: "Mail", kind: Kind.SMTP, optional: true }],
  });
  const install = (needs: Record<string, MessageInitShape<typeof CatalogInstallSchema>["pin"]>) =>
    create(CatalogInstallSchema, {
      projectId: "prj_1",
      project: "notes",
      name: "Notes",
      environmentId: ENV,
      webServiceId: "svc_notes",
      pin: { entry: "notes", needs: needs as never },
    });

  it("lists each installed app that wants mail, with Use for it, which plans its mail through it", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogNeedRequest | undefined;
    const { container } = renderAt("/connections/mail", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [relay] }) },
      catalog: {
        listCatalogInstalls: () => ({ installs: [install({ mail: { unset: true } } as never)] }),
        getCatalogEntry: () => ({ entry: notes, needs: [] }),
        setCatalogNeed: (req) => {
          asked = req;
          return {
            change: create(ChangeSchema, { id: "chg_mail", state: ChangeState.PLANNED, plan: { fingerprint: "a" } }),
          };
        },
      },
    });
    const usedBy = await screen.findByRole("region", { name: "Used by" });
    const wants = await within(usedBy).findByRole("list", { name: "Apps that want mail" });
    expect(wants).toHaveTextContent("Notes");
    expect(wants).toHaveTextContent("no mail yet");
    await expectAccessible(container);
    await user.click(within(wants).getByRole("button", { name: "Use for Notes" }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked).toMatchObject({ id: "prj_1", need: "mail", choice: "connection:mail" });
  });

  it("says so where no app installed wants mail", async () => {
    renderAt("/connections/mail", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [relay] }) },
      catalog: { listCatalogInstalls: () => ({ installs: [] }) },
    });
    expect(await screen.findByText("No app sends through it yet, and none installed wants mail.")).toBeVisible();
  });
});

describe("adding a connection (#799)", () => {
  it("names each tile in a line, with its full name to a screen reader, and says why Review plan is off", async () => {
    const user = userEvent.setup();
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [aBucket("uploads")] }) },
    });
    await user.click(await screen.findByRole("button", { name: "Add connection" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    const gcs = within(sheet).getByRole("button", { name: "Google Cloud Storage" });
    expect(gcs).toHaveTextContent("Google Cloud");
    expect(gcs).not.toHaveTextContent("Storage");
    expect(within(sheet).getByRole("button", { name: "Another S3-compatible store" })).toHaveTextContent("Other");
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
    expect(within(sheet).getByText("Type the account ID")).toBeVisible();
    // An example reads as one, not as a value someone typed.
    expect(within(sheet).getByRole("textbox", { name: "Bucket" })).toHaveAttribute("placeholder", "e.g. my-app-files");
    await user.type(within(sheet).getByRole("textbox", { name: "Account ID" }), R2);
    expect(await within(sheet).findByText("Type the bucket")).toBeVisible();
    await user.type(within(sheet).getByRole("textbox", { name: "Bucket" }), "files");
    expect(await within(sheet).findByText("Type both keys")).toBeVisible();
  });
});
