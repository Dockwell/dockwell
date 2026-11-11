// SPDX-License-Identifier: AGPL-3.0-only

import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  CertificateState,
  ChangeState,
  Connection,
  DomainCheckState,
  type ProposeChangeRequest,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { focusManager, onlineManager } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, ENV, places, projectPlaces, SERVER } from "../../test/apps";
import { moved, watched, webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt, untilAborted } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: ["49.12.33.7"] } });
const hz2 = aServer({ id: "srv_2", name: "hz-2", inventory: { addresses: ["49.12.33.8"] } });
const web = anApp("web", "nginx:1.5");
const api_ = anApp("api", "ghcr.io/acme/api:1.0", { ports: [{ hostPort: 8080, containerPort: 3000 }] });

const certificate = (state: CertificateState, message = "", days?: number) => ({
  state,
  message,
  ...(days === undefined ? {} : { expireTime: timestampFromDate(new Date(Date.now() + days * 86_400_000 - 60_000)) }),
});

interface DomainInit {
  name: string;
  service?: string;
  environment?: string;
  /** What its one server says of it, or leave out for a server that hasn't. */
  certificate?: ReturnType<typeof certificate>;
  route?: object;
  routes?: object[];
  server?: object;
  servers?: object[];
}

/** A domain as ListDomains has it: one route to service, on hz-1. */
function aDomain({ name, service = "web", environment = ENV, ...init }: DomainInit) {
  const id = name.replaceAll(".", "-");
  return {
    name,
    environmentId: environment,
    url: `https://${name}`,
    routes: init.routes ?? [{ id: `rte_${id}`, name: id, service, port: 80, ...init.route }],
    servers: init.servers ?? [
      {
        serverId: SERVER,
        serverName: "hz-1",
        ...(init.certificate ? { status: { domain: name, certificate: init.certificate } } : {}),
        ...init.server,
      },
    ],
  };
}

function api(
  domains: ReturnType<typeof aDomain>[],
  {
    services = [web],
    servers = [hz1],
    more = {},
  }: { services?: (typeof web)[]; servers?: (typeof hz1)[]; more?: Api } = {},
): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  return {
    resources: places(),
    services: { listServices: () => ({ services }) },
    servers: { watchServers: watch.stream },
    domains: { listDomains: () => ({ domains }) },
    ...more,
  };
}

const rowOf = (name: string | RegExp) => screen.findByRole("row", { name });

/** The names of the rows shown, in their order. */
const namesShown = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((row) => row.querySelector(".domains-at")?.textContent);

const longAgo = timestampFromDate(new Date(Date.now() - 3_600_000));

/** What the API says of what lives where, with when routes last changed, a page of them at a time. */
function withRoutes(...pages: { id: string; updateTime?: typeof longAgo }[][]): Api["resources"] {
  return {
    listResources: (req) => {
      if (req.kind !== "route") return places().listResources(req);
      const at = Number(req.pageToken || 0);
      return { resources: pages[at] ?? [], nextPageToken: at + 1 < pages.length ? String(at + 1) : "" };
    },
  } as Api["resources"];
}

const issued = certificate(CertificateState.ISSUED, "", 47);
const noRecord =
  "No DNS record answers for app.example.com, so the certificate can't come. Add an A record for it pointing at 49.12.33.7. Dockwell tries again by itself.";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  focusManager.setFocused(undefined);
  onlineManager.setOnline(true);
});

