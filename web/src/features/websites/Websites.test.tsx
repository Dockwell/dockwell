// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  AlertCheckState,
  CertificateState,
  ChangeSchema,
  type ChangeServiceRuntimeRequest,
  ChangeState,
  type CreateWebsiteRequest,
  FileType,
  HealthState,
  PlanAction,
  ServiceSchema,
  type UpdateWebsiteRequest,
  type WatchChangeResponse,
  type WatchLogsResponse,
  type WatchServersResponse,
  type WebsiteInfo,
  WebsiteInfoSchema,
  WebsiteReachSchema,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { aHealth, anApp, places, SERVER } from "../../test/apps";
import { moved, watched } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { changedLines } from "./website";

const SITE_ENV = "env_blog";
const PORT_ENV = "env_tools";

const blog = create(WebsiteInfoSchema, {
  id: "prj_blog",
  name: "blog-example-com",
  domain: "blog.example.com",
  spec: { serves: { case: "proxy", value: { url: "https://backend.example.com" } } },
  environmentId: SITE_ENV,
  serviceId: "svc_blogweb",
  routeId: "rte_blog",
  updateTime: timestampFromDate(new Date(2026, 10, 1, 12)),
  serverId: SERVER,
  serverName: "hz-1",
});

const tools = create(WebsiteInfoSchema, {
  id: "prj_tools",
  name: "tools-example-com",
  domain: "tools.example.com",
  spec: { serves: { case: "proxy", value: { port: 3000 } } },
  environmentId: PORT_ENV,
  routeId: "rte_tools",
  updateTime: timestampFromDate(new Date(2026, 9, 29, 12)),
  serverId: SERVER,
  serverName: "hz-1",
});

const shop = create(WebsiteInfoSchema, {
  id: "prj_shop",
  name: "shop-example-com",
  domain: "shop.example.com",
  spec: {
    serves: {
      case: "php",
      value: { root: "/var/www/shop", version: "8.3", index: [], user: "1000", uploadLimitMb: 0, filesOnly: false },
    },
  },
  environmentId: "env_shop",
  serviceId: "svc_shopweb",
  phpServiceId: "svc_shopphp",
  routeId: "rte_shop",
  updateTime: timestampFromDate(new Date(2026, 9, 13, 12)),
  serverId: SERVER,
  serverName: "hz-1",
});

const runtimes = [
  {
    language: "PHP",
    version: "8.4",
    release: "8.4.27",
    image: "wordpress@sha256:84",
    extensions: ["opcache", "mysqli", "gd", "intl", "imagick", "zip", "exif"],
  },
  {
    language: "PHP",
    version: "8.3",
    release: "8.3.36",
    image: "wordpress@sha256:83",
    extensions: ["opcache", "mysqli"],
  },
];

const docs = create(WebsiteInfoSchema, {
  id: "prj_docs",
  name: "docs-example-com",
  domain: "docs.example.com",
  spec: { serves: { case: "folder", value: { root: "/var/www/docs", index: [], singlePageApp: false } } },
  environmentId: "env_docs",
  serviceId: "svc_docsweb",
  routeId: "rte_docs",
  updateTime: timestampFromDate(new Date(2026, 9, 20, 12)),
  serverId: SERVER,
  serverName: "hz-1",
});

/** A folder's entries as ListFiles has them: a name and its mode. */
const entry = (name: string, mode = "-rw-r--r--", type = FileType.FILE, linkType = FileType.UNSPECIFIED) => ({
  name,
  mode,
  type,
  linkType,
});

/**
 * A server's disk, as ListFiles reads it: the folders given, each with its mode and
 * what's in it, and the folders above them, which hold them. What isn't there is
 * refused, as the agent refuses it, and listed in asked.
 */
function disk(
  folders: Record<string, { mode: string; uid?: number; entries: ReturnType<typeof entry>[] }>,
  asked: string[] = [],
) {
  return (req: { path: string }) => {
    asked.push(req.path);
    const folder = folders[req.path];
    // A site's folder is its person's, mira's, 1000:1000, unless it says.
    if (folder)
      return {
        path: req.path,
        directory: {
          mode: folder.mode,
          type: FileType.DIRECTORY,
          owner: folder.uid === 0 ? "root" : "mira",
          uid: folder.uid ?? 1000,
          gid: folder.uid ?? 1000,
        },
        entries: folder.entries,
      };
    const children = Object.entries(folders).filter(
      ([path]) =>
        path.slice(0, path.lastIndexOf("/")) === req.path || (req.path === "/" && path.lastIndexOf("/") === 0),
    );
    if (children.length > 0 || req.path === "/srv") {
      return {
        path: req.path,
        // /srv is a person's, so a folder made in it is theirs.
        directory:
          req.path === "/srv"
            ? { mode: "drwxr-xr-x", type: FileType.DIRECTORY, owner: "mira", uid: 1000, gid: 1000 }
            : { mode: "drwxr-xr-x", type: FileType.DIRECTORY },
        entries: children.map(([path, f]) => entry(path.slice(path.lastIndexOf("/") + 1), f.mode, FileType.DIRECTORY)),
      };
    }
    const err = new ConnectError(`there's nothing at ${req.path}`, Code.NotFound);
    err.metadata.set("Dockwell-Error", "not_found");
    throw err;
  };
}

type User = ReturnType<typeof userEvent.setup>;

/** The folder a folder picker has chosen, as its Change button names it. */
const chosenFolder = (scope: HTMLElement, path: string) =>
  within(scope).getByRole("button", { name: `Change the folder, ${path}` });

/** Picks a folder by typing its path, as a power user does, opening the picker first. */
async function pickFolder(user: User, scope: HTMLElement, path: string) {
  if (!within(scope).queryByRole("textbox", { name: "Path" })) {
    await user.click(within(scope).getByRole("button", { name: /^Change the folder|^Choose a folder/ }));
  }
  const typed = within(scope).getByRole("textbox", { name: "Path" });
  await user.clear(typed);
  await user.type(typed, path);
}

/** Opens a sheet's Advanced section by its label, unless it's open. */
async function openAdvanced(user: User, scope: HTMLElement, label = "Advanced") {
  const toggle = within(scope).getByRole("button", { name: label });
  if (toggle.getAttribute("aria-expanded") !== "true") await user.click(toggle);
}

const days = (n: number) => timestampFromDate(new Date(Date.now() + n * 86_400_000 - 60_000));

const domains = [
  {
    name: "blog.example.com",
    environmentId: SITE_ENV,
    url: "https://blog.example.com",
    routes: [{ id: "rte_blog", name: "site", service: "web", port: 8080 }],
    servers: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        status: { domain: "blog.example.com", certificate: { state: CertificateState.ISSUED, expireTime: days(61) } },
      },
    ],
  },
  {
    name: "tools.example.com",
    environmentId: PORT_ENV,
    url: "http://tools.example.com",
    routes: [{ id: "rte_tools", name: "site", hostPort: 3000, tls: "off" }],
    servers: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        status: { domain: "tools.example.com", certificate: { state: CertificateState.NONE } },
      },
    ],
  },
];

const webOfBlog = anApp(
  "web",
  "caddy:2.11.7-alpine@sha256:e70b",
  {},
  {
    id: "svc_blogweb",
    resource: { kind: "service", scope: SITE_ENV, name: "web" },
  },
);

