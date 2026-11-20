// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  BackupDestinationInfoSchema,
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
  ServiceSchema,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, aRevision, ENV, places, projectPlaces, SERVER } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { webPlanned } from "../../test/changes";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { freeConnectionName, placeOf, provider } from "./providers";
import { appsOn, freeLinkName, freePrefix, keptPrefix, suggestedStorage, suggestStoragePart } from "./storage";

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

  // Slice 1 review, H2: two links on one connection never keep to one prefix.
  it("picks a prefix no other link on the connection meets", () => {
    expect(freePrefix("ghost/", [])).toBe("ghost/");
    expect(freePrefix("ghost/", ["ghost/", "ghost-2/"])).toBe("ghost-3/");
    expect(freePrefix("ghost/", ["ghost/media/"])).toBe("ghost-2/");
    expect(freePrefix("ghost/", ["gh/"])).toBe("ghost/");
    expect(freePrefix("ghost/", [""])).toBe("ghost/");
    expect(freePrefix("ghost/media/", ["ghost/"])).toBe("ghost-2/");
  });
});

describe("where an app keeps its files", () => {
  // The review's H1: an app no link tells a prefix keeps its files at the bucket's root.
  it("reads a link that names no prefix setting as at the root, and suggests the setting by its name", () => {
    expect(keptPrefix({ prefix: "site/", sets: { S3_BUCKET: LinkPart.BUCKET } })).toBe("");
    expect(keptPrefix({ prefix: "site/", sets: { S3_PREFIX: LinkPart.KEY_PREFIX } })).toBe("site/");
    expect(suggestStoragePart("S3_PREFIX")).toBe(LinkPart.KEY_PREFIX);
    expect(suggestStoragePart("STORAGE_KEY_PREFIX")).toBe(LinkPart.KEY_PREFIX);
  });

  // The re-check's N2: an app is a service, and links no service reads yet are one app
  // of their environment.
  it("counts apps by the services that read a link", () => {
    expect(
      appsOn({
        uses: [
          { environment: ENV, appIds: ["svc_notes"] },
          { environment: ENV, appIds: ["svc_gallery"] },
        ],
      }),
    ).toBe(2);
    expect(
      appsOn({
        uses: [
          { environment: ENV, appIds: ["svc_notes"] },
          { environment: ENV, appIds: [] },
        ],
      }),
    ).toBe(1);
    expect(
      appsOn({
        uses: [
          { environment: ENV, appIds: [] },
          { environment: "env_other", appIds: [] },
        ],
      }),
    ).toBe(2);
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

  // Review L6: two apps of one name, in two environments, counted as one.
  it("counts the apps that read a connection by app, and tells same-named ones apart", async () => {
    const twice = aConnection({
      uses: [
        { linkId: "lnk_1", link: "storage", environment: ENV, project: "prj_1", appIds: ["svc_a"], apps: ["outline"] },
        {
          linkId: "lnk_2",
          link: "storage",
          environment: "env_staging",
          project: "prj_1",
          appIds: ["svc_b"],
          apps: ["outline"],
        },
      ],
    });
    renderAt("/connections", {
      ...fleet(),
      resources: places("staging"),
      connections: { listConnections: () => ({ connections: [twice] }) },
    });
    const row = await screen.findByRole("row", { name: /outline-uploads/ });
    expect(row).toHaveTextContent("2 apps");
    expect(row).toHaveTextContent("outline (production), outline (staging)");
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
    expect(await within(sheet).findByText("Keys work · bucket found", {}, { timeout: 3000 })).toBeVisible();
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
    expect(within(sheet).getByText(/It saves unchecked: each app's link checks it/)).toBeVisible();
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled();
  });

  // Review L1: opening Change checked the saved keys from a server at once, unasked.
  it("checks a saved connection only when asked, from the control server", async () => {
    const user = userEvent.setup();
    const checks: CheckConnectionRequest[] = [];
    const watch = feed<WatchServersResponse>();
    watch.push(
      changed(aServer({ id: "srv_web", name: "web-1" })),
      changed(aServer({ id: SERVER, name: "hz-1", controlServer: true })),
      snapshotComplete,
    );
    renderAt("/connections/outline-uploads", {
      servers: { watchServers: watch.stream },
      resources: places(),
      connections: {
        listConnections: () => ({ connections: [used] }),
        checkConnection: (req) => {
          checks.push(req);
          return { passed: true, server: "hz-1" };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Replace keys" }));
    const sheet = await screen.findByRole("dialog", { name: "Connection outline-uploads" });
    await new Promise((resolve) => setTimeout(resolve, 1000));
    expect(checks).toHaveLength(0);
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled();
    await user.click(within(sheet).getByRole("button", { name: "Check" }));
    expect(await within(sheet).findByText("Keys work · bucket found", {}, { timeout: 3000 })).toBeVisible();
    expect(checks).toHaveLength(1);
    expect(checks[0]).toMatchObject({ name: "outline-uploads", server: SERVER });
    expect(checks[0]?.s3?.secretKey).toBe("");
  });

  // Review L2: a check already sent ran on after its inputs changed or the sheet closed.
  it("stops a check that's running once what it checks changes, or the sheet closes", async () => {
    const user = userEvent.setup();
    const signals: AbortSignal[] = [];
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: {
        checkConnection: (_req, ctx) => {
          signals.push(ctx.signal);
          return new Promise(() => {});
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Another S3-compatible store" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    await user.type(within(sheet).getByRole("textbox", { name: "Endpoint" }), "http://minio:9000");
    await user.type(within(sheet).getByRole("textbox", { name: "Bucket" }), "wiki-files");
    await user.type(within(sheet).getByRole("textbox", { name: "Access key ID" }), "minio");
    await user.type(within(sheet).getByLabelText("Secret access key"), "minio-secret");
    await waitFor(() => expect(signals).toHaveLength(1), { timeout: 3000 });
    await user.type(within(sheet).getByRole("textbox", { name: "Bucket" }), "-2");
    await waitFor(() => expect(signals[0]?.aborted).toBe(true));
    await waitFor(() => expect(signals).toHaveLength(2), { timeout: 3000 });
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(signals[1]?.aborted).toBe(true));
  });

  // #710 L5: a saved connection's keys go only to the control server or a server an
  // app linked to it runs on, so until they're typed, those alone check it.
  it("checks a saved connection only from where its keys may go, until they're typed", async () => {
    const user = userEvent.setup();
    const checks: CheckConnectionRequest[] = [];
    const watch = feed<WatchServersResponse>();
    watch.push(
      changed(aServer({ id: "srv_cp", name: "cp-1", controlServer: true })),
      changed(aServer({ id: SERVER, name: "hz-1" })),
      changed(aServer({ id: "srv_db", name: "db-1" })),
      snapshotComplete,
    );
    renderAt("/connections/outline-uploads", {
      servers: { watchServers: watch.stream },
      resources: places(),
      // outline, which reads the connection through its link, runs on hz-1.
      services: {
        listServices: () => ({ services: [anApp("outline", "ghcr.io/acme/outline:1", {}, { id: "svc_outline" })] }),
      },
      connections: {
        listConnections: () => ({ connections: [used] }),
        checkConnection: (req) => {
          checks.push(req);
          return { passed: true, server: "hz-1" };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Replace keys" }));
    const sheet = await screen.findByRole("dialog", { name: "Connection outline-uploads" });
    await user.click(within(sheet).getByRole("button", { name: "Advanced" }));
    const from = within(sheet).getByRole("combobox", { name: "Checked from" });
    const offered = () =>
      within(from)
        .getAllByRole("option")
        .map((o) => o.textContent);
    await waitFor(() => expect(offered()).toEqual(["cp-1", "hz-1"]));
    expect(from).toHaveValue("srv_cp");
    expect(within(sheet).getByText("Its saved keys stay on these servers. Type them to pick any.")).toBeVisible();
    await user.selectOptions(from, "srv_1");
    await user.click(within(sheet).getByRole("button", { name: "Check" }));
    await waitFor(() => expect(checks).toHaveLength(1));
    expect(checks[0]).toMatchObject({ name: "outline-uploads", server: SERVER });

    // Typed keys go to any server.
    await user.type(within(sheet).getByRole("textbox", { name: "Access key ID" }), "AKIDNEW");
    await user.type(within(sheet).getByLabelText("Secret access key"), "a-new-secret");
    await waitFor(() => expect(offered()).toEqual(["cp-1", "hz-1", "db-1"]));
    expect(within(sheet).queryByText(/Its saved keys stay on these servers/)).toBeNull();
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
    expect(within(sheet).queryByText(/It saves unchecked/)).toBeNull();
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
  });

  // Review H1: a secret typed alone was planned beside an empty access key ID, which
  // every app reading the connection would then start with.
  it("asks for both keys once either is typed, and never plans a new secret without its ID", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    const minio = aConnection({
      name: "minio",
      displayName: "minio",
      endpoint: "http://minio:9000",
      bucket: "wiki-files",
    });
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
    const s3 = (
      proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value as { s3?: { accessKeyId: string; secretKey: string } }
    )?.s3;
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
    // Until the control plane's first check, it says so (review L4, #799).
    expect(screen.getByText("Not checked yet")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Check" }));
    expect(await screen.findByText("Check the bucket's name.")).toBeVisible();
    expect(screen.queryByText("Not checked yet")).toBeNull();
    await expectAccessible(container);
  });
});

describe("sharing a connection", () => {
  const specOfCall = (proposed: ReturnType<typeof planned>) =>
    proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value as {
      shared?: boolean;
      s3?: { accessKeyId: string; secretKey: string; bucket: string };
    };

  // ADR 0099: a connection serves one app unless an admin shares it, as a plan.
  it("plans sharing from the connection's switch, with no keys, and keeps it while more than one app uses it", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    renderAt("/connections/outline-uploads", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [used] }) },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    const details = await screen.findByRole("region", { name: "Details" });
    const toggle = within(details).getByRole("switch", { name: "More than one app may use it" });
    expect(toggle).toHaveAttribute("aria-checked", "false");
    expect(toggle).toHaveAccessibleDescription("Each keeps its files under a prefix of its own");
    await user.click(toggle);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(proposed.mock.calls[0]?.[0].summary).toBe("Share the connection Cloudflare R2 · outline-uploads");
    const spec = specOfCall(proposed);
    expect(spec.shared).toBe(true);
    expect(spec.s3).toMatchObject({ bucket: "outline-uploads", accessKeyId: "", secretKey: "" });
  });

  it("keeps a shared connection's switch off while two apps use it, with why", async () => {
    const use = (linkId: string, prefix: string, app: string, environment = ENV) => ({
      linkId,
      link: "storage",
      environment,
      project: "prj_1",
      prefix,
      sets: { S3_BUCKET: LinkPart.BUCKET },
      appIds: [`svc_${app}`],
      apps: [app],
    });
    const two = aConnection({
      shared: true,
      uses: [use("lnk_1", "outline/", "outline"), use("lnk_2", "wiki/", "wiki", "env_wiki")],
    });
    renderAt("/connections/outline-uploads", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [two] }) },
    });
    const details = await screen.findByRole("region", { name: "Details" });
    const toggle = within(details).getByRole("switch", { name: "More than one app may use it" });
    expect(toggle).toHaveAttribute("aria-checked", "true");
    expect(toggle).toBeDisabled();
    expect(toggle).toHaveAccessibleDescription("Stays shared while 2 apps use it: disconnect all but one first");
  });

  // The review's M2: two apps from before on an unshared connection keep working, and
  // sharing it is one step away.
  it("says when two apps use a connection that isn't shared, with Share it", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    const app = (linkId: string, environment: string, name: string) => ({
      linkId,
      link: "storage",
      environment,
      project: "prj_1",
      prefix: `${name}/`,
      sets: { S3_BUCKET: LinkPart.BUCKET },
      appIds: [`svc_${name}`],
      apps: [name],
    });
    const two = aConnection({ uses: [app("lnk_1", ENV, "outline"), app("lnk_2", "env_wiki", "wiki")] });
    renderAt("/connections/outline-uploads", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [two] }) },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    expect(await screen.findByText("2 apps use it, but it isn't shared")).toBeVisible();
    // The callout says it once: nothing else on the page repeats "isn't shared".
    expect(screen.queryByText("It serves an app already, and isn't shared")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Share it" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(specOfCall(proposed).shared).toBe(true);
  });

  // The owner's ask with ConnectionInfo.shared: the sheet's own save keeps sharing as it is.
  it("keeps a connection shared when its sheet saves another change", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    const shared = aConnection({ name: "minio", displayName: "minio", endpoint: "http://minio:9000", shared: true });
    renderAt("/connections/minio", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({ connections: [shared] }),
        checkConnection: () => ({ passed: false, step: "connect", code: "unreachable", server: "hz-1" }),
      },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    await user.click(await screen.findByRole("button", { name: "Replace keys" }));
    const sheet = await screen.findByRole("dialog", { name: "Connection minio" });
    await user.type(within(sheet).getByRole("textbox", { name: "Access key ID" }), "minio");
    await user.type(within(sheet).getByLabelText("Secret access key"), "rotated-secret");
    expect(await within(sheet).findByText("Can't reach it", {}, { timeout: 3000 })).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(specOfCall(proposed).shared).toBe(true);
  });

  it("says in the list which connections are shared", async () => {
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [aConnection({ ...used, shared: true })] }) },
    });
    expect(await screen.findByText("1 app · shared")).toBeVisible();
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

  // Re-check N1: below an install admin, Disconnect doesn't copy a connection's key
  // into a secret they could reveal; the names that hold one go, and the row says so.
  it("says below an install admin that a link's keys go with it, and not to an admin", async () => {
    const developer = {
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
          connections: [aConnection({ ...used, endpoint: "", bucket: "", reach: ConnectionReach.PROJECTS })],
        }),
      },
    };
    const { unmount } = renderAt("/connections/outline-uploads", developer);
    const usedBy = await screen.findByRole("region", { name: "Used by" });
    expect(within(usedBy).getByText("Its keys go with it")).toBeVisible();
    unmount();
    renderAt("/connections/outline-uploads", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [used] }) },
    });
    const asAdmin = await screen.findByRole("region", { name: "Used by" });
    expect(within(asAdmin).getByRole("button", { name: "Disconnect storage" })).toBeVisible();
    expect(within(asAdmin).queryByText("Its keys go with it")).toBeNull();
  });

  // The slice 3 review, L4: a link an install's pin names is the install's, so below
  // an admin Disconnect stays off, with why.
  it("keeps Disconnect off below an admin for a catalog install's storage, with why", async () => {
    renderAt("/connections/outline-uploads", {
      ...fleet(),
      resources: places(),
      auth: {
        getAuthState: () =>
          authState({
            user: { ...mira, role: Role.UNSPECIFIED },
            grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
          }),
      },
      catalog: {
        listCatalogInstalls: () => ({
          installs: [
            {
              project: "outline",
              projectId: "prj_1",
              environmentId: ENV,
              pin: { entry: "outline", needs: { storage: { connection: "outline-uploads", prefix: "outline/" } } },
            },
          ],
        }),
      },
      connections: {
        listConnections: () => ({
          connections: [aConnection({ ...used, endpoint: "", bucket: "", reach: ConnectionReach.PROJECTS })],
        }),
      },
    });
    const usedBy = await screen.findByRole("region", { name: "Used by" });
    const button = await within(usedBy).findByRole("button", { name: "Disconnect storage" });
    await waitFor(() => expect(button).toHaveAttribute("aria-disabled", "true"));
    expect(button).toHaveAccessibleDescription("An admin or the owner disconnects an install's storage");
  });

  it("keeps Disconnect off below an admin while it isn't known yet whose the link is", async () => {
    renderAt("/connections/outline-uploads", {
      ...fleet(),
      resources: places(),
      auth: {
        getAuthState: () =>
          authState({
            user: { ...mira, role: Role.UNSPECIFIED },
            grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
          }),
      },
      catalog: { listCatalogInstalls: () => new Promise<never>(() => {}) },
      connections: {
        listConnections: () => ({
          connections: [aConnection({ ...used, endpoint: "", bucket: "", reach: ConnectionReach.PROJECTS })],
        }),
      },
    });
    const usedBy = await screen.findByRole("region", { name: "Used by" });
    expect(await within(usedBy).findByRole("button", { name: "Disconnect storage" })).toBeDisabled();
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

describe("using a connection in an app", () => {
  // Review L3: Use in an app showed to a viewer, and offered apps of a project the
  // connection doesn't reach.
  const two = (role: Role) => ({
    ...fleet(),
    resources: projectPlaces("data"),
    auth: {
      getAuthState: () =>
        authState({
          user: { ...mira, role: Role.UNSPECIFIED },
          grants: [
            { projectId: "prj_1", role },
            { projectId: "prj_data", role },
          ],
        }),
    },
    services: {
      listServices: () => ({
        services: [
          anApp("outline", "ghcr.io/acme/outline:1"),
          anApp(
            "warehouse",
            "ghcr.io/acme/warehouse:1",
            {},
            {
              id: "svc_warehouse",
              resource: { kind: "service", scope: "env_data", name: "warehouse" },
            },
          ),
        ],
      }),
    },
    connections: {
      // Below an admin, the projects it reaches are those of the caller's that may.
      listConnections: () => ({
        connections: [
          create(ConnectionInfoSchema, {
            id: "cnx_1",
            name: "shared",
            kind: LinkKind.S3,
            reach: ConnectionReach.PROJECTS,
            projects: ["prj_1"],
          }),
        ],
      }),
    },
  });

  it("offers a developer only the apps of projects it reaches", async () => {
    const user = userEvent.setup();
    renderAt("/connections/shared", two(Role.DEVELOPER));
    const use = await screen.findAllByRole("button", { name: "Use in an app" }, { timeout: 5000 });
    await user.click(use[0] as HTMLElement);
    const sheet = await screen.findByRole("dialog", { name: "Use shared in an app" });
    const app = await within(sheet).findByRole("combobox", { name: "App" });
    expect(within(app).getByRole("option", { name: /outline/ })).toBeInTheDocument();
    expect(within(app).queryByRole("option", { name: /warehouse/ })).toBeNull();
  });

  it("offers a viewer no way to use it", async () => {
    renderAt("/connections/shared", two(Role.VIEWER));
    expect(await screen.findByRole("heading", { level: 1, name: "shared" })).toBeVisible();
    await screen.findByText("No app reads it yet.");
    expect(screen.queryByRole("button", { name: "Use in an app" })).toBeNull();
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

  // Review M3: after Connect storage, the names it set read as blank variables to edit.
  it("shows the names a link sets as its connection's, read-only, with a way to the connection", async () => {
    const linkedVariables = [
      variable({ id: "var_1", name: "S3_BUCKET", usedBy: uses("S3_BUCKET") }),
      variable({ id: "var_2", name: "S3_ENDPOINT", value: "https://old.example.com", usedBy: uses("S3_ENDPOINT") }),
      variable({ id: "sec_1", name: "S3_SECRET_ACCESS_KEY", secret: true, usedBy: uses("S3_SECRET_ACCESS_KEY") }),
    ];
    const base = api([]);
    const outline = anApp(
      "outline",
      "ghcr.io/acme/outline:1",
      {},
      {
        spec: create(ServiceSchema, {
          runs: { case: "app", value: { image: "ghcr.io/acme/outline:1" } },
          variables: { S3_BUCKET: "S3_BUCKET", S3_ENDPOINT: "S3_ENDPOINT" },
          secrets: { S3_SECRET_ACCESS_KEY: "S3_SECRET_ACCESS_KEY" },
        }),
      },
    );
    const { container } = renderAt("/apps/svc_outline/environment", {
      ...base,
      services: { ...base.services, listServices: () => ({ services: [outline] }) },
      variables: { listVariables: () => ({ environmentId: ENV, variables: linkedVariables }) },
      connections: { listConnections: () => ({ connections: [used] }) },
    });
    const from = await screen.findAllByRole("link", { name: "From outline-uploads" }, { timeout: 5000 });
    expect(from).toHaveLength(2);
    expect(from[0]).toHaveAttribute("href", "/connections/outline-uploads");
    expect(screen.getByText("bucket")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Change S3_BUCKET" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove S3_SECRET_ACCESS_KEY" })).toBeNull();
    // A name no link sets is the app's to change, as before.
    expect(screen.getByRole("button", { name: "Change S3_ENDPOINT" })).toBeVisible();
    await expectAccessible(container);
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

describe("a connection used by backups (#831)", () => {
  const backupConn = aConnection({
    id: "cnx_bkp",
    name: "offsite",
    displayName: "Offsite",
    bucket: "acme-backups",
    reach: ConnectionReach.EVERY,
  });
  const backupDest = create(BackupDestinationInfoSchema, {
    id: "bds_offsite",
    name: "offsite",
    endpoint: backupConn.endpoint,
    bucket: "acme-backups",
    prefix: "backups",
    backups: 4,
    usedBytes: 64n << 20n,
    connection: "offsite",
  });

  it("lists backup destinations under Used by on Connections without repeating a matching name", async () => {
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [used, backupConn] }) },
      backups: { listBackupDestinations: () => ({ destinations: [backupDest] }) },
    });
    expect(await screen.findByText("2 connections · used by 1 app · 1 backup destination")).toBeVisible();
    const row = screen.getByRole("row", { name: /offsite/ });
    const cells = within(row).getAllByRole("cell");
    expect(cells[1]).toHaveTextContent("Backups");
    expect(cells[1]).not.toHaveTextContent("offsite");
  });

  it("counts standalone legacy backup destinations separately and offers Use as a connection", async () => {
    const user = userEvent.setup();
    const converted = vi.fn((_: { destination: string }) => ({ change: webPlanned() }));
    const legacyDest = create(BackupDestinationInfoSchema, {
      id: "bds_legacy",
      name: "legacy-r2",
      endpoint: `https://${R2}.r2.cloudflarestorage.com`,
      bucket: "legacy-backups",
      prefix: "old",
    });
    const { container } = renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [used, backupConn] }) },
      backups: {
        listBackupDestinations: () => ({ destinations: [backupDest, legacyDest] }),
        useDestinationAsConnection: converted,
      },
      changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    });
    expect(await screen.findByText("2 connections · 1 backup destination · used by 1 app")).toBeVisible();
    const legacyRow = screen.getByRole("row", { name: /legacy-r2/ });
    expect(legacyRow).toHaveTextContent("Cloudflare R2 · legacy-backups");
    expect(legacyRow).toHaveTextContent("Backups");
    await expectAccessible(container);
    await user.click(within(legacyRow).getByRole("button", { name: "Use as a connection" }));
    await waitFor(() => expect(converted).toHaveBeenCalledTimes(1));
    expect(converted.mock.calls[0]?.[0]).toMatchObject({ destination: "legacy-r2" });
  });

  it("shows Backups once on a phone when the backup destination shares the connection name", async () => {
    const origMatchMedia = window.matchMedia;
    window.matchMedia = vi.fn((query: string) => ({
      matches: query.includes("max-width"),
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    })) as typeof window.matchMedia;
    try {
      renderAt("/connections", {
        ...fleet(),
        resources: places(),
        connections: { listConnections: () => ({ connections: [backupConn] }) },
        backups: { listBackupDestinations: () => ({ destinations: [backupDest] }) },
      });
      const link = await screen.findByRole("link", { name: /offsite/ });
      expect(link.querySelector(".connections-phone-text")).toHaveTextContent(/^offsiteBackups$/);
    } finally {
      window.matchMedia = origMatchMedia;
    }
  });

  it("shows backups under Used by and disables Use in an app, Share, and Remove with why", async () => {
    const user = userEvent.setup();
    renderAt("/connections/offsite", {
      ...fleet(),
      resources: places(),
      services: { listServices: () => ({ services: [anApp("outline", "ghcr.io/acme/outline:1")] }) },
      connections: { listConnections: () => ({ connections: [backupConn] }) },
      backups: { listBackupDestinations: () => ({ destinations: [backupDest] }) },
    });
    const usedBy = await screen.findByRole("region", { name: "Used by" });
    expect(within(usedBy).getByRole("link", { name: "offsite" })).toHaveAttribute("href", "/backups");
    expect(usedBy).toHaveTextContent("backups/");
    expect(usedBy).toHaveTextContent("4 backups, 64 MB");

    const useInApp = screen.getByRole("button", { name: "Use in an app" });
    expect(useInApp).toHaveAttribute("aria-disabled", "true");
    expect(useInApp).toHaveAccessibleDescription("Backups use it. An app's key could delete the backups.");

    const share = screen.getByRole("switch", { name: /More than one app may use it/ });
    expect(share).toBeDisabled();
    expect(screen.getByText("Backups use it, so it can't be shared with apps")).toBeVisible();

    await user.click(screen.getByRole("button", { name: "Replace keys" }));
    const sheet = await screen.findByRole("dialog", { name: "Connection offsite" });
    const remove = within(sheet).getByRole("button", { name: /Remove/ });
    expect(remove).toHaveAttribute("aria-disabled", "true");
    expect(sheet).toHaveTextContent("Backup destination offsite uses it. Remove it on Backups first.");
  });
});