describe("the list of domains", () => {
  it("says where each stands in the words an app's page has for it", async () => {
    const { container } = renderAt(
      "/domains",
      api([
        aDomain({ name: "a.example.com", certificate: issued }),
        aDomain({ name: "b.example.com", certificate: certificate(CertificateState.PENDING, "On its way.") }),
        aDomain({ name: "c.example.com", certificate: certificate(CertificateState.FAILED, noRecord) }),
        aDomain({ name: "d.example.com", certificate: certificate(CertificateState.NONE), route: { tls: "off" } }),
        aDomain({
          name: "e.example.com",
          certificate: certificate(CertificateState.NONE),
          route: { tls: "off", plainForAnyone: true },
        }),
        aDomain({
          name: "f.example.com",
          certificate: certificate(CertificateState.ISSUED, "Renewing it is failing."),
        }),
      ]),
    );
    expect(await rowOf(/a\.example\.com/)).toHaveTextContent("Valid for 47 days");
    expect(await rowOf(/b\.example\.com/)).toHaveTextContent("Issuing");
    expect(await rowOf(/c\.example\.com/)).toHaveTextContent("Failed");
    expect(await rowOf(/d\.example\.com/)).toHaveTextContent("TLS ends in front");
    expect(await rowOf(/e\.example\.com/)).toHaveTextContent("Plain HTTP");
    expect(await rowOf(/f\.example\.com/)).toHaveTextContent("Renewal failing");
    expect(container.querySelector(".page-lead")).toHaveTextContent("6 domains · 2 need a look");
    await expectAccessible(container);
  });

  it("never reads as serving what nothing has said, or what no server can say now", async () => {
    renderAt(
      "/domains",
      api([
        aDomain({ name: "nothing.example.com" }),
        aDomain({ name: "nowhere.example.com", servers: [] }),
        aDomain({ name: "gone.example.com", certificate: issued, server: { stale: true } }),
      ]),
    );
    for (const name of [/^nothing/, /^nowhere/, /^gone/]) {
      expect((await rowOf(name)).querySelector(".dw-tone-healthy")).toBeNull();
    }
    expect(await rowOf(/^nothing/)).toHaveTextContent("Issuing");
    expect(await rowOf(/^nowhere/)).toHaveTextContent("No server");
    const gone = await rowOf(/^gone/);
    expect(gone).toHaveTextContent("Not connected");
    expect(within(gone).queryByRole("link", { name: "Open gone.example.com" })).toBeNull();
    // What it last said is said as that, with where to look.
    expect(gone).toHaveTextContent(/hz-1 isn't connected, so this is what it last said.*Valid for 47 days/);
    expect(within(gone).getByRole("link", { name: "Check its page" })).toHaveAttribute("href", `/servers/${SERVER}`);
  });

  it("says at once that a server has gone quiet, as that started before this visit", async () => {
    const connected = aServer({ id: SERVER, name: "hz-1", connectTime: longAgo });
    const { container } = renderAt(
      "/domains",
      api([aDomain({ name: "quiet.example.com" })], {
        servers: [connected],
        more: { resources: withRoutes([{ id: "rte_quiet-example-com", updateTime: longAgo }]) },
      }),
    );
    await waitFor(async () => expect(await rowOf(/quiet/)).toHaveTextContent("No report"));
    expect(await rowOf(/quiet/)).toHaveTextContent("hz-1 hasn't reported yet.");
    expect(within(await rowOf(/quiet/)).queryByText("Issuing")).toBeNull();
    expect(container.querySelector(".page-lead")).toHaveTextContent("1 domain · 1 needs a look");
  });

  it("gives a server its minute from when it connected, or the route last changed", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const justNow = timestampFromDate(new Date());
    const cases = [
      { connectTime: justNow, updateTime: longAgo },
      { connectTime: longAgo, updateTime: justNow },
    ];
    for (const { connectTime, updateTime } of cases) {
      const { unmount } = renderAt(
        "/domains",
        api([aDomain({ name: "quiet.example.com" })], {
          servers: [aServer({ id: SERVER, name: "hz-1", connectTime })],
          more: { resources: withRoutes([{ id: "rte_quiet-example-com", updateTime }]) },
        }),
      );
      await screen.findByRole("row", { name: /quiet/ });
      await vi.advanceTimersByTimeAsync(1_000);
      expect(await rowOf(/quiet/)).toHaveTextContent("Issuing");
      unmount();
    }
  });

  it("finds when a route changed however many there are, a page at a time", async () => {
    const connected = aServer({ id: SERVER, name: "hz-1", connectTime: longAgo });
    renderAt(
      "/domains",
      api([aDomain({ name: "quiet.example.com" })], {
        servers: [connected],
        more: {
          resources: withRoutes(
            [{ id: "rte_other", updateTime: longAgo }],
            [{ id: "rte_quiet-example-com", updateTime: longAgo }],
          ),
        },
      }),
    );
    await waitFor(async () => expect(await rowOf(/quiet/)).toHaveTextContent("No report"));
  });

  it("says a server has gone quiet on a domain, once it has been a while", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderAt("/domains", api([aDomain({ name: "quiet.example.com" })]));
    expect(await rowOf(/quiet/)).toHaveTextContent("Issuing");
    await vi.advanceTimersByTimeAsync(61_000);
    expect(await rowOf(/quiet/)).toHaveTextContent("No report");
    expect(await rowOf(/quiet/)).toHaveTextContent("hz-1 hasn't reported yet.");
  });

  it("marks what a domain reaches by its route, while the apps are still loading or can't be read", async () => {
    renderAt(
      "/domains",
      api(
        [
          aDomain({ name: "app.example.com", certificate: issued }),
          aDomain({ name: "port.example.com", service: "", certificate: issued, route: { hostPort: 9000, port: 0 } }),
        ],
        { more: { services: { listServices: () => new Promise(() => {}) } } },
      ),
    );
    const app = await rowOf(/^app\.example\.com/);
    expect(app.querySelector(".domains-reaches-mark")).toHaveClass("is-app");
    const port = await rowOf(/^port\.example\.com/);
    expect(port.querySelector(".domains-reaches-mark")).toHaveClass("is-server");
  });

  it("leads each row to its app, in its environment, as a name means something only in one", async () => {
    const user = userEvent.setup();
    const staging = anApp(
      "web",
      "nginx:1.5",
      {},
      { id: "svc_web_staging", resource: { kind: "service", scope: "env_staging", name: "web" } },
    );
    const { router } = renderAt(
      "/domains",
      api(
        [
          aDomain({ name: "prod.example.com", certificate: issued }),
          aDomain({ name: "staging.example.com", certificate: issued, environment: "env_staging" }),
          aDomain({ name: "port.example.com", service: "", certificate: issued, route: { hostPort: 9000, port: 0 } }),
          aDomain({ name: "old.example.com", service: "deleted", certificate: issued }),
        ],
        { services: [web, staging] },
      ),
    );
    const prod = await rowOf(/prod\.example\.com/);
    expect(within(prod).getByRole("link", { name: "web" })).toHaveAttribute("href", "/apps/svc_web");
    const other = await rowOf(/staging\.example\.com/);
    expect(within(other).getByRole("link", { name: "web" })).toHaveAttribute("href", "/apps/svc_web_staging");
    // A port on the server is no app's, and what's gone has nowhere to lead.
    const port = await rowOf(/port\.example\.com/);
    expect(port).toHaveTextContent("port 9000 on the server");
    expect(within(port).queryByRole("link", { name: /port 9000/ })).toBeNull();
    await user.click(port);
    const old = await rowOf(/old\.example\.com/);
    expect(old).toHaveTextContent("deleted");
    expect(within(old).queryByRole("link", { name: "deleted" })).toBeNull();
    await user.click(old);
    expect(router.state.location.pathname).toBe("/domains");

    await user.click(other);
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_web_staging"));
  });

  it("opens the app from the row, and leaves a link in the row to do its own job", async () => {
    const user = userEvent.setup();
    const { router } = renderAt(
      "/domains",
      api([aDomain({ name: "gone.example.com", certificate: issued, server: { stale: true } })]),
    );
    const row = await rowOf(/gone/);
    await user.click(within(row).getByRole("link", { name: "Check its page" }));
    await waitFor(() => expect(router.state.location.pathname).toBe(`/servers/${SERVER}`));
  });

  it("isn't a keyboard stop where a row leads nowhere, and is where it leads somewhere", async () => {
    const user = userEvent.setup();
    renderAt(
      "/domains",
      api([
        aDomain({ name: "app.example.com", certificate: issued }),
        aDomain({ name: "port.example.com", service: "", certificate: issued, route: { hostPort: 9000, port: 0 } }),
        aDomain({ name: "old.example.com", service: "deleted", certificate: issued }),
      ]),
    );
    const app = await rowOf(/^app\.example\.com/);
    const port = await rowOf(/^port\.example\.com/);
    const old = await rowOf(/^old\.example\.com/);
    expect(app).toHaveAttribute("tabindex", "0");
    expect(port).not.toHaveAttribute("tabindex");
    expect(old).not.toHaveAttribute("tabindex");
    expect(port).not.toHaveClass("is-clickable");

    // Tabbing goes past each, to what's in it that does something.
    within(app).getByRole("link", { name: "Open app.example.com" }).focus();
    await user.tab();
    expect(screen.getByRole("link", { name: "Open port.example.com" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("link", { name: "Open old.example.com" })).toHaveFocus();
  });

  it("reads a certificate past its end as expired, in red, with what to do", async () => {
    const expired = certificate(CertificateState.ISSUED, "", -2);
    const { container } = renderAt("/domains", api([aDomain({ name: "old.example.com", certificate: expired })]));
    const row = await rowOf(/old\.example\.com/);
    expect(row).toHaveTextContent("Expired");
    expect(row).not.toHaveTextContent("Valid");
    expect(row.querySelector(".dw-tone-healthy")).toBeNull();
    expect(row.querySelector(".dw-tone-critical")).not.toBeNull();
    expect(row.querySelector(".domains-note")).toHaveClass("is-failed");
    expect(row).toHaveTextContent(/The certificate on hz-1 ran out .+, so browsers refuse it\./);
    expect(within(row).getByRole("link", { name: "Check its page" })).toHaveAttribute("href", `/servers/${SERVER}`);
    expect(within(row).queryByRole("link", { name: /^Open / })).toBeNull();
    expect(container.querySelector(".page-lead")).toHaveTextContent("1 domain · 1 needs a look");
  });

  it("says why renewing failed, with a certificate that has run out", async () => {
    const said = "Renewing it failed: the certificate authority's rate limit was hit.";
    renderAt(
      "/domains",
      api([aDomain({ name: "old.example.com", certificate: certificate(CertificateState.ISSUED, said, -2) })]),
    );
    const row = await rowOf(/old\.example\.com/);
    expect(row).toHaveTextContent("Expired");
    expect(row).toHaveTextContent(said);
  });

  it("measures again whether the line holds the note when the row draws again, not only when its width changes", async () => {
    const user = userEvent.setup();
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    let height = 0;
    const tall = vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockImplementation(() => height);
    try {
      renderAt(
        "/domains",
        api([aDomain({ name: "app.example.com", certificate: certificate(CertificateState.FAILED, noRecord) })]),
      );
      const row = await rowOf(/app\.example\.com/);
      expect(within(row).queryByRole("button", { name: /^More about/ })).toBeNull();
      // The words outgrow the line with the width unchanged, as when the list's next read
      // says more: the next time the row draws, More is there.
      height = 40;
      await user.type(screen.getByRole("searchbox", { name: /Filter/ }), "app");
      expect(
        await within(await rowOf(/app\.example\.com/)).findByRole("button", { name: "More about app.example.com" }),
      ).toBeVisible();
    } finally {
      tall.mockRestore();
      vi.unstubAllGlobals();
    }
  });

  it("doesn't cut short a note that ends in its own way to put it right", async () => {
    // However long the line, a note with a link in it is never one line with More, as
    // the link would be out of sight and still in the tab order.
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    const tall = vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockReturnValue(40);
    try {
      renderAt("/domains", api([aDomain({ name: "old.example.com", certificate: issued, server: { stale: true } })]));
      const row = await rowOf(/old\.example\.com/);
      await waitFor(() => expect(row.querySelector(".domains-note-text")).not.toHaveClass("is-one-line"));
      expect(within(row).getByRole("link", { name: "Check its page" })).toBeVisible();
      expect(within(row).queryByRole("button", { name: /^More about/ })).toBeNull();
    } finally {
      tall.mockRestore();
      vi.unstubAllGlobals();
    }
  });

  it("says a server that isn't connected last said a certificate had run out, not that it's valid", async () => {
    const expired = certificate(CertificateState.ISSUED, "", -2);
    renderAt("/domains", api([aDomain({ name: "old.example.com", certificate: expired, server: { stale: true } })]));
    const row = await rowOf(/old\.example\.com/);
    expect(row).toHaveTextContent("Not connected");
    expect(row).toHaveTextContent(/what it last said.*Expired/);
    expect(row).not.toHaveTextContent("Valid");
  });

  it("goes by the control plane's clock, so a browser days ahead doesn't call a good certificate expired", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const domains = [aDomain({ name: "good.example.com", certificate: issued })];
    // What the control plane stamps a server with says how far out this browser is.
    const stamped = timestampFromDate(new Date());
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete, changed(aServer({ id: SERVER, name: "hz-1", lastSeenTime: stamped })));
    vi.setSystemTime(Date.now() + 60 * 86_400_000);
    renderAt("/domains", api(domains, { more: { servers: { watchServers: watch.stream } } }));
    await waitFor(async () => expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Valid for 47 days"));
  });

  it("separates where a domain is, once it says both its servers and its environment", async () => {
    renderAt(
      "/domains",
      api([aDomain({ name: "app.example.com", certificate: issued })], {
        servers: [hz1, hz2],
        more: { resources: places("staging") },
      }),
    );
    await waitFor(async () => expect(await rowOf(/app\.example\.com/)).toHaveTextContent("hz-1 · production"));
  });

  it("gives each path of a domain its own row, and says what's wrong with the domain once", async () => {
    renderAt(
      "/domains",
      api(
        [
          aDomain({
            name: "app.example.com",
            certificate: certificate(CertificateState.FAILED, noRecord),
            routes: [
              { id: "rte_1", name: "root", service: "web", port: 80 },
              { id: "rte_2", name: "api", service: "api", path: "/api", port: 3000 },
            ],
          }),
        ],
        { services: [web, api_] },
      ),
    );
    const root = await screen.findByRole("row", { name: /^app\.example\.com(?!\/api)/ });
    const path = await rowOf(/app\.example\.com\/api/);
    expect(within(root).getByRole("link", { name: "web" })).toHaveAttribute("href", "/apps/svc_web");
    expect(within(path).getByRole("link", { name: "api" })).toHaveAttribute("href", "/apps/svc_api");
    expect(root).toHaveTextContent("No DNS record answers");
    expect(path).not.toHaveTextContent("No DNS record answers");
    expect(path).toHaveTextContent("Failed");
    // One domain, however many paths.
    expect(document.querySelector(".page-lead")).toHaveTextContent("1 domain · 1 needs a look");
  });

  it("says what's wrong with a failing domain, with the fix", async () => {
    renderAt(
      "/domains",
      api([aDomain({ name: "app.example.com", certificate: certificate(CertificateState.FAILED, noRecord) })]),
    );
    const row = await rowOf(/app\.example\.com/);
    expect(row).toHaveTextContent(noRecord);
    expect(within(row).queryByRole("link", { name: "Open app.example.com" })).toBeNull();
  });

  it("keeps what's wrong to a line, with More for the rest and the fix beside it", async () => {
    const user = userEvent.setup();
    // jsdom lays nothing out, so the line is said to hold more than it shows.
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    const tall = vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockReturnValue(40);
    try {
      renderAt(
        "/domains",
        api([aDomain({ name: "app.example.com", certificate: certificate(CertificateState.FAILED, noRecord) })]),
      );
      const row = await rowOf(/app\.example\.com/);
      const text = row.querySelector(".domains-note-text");
      expect(text).toHaveClass("is-one-line");
      // Each row's toggle is named for its domain, so a list of buttons tells them apart.
      const more = await within(row).findByRole("button", { name: "More about app.example.com" });
      expect(more).toHaveTextContent("More");
      expect(more).toHaveAttribute("aria-controls", text?.id ?? "");
      await user.click(more);
      expect(text).not.toHaveClass("is-one-line");
      expect(within(row).getByRole("button", { name: "Less about app.example.com" })).toHaveAttribute(
        "aria-expanded",
        "true",
      );
    } finally {
      tall.mockRestore();
      vi.unstubAllGlobals();
    }
  });

  it("says what an ingress that's down does to a domain with a good certificate", async () => {
    const held = "1Panel-openresty holds port 80, so Caddy can't start. Stop it, or move it to another port.";
    renderAt(
      "/domains",
      api([aDomain({ name: "app.example.com", certificate: issued, server: { ingress: { message: held } } })]),
    );
    const row = await rowOf(/app\.example\.com/);
    expect(row).toHaveTextContent("Failed");
    expect(row).toHaveTextContent(held);
  });

  it("offers the form's switch where the server's words name it", async () => {
    const user = userEvent.setup();
    const edits: ProposeChangeRequest["edits"] = [];
    const refused =
      "A certificate authority can't reach app.example.com. If a tunnel or a proxy ends TLS in front of this server, set tls to off on its routes.";
    renderAt(
      "/domains",
      api([aDomain({ name: "app.example.com", certificate: certificate(CertificateState.FAILED, refused) })], {
        more: {
          changes: {
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
            proposeChange: (req) => {
              edits.push(...req.edits);
              return { change: webPlanned() };
            },
          },
        },
      }),
    );
    const row = await rowOf(/app\.example\.com/);
    expect(row).toHaveTextContent(`turn on "A tunnel or a proxy in front handles HTTPS" for this domain`);
    expect(row).not.toHaveTextContent("set tls to off");
    await user.click(within(row).getByRole("button", { name: "It's behind a tunnel or a proxy" }));
    const sheet = await screen.findByRole("dialog", { name: "Move app.example.com behind what's in front" });
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits[0]?.resource).toMatchObject({ kind: "route", scope: ENV, name: "app-example-com" });
    expect(edits[0]?.spec?.kind.value).toMatchObject({ domain: "app.example.com", service: "web", tls: "off" });
  });

  it("visits a domain over HTTPS behind a tunnel, as a visitor does, and only once it serves", async () => {
    renderAt(
      "/domains",
      api([
        aDomain({ name: "t.example.com", certificate: certificate(CertificateState.NONE), route: { tls: "off" } }),
        aDomain({ name: "p.example.com", certificate: certificate(CertificateState.PENDING) }),
      ]),
    );
    const row = await rowOf(/t\.example\.com/);
    expect(within(row).getByRole("link", { name: "Open t.example.com" })).toHaveAttribute(
      "href",
      "https://t.example.com",
    );
    expect(within(await rowOf(/p\.example\.com/)).queryByRole("link", { name: /^Open / })).toBeNull();
  });

  it("names the servers a domain is on once there's a choice of them, and not before", async () => {
    const { unmount } = renderAt("/domains", api([aDomain({ name: "app.example.com", certificate: issued })]));
    expect(await rowOf(/app\.example\.com/)).not.toHaveTextContent("hz-1");
    unmount();
    renderAt("/domains", api([aDomain({ name: "app.example.com", certificate: issued })], { servers: [hz1, hz2] }));
    await waitFor(async () => expect(await rowOf(/app\.example\.com/)).toHaveTextContent("hz-1"));
  });

  it("says where an app is when environments show", async () => {
    renderAt(
      "/domains",
      api([aDomain({ name: "app.example.com", certificate: issued })], {
        more: { resources: places("staging") },
      }),
    );
    await waitFor(async () => expect(await rowOf(/app\.example\.com/)).toHaveTextContent("production"));
  });
});

