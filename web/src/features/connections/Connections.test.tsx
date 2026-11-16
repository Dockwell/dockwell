// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeSchema,
  ChangeState,
  type CheckConnectionRequest,
  ConnectionInfoSchema,
  ConnectionReach,
  type ConnectStorageRequest,
  type DisconnectStorageRequest,
  EnvironmentVariableSchema,
  LinkKind,
  LinkPart,
  type ProposeChangeRequest,
  Role,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, aRevision, ENV, places, SERVER } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { webPlanned } from "../../test/changes";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { freeConnectionName, placeOf, provider } from "./providers";
import { freeLinkName, suggestedStorage, suggestStoragePart } from "./storage";

const R2 = "0123456789abcdef0123456789abcdef";

function aConnection(init: MessageInitShape<typeof ConnectionInfoSchema> = {}) {
  return create(ConnectionInfoSchema, {
    id: "cnx_1",
    name: "outline-uploads",
    displayName: "Cloudflare R2 · outline-uploads",
    kind: LinkKind.S3,
    endpoint: `https://${R2}.r2.cloudflarestorage.com`,
    region: "auto",
    bucket: "outline-uploads",
    pathStyle: true,
    reach: ConnectionReach.PROJECTS,
    projects: ["prj_1"],
    ...init,
  } as MessageInitShape<typeof ConnectionInfoSchema>);
}

const used = aConnection({
  uses: [
    {
      linkId: "lnk_1",
      link: "storage",
      environment: ENV,
      project: "prj_1",
      prefix: "outline/",
      sets: { S3_BUCKET: LinkPart.BUCKET, S3_SECRET_ACCESS_KEY: LinkPart.SECRET_KEY },
      appIds: ["svc_outline"],
      apps: ["outline"],
    },
  ],
});

function planned() {
  return vi.fn((req: ProposeChangeRequest) => {
    void req;
    return { change: create(ChangeSchema, { id: "chg_1", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }) };
  });
}

/** The fleet: one server, hz-1, connected. */
function fleet(): Pick<Api, "servers"> {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return { servers: { watchServers: watch.stream } };
}

describe("what Dockwell works out for a connection", () => {
  it("reads which store an endpoint is at, and makes each store's from what's typed", () => {
    expect(placeOf(`https://${R2}.r2.cloudflarestorage.com`)).toEqual({ provider: "r2", account: R2, region: "" });
    expect(placeOf("https://s3.eu-central-1.amazonaws.com", "eu-central-1")).toMatchObject({
      provider: "s3",
      region: "eu-central-1",
    });
    expect(placeOf("https://s3.us-west-004.backblazeb2.com")).toMatchObject({ provider: "b2", region: "us-west-004" });
    expect(placeOf("https://storage.googleapis.com").provider).toBe("gcs");
    expect(placeOf("https://minio.example.com:9000", "home").provider).toBe("other");
    expect(provider("r2").endpoint({ account: R2, region: "" })).toBe(`https://${R2}.r2.cloudflarestorage.com`);
    expect(provider("r2").endpoint({ account: "not-an-id", region: "" })).toBe("");
    expect(provider("b2").endpoint({ account: "", region: "eu-central-003" })).toBe(
      "https://s3.eu-central-003.backblazeb2.com",
    );
    expect(freeConnectionName("Outline Uploads", ["outline-uploads"])).toBe("outline-uploads-2");
    expect(freeConnectionName("3d-models", [])).toBe("s-3d-models");
  });

  it("suggests what each of an app's names holds, a key only into a secret, each part once", () => {
    expect(suggestStoragePart("AWS_SECRET_ACCESS_KEY")).toBe(LinkPart.SECRET_KEY);
    expect(suggestStoragePart("S3_ACCESS_KEY_ID")).toBe(LinkPart.ACCESS_KEY_ID);
    expect(suggestStoragePart("STORAGE_ENDPOINT")).toBe(LinkPart.ENDPOINT);
    expect(suggestStoragePart("S3_FORCE_PATH_STYLE")).toBe(LinkPart.PATH_STYLE);
    expect(suggestStoragePart("SMTP_HOST")).toBe(LinkPart.UNSPECIFIED);
    const parts = suggestedStorage([
      { name: "S3_BUCKET", secret: false },
      { name: "S3_ACCESS_KEY_ID", secret: false },
      { name: "S3_SECRET_ACCESS_KEY", secret: true },
      { name: "AWS_SECRET_ACCESS_KEY", secret: true },
    ]);
    expect([...parts]).toEqual([
      ["S3_BUCKET", LinkPart.BUCKET],
      ["S3_SECRET_ACCESS_KEY", LinkPart.SECRET_KEY],
    ]);
    expect(freeLinkName(["storage"])).toBe("storage-2");
  });
});