const config = `{
\tadmin off
}

:8080 {
\treverse_proxy https://backend.example.com
}
`;

function api(websites: WebsiteInfo[], more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return {
    resources: places(),
    servers: { watchServers: watch.stream },
    services: { listServices: () => ({ services: [anApp("shop", "ghcr.io/acme/shop:1.4"), webOfBlog] }) },
    health: { listHealth: () => ({ services: [aHealth("svc_blogweb", { state: HealthState.HEALTHY })] }) },
    domains: { listDomains: () => ({ domains }) },
    websites: {
      listWebsites: () => ({ websites }),
      listWebsiteRuntimes: () => ({ runtimes }),
      previewWebsite: (req) => {
        const proxy = req.website?.serves.case === "proxy" ? req.website.serves.value : undefined;
        if (proxy?.url === "http://localhost") {
          const err = new ConnectError(
            "localhost is the web server's own container, not the server. To send to a port on the server itself, give the port in place of a URL",
            Code.InvalidArgument,
          );
          err.metadata.set("Dockwell-Error", "invalid");
          throw err;
        }
        if (req.website?.serves.case === "php") {
          return { website: req.website, config: ":8080 {\n\troot * /var/www/html\n\tphp_fastcgi php:9000\n}\n" };
        }
        if (req.website?.serves.case === "folder") {
          return { website: req.website, config: ":8080 {\n\troot * /srv\n\tfile_server\n}\n" };
        }
        return {
          website: req.website,
          config: proxy?.port ? "" : config.replace("https://backend.example.com", proxy?.url ?? ""),
        };
      },
      ...more.websites,
    },
    ...Object.fromEntries(Object.entries(more).filter(([key]) => key !== "websites")),
  };
}

