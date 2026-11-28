// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  type ConnectCustomRequest,
  ConnectionCustomType,
  ConnectionInfoSchema,
  ConnectionReach,
  LinkKind,
  type ProposeChangeRequest,
  Role,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { anApp, ENV, places } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { webPlanned } from "../../test/changes";
import { type Api, feed, renderAt } from "../../test/render";

const saved = (name = "example-api", inUse = false) =>
  create(ConnectionInfoSchema, {
    id: `cnx_${name}`,
    name,
    displayName: name,
    kind: LinkKind.CUSTOM,
    reach: ConnectionReach.EVERY,
    revision: 3n,
    custom: {
      inUse,
      fields: [{ name: "api_key", type: ConnectionCustomType.SECRET, set: true, defaultEnv: "EXAMPLE_API_KEY" }],
    },
  });

function appApi(): Api {
  return {
    resources: places(),
    services: { listServices: () => ({ services: [anApp("web", "example.com/web:1", { env: { OCCUPIED: "1" } })] }) },
    variables: { listVariables: () => ({ environmentId: ENV, variables: [] }) },
  };
}

const picker = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(await screen.findByRole("button", { name: "Add from a connection" }));
  return within(await screen.findByRole("dialog", { name: "Add from a connection" }));
};

describe("editing a Custom connection", () => {
  it.each(["keep", "clear", "rename"] as const)("sends the right secret value when you %s a field", async (action) => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/connections/example-api", {
      resources: places(),
      connections: { listConnections: () => ({ connections: [saved()] }) },
      changes: {
        proposeChange: (req) => {
          proposals.push(req);
          return { change: webPlanned() };
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });

    await user.click(await screen.findByRole("button", { name: /^Change$/ }));
    const editor = within(await screen.findByRole("dialog", { name: "Connection example-api" }));
    const review = editor.getByRole("button", { name: "Review plan" });
    const value = editor.getByLabelText("Field 1 value");
    expect(value).toHaveValue("");
    expect(value).toHaveAttribute("placeholder", "Secret is set");
    if (action === "clear") await user.click(editor.getByRole("button", { name: "Clear secret" }));
    if (action === "rename") {
      const name = editor.getByLabelText("Field 1 name");
      await user.clear(name);
      await user.type(name, "token");
      expect(value).not.toHaveAttribute("placeholder");
      expect(editor.getByText("Enter the secret again to save the field under its new name.")).toBeVisible();
      expect(review).toBeDisabled();
      await user.type(value, "local-replacement");
    }
    await user.click(review);
    await waitFor(() => expect(proposals).toHaveLength(1));
    const spec = proposals[0]?.edits[0]?.spec?.kind;
    const field = spec?.case === "connection" ? spec.value.custom?.fields[0] : undefined;
    expect(field?.name).toBe(action === "rename" ? "token" : "api_key");
    expect(field?.secretValue).toBe(action === "rename" ? "local-replacement" : action === "clear" ? "" : undefined);
  });
});

describe("mapping a Custom connection", () => {
  it("uses the available connection, suggests an alias and sends the reviewed mapping", async () => {
    const user = userEvent.setup();
    const requests: ConnectCustomRequest[] = [];
    renderAt("/apps/svc_web/environment", {
      ...appApi(),
      connections: {
        listConnections: () => ({ connections: [saved("busy", true), saved()] }),
        connectCustom: (req) => {
          requests.push(req);
          return { change: webPlanned() };
        },
      },
      changes: { watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await picker(user);
    const connection = await sheet.findByLabelText("Connection");
    expect(within(connection).queryByRole("option", { name: "busy" })).toBeNull();
    const field = sheet.getByLabelText("api_key");
    expect(field).toHaveValue("EXAMPLE_API_KEY");
    await user.clear(field);
    await user.type(field, "OCCUPIED");
    const review = sheet.getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    await user.clear(field);
    await user.type(field, "APP_TOKEN");
    await user.click(review);
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0]).toMatchObject({
      project: "prj_1",
      environment: ENV,
      app: "web",
      connection: "example-api",
      fields: { api_key: "APP_TOKEN" },
    });
  });

  it("offers an administrator the creation form from the empty picker", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_web/environment", {
      ...appApi(),
      connections: { listConnections: () => ({ connections: [] }) },
    });
    const sheet = await picker(user);
    await user.click(await sheet.findByRole("button", { name: "Add a Custom connection" }));
    const editor = within(await screen.findByRole("dialog", { name: "Add a Custom connection" }));
    await user.click(editor.getByRole("button", { name: "Advanced" }));
    expect(editor.getByLabelText("Who may use it")).toHaveDisplayValue("Selected projects");
  });

  it("keeps an unshared connection used elsewhere out of a developer's picker", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_web/environment", {
      ...appApi(),
      auth: {
        getAuthState: () =>
          authState({
            user: { ...mira, role: Role.UNSPECIFIED },
            grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
          }),
      },
      connections: { listConnections: () => ({ connections: [saved("busy", true)] }) },
    });
    const sheet = await picker(user);
    expect(await sheet.findByText("An admin adds the connections your apps may use.")).toBeVisible();
    expect(sheet.queryByRole("button", { name: "Add a Custom connection" })).toBeNull();
  });
});