describe("Connections", () => {
  it("lists each connection with what reads it, and adds one from a store", async () => {
    const { container } = renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({
          connections: [
            used,
            aConnection({ id: "cnx_2", name: "spare", bucket: "spare-bucket", displayName: "Spare" }),
          ],
        }),
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Connections" })).toBeVisible();
    expect(await screen.findByText("2 connections · used by 1 app")).toBeVisible();
    const row = screen.getByRole("row", { name: /outline-uploads/ });
    expect(row).toHaveTextContent("Cloudflare R2 · outline-uploads");
    expect(row).toHaveTextContent("1 app");
    expect(screen.getByRole("row", { name: /spare/ })).toHaveTextContent("No app yet");
    expect(screen.getByRole("button", { name: "Add connection" })).toBeVisible();
    await expectAccessible(container);
  });

  it("starts empty with a tile for each store, and holds nothing back below an admin but the way to add one", async () => {
    const user = userEvent.setup();
    renderAt("/connections", { ...fleet(), resources: places() });
    await user.click(await screen.findByRole("button", { name: "Backblaze B2" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    expect(within(sheet).getByRole("button", { name: "Backblaze B2" })).toHaveAttribute("aria-pressed", "true");
    expect(within(sheet).getByRole("combobox", { name: "Region" })).toHaveValue("us-west-004");
  });

  it("shows a developer the connections their project may use, with no way to add one", async () => {
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      auth: {
        getAuthState: () =>
          authState({
            user: { ...mira, role: Role.UNSPECIFIED },
            grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
          }),
      },
      connections: {
        listConnections: () => ({
          connections: [create(ConnectionInfoSchema, { id: "cnx_1", name: "shared", kind: LinkKind.S3 })],
        }),
      },
    });
    expect(await screen.findByRole("row", { name: /shared/ })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Add connection" })).toBeNull();
    expect(screen.queryByRole("columnheader", { name: "Projects" })).toBeNull();
  });

  it("checks a bucket from a server as its keys are typed, and plans it with its store's endpoint", async () => {
    const user = userEvent.setup();
    const checks: CheckConnectionRequest[] = [];
    const proposed = planned();
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({ connections: [used] }),
        checkConnection: (req) => {
          checks.push(req);
          return req.s3?.secretKey === "right-secret"
            ? { passed: true, server: "hz-1" }
            : { passed: false, step: "write", code: "SignatureDoesNotMatch", status: 403 };
        },
      },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    await user.click(await screen.findByRole("button", { name: "Add connection" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    await user.type(within(sheet).getByRole("textbox", { name: "Account ID" }), R2);
    await user.type(within(sheet).getByRole("textbox", { name: "Bucket" }), "wiki-files");
    await user.type(within(sheet).getByRole("textbox", { name: "Access key ID" }), "AKIDEXAMPLE");
    await user.type(within(sheet).getByLabelText("Secret access key"), "wrong");
    expect(await within(sheet).findByText("Keys refused", {}, { timeout: 3000 })).toBeVisible();
    expect(review).toBeDisabled();
    await user.clear(within(sheet).getByLabelText("Secret access key"));
    await user.type(within(sheet).getByLabelText("Secret access key"), "right-secret");
    expect(await within(sheet).findByText("Reached from hz-1", {}, { timeout: 3000 })).toBeVisible();
    expect(checks.at(-1)?.s3).toMatchObject({
      endpoint: `https://${R2}.r2.cloudflarestorage.com`,
      region: "auto",
      bucket: "wiki-files",
      pathStyle: true,
    });
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "connection", name: "wiki-files" });
    expect(edit?.spec?.kind.value).toMatchObject({
      kind: LinkKind.S3,
      reach: ConnectionReach.EVERY,
      s3: { bucket: "wiki-files", accessKeyId: "AKIDEXAMPLE", secretKey: "right-secret" },
    });
  });

  it("plans a bucket this server can't reach, as an app may reach it from its own network", async () => {
    const user = userEvent.setup();
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: { checkConnection: () => ({ passed: false, step: "connect", code: "unreachable", server: "hz-1" }) },
    });
    await user.click(await screen.findByRole("button", { name: "Another S3-compatible store" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    await user.type(within(sheet).getByRole("textbox", { name: "Endpoint" }), "http://minio:9000");
    await user.type(within(sheet).getByRole("textbox", { name: "Bucket" }), "wiki-files");
    await user.type(within(sheet).getByRole("textbox", { name: "Access key ID" }), "minio");
    await user.type(within(sheet).getByLabelText("Secret access key"), "minio-secret");
    expect(await within(sheet).findByText("Can't reach it", {}, { timeout: 3000 })).toBeVisible();
    expect(within(sheet).getByText(/An app's link checks it again from where the app runs/)).toBeVisible();
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled();
  });

  // Review M1: an address Dockwell keeps every check and app from, such as loopback or a
  // metadata service, can never work, so it isn't saved as one nothing answered.
  it("doesn't plan an address Dockwell keeps apps from, and says why", async () => {
    const user = userEvent.setup();
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: {
        checkConnection: () => ({
          passed: false,
          step: "connect",
          code: "refused",
          words: "127.0.0.1 is this server's own address, so Dockwell sends nothing there",
          server: "hz-1",
        }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Another S3-compatible store" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    await user.type(within(sheet).getByRole("textbox", { name: "Endpoint" }), "http://127.0.0.1:9000");
    await user.type(within(sheet).getByRole("textbox", { name: "Bucket" }), "wiki-files");
    await user.type(within(sheet).getByRole("textbox", { name: "Access key ID" }), "minio");
    await user.type(within(sheet).getByLabelText("Secret access key"), "minio-secret");
    expect(await within(sheet).findByText("Address refused", {}, { timeout: 3000 })).toBeVisible();
    expect(within(sheet).getByText(/this server's own address.*No app can use it either/)).toBeVisible();
    expect(within(sheet).queryByText(/An app's link checks it again/)).toBeNull();
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
  });

  // Review H1: a secret typed alone was planned beside an empty access key ID, which
  // every app reading the connection would then start with.
  it("asks for both keys once either is typed, and never plans a new secret without its ID", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    const minio = aConnection({ name: "minio", displayName: "minio", endpoint: "http://minio:9000", bucket: "wiki-files" });
    renderAt("/connections/minio", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({ connections: [minio] }),
        checkConnection: () => ({ passed: false, step: "connect", code: "unreachable", server: "hz-1" }),
      },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    await user.click(await screen.findByRole("button", { name: "Replace keys" }));
    const sheet = await screen.findByRole("dialog", { name: "Connection minio" });
    await user.type(within(sheet).getByLabelText("Secret access key"), "rotated-secret");
    expect(await within(sheet).findByText("Type both keys")).toBeVisible();
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
    expect(within(sheet).getByRole("textbox", { name: "Access key ID" })).not.toHaveAttribute("placeholder");

    await user.type(within(sheet).getByRole("textbox", { name: "Access key ID" }), "minio");
    expect(await within(sheet).findByText("Can't reach it", {}, { timeout: 3000 })).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const s3 = (proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value as { s3?: { accessKeyId: string; secretKey: string } })
      ?.s3;
    expect(s3).toMatchObject({ accessKeyId: "minio", secretKey: "rotated-secret" });
  });

  it("shows one connection: what reads it as what, where it is, and what a check found", async () => {
    const user = userEvent.setup();
    const { container } = renderAt("/connections/outline-uploads", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({ connections: [used] }),
        checkConnection: () => ({ passed: false, step: "write", code: "NoSuchBucket", status: 404, server: "hz-1" }),
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "outline-uploads" })).toBeVisible();
    const usedBy = screen.getByRole("region", { name: "Used by" });
    expect(within(usedBy).getByRole("link", { name: "outline" })).toHaveAttribute(
      "href",
      "/apps/svc_outline/environment",
    );
    expect(usedBy).toHaveTextContent("S3_SECRET_ACCESS_KEY");
    expect(usedBy).toHaveTextContent("secret access key");
    expect(usedBy).toHaveTextContent("outline/");
    const details = screen.getByRole("region", { name: "Details" });
    expect(details).toHaveTextContent(`https://${R2}.r2.cloudflarestorage.com`);
    expect(details).toHaveTextContent("Sealed, never shown");
    await user.click(screen.getByRole("button", { name: "Check" }));
    expect(await screen.findByText("Check the bucket's name.")).toBeVisible();
    await expectAccessible(container);
  });
});

describe("disconnecting an app from a connection", () => {
  // Review M2: Remove said "Disconnect them first", and nothing in the panel could.
  it("plans each link's disconnect from Used by, and says why Remove waits for it", async () => {
    const user = userEvent.setup();
    const asked: DisconnectStorageRequest[] = [];
    renderAt("/connections/outline-uploads", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({ connections: [used] }),
        disconnectStorage: (req) => {
          asked.push(req);
          return { change: webPlanned() };
        },
      },
      changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    });
    const usedBy = await screen.findByRole("region", { name: "Used by" });
    await user.click(within(usedBy).getByRole("button", { name: "Disconnect storage" }));
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0]).toMatchObject({ project: "prj_1", environment: ENV, link: "storage" });
    expect(await screen.findByRole("button", { name: /Apply plan/ })).toBeVisible();
  });

  it("keeps Remove off while a link names the connection, with the reason", async () => {
    const user = userEvent.setup();
    renderAt("/connections/outline-uploads", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [used] }) },
    });
    await user.click(await screen.findByRole("button", { name: "Replace keys" }));
    const sheet = await screen.findByRole("dialog", { name: "Connection outline-uploads" });
    const remove = within(sheet).getByRole("button", { name: /Remove/ });
    expect(remove).toHaveAttribute("aria-disabled", "true");
    expect(sheet).toHaveTextContent("1 link names it. Disconnect it under Used by first.");
  });
});