const planned = create(ChangeSchema, {
  id: "chg_site",
  summary: "Add the website shop.example.com",
  state: ChangeState.PLANNED,
  plan: {
    fingerprint: "feedfacefeedfacefeedfacefeedface",
    summary: { create: 3 },
    resources: [
      { id: "svc_web", resource: { kind: "service", scope: "env_new", name: "web" }, action: PlanAction.CREATE },
    ],
  },
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const rowOf = (name: string | RegExp) => screen.findByRole("row", { name });

/** A screen as wide as the mock's, where every column has room. */
function onWideScreen() {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query.startsWith("(min-width"),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
}

describe("The websites", () => {
  it("lists each with what it sends to, how it serves, its certificate and when it changed", async () => {
    onWideScreen();
    const { container } = renderAt("/websites", api([blog, tools]));
    expect(await screen.findByRole("heading", { level: 1, name: "Websites" })).toBeVisible();
    const first = await rowOf(/blog\.example\.com/);
    expect(first).toHaveTextContent("https://backend.example.com");
    expect(first).toHaveTextContent("Reverse proxy");
    await waitFor(() => expect(first).toHaveTextContent("Valid · 61 days left"));
    expect(first).toHaveTextContent("1 Nov");
    const second = await rowOf(/tools\.example\.com/);
    expect(second).toHaveTextContent("Port 3000 on the server");
    await waitFor(() => expect(second).toHaveTextContent("TLS ends in front"));
    expect(container.querySelector(".page-lead")).toHaveTextContent("2 sites on hz-1");
    expect(screen.getByRole("link", { name: "Files" })).toHaveAttribute("href", `/servers/${SERVER}/files`);
    expect(screen.getByRole("link", { name: "New website" })).toHaveAttribute("href", "/websites/new");
    await expectAccessible(container);
  });

  it("gives up what a website is before its domain, on a narrower screen", async () => {
    renderAt("/websites", api([blog]));
    const row = await rowOf(/blog\.example\.com/);
    expect(row).toHaveTextContent("https://backend.example.com");
    expect(screen.queryByRole("columnheader", { name: "Type" })).not.toBeInTheDocument();
  });

  it("says there are none yet, and how to start", async () => {
    renderAt("/websites", api([]));
    expect(await screen.findByRole("heading", { name: "No websites yet" })).toBeVisible();
    expect(screen.getByRole("link", { name: "New website" })).toHaveAttribute("href", "/websites/new");
  });

  it("holds no empty line under the title once it knows there are none", async () => {
    const { container } = renderAt("/websites", api([]));
    await screen.findByRole("heading", { name: "No websites yet" });
    expect(container.querySelector(".page-lead")).toBeNull();
  });

  it("says when it can't read them, and tries again", async () => {
    const user = userEvent.setup();
    let calls = 0;
    renderAt(
      "/websites",
      api([], {
        websites: {
          listWebsites: () => {
            calls++;
            throw new ConnectError("the control plane couldn't complete the request", Code.Internal);
          },
        },
      }),
    );
    expect(await screen.findByText("Can't load the websites")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(calls).toBe(2));
  });

  it("keeps a website's own services out of the apps", async () => {
    renderAt("/apps", api([blog]));
    expect(await screen.findByRole("row", { name: /shop/ })).toBeVisible();
    expect(screen.queryByRole("row", { name: /^web\b/ })).not.toBeInTheDocument();
  });

  it("says it can't load the apps when it can't tell which are a website's own", async () => {
    renderAt(
      "/apps",
      api([], {
        websites: {
          listWebsites: () => {
            throw new ConnectError("the control plane couldn't complete the request", Code.Internal);
          },
        },
      }),
    );
    expect(await screen.findByText("Can't load the apps")).toBeVisible();
    expect(screen.queryByRole("row", { name: /^web\b/ })).not.toBeInTheDocument();
  });

  it("leads from a website's domain in Domains to the website, not its web server", async () => {
    onWideScreen();
    renderAt("/domains", api([blog]));
    const row = await screen.findByRole("row", { name: /blog\.example\.com/ }, { timeout: 5_000 });
    const links = within(row).getAllByRole("link");
    expect(links.some((a) => a.getAttribute("href") === "/websites/prj_blog")).toBe(true);
    expect(links.some((a) => a.getAttribute("href")?.startsWith("/apps/"))).toBe(false);
  });

  it("keeps a website's own services out of the command bar", async () => {
    const user = userEvent.setup();
    renderAt("/websites", api([blog]));
    await rowOf(/blog\.example\.com/);
    await user.keyboard("{Control>}k{/Control}");
    const menu = await screen.findByRole("dialog");
    expect(await within(menu).findByRole("option", { name: /Open shop/ })).toBeVisible();
    expect(within(menu).queryByRole("option", { name: /^Open web\b/ })).not.toBeInTheDocument();
    expect(within(menu).getByRole("option", { name: /Open the apps/ })).toHaveTextContent("1");
  });

  it("says whether what each sends to answers, and never Healthy before its server has", async () => {
    onWideScreen();
    const reach = (answered: boolean, said: string) =>
      create(WebsiteReachSchema, { answered, said, checkTime: timestampFromDate(new Date()) });
    const cases: [WebsiteInfo, RegExp, string][] = [
      [create(WebsiteInfoSchema, { ...blog, reach: reach(true, "200 OK") }), /blog\.example\.com/, "Healthy"],
      [blog, /blog\.example\.com/, "Running"],
      [tools, /tools\.example\.com/, "Not checked"],
      [
        create(WebsiteInfoSchema, { ...blog, reach: reach(false, "connection refused") }),
        /blog\.example\.com/,
        "Can't reach backend.example.com",
      ],
      [
        create(WebsiteInfoSchema, { ...blog, reach: reach(false, "503 Service Unavailable") }),
        /blog\.example\.com/,
        "backend.example.com answers 503",
      ],
      [
        create(WebsiteInfoSchema, { ...tools, reach: reach(false, "nothing listens on port 3000") }),
        /tools\.example\.com/,
        "Nothing listens on port 3000",
      ],
    ];
    for (const [site, name, says] of cases) {
      const { unmount } = renderAt("/websites", api([site]));
      const row = await screen.findByRole("row", { name }, { timeout: 5_000 });
      await waitFor(() => expect(row).toHaveTextContent(says), { timeout: 5_000 });
      if (says !== "Healthy") expect(row).not.toHaveTextContent("Healthy");
      unmount();
    }
  }, 60_000);

  it("stacks on a phone, a row to each website", async () => {
    onPhone();
    renderAt("/websites", api([blog, tools]));
    const row = await screen.findByRole("link", { name: /blog\.example\.com/ });
    expect(row).toHaveAttribute("href", "/websites/prj_blog");
    expect(row).toHaveTextContent("Reverse proxy");
  });
});

describe("New website", () => {
  it("sends a domain to a URL, and plans it", async () => {
    const user = userEvent.setup();
    let asked: CreateWebsiteRequest | undefined;
    const { container } = renderAt(
      "/websites/new",
      api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await expectAccessible(container);
    await user.type(within(sheet).getByLabelText("Domain"), "Shop.Example.com");
    await user.click(within(sheet).getByRole("radio", { name: /^A URL/ }));
    await user.type(within(sheet).getByLabelText("URL"), "https://backend.example.com/shop");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.domain).toBe("shop.example.com");
    expect(asked?.website?.serves).toEqual({
      case: "proxy",
      value: expect.objectContaining({ url: "https://backend.example.com/shop", sendTargetHost: false }),
    });
    expect(asked?.tls).toBe("");
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
  });

  it("sends a domain to a port on the server behind a tunnel", async () => {
    const user = userEvent.setup();
    let asked: CreateWebsiteRequest | undefined;
    renderAt(
      "/websites/new",
      api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.type(within(sheet).getByLabelText("Domain"), "tools.example.com");
    await user.click(within(sheet).getByRole("radio", { name: /A port on this server/ }));
    await user.type(within(sheet).getByLabelText("Port on the server"), "3000");
    // A port on the server has nothing under Advanced, and HTTPS says it's on.
    expect(within(sheet).queryByRole("button", { name: "Advanced" })).not.toBeInTheDocument();
    expect(within(sheet).getByRole("button", { name: "HTTPS" })).toHaveAccessibleDescription(
      "On, with a certificate that comes by itself",
    );
    await openAdvanced(user, sheet, "HTTPS");
    await user.click(within(sheet).getByRole("switch", { name: /tunnel/i }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.website?.serves).toEqual({ case: "proxy", value: expect.objectContaining({ port: 3000 }) });
    expect(asked?.tls).toBe("off");
    expect(asked?.trustedProxies).toEqual(["127.0.0.1"]);
  });

  it("opens the website it made once its plan applies", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchChangeResponse>();
    // The website is its project, which the plan makes with its web server.
    const made = create(ChangeSchema, {
      id: "chg_site",
      summary: "Add the website shop.example.com",
      state: ChangeState.PLANNED,
      plan: {
        fingerprint: "feedfacefeedfacefeedfacefeedface",
        summary: { create: 2 },
        resources: [
          { id: "prj_shop", resource: { kind: "project", name: "shop-example-com" }, action: PlanAction.CREATE },
          { id: "svc_web", resource: { kind: "service", scope: "env_new", name: "web" }, action: PlanAction.CREATE },
        ],
      },
    });
    const { router } = renderAt(
      "/websites/new",
      api([], {
        websites: { createWebsite: () => ({ change: made }) },
        changes: {
          watchChange: watch.stream,
          applyChange: () => ({ change: moved(made, { state: ChangeState.APPLYING }) }),
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.type(within(sheet).getByLabelText("Domain"), "shop.example.com");
    await user.click(within(sheet).getByRole("radio", { name: /^A URL/ }));
    await user.type(within(sheet).getByLabelText("URL"), "https://backend.example.com");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(plan).getByRole("button", { name: /Apply plan/ }));
    watch.push(watched(moved(made, { state: ChangeState.APPLIED })));
    await user.click(await screen.findByRole("button", { name: "Go to the website" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/websites/prj_shop"));
  });

  it("says at the field what's missing, and what the server refused", async () => {
    const user = userEvent.setup();
    // The refusal is waited on by the request that brings it, not by the clock, as a
    // slow runner takes its time getting there.
    let refused!: () => void;
    const asked = new Promise<void>((resolve) => {
      refused = resolve;
    });
    renderAt(
      "/websites/new",
      api([], {
        websites: {
          createWebsite: () => {
            refused();
            const err = new ConnectError(
              "the path /a;b has a character a website can't send on. Use letters, digits, -, ., _, ~ and /",
              Code.InvalidArgument,
            );
            err.metadata.set("Dockwell-Error", "invalid");
            throw err;
          },
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.click(within(sheet).getByRole("radio", { name: /^A URL/ }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(within(sheet).getByLabelText("Domain")).toHaveAttribute("aria-invalid", "true");
    expect(within(sheet).getByLabelText("URL")).toHaveAttribute("aria-invalid", "true");
    await user.click(within(sheet).getByLabelText("Domain"));
    await user.paste("shop.example.com");
    await user.click(within(sheet).getByLabelText("URL"));
    await user.paste("https://backend.example.com/a;b");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await asked;
    expect(await within(sheet).findByText(/a character a website can't send on/)).toBeVisible();
    expect(within(sheet).getByLabelText("URL")).toHaveAttribute("aria-invalid", "true");
  });
});

describe("A website that serves a folder", () => {
  it("starts from a folder that follows the domain, says what's in it, and plans it", async () => {
    const user = userEvent.setup();
    let asked: CreateWebsiteRequest | undefined;
    const read: string[] = [];
    renderAt(
      "/websites/new",
      api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        files: {
          listFiles: disk(
            {
              "/srv/site": {
                mode: "drwxr-xr-x",
                entries: [entry("index.html"), entry("style.css"), entry("img", "drwxr-xr-x", FileType.DIRECTORY)],
              },
            },
            read,
          ),
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    expect(within(sheet).getByRole("radio", { name: /^Files in a folder/ })).toBeChecked();
    // What starts at a default is a line under Advanced, and nothing to fill in.
    expect(within(sheet).queryByLabelText("Index files")).not.toBeInTheDocument();
    expect(within(sheet).getByRole("button", { name: "Advanced" })).toHaveAccessibleDescription("index.html index.htm");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    // Sent back with no folder, the picker is open at its path, ready to type.
    expect(within(sheet).getByRole("textbox", { name: "Path" })).toHaveAttribute("aria-invalid", "true");
    await user.type(within(sheet).getByLabelText("Domain"), "shop.example.com");
    // The domain gave it a folder, so what it said of none goes.
    // The picker stays open, as it was opened on what was refused, with the path it follows.
    expect(within(sheet).getByRole("textbox", { name: "Path" })).toHaveValue("/var/www/shop.example.com");
    expect(within(sheet).queryByText(/Give the folder/)).not.toBeInTheDocument();
    expect(await within(sheet).findByText("New. Made with the site.", {}, { timeout: 3_000 })).toBeVisible();
    // A folder that isn't there is seen from the folder above, as asking for it would go on Activity as refused.
    expect(read).not.toContain("/var/www/shop.example.com");

    await pickFolder(user, sheet, "/srv/site");
    expect(await within(sheet).findByText("Holds index.html and 2 more", {}, { timeout: 3_000 })).toBeVisible();
    // The domain doesn't take a folder that was picked.
    await user.type(within(sheet).getByLabelText("Domain"), "x");
    expect(within(sheet).getByRole("textbox", { name: "Path" })).toHaveValue("/srv/site");

    await openAdvanced(user, sheet);
    await user.click(within(sheet).getByRole("switch", { name: /Single-page app/ }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.website?.serves).toEqual({
      case: "folder",
      value: expect.objectContaining({ root: "/srv/site", index: [], singlePageApp: true }),
    });
    expect(read).toContain("/srv/site");
  });

  it("makes a folder that isn't there as the folder above's owner, and says so", async () => {
    const user = userEvent.setup();
    let asked: CreateWebsiteRequest | undefined;
    renderAt(
      "/websites/new",
      api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        files: { listFiles: disk({ "/srv/site": { mode: "drwxr-xr-x", entries: [entry("index.html")] } }) },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.type(within(sheet).getByLabelText("Domain"), "docs.example.com");
    // Nothing is there, nor above it: it's made with the site, with nothing to run first.
    expect(await within(sheet).findByText("New. Made with the site.", {}, { timeout: 3_000 })).toBeVisible();
    expect(within(sheet).queryByText(/sudo/)).not.toBeInTheDocument();
    await pickFolder(user, sheet, "/srv/new");
    // Made in a person's folder, it's theirs, so they can put the site's files in it.
    expect(
      await within(sheet).findByText("New. Made with the site, owned by mira.", {}, { timeout: 3_000 }),
    ).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.website?.serves).toEqual({
      case: "folder",
      value: expect.objectContaining({ root: "/srv/new", owner: "1000:1000" }),
    });
  });

  it("says a PHP site's folder runs no script as files in a folder", async () => {
    const user = userEvent.setup();
    renderAt(
      "/websites/new",
      api([], {
        files: {
          listFiles: disk({ "/srv/wp": { mode: "drwxr-xr-x", entries: [entry("index.php"), entry("wp-config.php")] } }),
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await pickFolder(user, sheet, "/srv/wp");
    expect(
      await within(sheet).findByText("Holds index.php, which only a PHP site runs.", {}, { timeout: 3_000 }),
    ).toBeVisible();
  });

  it("says when the web server can't read the folder, or there's no index in it", async () => {
    const user = userEvent.setup();
    renderAt(
      "/websites/new",
      api([], {
        files: {
          listFiles: disk({
            "/srv/closed": { mode: "drwx------", entries: [entry("index.html")] },
            "/srv/empty": { mode: "drwxr-xr-x", entries: [entry("readme.md")] },
            "/srv/shut": { mode: "drwxr-xr-x", entries: [entry("index.html", "-rw-------")] },
          }),
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await pickFolder(user, sheet, "/srv/closed");
    expect(
      await within(sheet).findByText(
        "Closed to the web server (drwx------), so it would answer 403.",
        {},
        { timeout: 3_000 },
      ),
    ).toBeVisible();
    await pickFolder(user, sheet, "/srv/shut");
    expect(
      await within(sheet).findByText("The web server can't read index.html (-rw-------).", {}, { timeout: 3_000 }),
    ).toBeVisible();
    await pickFolder(user, sheet, "/srv/empty");
    expect(
      await within(sheet).findByText(
        "No index.html here yet, so it answers 404 until there is.",
        {},
        { timeout: 3_000 },
      ),
    ).toBeVisible();
  });

  it("says a site's folder isn't there, with where to make it, and promises no plan", async () => {
    onWideScreen();
    renderAt(
      "/websites/prj_docs/configuration",
      api([docs], { files: { listFiles: disk({ "/srv/x": { mode: "drwxr-xr-x", entries: [] } }) } }),
    );
    expect(await screen.findByText(/Not on the server yet\./, {}, { timeout: 3_000 })).toBeVisible();
    expect(screen.getByRole("link", { name: "Make it in Files" })).toHaveAttribute(
      "href",
      `/servers/${SERVER}/files?path=%2Fvar%2Fwww`,
    );
    expect(screen.queryByText(/^New\./)).not.toBeInTheDocument();
    expect(screen.queryByText(/the plan applies/)).not.toBeInTheDocument();
  });

  it("sends a site's stored upload limit when 64 is typed over its default, so the plan lists no change", async () => {
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
        changes: { watchChange: feed<WatchChangeResponse>().stream },
        files: { listFiles: disk({ "/var/www/shop": { mode: "drwxr-xr-x", entries: [entry("index.php")] } }) },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "shop.example.com" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Advanced" }));
    await user.type(screen.getByLabelText("Upload limit"), "64");
    const version = await screen.findByLabelText("PHP version");
    await waitFor(() => expect(version).toHaveValue("8.3"));
    await user.selectOptions(version, "8.4");
    expect(screen.getByText("1 change")).toBeVisible();
    const plan = screen.getByRole("button", { name: /Review plan/ });
    await waitFor(() => expect(plan).toBeEnabled(), { timeout: 3_000 });
    await user.click(plan);
    await waitFor(() => expect(asked?.website?.serves.case).toBe("php"));
    expect(asked?.website?.serves.value).toMatchObject({ version: "8.4", uploadLimitMb: 0 });
  });

  it("offers a site with no domain its configuration, not a procedure", async () => {
    onWideScreen();
    renderAt("/websites/prj_docs/certificate", api([docs]));
    expect(await screen.findByRole("heading", { name: "No domain, so no certificate" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Open its configuration" })).toHaveAttribute(
      "href",
      "/websites/prj_docs/configuration",
    );
    expect(screen.queryByText(/Delete the website/)).not.toBeInTheDocument();
  });

  it("shows what serves it, and leads to its files", async () => {
    onWideScreen();
    renderAt(
      "/websites/prj_docs/configuration",
      api([docs], {
        files: {
          listFiles: disk({ "/var/www/docs": { mode: "drwxr-xr-x", entries: [entry("index.html")] } }),
        },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "docs.example.com" })).toBeVisible();
    expect(screen.getByText(/Static files from \/var\/www\/docs/)).toBeVisible();
    expect(screen.getByRole("link", { name: "Edit files" })).toHaveAttribute(
      "href",
      `/servers/${SERVER}/files?path=%2Fvar%2Fwww%2Fdocs`,
    );
    expect(screen.getByLabelText("Serves")).toHaveValue("folder");
    expect(chosenFolder(document.body, "/var/www/docs")).toBeVisible();
    const code = await screen.findByRole("region", { name: "The web server's config" });
    await waitFor(() => expect(code).toHaveTextContent("file_server"));
  });

  it("lists as static files, with its folder under its domain", async () => {
    onWideScreen();
    renderAt("/websites", api([docs]));
    const row = await rowOf(/docs\.example\.com/);
    expect(row).toHaveTextContent("/var/www/docs");
    expect(row).toHaveTextContent("Static files");
  });
});

describe("A PHP website", () => {
  it("is made with the version chosen, who it runs as and its upload limit, and plans it", async () => {
    const user = userEvent.setup();
    let asked: CreateWebsiteRequest | undefined;
    renderAt(
      "/websites/new?php=8.3",
      api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        files: {
          listFiles: disk({ "/var/www/shop.example.com": { mode: "drwxr-xr-x", entries: [entry("index.php")] } }),
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    // A runtime's card starts it on its version.
    expect(within(sheet).getByRole("radio", { name: /^PHP/ })).toBeChecked();
    await user.click(within(sheet).getByLabelText("Domain"));
    await user.paste("shop.example.com");
    expect(await within(sheet).findByText("Holds index.php", {}, { timeout: 3_000 })).toBeVisible();
    expect(within(sheet).getByLabelText("PHP version")).toHaveValue("8.3");
    // PHP runs as the folder's owner, said in Advanced's line.
    expect(within(sheet).getByRole("button", { name: "Advanced" })).toHaveAccessibleDescription(
      "Runs as 1000:1000 · 64 MiB uploads",
    );
    await openAdvanced(user, sheet);
    await user.clear(within(sheet).getByLabelText("Runs as"));
    await user.click(within(sheet).getByLabelText("Runs as"));
    await user.paste("www");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(within(sheet).getByLabelText("Runs as")).toHaveAttribute("aria-invalid", "true");
    await user.clear(within(sheet).getByLabelText("Runs as"));
    await user.paste("1000");
    await user.click(within(sheet).getByLabelText("Upload limit"));
    await user.paste("128");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.website?.serves).toEqual({
      case: "php",
      value: expect.objectContaining({
        root: "/var/www/shop.example.com",
        version: "8.3",
        user: "1000",
        uploadLimitMb: 128,
      }),
    });
  });

  it("lists as its PHP version, and the runtimes say who uses each", async () => {
    onWideScreen();
    const { container } = renderAt("/websites", api([shop, blog]));
    const row = await rowOf(/shop\.example\.com/);
    expect(row).toHaveTextContent("PHP 8.3");
    expect(row).toHaveTextContent("/var/www/shop");
    const runtimes = await screen.findByRole("region", { name: "Runtimes" });
    const cards = within(runtimes).getAllByRole("article");
    expect(cards).toHaveLength(2);
    expect(cards[1]).toHaveTextContent("PHP 8.3");
    expect(cards[1]).toHaveTextContent("8.3.36 · php-fpm");
    expect(cards[1]).toHaveTextContent("1 website · shop.example.com");
    expect(cards[0]).toHaveTextContent("Not used");
    expect(cards[0]).toHaveTextContent("opcache, mysqli, gd, intl, imagick and 2 more");
    expect(within(cards[0] as HTMLElement).getByRole("link", { name: "New website on 8.4" })).toHaveAttribute(
      "href",
      expect.stringMatching(/^\/websites\/new\?php=(8\.4|%228\.4%22)$/),
    );
    await expectAccessible(container);
  });

  it("offers to move a website on an older build of its version to the runtime's", async () => {
    onWideScreen();
    const user = userEvent.setup();
    let asked: UpdateWebsiteRequest | undefined;
    renderAt(
      "/websites",
      api([create(WebsiteInfoSchema, { ...shop, phpImage: "wordpress@sha256:old83" })], {
        websites: {
          updateWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const runtimes = await screen.findByRole("region", { name: "Runtimes" });
    expect(within(runtimes).getByText(/Other languages run as apps from an image/)).toBeVisible();
    const card = within(runtimes).getAllByRole("article")[1] as HTMLElement;
    expect(card).toHaveTextContent("shop.example.com runs an older build of 8.3");
    await user.click(within(card).getByRole("button", { name: "Update to 8.3.36" }));
    await waitFor(() => expect(asked?.id).toBe("prj_shop"));
    expect(asked?.website?.serves.case).toBe("php");
  });

  it("lists each language an app from a folder runs on, with the apps on it", async () => {
    onWideScreen();
    const user = userEvent.setup();
    let asked: ChangeServiceRuntimeRequest | undefined;
    const node = (version: string, release: string) => ({
      id: "node",
      language: "Node.js",
      version,
      release,
      image: `node:${release}@sha256:${version}`,
      start: "npm start",
      port: 3000,
      extensions: [],
    });
    const api24 = anApp(
      "api",
      "node:24.21.0@sha256:old",
      {},
      {
        spec: create(ServiceSchema, {
          runtime: { language: "node", version: "24", folder: "/srv/api", port: 3000, user: "1000:1000" },
          runs: { case: "app", value: { image: "node:24.21.0@sha256:old" } },
        }),
      },
    );
    renderAt(
      "/websites",
      api([shop], {
        websites: {
          listWebsiteRuntimes: () => ({ runtimes: [...runtimes, node("26", "26.12.0"), node("24", "24.22.0")] }),
        },
        services: {
          listServices: () => ({ services: [api24, webOfBlog] }),
          changeServiceRuntime: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const runtimesPanel = await screen.findByRole("region", { name: "Runtimes" });
    const card = (await within(runtimesPanel).findByRole("heading", { name: "Node.js" })).closest(
      "article",
    ) as HTMLElement;
    expect(card).toHaveTextContent("26.12.0 · 24.22.0");
    expect(card).toHaveTextContent("1 app · api 24");
    expect(card).toHaveTextContent("Versions26, 24");
    expect(within(card).getByRole("link", { name: "New app on Node.js 26" })).toHaveAttribute(
      "href",
      expect.stringMatching(/^\/apps\/new\?source=folder&language=node&version=(26|%2226%22)$/),
    );
    expect(card).toHaveTextContent("api runs an older build of 24");
    await user.click(within(card).getByRole("button", { name: "Update to 24.22.0" }));
    await waitFor(() =>
      expect(asked).toMatchObject({ id: "svc_api", version: "24", summary: "Update api to Node.js 24.22.0" }),
    );
  });

  it("says its pages answer 502 while its PHP is stopped", async () => {
    onWideScreen();
    renderAt(
      "/websites",
      api([shop], {
        health: {
          listHealth: () => ({
            services: [
              aHealth("svc_shopweb", { state: HealthState.HEALTHY }),
              aHealth("svc_shopphp", { state: HealthState.NOT_RUNNING }),
            ],
          }),
        },
      }),
    );
    const row = await rowOf(/shop\.example\.com/);
    await waitFor(() => expect(row).toHaveTextContent("PHP stopped · pages answer 502"), { timeout: 3_000 });
  });

  it("runs as the folder's owner, and says when PHP can't write there", async () => {
    const user = userEvent.setup();
    renderAt(
      "/websites/new?php=8.3",
      api([], {
        files: {
          listFiles: disk({
            "/srv/mine": { mode: "drwxr-xr-x", entries: [entry("index.php", "-rw-r-----")] },
            "/srv/roots": { mode: "drwxr-xr-x", uid: 0, entries: [entry("index.php")] },
            "/srv/linked": {
              mode: "drwx--x--x",
              entries: [{ ...entry("index.php"), type: FileType.SYMLINK, linkType: FileType.FILE }],
            },
          }),
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await pickFolder(user, sheet, "/srv/mine");
    // index.php at 0640 is PHP's to read, as its owner, so nothing's wrong.
    expect(await within(sheet).findByText("Holds index.php", {}, { timeout: 3_000 })).toBeVisible();
    await openAdvanced(user, sheet);
    expect(within(sheet).getByLabelText("Runs as")).toHaveValue("1000:1000");
    await user.clear(within(sheet).getByLabelText("Runs as"));
    await user.type(within(sheet).getByLabelText("Runs as"), "82:82");
    expect(
      await within(sheet).findByText("PHP runs as 82:82 and can't write there.", {}, { timeout: 3_000 }),
    ).toBeVisible();
    // The state comes with what puts it right.
    await user.click(within(sheet).getByRole("button", { name: "Run as 1000:1000" }));
    expect(within(sheet).getByLabelText("Runs as")).toHaveValue("1000:1000");
    expect(await within(sheet).findByText("Holds index.php", {}, { timeout: 3_000 })).toBeVisible();
    await pickFolder(user, sheet, "/srv/roots");
    expect(
      await within(sheet).findByText("Root owns it, so PHP can't save uploads there.", {}, { timeout: 3_000 }),
    ).toBeVisible();
    // A folder anyone may walk but not list, with its index a symlink, is fine.
    await user.clear(within(sheet).getByLabelText("Runs as"));
    await user.type(within(sheet).getByLabelText("Runs as"), "1000:1000");
    await pickFolder(user, sheet, "/srv/linked");
    expect(await within(sheet).findByText("Holds index.php", {}, { timeout: 3_000 })).toBeVisible();
  }, 30_000);

  it("plans a site in a folder that's there as the folder's owner, without opening Advanced", async () => {
    const user = userEvent.setup();
    let asked: CreateWebsiteRequest | undefined;
    renderAt(
      "/websites/new?php=8.3",
      api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        files: {
          listFiles: disk({ "/var/www/shop.example.com": { mode: "drwxr-xr-x", entries: [entry("index.php")] } }),
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.click(within(sheet).getByLabelText("Domain"));
    await user.paste("shop.example.com");
    expect(await within(sheet).findByText("Holds index.php", {}, { timeout: 3_000 })).toBeVisible();
    expect(within(sheet).getByRole("button", { name: "Advanced" })).toHaveAccessibleDescription(
      "Runs as 1000:1000 · 64 MiB uploads",
    );
    // What the form filled in, it takes: uid:gid, as the server stores it.
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.website?.serves.value).toMatchObject({ user: "1000:1000" });
    expect(within(sheet).queryByText(/Give the user/)).not.toBeInTheDocument();
  });

  it("keeps Advanced open while a field in it that was refused is fixed", async () => {
    const user = userEvent.setup();
    renderAt("/websites/new?php=8.3", api([]));
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.type(within(sheet).getByLabelText("Domain"), "shop.example.com");
    await openAdvanced(user, sheet);
    await user.type(within(sheet).getByLabelText("Runs as"), "www");
    await user.click(within(sheet).getByRole("button", { name: "Advanced" }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    // Refused, it opens on the field, and one key clearing the error doesn't fold it away.
    const field = await within(sheet).findByLabelText("Runs as");
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(within(sheet).getByText("Give the user and group by number, such as 1000:1000.")).toBeVisible();
    await user.clear(field);
    await user.type(field, "1");
    expect(within(sheet).getByLabelText("Runs as")).toHaveValue("1");
    expect(within(sheet).getByRole("button", { name: "Advanced" })).toHaveAttribute("aria-expanded", "true");
  });

  it("follows the folder picked with who PHP runs as, until that's typed", async () => {
    const user = userEvent.setup();
    renderAt(
      "/websites/new?php=8.3",
      api([], {
        files: {
          listFiles: disk({
            "/srv/mine": { mode: "drwxr-xr-x", entries: [entry("index.php")] },
            "/srv/theirs": { mode: "drwxr-xr-x", uid: 33, entries: [entry("index.php")] },
          }),
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    const advanced = within(sheet).getByRole("button", { name: "Advanced" });
    await pickFolder(user, sheet, "/srv/mine");
    await waitFor(() => expect(advanced).toHaveAccessibleDescription("Runs as 1000:1000 · 64 MiB uploads"), {
      timeout: 3_000,
    });
    await pickFolder(user, sheet, "/srv/theirs");
    await waitFor(() => expect(advanced).toHaveAccessibleDescription("Runs as 33:33 · 64 MiB uploads"), {
      timeout: 3_000,
    });
    // Typed, it stays, and the note says when PHP can't write there.
    await openAdvanced(user, sheet);
    await user.clear(within(sheet).getByLabelText("Runs as"));
    await user.type(within(sheet).getByLabelText("Runs as"), "1000");
    await pickFolder(user, sheet, "/srv/mine");
    expect(await within(sheet).findByText("Holds index.php", {}, { timeout: 3_000 })).toBeVisible();
    expect(within(sheet).getByLabelText("Runs as")).toHaveValue("1000");
  }, 30_000);

  it("opens a stored site that runs as a user and group as valid", async () => {
    onWideScreen();
    const both = create(WebsiteInfoSchema, {
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
            user: "1000:1000",
            uploadLimitMb: 0,
            filesOnly: false,
          },
        },
      },
      environmentId: "env_shop",
      serviceId: "svc_shopweb",
      phpServiceId: "svc_shopphp",
      routeId: "rte_shop",
      updateTime: timestampFromDate(new Date(2026, 9, 13, 12)),
      serverId: SERVER,
      serverName: "hz-1",
    });
    renderAt(
      "/websites/prj_shop/configuration",
      api([both], {
        files: { listFiles: disk({ "/var/www/shop": { mode: "drwxr-xr-x", entries: [entry("index.php")] } }) },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "shop.example.com" })).toBeVisible();
    expect(await screen.findByText("Valid")).toBeVisible();
    expect(screen.queryByText("Not valid")).not.toBeInTheDocument();
    expect(screen.queryByText(/Give the user/)).not.toBeInTheDocument();
  });

  it("says what a site's folder was and what's in the one picked, as it changes", async () => {
    onWideScreen();
    const user = userEvent.setup();
    renderAt(
      "/websites/prj_docs/configuration",
      api([docs], {
        files: {
          listFiles: disk({
            "/var/www/docs": { mode: "drwxr-xr-x", entries: [entry("index.html")] },
            "/srv/site": { mode: "drwxr-xr-x", entries: [entry("readme.md")] },
          }),
        },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "docs.example.com" })).toBeVisible();
    await pickFolder(user, document.body, "/srv/site");
    expect(await screen.findByText("Was /var/www/docs")).toBeVisible();
    // The live check stays beside it, where the edit needs it most.
    expect(
      await screen.findByText("No index.html here yet, so it answers 404 until there is.", {}, { timeout: 3_000 }),
    ).toBeVisible();
  });

  it("keeps a refused folder's picker open while its path is fixed", async () => {
    const user = userEvent.setup();
    renderAt("/websites/new", api([]));
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    const path = within(sheet).getByRole("textbox", { name: "Path" });
    expect(path).toHaveAttribute("aria-invalid", "true");
    // The form takes you to the first field refused, the domain, on the next frame.
    await waitFor(() => expect(within(sheet).getByLabelText("Domain")).toHaveFocus());
    await user.type(path, "/srv/x");
    // The first key clears what was refused, and the picker stays where it's typed in.
    expect(within(sheet).getByRole("textbox", { name: "Path" })).toHaveValue("/srv/x");
    expect(within(sheet).getByRole("textbox", { name: "Path" })).toHaveFocus();
  });

  it("offers Watch on a domain that serves, and says how the check that watches it stands", async () => {
    onWideScreen();
    const user = userEvent.setup();
    let checks: { id: string; name: string; spec: { address: string }; state: AlertCheckState }[] = [];
    const alerts = {
      listAlertChecks: () => ({ checks }),
      listAlertRules: () => ({}),
      listAlertChannels: () => ({}),
    };
    const { unmount } = renderAt("/websites/prj_blog", api([blog], { alerts }));
    const parts = await screen.findByRole("region", { name: "How it serves" });
    await user.click(await within(parts).findByRole("button", { name: "Watch blog.example.com" }));
    const sheet = await screen.findByRole("dialog", { name: "Watch this address" });
    expect(within(sheet).getByLabelText("Address")).toHaveValue("https://blog.example.com");
    unmount();

    checks = [
      {
        id: "chk_blog",
        name: "blog-example-com",
        spec: { address: "https://blog.example.com/" },
        state: AlertCheckState.UP,
      },
    ];
    renderAt("/websites/prj_blog", api([blog], { alerts }));
    const again = await screen.findByRole("region", { name: "How it serves" });
    const link = await within(again).findByRole("link", { name: "blog.example.com is watched: Up" });
    expect(link).toHaveAttribute("href", "/alerts/checks");
    expect(within(again).queryByRole("button", { name: /^Watch / })).toBeNull();
  });

  it("opens on how each part of it runs, and what it serves", async () => {
    onWideScreen();
    const { container } = renderAt("/websites/prj_shop", api([shop]));
    expect(await screen.findByRole("heading", { level: 1, name: "shop.example.com" })).toBeVisible();
    expect(screen.getByText(/PHP 8\.3 from \/var\/www\/shop/)).toBeVisible();
    const parts = screen.getByRole("region", { name: "How it serves" });
    expect(within(parts).getByText("Web server")).toBeVisible();
    expect(within(parts).getByText("PHP 8.3")).toBeVisible();
    const logs = within(parts).getAllByRole("link", { name: "Logs" });
    expect(logs[1]).toHaveAttribute("href", "/websites/prj_shop/logs?which=php");
    const serves = screen.getByRole("region", { name: "What it serves" });
    expect(serves).toHaveTextContent("Runs as1000");
    expect(serves).toHaveTextContent("Upload limit64 MiB");
    expect(screen.getByRole("link", { name: "Files" })).toHaveAttribute("href", "/websites/prj_shop/files");
    await expectAccessible(container);
  });

  it("configures as PHP, and plans a new version", async () => {
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
        changes: { watchChange: feed<WatchChangeResponse>().stream },
        files: {
          listFiles: disk({ "/var/www/shop": { mode: "drwxr-xr-x", entries: [entry("index.php")] } }),
        },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "shop.example.com" })).toBeVisible();
    expect(screen.getByLabelText("Serves")).toHaveValue("php");
    expect(screen.getByText("Its folder stays on the server, with what PHP wrote there.")).toBeVisible();
    // What has a default is a line under Advanced, until it's opened.
    const advanced = screen.getByRole("button", { name: "Advanced" });
    expect(advanced).toHaveAccessibleDescription("Runs as 1000 · 64 MiB uploads");
    expect(screen.queryByLabelText("Upload limit")).not.toBeInTheDocument();
    await user.click(advanced);
    await user.clear(screen.getByLabelText("Upload limit"));
    await user.type(screen.getByLabelText("Upload limit"), "128");
    // Closed again, it says an edit is in it.
    await user.click(advanced);
    expect(advanced).toHaveTextContent("1 changed");
    await user.click(advanced);
    await user.clear(screen.getByLabelText("Upload limit"));
    await user.type(screen.getByLabelText("Upload limit"), "64");
    expect(advanced).not.toHaveTextContent("changed");
    const version = await screen.findByLabelText("PHP version");
    await waitFor(() => expect(version).toHaveValue("8.3"));
    await user.selectOptions(version, "8.4");
    expect(screen.getByText(/Was PHP 8\.3/)).toBeVisible();
    const plan = screen.getByRole("button", { name: /Review plan/ });
    await waitFor(() => expect(plan).toBeEnabled(), { timeout: 3_000 });
    await user.click(plan);
    await waitFor(() => expect(asked?.website?.serves.case).toBe("php"));
    expect(asked?.website?.serves.value).toMatchObject({ version: "8.4", root: "/var/www/shop" });
  });

  it("offers to upload into an empty folder, from where its files are listed", async () => {
    const user = userEvent.setup();
    renderAt(
      "/websites/prj_shop/files",
      api([shop], { files: { listFiles: disk({ "/var/www/shop": { mode: "drwxr-xr-x", entries: [] } }) } }),
    );
    expect(await screen.findByRole("heading", { name: "The folder is empty" })).toBeVisible();
    expect(screen.queryByText(/scp|rsync/)).not.toBeInTheDocument();
    const upload = screen.getAllByRole("button", { name: "Upload files" });
    // One way in: the empty folder's own button, not a second in the panel's head.
    expect(upload).toHaveLength(1);
    await user.click(upload[0] as HTMLElement);
    expect(await screen.findByRole("dialog", { name: "Upload to shop" })).toBeVisible();
  });

  it("lists what's at the top of its folder, each opening in Files", async () => {
    renderAt(
      "/websites/prj_shop/files",
      api([shop], {
        files: {
          listFiles: disk({
            "/var/www/shop": {
              mode: "drwxr-xr-x",
              entries: [entry("index.php"), entry("wp-content", "drwxr-xr-x", FileType.DIRECTORY)],
            },
          }),
        },
      }),
    );
    expect(await screen.findByRole("link", { name: "wp-content" })).toHaveAttribute(
      "href",
      `/servers/${SERVER}/files?path=%2Fvar%2Fwww%2Fshop%2Fwp-content`,
    );
    expect(screen.getByRole("link", { name: "index.php" })).toHaveAttribute(
      "href",
      `/servers/${SERVER}/files?path=%2Fvar%2Fwww%2Fshop&file=%2Fvar%2Fwww%2Fshop%2Findex.php`,
    );
  });

  it("says where its certificate stands", async () => {
    renderAt("/websites/prj_blog/certificate", api([blog]));
    expect(await screen.findByText("Issued")).toBeVisible();
    expect(screen.getByText("Expires")).toBeVisible();
  });
});

describe("New website on a fleet", () => {
  it("asks which server, and takes you to it", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchServersResponse>();
    watch.push(
      changed(aServer({ id: SERVER, name: "hz-1" })),
      changed(aServer({ id: "srv_2", name: "hz-2" })),
      snapshotComplete,
    );
    let asked: CreateWebsiteRequest | undefined;
    renderAt("/websites/new", {
      ...api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
      servers: { watchServers: watch.stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.type(within(sheet).getByRole("textbox", { name: "Domain" }), "shop.example.com");
    await user.click(within(sheet).getByRole("radio", { name: /^A URL/ }));
    await user.type(within(sheet).getByRole("textbox", { name: "URL" }), "https://backend.example.com");
    const server = await within(sheet).findByRole("combobox", { name: "Server" });
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await within(sheet).findByText("Choose the server it runs on.")).toBeVisible();
    await waitFor(() => expect(server).toHaveFocus());
    await user.selectOptions(server, "hz-2");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked?.servers).toEqual(["srv_2"]));
  });
});

describe("A website's configuration", () => {
  it("shows what its web server runs, marks what an edit changes, and plans it", async () => {
    const user = userEvent.setup();
    let asked: UpdateWebsiteRequest | undefined;
    const { container } = renderAt(
      "/websites/prj_blog/configuration",
      api([blog, tools], {
        websites: {
          updateWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "blog.example.com" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Visit site" })).toHaveAttribute("href", "https://blog.example.com");
    const code = await screen.findByRole("region", { name: "The web server's config" });
    await waitFor(() => expect(code).toHaveTextContent("reverse_proxy https://backend.example.com"));
    expect(code.querySelector(".is-changed")).toBeNull();
    const review = screen.getByRole("button", { name: /Review plan/ });
    expect(review).toBeDisabled();
    await expectAccessible(container);

    const url = screen.getByLabelText("URL");
    await user.clear(url);
    await user.type(url, "https://new.example.com");
    await waitFor(() => expect(code).toHaveTextContent("reverse_proxy https://new.example.com"));
    expect(code.querySelectorAll(".is-changed")).toHaveLength(1);
    expect(screen.getByText("1 change")).toBeVisible();
    expect(screen.getByText("Was https://backend.example.com")).toBeVisible();
    await waitFor(() => expect(review).toBeEnabled());
    await user.click(review);
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.id).toBe("prj_blog");
    expect(asked?.website?.serves).toEqual({
      case: "proxy",
      value: expect.objectContaining({ url: "https://new.example.com" }),
    });

    // Discard puts it back as it is.
    await user.keyboard("{Escape}");
    await user.click(await screen.findByRole("button", { name: "Discard" }));
    expect(screen.getByLabelText("URL")).toHaveValue("https://backend.example.com");
  });

  it("shows what its web server really runs, and plans its settings back when that differs", async () => {
    const user = userEvent.setup();
    let asked: UpdateWebsiteRequest | undefined;
    const odd = ':8080 {\n\trespond "elsewhere"\n}\n';
    renderAt(
      "/websites/prj_blog/configuration",
      api([create(WebsiteInfoSchema, { ...blog, config: odd, configDiffers: true })], {
        websites: {
          updateWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const code = await screen.findByRole("region", { name: "The web server's config" });
    await waitFor(() => expect(code).toHaveTextContent("reverse_proxy https://backend.example.com"));
    expect(code.querySelectorAll(".is-changed").length).toBeGreaterThan(0);
    expect(screen.getByText(/edited outside Websites\. Review plan puts it back\./)).toBeVisible();
    const review = screen.getByRole("button", { name: /Review plan/ });
    await waitFor(() => expect(review).toBeEnabled());
    await user.click(review);
    await waitFor(() =>
      expect(asked?.website?.serves).toEqual({
        case: "proxy",
        value: expect.objectContaining({ url: "https://backend.example.com" }),
      }),
    );
  });

  it("says what won't do, as you type, and holds the plan back", async () => {
    const user = userEvent.setup();
    renderAt("/websites/prj_blog/configuration", api([blog]));
    const url = await screen.findByLabelText("URL");
    await user.clear(url);
    await user.type(url, "http://localhost");
    expect(await screen.findByText("Not valid")).toBeVisible();
    expect(url).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("button", { name: /Review plan/ })).toBeDisabled();
  });

  it("says how the ingress reaches a port, which has no config of its own, and no logs", async () => {
    const user = userEvent.setup();
    renderAt("/websites/prj_tools/configuration", api([blog, tools]));
    expect(await screen.findByRole("heading", { level: 2, name: "Route" })).toBeVisible();
    expect(screen.getByText("127.0.0.1:3000")).toBeVisible();
    expect(screen.getByLabelText("Port on the server")).toHaveValue("3000");
    await user.click(screen.getByRole("link", { name: "Logs" }));
    expect(await screen.findByRole("heading", { name: "Nothing here writes logs" })).toBeVisible();
  });

  it("says when there's no website by that ID", async () => {
    renderAt("/websites/prj_gone/configuration", api([blog]));
    expect(await screen.findByRole("heading", { name: "No website with this ID" })).toBeVisible();
  });
});

describe("A website's width", () => {
  const kindOf = (heading: HTMLElement) => {
    const page = heading.closest(".page");
    if (!page) throw new Error("no page around the heading");
    const kind = page.className.match(/\bis-(wide|narrow)\b/)?.[1] ?? "standard";
    return page.classList.contains("has-full-body") ? `${kind}, its body full` : kind;
  };

  // Page.width.test.tsx has the other pages, and the rule is Layout in design/system/README.md.
  it("makes the list and a website standard pages, and gives a log view the whole width", async () => {
    const list = renderAt("/websites", api([blog, tools]));
    expect(kindOf(await screen.findByRole("heading", { level: 1, name: "Websites" }))).toBe("standard");
    list.unmount();
    const site = renderAt("/websites/prj_blog/configuration", api([blog, tools]));
    expect(kindOf(await screen.findByRole("heading", { level: 1, name: "blog.example.com" }))).toBe("standard");
    site.unmount();
    const logs = renderAt(
      "/websites/prj_blog/logs",
      api([blog, tools], { logs: { watchLogs: feed<WatchLogsResponse>().stream } }),
    );
    expect(kindOf(await screen.findByRole("heading", { level: 1, name: "blog.example.com" }))).toBe(
      "standard, its body full",
    );
    logs.unmount();
    // A website with no web server of its own has no log view to widen.
    renderAt("/websites/prj_tools/logs", api([blog, tools]));
    expect(kindOf(await screen.findByRole("heading", { level: 1, name: "tools.example.com" }))).toBe("standard");
  });
});

describe("Marking what a config's edit changes", () => {
  it("marks the lines that are new, and none when nothing was there to compare", () => {
    expect(changedLines("", "a\nb\n").map((l) => l.changed)).toEqual([false, false]);
    expect(changedLines("a\nb\nc\n", "a\nx\nc\n").map((l) => [l.n, l.text, l.changed])).toEqual([
      [1, "a", false],
      [2, "x", true],
      [3, "c", false],
    ]);
    expect(changedLines("a\nc\n", "a\nb\nc\n").map((l) => l.changed)).toEqual([false, true, false]);
  });
});
