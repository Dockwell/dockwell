// SPDX-License-Identifier: AGPL-3.0-only

import {
  AuthService,
  CatalogService,
  ChangeService,
  FileService,
  LogService,
  Role,
  ServerService,
  ServiceService,
} from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { decide, type Holding } from "./may";

const envs: Record<string, { project: string; protected: boolean }> = {
  env_prod: { project: "prj_blog", protected: true },
  env_staging: { project: "prj_blog", protected: false },
  env_shop: { project: "prj_shop", protected: true },
};

const holds = (...grants: [string, Role][]): Holding => ({
  grants: grants.map(([projectId, role]) => ({ projectId, role })),
  environment: (id) => envs[id],
});

describe("decide", () => {
  it("lets anyone make a call that needs nothing", () => {
    expect(decide(AuthService.method.getAuthState, holds()).allowed).toBe(true);
  });

  it("refuses someone with no role everything else", () => {
    expect(decide(ServiceService.method.listServices, holds())).toEqual({
      allowed: false,
      why: "role",
      needs: Role.VIEWER,
    });
  });

  it("checks the lowest role a method needs against the highest held anywhere", () => {
    const viewer = holds(["prj_blog", Role.VIEWER]);
    expect(decide(ServiceService.method.listServices, viewer).allowed).toBe(true);
    expect(decide(ChangeService.method.proposeChange, viewer)).toEqual({
      allowed: false,
      why: "role",
      needs: Role.DEVELOPER,
    });
    expect(decide(ChangeService.method.proposeChange, holds(["prj_blog", Role.DEVELOPER])).allowed).toBe(true);
  });

  it("keeps the install's own for an admin", () => {
    const developer = holds(["", Role.DEVELOPER]);
    expect(decide(FileService.method.listFiles, developer)).toEqual({ allowed: false, why: "role", needs: Role.ADMIN });
    expect(decide(CatalogService.method.installCatalogEntry, developer).allowed).toBe(false);
    expect(decide(FileService.method.listFiles, holds(["", Role.ADMIN])).allowed).toBe(true);
    expect(decide(FileService.method.listFiles, holds(["", Role.OWNER])).allowed).toBe(true);
  });

  it("asks more in a protected environment", () => {
    const developer = holds(["prj_blog", Role.DEVELOPER]);
    expect(decide(ChangeService.method.applyChange, developer, { environment: "env_staging" }).allowed).toBe(true);
    expect(decide(ChangeService.method.applyChange, developer, { environment: "env_prod" })).toEqual({
      allowed: false,
      why: "protected",
      needs: Role.ADMIN,
    });
    // Planning there is still a developer's.
    expect(decide(ServiceService.method.deployService, developer, { environment: "env_prod" }).allowed).toBe(true);
  });

  it("holds a place by its project's grant or the install's, and nothing else", () => {
    const developer = holds(["prj_blog", Role.DEVELOPER]);
    expect(decide(ServiceService.method.deployService, developer, { environment: "env_shop" })).toEqual({
      allowed: false,
      why: "reach",
      needs: Role.DEVELOPER,
    });
    expect(decide(ServiceService.method.deployService, developer, { project: "prj_shop" }).why).toBe("reach");
    expect(
      decide(ServiceService.method.deployService, holds(["", Role.DEVELOPER]), { environment: "env_shop" }).allowed,
    ).toBe(true);
    // The higher of the two counts.
    const both = holds(["", Role.VIEWER], ["prj_blog", Role.DEVELOPER]);
    expect(decide(ServiceService.method.deployService, both, { environment: "env_staging" }).allowed).toBe(true);
    expect(decide(ServiceService.method.deployService, both, { environment: "env_shop" })).toEqual({
      allowed: false,
      why: "role",
      needs: Role.DEVELOPER,
    });
  });

  it("keeps what's in no project for an admin, whatever the method needs elsewhere", () => {
    const viewer = holds(["", Role.VIEWER]);
    expect(decide(LogService.method.readLogs, viewer, { environment: "env_prod" }).allowed).toBe(true);
    expect(decide(LogService.method.readLogs, viewer, { install: true })).toEqual({
      allowed: false,
      why: "role",
      needs: Role.ADMIN,
    });
    expect(decide(LogService.method.readLogs, holds(["", Role.ADMIN]), { install: true }).allowed).toBe(true);
  });

  it("takes an environment it hasn't read yet as protected", () => {
    const developer = holds(["", Role.DEVELOPER]);
    expect(decide(ChangeService.method.applyChange, developer, { environment: "env_unread" }).allowed).toBe(false);
    expect(decide(ServiceService.method.deployService, developer, { environment: "env_unread" }).allowed).toBe(true);
  });

  it("lets anyone with a role read what projects name on the install", () => {
    expect(decide(ServerService.method.listServers, holds(["prj_blog", Role.VIEWER])).allowed).toBe(true);
  });
});