describe("DNS and certificates, as Domains-List has them", () => {
  /** A domain whose one server says what its certificate and DNS are. */
  const said = (name: string, cert: ReturnType<typeof certificate>, dns?: object) =>
    aDomain({ name, server: { status: { domain: name, certificate: cert, ...(dns ? { dns } : {}) } } });

  it("says where each name's DNS leads, with the addresses it found", async () => {
    renderAt(
      "/domains",
      api([
        said("here.example.com", certificate(CertificateState.ISSUED, "", 61), {
          state: DomainCheckState.POINTS_HERE,
          found: ["198.51.100.7"],
        }),
        said("proxied.example.com", certificate(CertificateState.NONE), {
          state: DomainCheckState.ELSEWHERE,
          found: ["203.0.113.10"],
          proxied: true,
        }),
        said("new.example.com", certificate(CertificateState.PENDING), { state: DomainCheckState.NO_RECORD }),
        said("away.example.com", certificate(CertificateState.PENDING), {
          state: DomainCheckState.ELSEWHERE,
          found: ["203.0.113.9"],
        }),
      ]),
    );
    const table = await screen.findByRole("table", { name: "Domains" });
    expect(within(table).getByRole("columnheader", { name: "DNS" })).toBeVisible();
    expect(await rowOf(/here\.example\.com/)).toHaveTextContent("Points here198.51.100.7");
    expect(await rowOf(/proxied\.example\.com/)).toHaveTextContent("Proxied by Cloudflare203.0.113.10");
    expect(await rowOf(/new\.example\.com/)).toHaveTextContent("No record yet");
    expect(await rowOf(/away\.example\.com/)).toHaveTextContent("Points elsewhere203.0.113.9");
  });

  it("says beside the list how the certificates stand, and which runs out first", async () => {
    renderAt(
      "/domains",
      api([
        said("soon.example.com", certificate(CertificateState.ISSUED, "", 2)),
        said("late.example.com", certificate(CertificateState.ISSUED, "", 61)),
        said("new.example.com", certificate(CertificateState.PENDING)),
      ]),
    );
    const panel = await screen.findByRole("region", { name: "Certificates" });
    expect(panel).toHaveTextContent("Renewed automatically");
    expect(panel).toHaveTextContent("Runs out firstsoon.example.com in 2 days");
    expect(panel).toHaveTextContent("Valid2");
    expect(panel).toHaveTextContent("Issuing1");
    expect(panel).not.toHaveTextContent("Failed");
  });

  it("counts every domain once, whatever the filters show, so the panel stays put", async () => {
    const user = userEvent.setup();
    renderAt(
      "/domains",
      api([
        said("soon.example.com", certificate(CertificateState.ISSUED, "", 2)),
        said("new.example.com", certificate(CertificateState.PENDING)),
        aDomain({
          name: "far.example.com",
          certificate: certificate(CertificateState.ISSUED, "", 61),
          servers: [{ serverId: "srv_2", serverName: "hz-2", stale: true }],
        }),
      ]),
    );
    const panel = await screen.findByRole("region", { name: "Certificates" });
    const stands = () => {
      expect(panel).toHaveTextContent("Valid1");
      expect(panel).toHaveTextContent("Issuing1");
      // Nobody can say how a domain stands whose server isn't connected, and it still counts.
      expect(panel).toHaveTextContent("Not connected1");
    };
    stands();
    await user.type(screen.getByRole("searchbox", { name: "Filter domains" }), "nothing-like-this");
    expect(await screen.findByText("Nothing matches")).toBeVisible();
    expect(screen.getByRole("region", { name: "Certificates" })).toBe(panel);
    stands();
  });

  it("takes the DNS a name has on each of its servers at its worst", async () => {
    const { dnsOf } = await import("./dns");
    const on = (state: DomainCheckState, proxied = false) => ({ status: { dns: { state, proxied, found: [] } } });
    const domain = aDomain({
      name: "two.example.com",
      servers: [on(DomainCheckState.POINTS_HERE), on(DomainCheckState.ELSEWHERE)],
    });
    expect(dnsOf(domain as never)?.state).toBe(DomainCheckState.ELSEWHERE);
  });
});

