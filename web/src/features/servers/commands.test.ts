// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { ServerInventorySchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { serviceCommands } from "./commands";

describe("the commands a server's service manager takes", () => {
  it("are OpenRC's on Alpine, as install.sh sets Dockwell up there", () => {
    expect(serviceCommands({ inventory: create(ServerInventorySchema, { osId: "alpine" }) })).toEqual({
      restartAgent: "sudo rc-service dockwell restart",
      startAgent: "sudo rc-service dockwell start",
      restartDocker: "sudo rc-service docker restart",
    });
  });

  it("are systemd's elsewhere, and where the system isn't known yet", () => {
    for (const inventory of [create(ServerInventorySchema, { osId: "ubuntu" }), undefined]) {
      expect(serviceCommands({ inventory })).toEqual({
        restartAgent: "sudo systemctl restart dockwell",
        startAgent: "sudo systemctl enable --now dockwell",
        restartDocker: "sudo systemctl restart docker",
      });
    }
  });
});
