// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { HostAccessKind, PlanAction, PlanSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { needsConfirming, needsPasskey } from "./notices";

const host = { kind: "server.host", scope: "srv_1", name: "host" };

/** A plan of host edits on one server, as resources and as its steps: installs, a reboot, or more beside them. */
function plan(...resources: Record<string, unknown>[]) {
  const edits: Record<string, unknown>[] = resources.map((r, i) => ({
    id: `res_${i}`,
    resource: host,
    action: PlanAction.UPDATE,
    ...r,
  }));
  return create(PlanSchema, {
    hostAccess: true,
    resources: edits,
    servers: [
      {
        serverId: "srv_1",
        serverName: "web-1",
        steps: edits.map(({ id, hostAccess: _, ...rest }) => ({ resourceId: id as string, ...rest })),
      },
    ],
  });
}

const openssl = { name: "openssl", installed: "3.0.13-0ubuntu3.4", available: "3.0.13-0ubuntu3.5", security: true };

describe("what asks for the typed code (ADR 0064)", () => {
  it("asks for the passkey alone for an install that restarts nothing", () => {
    const p = plan({ install: { packages: [openssl] } });
    expect(needsConfirming(p)).toBe(false);
    expect(needsPasskey(p)).toBe(true);
    // A kernel waits for a reboot, which is a change of its own: installing it stops nothing.
    expect(needsConfirming(plan({ install: { packages: [openssl], reboot: true } }))).toBe(false);
  });

  it("asks for the code for an install that restarts Docker, as that stops what runs there", () => {
    expect(needsConfirming(plan({ install: { packages: [openssl], dockerRestarts: true } }))).toBe(true);
    expect(needsConfirming(plan({ install: { packages: [openssl], dockerRestarts: true, keepsRunning: true } }))).toBe(
      true,
    );
  });

  it("asks for the code for a reboot, the control server's included", () => {
    expect(needsConfirming(plan({ reboot: { controlServer: true } }))).toBe(true);
    expect(needsConfirming(plan({ reboot: {} }))).toBe(true);
  });

  it("asks for it as before for what loses data, grants root, or doesn't say what it reaches", () => {
    expect(
      needsConfirming(create(PlanSchema, { destructive: true, resources: [{ install: { packages: [openssl] } }] })),
    ).toBe(true);
    expect(
      needsConfirming(
        plan(
          { install: { packages: [openssl] } },
          { hostAccess: [{ kind: HostAccessKind.DOCKER_SOCKET, path: "/var/run/docker.sock" }] },
        ),
      ),
    ).toBe(true);
    expect(needsConfirming(plan({ install: { packages: [openssl] } }, {}))).toBe(true);
    expect(needsConfirming(plan({}))).toBe(true);
  });
});

describe("what asks for a passkey up front (ADR 0105)", () => {
  it("asks for an Open whose owner isn't made, as who reaches it first takes it", () => {
    const open = create(PlanSchema, {
      notes: [{ reason: "opens_to_everyone", message: "Anyone who reaches git.example.com first takes Gitea" }],
    });
    expect(needsPasskey(open)).toBe(true);
    expect(needsPasskey(create(PlanSchema, { notes: [{ reason: "owner_held", message: "Held" }] }))).toBe(false);
  });

  it("asks for an Open of an app that signs in with a password everyone knows", () => {
    const open = create(PlanSchema, {
      notes: [{ reason: "known_password", message: "It signs in as admin with a password everyone knows" }],
    });
    expect(needsPasskey(open)).toBe(true);
  });
});

describe("taking on a record made in Cloudflare (ADR 0104, section 5)", () => {
  it("asks for a fresh passkey and no typed code, as it runs nothing on a server", () => {
    const takeover = create(PlanSchema, {
      hostAccess: true,
      resources: [
        {
          id: "rte_1",
          resource: { kind: "route", scope: "env_1", name: "vpn" },
          action: PlanAction.CREATE,
          hostAccess: [
            {
              kind: HostAccessKind.CLOUDFLARE_TAKEOVER,
              path: "vpn.example.com",
              detail: "A 203.0.113.9",
              sameAsRoot: true,
            },
          ],
        },
      ],
    });
    expect(needsPasskey(takeover)).toBe(true);
    expect(needsConfirming(takeover)).toBe(false);
  });
});