describe("filtering the list of domains", () => {
  const domains = [
    aDomain({ name: "shop.example.com", certificate: issued }),
    aDomain({ name: "bad.example.com", service: "api", certificate: certificate(CertificateState.FAILED, noRecord) }),
    aDomain({ name: "new.example.com", certificate: certificate(CertificateState.PENDING) }),
    aDomain({ name: "tun.example.com", certificate: certificate(CertificateState.NONE), route: { tls: "off" } }),
    aDomain({
      name: "far.example.com",
      certificate: issued,
      servers: [{ serverId: "srv_2", serverName: "hz-2", stale: true }],
    }),
  ];
  const shown = () =>
    screen
      .getAllByRole("row")
      .slice(1)
      .map((row) => row.querySelector(".domains-at")?.textContent);

  it("narrows by a domain's name, its app or its server, and keeps it in the URL", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/domains", api(domains, { services: [web, api_], servers: [hz1, hz2] }));
    const filter = await screen.findByRole("searchbox", { name: "Filter domains" });
    await screen.findByRole("row", { name: /shop\.example\.com/ });

    await user.type(filter, "shop");
    await waitFor(() => expect(shown()).toEqual(["shop.example.com"]));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ q: "shop" }));

    await user.clear(filter);
    await user.type(filter, "api");
    await waitFor(() => expect(shown()).toEqual(["bad.example.com"]));

    await user.clear(filter);
    await user.type(filter, "HZ-2");
    await waitFor(() => expect(shown()).toEqual(["far.example.com"]));
  });

  it("narrows by the state a row is in, and keeps it in the URL", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/domains", api(domains));
    const state = await screen.findByRole("combobox", { name: "State" });
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    for (const [option, name] of [
      ["Failing", "bad.example.com"],
      ["Waiting", "new.example.com"],
      ["Serving", "shop.example.com"],
      ["Behind a tunnel or a proxy", "tun.example.com"],
      ["Not connected", "far.example.com"],
    ] as const) {
      await user.selectOptions(state, option);
      await waitFor(() => expect(shown()).toEqual([name]));
    }
    expect(router.state.location.search).toMatchObject({ state: "not-connected" });
  });

  it("lists what answers anyone over plain HTTP as serving, not behind a tunnel or a proxy", async () => {
    const plain = aDomain({
      name: "plain.example.com",
      certificate: certificate(CertificateState.NONE),
      route: { tls: "off", plainForAnyone: true },
    });
    const { unmount } = renderAt("/domains?state=tunnel", api([...domains, plain]));
    await rowOf(/tun\.example\.com/);
    expect(screen.queryByRole("row", { name: /plain\.example\.com/ })).toBeNull();
    unmount();

    renderAt("/domains?state=serving", api([...domains, plain]));
    expect(await rowOf(/plain\.example\.com/)).toHaveTextContent("Plain HTTP");
    expect(shown()).toEqual(["shop.example.com", "plain.example.com"]);
  });

  it("lists a server that has gone quiet under its own word, not as waiting", async () => {
    const quiet = aDomain({ name: "quiet.example.com" });
    const connected = aServer({ id: SERVER, name: "hz-1", connectTime: longAgo });
    const more = { resources: withRoutes([{ id: "rte_quiet-example-com", updateTime: longAgo }]) };
    const { unmount } = renderAt("/domains?state=no-report", api([...domains, quiet], { servers: [connected], more }));
    expect(await rowOf(/quiet\.example\.com/)).toHaveTextContent("No report");
    expect(shown()).toEqual(["quiet.example.com"]);
    unmount();

    renderAt("/domains?state=waiting", api([...domains, quiet], { servers: [connected], more }));
    await rowOf(/new\.example\.com/);
    await waitFor(() => expect(shown()).toEqual(["new.example.com"]));
  });

  it("narrows by server, only once there's a choice of them", async () => {
    const user = userEvent.setup();
    const { unmount } = renderAt("/domains", api(domains));
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    expect(screen.queryByRole("combobox", { name: "Server" })).toBeNull();
    unmount();

    renderAt("/domains", api(domains, { servers: [hz1, hz2] }));
    const server = await screen.findByRole("combobox", { name: "Server" });
    await waitFor(() => expect(within(server).getByRole("option", { name: "hz-2" })).toBeInTheDocument());
    await user.selectOptions(server, "hz-2");
    await waitFor(() => expect(shown()).toEqual(["far.example.com"]));
  });

  it("starts from what a link says, and leaves out a state it doesn't know", async () => {
    const { unmount } = renderAt("/domains?state=failing&q=bad", api(domains));
    await screen.findByRole("row", { name: /bad\.example\.com/ });
    expect(screen.getByRole("searchbox", { name: "Filter domains" })).toHaveValue("bad");
    expect(screen.getByRole("combobox", { name: "State" })).toHaveValue("failing");
    unmount();

    renderAt("/domains?state=healthy", api(domains));
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    expect(screen.getByRole("combobox", { name: "State" })).toHaveValue("");
    expect(shown()).toHaveLength(5);
  });

  it("says when nothing matches, and offers the way back", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/domains?q=nothing-here", api(domains));
    expect(await screen.findByRole("heading", { name: "Nothing matches" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Clear the filters" }));
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    await waitFor(() => expect(router.state.location.search).toEqual({}));
  });
});