describe("connecting an app to storage", () => {
  const variable = (init: MessageInitShape<typeof EnvironmentVariableSchema>) =>
    create(EnvironmentVariableSchema, init);
  const uses = (name: string) => [{ serviceId: "svc_outline", service: "outline", name }];
  const variables = [
    variable({ id: "var_1", name: "S3_BUCKET", value: "old", usedBy: uses("S3_BUCKET") }),
    variable({ id: "var_2", name: "S3_ENDPOINT", value: "https://old.example.com", usedBy: uses("S3_ENDPOINT") }),
    variable({ id: "var_3", name: "S3_ACCESS_KEY_ID", value: "old", usedBy: uses("S3_ACCESS_KEY_ID") }),
    variable({ id: "sec_1", name: "S3_SECRET_ACCESS_KEY", secret: true, usedBy: uses("S3_SECRET_ACCESS_KEY") }),
  ];
  function api(asked: ConnectStorageRequest[], refuse?: ConnectError): Api {
    return {
      ...fleet(),
      resources: places(),
      services: {
        listServices: () => ({ services: [anApp("outline", "ghcr.io/acme/outline:1")] }),
        listServiceRevisions: () => ({ revisions: [aRevision(3n, "ghcr.io/acme/outline:1")] }),
      },
      variables: { listVariables: () => ({ environmentId: ENV, variables }) },
      connections: {
        listConnections: () => ({ connections: [aConnection()] }),
        connectStorage: (req) => {
          asked.push(req);
          if (refuse) throw refuse;
          return { change: webPlanned() };
        },
      },
      changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    };
  }

  it("sets the names the app reads from the connection, a key only into a secret, as one plan", async () => {
    const user = userEvent.setup();
    const asked: ConnectStorageRequest[] = [];
    const { container } = renderAt("/apps/svc_outline/environment", api(asked));
    await user.click(await screen.findByRole("button", { name: "Connect storage" }, { timeout: 5000 }));
    const sheet = await screen.findByRole("dialog", { name: "Connect outline to storage" });
    expect(await within(sheet).findByRole("combobox", { name: "Connection" })).toHaveValue("outline-uploads");
    expect(within(sheet).getByRole("combobox", { name: "S3_BUCKET" })).toHaveValue(String(LinkPart.BUCKET));
    expect(within(sheet).getByRole("combobox", { name: "S3_SECRET_ACCESS_KEY" })).toHaveValue(
      String(LinkPart.SECRET_KEY),
    );
    // A variable shows in a plan, so a key isn't offered for it.
    const keyId = within(sheet).getByRole("combobox", { name: "S3_ACCESS_KEY_ID" });
    expect(keyId).toHaveValue(String(LinkPart.UNSPECIFIED));
    expect(within(keyId).queryByRole("option", { name: "Access key ID" })).toBeNull();
    await expectAccessible(container);

    await user.click(within(sheet).getByRole("button", { name: /Check and review plan/ }));
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0]).toMatchObject({
      environment: ENV,
      app: "outline",
      connection: "outline-uploads",
      link: "storage",
      prefix: "outline/",
      sets: { S3_BUCKET: LinkPart.BUCKET, S3_ENDPOINT: LinkPart.ENDPOINT, S3_SECRET_ACCESS_KEY: LinkPart.SECRET_KEY },
    });
  });

  it("plans nothing when the check from the app's server fails, and says what it found", async () => {
    const user = userEvent.setup();
    const refused = new ConnectError(
      "The bucket refused the check at its write step, with AccessDenied, so nothing was planned. Check the connection's keys, and that they may write in its bucket",
      Code.FailedPrecondition,
    );
    renderAt("/apps/svc_outline/environment", api([], refused));
    await user.click(await screen.findByRole("button", { name: "Connect storage" }, { timeout: 5000 }));
    const sheet = await screen.findByRole("dialog", { name: "Connect outline to storage" });
    await within(sheet).findByRole("combobox", { name: "Connection" });
    await user.click(within(sheet).getByRole("button", { name: /Check and review plan/ }));
    expect(await within(sheet).findByText(/with AccessDenied, so nothing was planned/)).toBeVisible();
    expect(within(sheet).getByText("Nothing was planned")).toBeVisible();
  });
});
