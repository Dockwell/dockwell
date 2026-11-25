// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type BatchUpdateWebsitesRequest,
  CertificateState,
  ChangeSchema,
  ChangeState,
  type DownloadCertificateRequest,
  FileType,
  HealthState,
  PlanAction,
  type RenewCertificateRequest,
  type SetCustomCertificateRequest,
  type UpdateWebsiteRequest,
  type WatchChangeResponse,
  type WatchServersResponse,
  type WebsiteInfo,
  WebsiteInfoSchema,
  WebsitePauseMode,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { aHealth, anApp, places, SERVER } from "../../test/apps";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const SHOP_ENV = "env_shop";
const DOCS_ENV = "env_docs";

const shop = create(WebsiteInfoSchema, {
  id: "prj_shop",
  name: "shop-example-com",
  domain: "shop.example.com",
  spec: {
    serves: {
      case: "php",
      value: {
        root: "/var/www/shop",
        version: "8.3",
        index: [],
        user: "1000",
        uploadLimitMb: 64,
        filesOnly: false,
      },
    },
  },
  environmentId: SHOP_ENV,
  serviceId: "svc_shopweb",
  phpServiceId: "svc_shopphp",
  routeId: "rte_shop",
  updateTime: timestampFromDate(new Date(2026, 9, 13, 12)),
  serverId: SERVER,
  serverName: "hz-1",
});

const docs = create(WebsiteInfoSchema, {
  id: "prj_docs",
  name: "docs-example-com",
  domain: "docs.example.com",
  spec: {
    serves: { case: "folder", value: { root: "/var/www/docs", index: [], singlePageApp: false } },
  },
  environmentId: DOCS_ENV,
  serviceId: "svc_docsweb",
  routeId: "rte_docs",
  updateTime: timestampFromDate(new Date(2026, 9, 20, 12)),
  serverId: SERVER,
  serverName: "hz-1",
});

const runtimes = [
  {
    language: "PHP",
    version: "8.4",
    release: "8.4.27",
    image: "wordpress@sha256:84",
    extensions: ["opcache", "mysqli", "gd", "intl", "imagick", "zip"],
    optionalExtensions: ["imagick", "intl", "exif"],
  },
  {
    language: "PHP",
    version: "8.3",
    release: "8.3.36",
    image: "wordpress@sha256:83",
    extensions: ["opcache", "mysqli", "imagick", "intl"],
    optionalExtensions: ["imagick", "intl"],
  },
];

const planned = create(ChangeSchema, {
  id: "chg_handy",
  summary: "Update website",
  state: ChangeState.PLANNED,
  plan: {
    fingerprint: "feedfacefeedfacefeedfacefeedface",
    summary: { create: 1 },
    resources: [
      {
        id: "prj_shop",
        resource: { kind: "project", scope: "shop-example-com", name: "shop-example-com" },
        action: PlanAction.UPDATE,
      },
    ],
  },
});

const days = (n: number) => timestampFromDate(new Date(Date.now() + n * 86_400_000 - 60_000));

const domains = [
  {
    name: "shop.example.com",
    environmentId: SHOP_ENV,
    url: "https://shop.example.com",
    routes: [{ id: "rte_shop", name: "site", service: "web", port: 8080 }],
    servers: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        status: {
          domain: "shop.example.com",
          certificate: {
            state: CertificateState.ISSUED,
            expireTime: days(60),
            issuer: "Let's Encrypt",
            sans: ["shop.example.com"],
          },
        },
      },
    ],
  },
  {
    name: "docs.example.com",
    environmentId: DOCS_ENV,
    url: "https://docs.example.com",
    routes: [{ id: "rte_docs", name: "site", service: "web", port: 8080 }],
    servers: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        status: {
          domain: "docs.example.com",
          certificate: { state: CertificateState.ISSUED, expireTime: days(45) },
        },
      },
    ],
  },
];