describe("how long what the list says stands", () => {
  /** A control plane that answers until told not to, or hangs, as a frozen one does. */
  function answering(first: ReturnType<typeof aDomain>[]) {
    const state = { domains: first, answering: true, hangs: false, reads: 0 };
    const listDomains = async (_req: unknown, ctx: { signal: AbortSignal }) => {
      state.reads += 1;
      if (state.hangs) {
        await untilAborted(ctx.signal);
        throw new ConnectError("the call was cancelled", Code.DeadlineExceeded);
      }
      if (!state.answering) throw new ConnectError("the control plane is down", Code.Unavailable);
      return { domains: state.domains };
    };
    return { state, listDomains };
  }

  const domains = [
    aDomain({ name: "good.example.com", certificate: issued }),
    aDomain({ name: "bad.example.com", certificate: certificate(CertificateState.FAILED, noRecord) }),
  ];

  /** Answers at once until told to hold, then keeps each read until let go. */
  function holding(first: ReturnType<typeof aDomain>[]) {
    const state = { hold: false, held: [] as (() => void)[] };
    const listDomains = async (_req: unknown, ctx: { signal: AbortSignal }) => {
      if (state.hold) {
        await new Promise<void>((done) => {
          state.held.push(done);
          void untilAborted(ctx.signal).then(done);
        });
      }
      if (ctx.signal.aborted) throw new ConnectError("the call was cancelled", Code.Canceled);
      return { domains: first };
    };
    return { state, listDomains };
  }

  it("says it's checking, coming back to the list, and doesn't say something failed", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = holding(domains);
    const { router } = renderAt("/domains", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Valid for 47 days");
    await act(() => router.navigate({ to: "/activity" }));
    await vi.advanceTimersByTimeAsync(35_000);
    read.state.hold = true;
    await act(() => router.navigate({ to: "/domains" }));
    await vi.advanceTimersByTimeAsync(300);
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Checking");
    expect(screen.queryByText(/can't reach the control plane/i)).toBeNull();
    for (const done of read.state.held) done();
    await waitFor(async () => expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Valid for 47 days"));
  });

  it("keeps its filter coming back, rather than list everything and then narrow", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = holding(domains);
    const { router } = renderAt(
      "/domains?state=failing",
      api([], { more: { domains: { listDomains: read.listDomains as never } } }),
    );
    await rowOf(/bad\.example\.com/);
    await act(() => router.navigate({ to: "/activity" }));
    await vi.advanceTimersByTimeAsync(35_000);
    read.state.hold = true;
    await act(() => router.navigate({ to: "/domains", search: { state: "failing" } }));
    await vi.advanceTimersByTimeAsync(300);
    await rowOf(/bad\.example\.com/);
    expect(screen.queryByRole("row", { name: /good\.example\.com/ })).toBeNull();
    expect(screen.getByRole("combobox", { name: "State" })).toBeEnabled();
    for (const done of read.state.held) done();
  });

  it("doesn't count a read called off by leaving as one that failed", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = holding(domains);
    const { router } = renderAt("/domains", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    await rowOf(/good\.example\.com/);
    // Away with a read under way, as a slow link makes likely.
    read.state.hold = true;
    await vi.advanceTimersByTimeAsync(11_000);
    await act(() => router.navigate({ to: "/activity" }));
    await vi.advanceTimersByTimeAsync(35_000);
    await act(() => router.navigate({ to: "/domains" }));
    await vi.advanceTimersByTimeAsync(300);
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Checking");
    expect(screen.queryByText(/can't reach the control plane/i)).toBeNull();
    for (const done of read.state.held) done();
  });

  it("still says it can't read, coming back, once that read hangs too", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = holding(domains);
    const { router } = renderAt("/domains", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    await rowOf(/good\.example\.com/);
    await act(() => router.navigate({ to: "/activity" }));
    await vi.advanceTimersByTimeAsync(35_000);
    read.state.hold = true;
    await act(() => router.navigate({ to: "/domains" }));
    await vi.advanceTimersByTimeAsync(9_000);
    await waitFor(async () => expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Unknown"));
    expect(screen.getByText(/can't reach the control plane/i)).toBeVisible();
  });

  it("says it's checking once, not each time, coming back to the same read", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = answering(domains);
    const { router } = renderAt("/domains", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    await rowOf(/good\.example\.com/);
    await act(() => router.navigate({ to: "/activity" }));
    await vi.advanceTimersByTimeAsync(35_000);
    // The last read back failed, so nobody here can say until one doesn't.
    read.state.answering = false;
    await act(() => router.navigate({ to: "/domains" }));
    await vi.advanceTimersByTimeAsync(1_000);
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Unknown");
    await act(() => router.navigate({ to: "/activity" }));
    await vi.advanceTimersByTimeAsync(5_000);
    await act(() => router.navigate({ to: "/domains" }));
    await vi.advanceTimersByTimeAsync(300);
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Unknown");
  });

  it("keeps reading once every domain serves, so one that stops says so", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = answering([aDomain({ name: "good.example.com", certificate: issued })]);
    renderAt("/domains", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Valid for 47 days");
    read.state.domains = [
      aDomain({ name: "good.example.com", certificate: certificate(CertificateState.FAILED, noRecord) }),
    ];
    await vi.advanceTimersByTimeAsync(11_000);
    await waitFor(async () => expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Failed"));
    expect(document.querySelector(".page-lead")).toHaveTextContent("1 domain · 1 needs a look");
  });

  it("reads again as a server goes, and doesn't wait for the next read to say so", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    const read = answering([aDomain({ name: "good.example.com", certificate: issued })]);
    renderAt(
      "/domains",
      api([], {
        more: { domains: { listDomains: read.listDomains as never }, servers: { watchServers: watch.stream } },
      }),
    );
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Valid for 47 days");
    read.state.domains = [aDomain({ name: "good.example.com", certificate: issued, server: { stale: true } })];
    act(() => watch.push(changed(aServer({ id: SERVER, name: "hz-1", connection: Connection.UNREACHABLE }))));
    await waitFor(async () => expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Not connected"));
  });

  it("reads Unknown once the panel can't read, says why once, and counts nothing as needing a look", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = answering(domains);
    const { container } = renderAt(
      "/domains",
      api([], { more: { domains: { listDomains: read.listDomains as never } } }),
    );
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Valid for 47 days");
    expect(container.querySelector(".page-lead")).toHaveTextContent("2 domains · 1 needs a look");

    read.state.answering = false;
    await vi.advanceTimersByTimeAsync(35_000);

    // What's there holds; how it stands doesn't.
    const good = await rowOf(/good\.example\.com/);
    await waitFor(() => expect(good).toHaveTextContent("Unknown"));
    expect(good).not.toHaveTextContent("Valid");
    expect(within(good).queryByRole("link", { name: /^Open / })).toBeNull();
    expect(good.querySelector(".dw-tone-healthy")).toBeNull();
    const bad = await rowOf(/bad\.example\.com/);
    expect(bad).toHaveTextContent("Unknown");
    expect(bad).not.toHaveTextContent("Failed");
    expect(bad).not.toHaveTextContent("No DNS record");

    expect(container.querySelector(".page-lead")).toHaveTextContent("2 domains");
    expect(container.querySelector(".page-lead")).not.toHaveTextContent("look");
    expect(screen.getAllByText(/Can't reach the control plane, so your domains read Unknown/)).toHaveLength(1);
    expect(screen.getByRole("combobox", { name: "State" })).toBeDisabled();

    // Back again, it says where they stand with no help.
    read.state.answering = true;
    await vi.advanceTimersByTimeAsync(11_000);
    await waitFor(async () => expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Valid for 47 days"));
    expect(screen.queryByText(/your domains read Unknown/)).toBeNull();
    expect(container.querySelector(".page-lead")).toHaveTextContent("2 domains · 1 needs a look");
  });

  it("gives way to Unknown when a read hangs", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = answering(domains);
    renderAt("/domains", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Valid for 47 days");
    read.state.hangs = true;
    await vi.advanceTimersByTimeAsync(40_000);
    await waitFor(async () => expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Unknown"));
  });

  it("doesn't narrow by a state nobody can say", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = answering(domains);
    renderAt("/domains?state=failing", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    await rowOf(/bad\.example\.com/);
    expect(screen.queryByRole("row", { name: /good\.example\.com/ })).toBeNull();
    read.state.answering = false;
    await vi.advanceTimersByTimeAsync(35_000);
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Unknown");
    expect(screen.queryByRole("heading", { name: "Nothing matches" })).toBeNull();
  });

  it("keeps a filter through the moment a read back takes, as a screen back in view has", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = answering(domains);
    renderAt("/domains?state=failing", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    await rowOf(/bad\.example\.com/);
    // Nothing reads while the screen is away, so what it read is old when it's back.
    focusManager.setFocused(false);
    await vi.advanceTimersByTimeAsync(31_000);
    read.state.hangs = true;
    focusManager.setFocused(true);
    window.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(500);
    await waitFor(async () => expect(await rowOf(/bad\.example\.com/)).toHaveTextContent("Checking"));
    expect(screen.queryByRole("row", { name: /good\.example\.com/ })).toBeNull();
    expect(screen.getByRole("combobox", { name: "State" })).toBeEnabled();
  });

  it("says so in the browser's words when it's the browser that went offline", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const read = answering(domains);
    renderAt("/domains", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    await rowOf(/good\.example\.com/);
    read.state.answering = false;
    onlineManager.setOnline(false);
    await vi.advanceTimersByTimeAsync(35_000);
    expect(await screen.findByText(/This browser is offline, so your domains read Unknown/)).toBeVisible();
  });

  it("holds the list's place while the first read is on its way, and never calls it failed", async () => {
    // Even for a frame, as a read that hasn't begun isn't one that can't be done.
    let failed = false;
    const watching = new MutationObserver(() => {
      failed ||= document.querySelector(".page")?.textContent?.includes("Can't load the domains") ?? false;
    });
    watching.observe(document.body, { childList: true, subtree: true, characterData: true });
    const { container } = renderAt(
      "/domains",
      api([], { more: { domains: { listDomains: () => new Promise(() => {}) } } }),
    );
    expect(await screen.findByText("Loading domains")).toBeInTheDocument();
    watching.disconnect();
    expect(failed).toBe(false);
    expect(container.querySelector(".page-lead")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add a domain" })).toBeNull();
  });

  it("says it can't load the list, and offers to try again, when it never could", async () => {
    const user = userEvent.setup();
    const read = answering(domains);
    read.state.answering = false;
    renderAt("/domains", api([], { more: { domains: { listDomains: read.listDomains as never } } }));
    const failed = await screen.findByRole("alert");
    expect(failed).toHaveTextContent("Can't load the domains");
    expect(failed).toHaveTextContent("The control plane is down.");
    read.state.answering = true;
    await user.click(within(failed).getByRole("button", { name: "Try again" }));
    expect(await rowOf(/good\.example\.com/)).toHaveTextContent("Valid for 47 days");
  });
});

describe("what needs a look comes first", () => {
  /** A list of names, in the API's order, of which those given are failing. */
  const list = (count: number, failing: readonly number[]) =>
    Array.from({ length: count }, (_, n) =>
      aDomain({
        name: `site-${String(n).padStart(3, "0")}.example.com`,
        certificate: failing.includes(n) ? certificate(CertificateState.FAILED, noRecord) : issued,
      }),
    );

  it("brings what needs a look forward from anywhere in a long list", async () => {
    renderAt("/domains", api(list(220, [5, 90, 219])));
    await rowOf(/site-219\.example\.com/);
    expect(namesShown().slice(0, 4)).toEqual([
      "site-005.example.com",
      "site-090.example.com",
      "site-219.example.com",
      "site-000.example.com",
    ]);
  });

  it("puts what needs a look, then what's on its way, then the rest", async () => {
    renderAt(
      "/domains",
      api([
        aDomain({ name: "a-fine.example.com", certificate: issued }),
        aDomain({ name: "b-new.example.com", certificate: certificate(CertificateState.PENDING) }),
        aDomain({
          name: "c-tunnel.example.com",
          certificate: certificate(CertificateState.NONE),
          route: { tls: "off" },
        }),
        aDomain({ name: "d-gone.example.com", certificate: issued, server: { stale: true } }),
        aDomain({ name: "e-bad.example.com", certificate: certificate(CertificateState.FAILED, noRecord) }),
      ]),
    );
    await rowOf(/e-bad\.example\.com/);
    expect(namesShown()).toEqual([
      "d-gone.example.com",
      "e-bad.example.com",
      "b-new.example.com",
      "a-fine.example.com",
      "c-tunnel.example.com",
    ]);
  });

  it("holds each in its place as things change, and offers to bring up what went wrong", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    let domains = list(4, [2]);
    const { container } = renderAt("/domains", api([], { more: { domains: { listDomains: () => ({ domains }) } } }));
    await rowOf(/site-002\.example\.com/);
    const order = ["site-002", "site-000", "site-001", "site-003"].map((n) => `${n}.example.com`);
    expect(namesShown()).toEqual(order);
    expect(screen.queryByRole("button", { name: "Show them first" })).toBeNull();

    // Another goes wrong, and the first comes right, while someone's looking.
    domains = list(4, [0, 3]);
    await vi.advanceTimersByTimeAsync(11_000);
    await waitFor(async () => expect(await rowOf(/site-003/)).toHaveTextContent("Failed"));
    expect(namesShown()).toEqual(order);
    const first = screen.getByRole("button", { name: "Show them first" });
    await waitFor(() =>
      expect(container.querySelector('.apps-section [aria-live="polite"]')).toHaveTextContent(
        "2 need a look. Show them first puts them at the top of the list.",
      ),
    );

    await user.click(first);
    await waitFor(() =>
      expect(namesShown()).toEqual(["site-000", "site-003", "site-001", "site-002"].map((n) => `${n}.example.com`)),
    );
    expect(screen.queryByRole("button", { name: "Show them first" })).toBeNull();
  });

  it("puts a domain that's new where you'll see it, moving nothing else", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let domains = list(3, []);
    renderAt("/domains", api([], { more: { domains: { listDomains: () => ({ domains }) } } }));
    await rowOf(/site-002\.example\.com/);
    domains = [...list(3, []), aDomain({ name: "zz-new.example.com", certificate: issued })];
    await vi.advanceTimersByTimeAsync(11_000);
    await rowOf(/zz-new\.example\.com/);
    expect(namesShown()).toEqual([
      "zz-new.example.com",
      "site-000.example.com",
      "site-001.example.com",
      "site-002.example.com",
    ]);
  });

  it("holds its order while nothing can be read, and offers nothing it can't back", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let fails = false;
    const domains = list(3, [2]);
    renderAt(
      "/domains",
      api([], {
        more: {
          domains: {
            listDomains: () => {
              if (fails) throw new ConnectError("the control plane is down", Code.Unavailable);
              return { domains };
            },
          },
        },
      }),
    );
    await rowOf(/site-002\.example\.com/);
    const order = namesShown();
    fails = true;
    await vi.advanceTimersByTimeAsync(35_000);
    await waitFor(async () => expect(await rowOf(/site-002/)).toHaveTextContent("Unknown"));
    expect(namesShown()).toEqual(order);
    expect(screen.queryByRole("button", { name: "Show them first" })).toBeNull();
  });

  it("goes by a server that went quiet too, as that needs a look", async () => {
    const connected = aServer({ id: SERVER, name: "hz-1", connectTime: longAgo });
    renderAt(
      "/domains",
      api([aDomain({ name: "a.example.com", certificate: issued }), aDomain({ name: "quiet.example.com" })], {
        servers: [connected],
        more: { resources: withRoutes([{ id: "rte_quiet-example-com", updateTime: longAgo }]) },
      }),
    );
    await waitFor(() => expect(namesShown()).toEqual(["quiet.example.com", "a.example.com"]));
  });
});

describe("adding a domain from the list", () => {
  const changes = (edits: ProposeChangeRequest["edits"]): Api["changes"] => ({
    watchChange: feed<WatchChangeResponse>().stream,
    discardChange: () => ({}),
    proposeChange: (req) => {
      edits.push(...req.edits);
      return { change: webPlanned() };
    },
  });
  const onTwo = (service: string) =>
    anApp(
      service,
      "nginx:1.5",
      {},
      {
        deployments: [
          { serverId: SERVER, serverName: "hz-1" },
          { serverId: "srv_2", serverName: "hz-2" },
        ],
      },
    );
  const some = [aDomain({ name: "shop.example.com", certificate: issued })];

  it("picks the port from the app's own, and lands on the app once its domain is added", async () => {
    const user = userEvent.setup();
    const edits: ProposeChangeRequest["edits"] = [];
    const watch = feed<WatchChangeResponse>();
    const { router } = renderAt(
      "/domains",
      api(some, {
        services: [web, api_],
        more: {
          changes: {
            ...changes(edits),
            watchChange: watch.stream,
            applyChange: () => ({ change: moved(webPlanned(), { state: ChangeState.APPLYING }) }),
          },
        },
      }),
    );
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain" });
    await user.selectOptions(within(sheet).getByRole("combobox", { name: "App" }), "api");
    await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
    // The port is picked from what the app listens on, with another one a choice away.
    const port = within(sheet).getByRole("combobox", { name: "Port in the app" });
    expect(port).toHaveValue("3000");
    expect(
      within(port)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["3000", "Another port"]);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits[0]?.spec?.kind.value).toMatchObject({ domain: "app.example.com", service: "api", port: 3000 });
    await user.click(within(plan).getByRole("button", { name: /Apply plan/ }));
    act(() => watch.push(watched(moved(webPlanned(), { state: ChangeState.APPLIED }))));
    await user.click(await screen.findByRole("button", { name: /^Done/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe(`/apps/${api_.id}`));
  });

  it("asks for the app, once there's a choice, and refuses to guess", async () => {
    const user = userEvent.setup();
    const edits: ProposeChangeRequest["edits"] = [];
    renderAt("/domains", api(some, { services: [web, api_], more: { changes: changes(edits) } }));
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain" });

    const app = within(sheet).getByRole("combobox", { name: "App" });
    expect(app).toHaveValue("");
    expect(
      within(app)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Choose an app", "api", "web"]);

    await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(app).toHaveAccessibleDescription("Choose the app this domain reaches.");
    await waitFor(() => expect(app).toHaveFocus());
    expect(edits).toHaveLength(0);

    await user.selectOptions(app, "api");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits[0]?.resource).toMatchObject({ kind: "route", scope: ENV, name: "app-example-com" });
    expect(edits[0]?.spec?.kind.value).toMatchObject({ domain: "app.example.com", service: "api", port: 3000 });
  });

  it("starts from what the app it's for would, until you say otherwise", async () => {
    const user = userEvent.setup();
    const onServer = anApp("host", "nginx:1.5", { hostNetwork: true });
    renderAt("/domains", api(some, { services: [web, api_, onServer] }));
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain" });
    const app = within(sheet).getByRole("combobox", { name: "App" });

    await user.selectOptions(app, "api");
    expect(within(sheet).getByLabelText("Port in the app")).toHaveValue("3000");
    await user.selectOptions(app, "web");
    expect(within(sheet).getByLabelText("Port in the app")).toHaveValue("80");
    await user.selectOptions(app, "host");
    expect(within(sheet).getByLabelText("Port on the server")).toHaveValue("");

    await user.type(within(sheet).getByLabelText("Port on the server"), "9000");
    await user.selectOptions(app, "api");
    expect(within(sheet).getByLabelText("Port in the app")).toHaveValue("9000");
  });

  it("offers the servers of the app you chose", async () => {
    const user = userEvent.setup();
    const edits: ProposeChangeRequest["edits"] = [];
    const both = onTwo("both");
    renderAt("/domains", api(some, { services: [web, both], more: { changes: changes(edits) } }));
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain" });
    expect(within(sheet).queryByRole("combobox", { name: "Server" })).toBeNull();

    await user.selectOptions(within(sheet).getByRole("combobox", { name: "App" }), "both");
    await user.selectOptions(within(sheet).getByRole("combobox", { name: "Server" }), "hz-2");
    await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits[0]?.servers).toEqual(["srv_2"]);
  });

  it("names apps by their environment where the same name is in two", async () => {
    const user = userEvent.setup();
    const staging = anApp(
      "web",
      "nginx:1.5",
      {},
      { id: "svc_web_staging", resource: { kind: "service", scope: "env_stage", name: "web" } },
    );
    const edits: ProposeChangeRequest["edits"] = [];
    renderAt(
      "/domains",
      api(some, {
        services: [web, staging],
        more: { changes: changes(edits), resources: projectPlaces("prod", "stage") },
      }),
    );
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain" });
    const app = within(sheet).getByRole("combobox", { name: "App" });
    await waitFor(() =>
      expect(
        within(app)
          .getAllByRole("option")
          .map((o) => o.textContent),
      ).toEqual(["Choose an app", "web (prod)", "web (stage)"]),
    );
    await user.selectOptions(app, "web (stage)");
    await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits[0]?.resource).toMatchObject({ scope: "env_stage" });
  });

  it("needs no choice with one app", async () => {
    const user = userEvent.setup();
    renderAt("/domains", api(some));
    await screen.findByRole("row", { name: /shop\.example\.com/ });
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain to web" });
    expect(within(sheet).queryByRole("combobox", { name: "App" })).toBeNull();
    expect(within(sheet).getByLabelText("Port in the app")).toHaveValue("80");
  });

  it("invites the first domain, with one primary button", async () => {
    const { container } = renderAt("/domains", api([]));
    expect(await screen.findByRole("heading", { name: "No domains yet" })).toBeVisible();
    await screen.findByRole("button", { name: "Add a domain" });
    expect(screen.getAllByRole("button", { name: "Add a domain" })).toHaveLength(1);
    expect(screen.queryByRole("searchbox", { name: "Filter domains" })).toBeNull();
    await expectAccessible(container);
  });

  it("asks for an app first when there's none to send a domain to", async () => {
    renderAt("/domains", api([], { services: [] }));
    expect(await screen.findByRole("heading", { name: "No domains yet" })).toBeVisible();
    expect(await screen.findByRole("link", { name: "New app" })).toHaveAttribute("href", "/apps/new");
    expect(screen.queryByRole("button", { name: "Add a domain" })).toBeNull();
  });

  it("doesn't say there are no apps when it couldn't read them", async () => {
    renderAt(
      "/domains",
      api([], {
        more: {
          services: {
            listServices: () => {
              throw new ConnectError("the control plane is down", Code.Unavailable);
            },
          },
        },
      }),
    );
    expect(await screen.findByText("Can't read your apps")).toBeVisible();
    expect(screen.getByRole("heading", { name: "No domains yet" })).toBeVisible();
    expect(screen.queryByText(/run one first/)).toBeNull();
    expect(screen.queryByRole("link", { name: "New app" })).toBeNull();
  });

  it("says when it can't read the apps, and goes on listing the domains", async () => {
    let answers = false;
    const user = userEvent.setup();
    renderAt(
      "/domains",
      api(some, {
        more: {
          services: {
            listServices: () => {
              if (!answers) throw new ConnectError("the control plane is down", Code.Unavailable);
              return { services: [web] };
            },
          },
        },
      }),
    );
    const row = await rowOf(/shop\.example\.com/);
    expect(await screen.findByText("Can't read your apps")).toBeVisible();
    expect(within(row).queryByRole("link", { name: "web" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add a domain" })).toBeNull();
    answers = true;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await within(await rowOf(/shop\.example\.com/)).findByRole("link", { name: "web" })).toBeVisible();
  });
});

describe("the list of domains on a phone", () => {
  it("names each row's environment, and its servers, so two apps of a name read apart", async () => {
    onPhone();
    const staging = anApp(
      "web",
      "nginx:1.5",
      {},
      { id: "svc_web_staging", resource: { kind: "service", scope: "env_staging", name: "web" } },
    );
    renderAt(
      "/domains",
      api(
        [
          aDomain({ name: "prod.example.com", certificate: issued }),
          aDomain({ name: "staging.example.com", certificate: issued, environment: "env_staging" }),
        ],
        { services: [web, staging], servers: [hz1, hz2], more: { resources: places("staging") } },
      ),
    );
    const prod = await screen.findByRole("link", { name: /^prod\.example\.com/ });
    await waitFor(() => expect(prod).toHaveTextContent("hz-1 · production"));
    expect(screen.getByRole("link", { name: /^staging\.example\.com/ })).toHaveTextContent("hz-1 · staging");
  });

  it("says where each name's DNS leads under what it reaches, and nothing before a server has read it", async () => {
    onPhone();
    const on = (name: string, dns?: object) =>
      aDomain({ name, server: { status: { domain: name, certificate: issued, ...(dns ? { dns } : {}) } } });
    renderAt(
      "/domains",
      api([
        on("here.example.com", { state: DomainCheckState.POINTS_HERE, found: ["198.51.100.7"] }),
        on("away.example.com", { state: DomainCheckState.ELSEWHERE, found: ["203.0.113.9"] }),
        on("unread.example.com"),
      ]),
    );
    expect(await screen.findByRole("link", { name: /^here\.example\.com/ })).toHaveTextContent(
      "Points here · 198.51.100.7",
    );
    expect(screen.getByRole("link", { name: /^away\.example\.com/ })).toHaveTextContent(
      "Points elsewhere · 203.0.113.9",
    );
    expect(screen.getByRole("link", { name: /^unread\.example\.com/ })).not.toHaveTextContent("Not read yet");
  });

  it("clips neither what a row reaches nor where", async () => {
    onPhone();
    const name = "a-service-with-a-name-long-enough-to-wrap-on-a-phone";
    const long = anApp(name, "nginx:1.5");
    const { container } = renderAt(
      "/domains",
      api([aDomain({ name: "app.example.com", service: name, certificate: issued })], { services: [long] }),
    );
    await screen.findByRole("link", { name: /^app\.example\.com/ });
    expect(container.querySelector(".domains-phone-target")).toHaveTextContent(name);
    // That class is the one that clips on an app's row.
    expect(container.querySelector(".domains-phone .apps-phone-image")).toBeNull();
  });

  it("stacks each row, leads to the app, and keeps the note's own link out of the row's", async () => {
    onPhone();
    const { container } = renderAt(
      "/domains",
      api([
        aDomain({ name: "good.example.com", certificate: issued }),
        aDomain({ name: "bad.example.com", certificate: certificate(CertificateState.FAILED, noRecord) }),
        aDomain({ name: "gone.example.com", certificate: issued, server: { stale: true } }),
      ]),
    );
    const row = await screen.findByRole("link", { name: /^good\.example\.com/ });
    expect(row).toHaveAttribute("href", "/apps/svc_web");
    expect(row).toHaveTextContent("Valid for 47 days");
    expect(screen.getByRole("link", { name: "Open good.example.com" })).toHaveAttribute(
      "href",
      "https://good.example.com",
    );
    expect(screen.getByText(noRecord)).toBeVisible();
    const check = screen.getByRole("link", { name: "Check its page" });
    expect(check.parentElement?.closest("a")).toBeNull();
    // A phone's back, its way to the form, and how many, where the title's line would be.
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/more");
    expect(screen.getByRole("button", { name: "Add a domain" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("3 domains");
    await expectAccessible(container);
  });
});