function api(websites: WebsiteInfo[], more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return {
    resources: places(),
    servers: { watchServers: watch.stream },
    services: { listServices: () => ({ services: [anApp("api", "ghcr.io/acme/api:1.0")] }) },
    health: {
      listHealth: () => ({
        services: [
          aHealth("svc_shopweb", { state: HealthState.HEALTHY }),
          aHealth("svc_docsweb", { state: HealthState.HEALTHY }),
        ],
      }),
    },
    domains: { listDomains: () => ({ domains }), ...more.domains },
    websites: {
      listWebsites: () => ({ websites }),
      listWebsiteRuntimes: () => ({ runtimes }),
      previewWebsite: (req) => ({
        config: `:8080 {\n\t# preview\n\t${req.website?.customCaddy ?? ""}\n}\n`,
        caddyValidated: Boolean(req.website?.customCaddy && req.serverId),
      }),
      ...more.websites,
    },
    files: {
      listFiles: (req) => ({
        path: req.path,
        directory: { mode: "drwxr-xr-x", type: FileType.DIRECTORY, owner: "mira", uid: 1000, gid: 1000 },
        entries: [{ name: "index.php", mode: "-rw-r--r--", type: FileType.FILE, linkType: FileType.UNSPECIFIED }],
      }),
      ...more.files,
    },
    backups: {
      listBackups: () => ({ backups: [] }),
      listBackupPolicies: () => ({ policies: [] }),
      listBackupDestinations: () => ({ destinations: [] }),
      previewBackup: () => ({
        running: ["web"],
        estimateBytes: 1024n,
        freeBytes: 10_000_000n,
        reserveBytes: 1_000_000n,
      }),
      ...more.backups,
    },
    changes: { watchChange: feed<WatchChangeResponse>().stream, ...more.changes },
    ...more,
    ...(more.domains ? { domains: { listDomains: () => ({ domains }), ...more.domains } } : {}),
    ...(more.websites
      ? {
          websites: {
            listWebsites: () => ({ websites }),
            listWebsiteRuntimes: () => ({ runtimes }),
            previewWebsite: (req) => ({
              config: `:8080 {\n\t# preview\n\t${req.website?.customCaddy ?? ""}\n}\n`,
              caddyValidated: Boolean(req.website?.customCaddy && req.serverId),
            }),
            ...more.websites,
          },
        }
      : {}),
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

function onWideScreen() {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query.startsWith("(min-width"),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
}

describe("Websites everyday depth", () => {
  it("pauses a website with chosen mode (maintenance page or plain 503) as a plan", async () => {
    onWideScreen();
    const user = userEvent.setup();
    let asked: UpdateWebsiteRequest | undefined;
    renderAt(
      "/websites/prj_shop",
      api([shop], {
        websites: {
          updateWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
      }),
    );

    await user.click(await screen.findByRole("button", { name: "Pause" }));
    const dialog = await screen.findByRole("dialog", { name: "Pause shop.example.com" });
    await user.click(within(dialog).getByRole("radio", { name: /Plain 503/ }));
    await user.click(within(dialog).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.website?.paused).toBe(true);
    expect(asked?.website?.pauseMode).toBe(WebsitePauseMode.PLAIN_503);
  });

  it("edits rules without writing config and validates custom Caddy directives on the agent", async () => {
    onWideScreen();
    const user = userEvent.setup();
    let asked: UpdateWebsiteRequest | undefined;
    let sawServerValidation = false;
    renderAt(
      "/websites/prj_shop/configuration",
      api([shop], {
        websites: {
          previewWebsite: (req) => {
            if (req.website?.customCaddy && req.serverId === SERVER) sawServerValidation = true;
            return {
              config: `:8080 {\n\t${req.website?.customCaddy ?? ""}\n}\n`,
              caddyValidated: Boolean(req.website?.customCaddy && req.serverId),
            };
          },
          updateWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
      }),
    );

    expect(await screen.findByRole("heading", { level: 1, name: "shop.example.com" })).toBeVisible();

    // Open Rules & visitors panel and enable Rate limit + Static cache
    await user.click(screen.getByRole("switch", { name: /Rate limit/ }));
    await user.click(screen.getByRole("switch", { name: /Static file caching/ }));

    // Open Advanced Caddy config and enter custom directives
    await user.click(screen.getByRole("button", { name: "Advanced Caddy config" }));
    const customBox = screen.getByLabelText("Custom Caddy directives");
    await user.type(customBox, "respond /healthz 200");

    await waitFor(() => expect(sawServerValidation).toBe(true), { timeout: 2_000 });
    expect(await screen.findByText("Validated by Caddy")).toBeVisible();

    const planBtn = screen.getByRole("button", { name: /Review plan/ });
    await waitFor(() => expect(planBtn).toBeEnabled(), { timeout: 2_000 });
    await user.click(planBtn);

    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.website?.rules?.rateLimit?.enabled).toBe(true);
    expect(asked?.website?.rules?.staticCache?.enabled).toBe(true);
    expect(asked?.website?.customCaddy).toBe("respond /healthz 200");
  });

  it("configures PHP tuning fields and optional extension switches", async () => {
    onWideScreen();
    const user = userEvent.setup();
    let asked: UpdateWebsiteRequest | undefined;
    renderAt(
      "/websites/prj_shop/configuration",
      api([shop], {
        websites: {
          updateWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
      }),
    );

    expect(await screen.findByRole("heading", { level: 1, name: "shop.example.com" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Advanced" }));
    await user.type(screen.getByLabelText("Memory limit"), "256");
    await user.type(screen.getByLabelText("Max execution time"), "90");
    await user.type(screen.getByLabelText("FPM pool size"), "12");
    await user.click(screen.getByRole("switch", { name: "imagick" }));

    const planBtn = screen.getByRole("button", { name: /Review plan/ });
    await waitFor(() => expect(planBtn).toBeEnabled(), { timeout: 2_000 });
    await user.click(planBtn);

    await waitFor(() => expect(asked?.website?.serves.case).toBe("php"));
    expect(asked?.website?.serves.value).toMatchObject({
      memoryLimitMb: 256,
      maxExecutionTimeS: 90,
      fpmMaxChildren: 12,
      disabledExtensions: ["imagick"],
    });
  });

  it("handles Renew now (with force confirmation), Upload certificate, and Download cert/key", async () => {
    onWideScreen();
    const user = userEvent.setup();
    const renews: RenewCertificateRequest[] = [];
    let customReq: SetCustomCertificateRequest | undefined;
    const downloads: DownloadCertificateRequest[] = [];

    renderAt(
      "/websites/prj_shop/certificate",
      api([shop], {
        domains: {
          renewCertificate: (req) => {
            renews.push(req);
            if (!req.force) {
              throw new ConnectError(
                "shop.example.com isn't due for renewal yet (60 days left). Force only if needed.",
                Code.FailedPrecondition,
              );
            }
            return { message: "Certificate renewal started for shop.example.com." };
          },
          setCustomCertificate: (req) => {
            customReq = req;
            return { change: planned };
          },
          downloadCertificate: (req) => {
            downloads.push(req);
            return {
              certPem: "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n",
              keyPem: req.includeKey ? "-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----\n" : "",
            };
          },
        },
      }),
    );

    expect(await screen.findByText("Let's Encrypt")).toBeVisible();

    // Renew now refused -> force confirmation appears -> Force renewal succeeds
    await user.click(screen.getByRole("button", { name: "Renew now" }));
    expect(await screen.findByText(/isn't due for renewal yet/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Renew anyway" }));
    await waitFor(() => expect(renews).toHaveLength(2));
    expect(renews[0]?.force).toBe(false);
    expect(renews[1]?.force).toBe(true);
    expect(await screen.findByText(/Certificate renewal started/)).toBeVisible();

    // Download certificate (no key)
    await user.click(screen.getByRole("button", { name: "Download certificate" }));
    await waitFor(() => expect(downloads).toHaveLength(1));
    expect(downloads[0]?.includeKey).toBe(false);

    // Upload custom certificate sheet -> plans custom cert
    await user.click(screen.getByRole("button", { name: "Upload certificate" }));
    const uploadDialog = await screen.findByRole("dialog", { name: "Upload certificate for shop.example.com" });
    await user.type(within(uploadDialog).getByLabelText("Certificate chain (PEM)"), "-----BEGIN CERTIFICATE-----\nABC");
    await user.type(within(uploadDialog).getByLabelText("Private key (PEM)"), "-----BEGIN PRIVATE KEY-----\nXYZ");
    await user.click(within(uploadDialog).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(customReq).toBeDefined());
    expect(customReq?.domain).toBe("shop.example.com");
    expect(customReq?.certPem).toContain("BEGIN CERTIFICATE");
  });

  it("offers Files, Logs, Pause, and Back up now in row menu and batches Switch PHP / HTTPS / Pause in one plan", async () => {
    onWideScreen();
    const user = userEvent.setup();
    const batchCalls: BatchUpdateWebsitesRequest[] = [];
    renderAt(
      "/websites",
      api([shop, docs], {
        websites: {
          batchUpdateWebsites: (req) => {
            batchCalls.push(req);
            return { change: planned };
          },
        },
      }),
    );

    // Open row menu for shop.example.com
    const row = await screen.findByRole("row", { name: /shop\.example\.com/ });
    await user.click(within(row).getByRole("button", { name: /Actions|More/i }));
    expect(await screen.findByRole("menuitem", { name: "Files" })).toBeVisible();
    expect(screen.getByRole("menuitem", { name: "Logs" })).toBeVisible();
    expect(screen.getByRole("menuitem", { name: "Pause" })).toBeVisible();
    expect(screen.getByRole("menuitem", { name: "Back up now" })).toBeVisible();
    await user.keyboard("{Escape}");

    // Select both websites and trigger batch HTTPS update
    const checkboxes = screen.getAllByRole("checkbox");
    await user.click(checkboxes[1] as HTMLElement);
    await user.click(checkboxes[2] as HTMLElement);
    await user.click(await screen.findByRole("button", { name: "HTTPS" }));
    const httpsDialog = await screen.findByRole("dialog", { name: /HTTPS for 2 websites/i });
    await user.click(within(httpsDialog).getByRole("button", { name: /Review plan/ }));

    await waitFor(() => expect(batchCalls).toHaveLength(1));
    expect(batchCalls[0]?.websiteIds).toHaveLength(2);
    expect(batchCalls[0]?.https).toBe(true);
  });
});
