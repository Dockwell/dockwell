// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type CheckDomainRequest,
  type CloudflareCheckSchema,
  CloudflareName,
  type ProposeChangeRequest,
  Role,
  RouteWay,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, places, SERVER } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { webPlanned } from "../../test/changes";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: ["203.0.113.10"] } });
const wordpress = anApp("wordpress", "wordpress:6.8-apache", { ports: [{ hostPort: 8085, containerPort: 80 }] });
const TUNNEL = "3f2d1c0b-4a5b-4c6d-8e7f-9a0b1c2d3e4f";

type Check = MessageInitShape<typeof CloudflareCheckSchema>;

/** The ways a server with a tunnel Dockwell can use offers, with the zone's rule to add. */
const throughWebOne: Check["ways"] = [
  { way: RouteWay.SERVER_TUNNEL, tunnel: TUNNEL, tunnelName: "web-1", addsRule: "*.example.com" },
  { way: RouteWay.PROXY },
  { way: RouteWay.OWN_HTTPS, off: "Port 443 on hz-1 is held by nginx" },
  { way: RouteWay.FRONT },
];

const free = (more: Check = {}): Check => ({
  zone: "example.com",
  provider: "cloudflare",
  state: CloudflareName.FREE,
  way: RouteWay.SERVER_TUNNEL,
  ways: throughWebOne,
  sslMode: "full",
  ...more,
});

/** An app's page on hz-1, with a Cloudflare provider writing example.com and example.org for its project. */
function publishApi(
  cloudflare: (req: CheckDomainRequest) => Check | undefined,
  edits: ProposeChangeRequest["edits"],
  asked: CheckDomainRequest[] = [],
  companion = "",
): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  return {
    resources: places(),
    services: { listServices: () => ({ services: [wordpress] }), listServiceRevisions: () => ({ revisions: [] }) },
    servers: { watchServers: watch.stream },
    domains: {
      listDomains: () => ({ domains: [] }),
      listDnsProviders: () => ({
        providers: [
          {
            id: "dnp_1",
            name: "cloudflare",
            provider: "cloudflare",
            zones: ["example.com", "example.org"],
            projects: ["prj_1"],
            strictZones: [],
          },
        ],
      }),
      checkDomain: (req) => {
        asked.push(req);
        const cf = cloudflare(req);
        return { companion: req.domain === "example.com" ? companion : "", ...(cf ? { cloudflare: cf } : {}) };
      },
    },
    changes: {
      watchChange: feed<WatchChangeResponse>().stream,
      discardChange: () => ({}),
      proposeChange: (req) => {
        edits.push(...req.edits);
        return { change: webPlanned() };
      },
    },
  };
}

async function openSheet() {
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Add a domain" }));
  const sheet = await screen.findByRole("dialog", { name: "Add a domain to wordpress" });
  return { user, sheet };
}

describe("adding a domain in a connected zone (ADR 0104)", () => {
  it("picks the name from a zone, checks it live, and publishes it through the server's tunnel", async () => {
    const edits: ProposeChangeRequest["edits"] = [];
    const asked: CheckDomainRequest[] = [];
    renderAt(
      "/apps/svc_wordpress",
      publishApi(() => free(), edits, asked),
    );
    const { user, sheet } = await openSheet();
    const zone = within(sheet).getByRole("combobox", { name: "Zone" });
    expect(zone).toHaveValue("example.com");
    await user.type(within(sheet).getByRole("textbox", { name: "Subdomain" }), "wiki");
    expect(await within(sheet).findByText("Free in Dockwell and at Cloudflare", {}, { timeout: 3000 })).toBeVisible();
    expect(asked.at(-1)).toMatchObject({ serverId: SERVER, domain: "wiki.example.com", projectId: "prj_1" });
    expect(sheet).toHaveTextContent("Through the tunnel web-1 on hz-1");
    expect(sheet).toHaveTextContent("Adds *.example.com to web-1, once");
    expect(sheet).toHaveTextContent("Reacheswordpress on port 80");
    // Nothing to point by hand: no record to make, and no switch for HTTPS.
    expect(sheet).not.toHaveTextContent(/Make this record|Point an A record/);
    expect(within(sheet).queryByRole("switch")).toBeNull();
    await expectAccessible(document.body);

    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits[0]?.spec?.kind.value).toMatchObject({
      domain: "wiki.example.com",
      service: "wordpress",
      port: 80,
      tls: "off",
      trustedProxies: ["127.0.0.1"],
      publish: { way: RouteWay.SERVER_TUNNEL, tunnel: TUNNEL, provider: "cloudflare" },
    });
  });

  it.each([true, false])("handles an unserved zone in the shared app sheet (admin %s)", async (admin) => {
    const edits: ProposeChangeRequest["edits"] = [];
    const asked: CheckDomainRequest[] = [];
    const api = publishApi(
      () => ({
        state: CloudflareName.OUTSIDE_PROJECT,
        ...(admin ? { provider: "cloudflare", zone: "dev.example.com" } : {}),
      }),
      edits,
      asked,
    );
    if (!admin) api.auth = { getAuthState: () => authState({ user: { ...mira, role: Role.DEVELOPER } }) };
    const base = api.domains;
    api.domains = {
      ...base,
      listDnsProviders: (req) => ({
        providers: [
          { id: "dnp_parent", name: "parent", zones: ["example.com"], everyProject: true },
          ...(req.projectId
            ? []
            : [
                {
                  id: "dnp_child",
                  name: "cloudflare",
                  zones: ["dev.example.com"],
                  provider: admin ? "cloudflare" : "",
                  revision: 7n,
                },
              ]),
        ],
      }),
    };
    renderAt("/apps/svc_wordpress", api);
    const { user, sheet } = await openSheet();
    const subdomain = await within(sheet).findByRole("textbox", { name: "Subdomain" });
    await user.click(subdomain);
    await user.paste("wiki.dev.example.com");
    const use = "Use cloudflare here";
    if (admin) {
      await user.click(await within(sheet).findByRole("button", { name: use }));
      await waitFor(() => expect(edits).toHaveLength(1));
      expect(edits[0]).toMatchObject({
        resource: { kind: "dns.provider", name: "cloudflare" },
        baseRevision: 7n,
        spec: { kind: { value: { projects: ["prj_1"], everyProject: false } } },
      });
    } else {
      expect(await within(sheet).findByText(/An admin can give it access/)).toBeVisible();
      expect(within(sheet).queryByRole("button", { name: use })).toBeNull();
      expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    }
    expect(asked.at(-1)).toMatchObject({ domain: "wiki.dev.example.com", projectId: "prj_1" });
    expect(sheet).not.toHaveTextContent(/Make this record|Point an A record/);
  });

  it("offers the other ways one step away, each that can't be taken saying why", async () => {
    const edits: ProposeChangeRequest["edits"] = [];
    renderAt(
      "/apps/svc_wordpress",
      publishApi(() => free(), edits),
    );
    const { user, sheet } = await openSheet();
    await user.type(within(sheet).getByRole("textbox", { name: "Subdomain" }), "wiki");
    await within(sheet).findByText("Through the tunnel web-1 on hz-1", {}, { timeout: 3000 });
    await user.click(within(sheet).getByRole("button", { name: "Change the way in" }));
    const ways = within(sheet).getByRole("radiogroup", { name: "Way in" });
    expect(within(ways).getByRole("radio", { name: /Through the tunnel web-1 on hz-1/ })).toBeChecked();
    const own = within(ways).getByRole("radio", { name: /This server's own HTTPS/ });
    expect(own).toBeDisabled();
    expect(ways).toHaveTextContent("Port 443 on hz-1 is held by nginx");
    await user.click(within(ways).getByRole("radio", { name: /Through Cloudflare's proxy/ }));
    expect(sheet).toHaveTextContent("A proxied record to hz-1, and a certificate by DNS");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    const route = edits[0]?.spec?.kind.value as { publish?: { tunnel?: string } };
    expect(route).toMatchObject({
      domain: "wiki.example.com",
      tls: "",
      proof: "dns",
      trustedProxies: ["cloudflare"],
      publish: { way: RouteWay.PROXY, provider: "cloudflare" },
    });
    expect(route.publish?.tunnel ?? "").toBe("");
  });

  it("replaces a record made in Cloudflare only once an admin says so", async () => {
    const edits: ProposeChangeRequest["edits"] = [];
    renderAt(
      "/apps/svc_wordpress",
      publishApi(() => free({ state: CloudflareName.TAKEN, record: "A 203.0.113.9", recordId: "rec_1" }), edits),
    );
    const { user, sheet } = await openSheet();
    await user.type(within(sheet).getByRole("textbox", { name: "Subdomain" }), "vpn");
    expect(
      await within(sheet).findByText(/A record exists: A 203.0.113.9, made in Cloudflare/, {}, { timeout: 3000 }),
    ).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Replace it" }));
    expect(sheet).toHaveTextContent("Replaces A 203.0.113.9, made in Cloudflare. Undo puts it back");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits[0]?.spec?.kind.value).toMatchObject({
      publish: { way: RouteWay.SERVER_TUNNEL, takesOver: [{ recordId: "rec_1" }] },
    });
  });

  it("offers a free name beside one that's taken, and one under Cloudflare's free certificate", async () => {
    renderAt(
      "/apps/svc_wordpress",
      publishApi(
        (req) =>
          req.domain === "vpn.example.com"
            ? free({ state: CloudflareName.TAKEN })
            : req.domain === "a.docs.example.com"
              ? free({ state: CloudflareName.PAST_CERTIFICATE })
              : free(),
        [],
      ),
    );
    const { user, sheet } = await openSheet();
    const sub = within(sheet).getByRole("textbox", { name: "Subdomain" });
    await user.type(sub, "vpn");
    expect(await within(sheet).findByText(/Taken at Cloudflare/, {}, { timeout: 3000 })).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Use vpn-2" }));
    expect(sub).toHaveValue("vpn-2");
    await user.clear(sub);
    await user.type(sub, "a.docs");
    expect(
      await within(sheet).findByText(/a.docs.example.com is past Cloudflare's free certificate/, {}, { timeout: 3000 }),
    ).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Use a-docs" }));
    expect(sub).toHaveValue("a-docs");
    // A whole name typed in another connected zone picks that zone.
    await user.clear(sub);
    await user.type(sub, "status.example.org");
    expect(within(sheet).getByRole("combobox", { name: "Zone" })).toHaveValue("example.org");
    expect(sub).toHaveValue("status");
  });

  it("says why a server's tunnel can't be used, and writes nothing at Cloudflare for what's in front", async () => {
    const edits: ProposeChangeRequest["edits"] = [];
    renderAt(
      "/apps/svc_wordpress",
      publishApi(
        () =>
          free({
            way: RouteWay.FRONT,
            ways: [
              {
                way: RouteWay.SERVER_TUNNEL,
                tunnel: TUNNEL,
                tunnelName: "web-1",
                off: "hz-1's tunnel is managed in a file, so Dockwell writes nothing at Cloudflare",
              },
              { way: RouteWay.PROXY },
              { way: RouteWay.OWN_HTTPS },
              { way: RouteWay.FRONT },
            ],
          }),
        edits,
      ),
    );
    const { user, sheet } = await openSheet();
    await user.type(within(sheet).getByRole("textbox", { name: "Subdomain" }), "wiki");
    expect(await within(sheet).findByText("Something else in front", {}, { timeout: 3000 })).toBeVisible();
    expect(sheet).toHaveTextContent("hz-1's tunnel is managed in a file, so Dockwell writes nothing at Cloudflare");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    const route = edits[0]?.spec?.kind.value as { publish?: unknown };
    expect(route).toMatchObject({ domain: "wiki.example.com", tls: "off", trustedProxies: ["127.0.0.1"] });
    expect(route.publish).toBeUndefined();
  });

  it("sets a zone on Flexible to Full (strict) on its provider's own plan, keeping what it serves", async () => {
    const edits: ProposeChangeRequest["edits"] = [];
    renderAt(
      "/apps/svc_wordpress",
      publishApi(
        () =>
          free({
            sslMode: "flexible",
            way: RouteWay.PROXY,
            ways: [{ way: RouteWay.PROXY }, { way: RouteWay.OWN_HTTPS }, { way: RouteWay.FRONT }],
          }),
        edits,
      ),
    );
    const { user, sheet } = await openSheet();
    await user.type(within(sheet).getByRole("textbox", { name: "Subdomain" }), "docs");
    expect(
      await within(sheet).findByText(
        /example.com is set to Flexible, so Cloudflare reaches hz-1 over HTTP/,
        {},
        {
          timeout: 3000,
        },
      ),
    ).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Use Full (strict)" }));
    await waitFor(() => expect(edits).toHaveLength(1));
    expect(edits[0]).toMatchObject({
      resource: { kind: "dns.provider", name: "cloudflare" },
      spec: {
        kind: {
          case: "dnsProvider",
          value: { zones: ["example.com", "example.org"], projects: ["prj_1"], strictZones: ["example.com"] },
        },
      },
    });
  });

  it("types another domain whole, as before, with nothing published", async () => {
    const edits: ProposeChangeRequest["edits"] = [];
    renderAt(
      "/apps/svc_wordpress",
      publishApi(() => undefined, edits),
    );
    const { user, sheet } = await openSheet();
    await user.selectOptions(within(sheet).getByRole("combobox", { name: "Zone" }), "");
    const field = within(sheet).getByRole("textbox", { name: "Domain" });
    await user.type(field, "shop.example.net");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    const route = edits[0]?.spec?.kind.value as { publish?: unknown };
    expect(route).toMatchObject({ domain: "shop.example.net", service: "wordpress" });
    expect(route.publish).toBeUndefined();
  });

  it("says a check Cloudflare couldn't answer, with Check again, and never plans a plain route", async () => {
    const edits: ProposeChangeRequest["edits"] = [];
    let answered = false;
    renderAt(
      "/apps/svc_wordpress",
      publishApi(() => (answered ? free() : { provider: "cloudflare" }), edits),
    );
    const { user, sheet } = await openSheet();
    await user.type(within(sheet).getByRole("textbox", { name: "Subdomain" }), "wiki");
    expect(await within(sheet).findByText(/Couldn't read Cloudflare/, {}, { timeout: 3000 })).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(edits).toHaveLength(0);
    answered = true;
    const state = within(sheet)
      .getByText(/Couldn't read Cloudflare/)
      .closest(".domain-step-state") as HTMLElement;
    await user.click(within(state).getByRole("button", { name: "Check again" }));
    expect(await within(sheet).findByText("Free in Dockwell and at Cloudflare", {}, { timeout: 3000 })).toBeVisible();
  });

  it("says why a check was refused, as the control plane words it", async () => {
    const edits: ProposeChangeRequest["edits"] = [];
    const api = publishApi(() => free(), edits);
    api.domains = {
      ...api.domains,
      checkDomain: () => {
        throw new ConnectError(
          "you've checked 60 domains in the last minute. Wait a minute, then check again",
          Code.ResourceExhausted,
        );
      },
    };
    renderAt("/apps/svc_wordpress", api);
    const { user, sheet } = await openSheet();
    await user.type(within(sheet).getByRole("textbox", { name: "Subdomain" }), "wiki");
    expect(
      await within(sheet).findByText(/checked 60 domains in the last minute/, {}, { timeout: 3000 }),
    ).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(edits).toHaveLength(0);
  });

  it("moves a tunnel rule made by hand to Dockwell, with an admin's consent", async () => {
    const edits: ProposeChangeRequest["edits"] = [];
    renderAt(
      "/apps/svc_wordpress",
      publishApi(
        () =>
          free({
            ways: throughWebOne.map((w) => ({ ...w, addsRule: "" })),
            handRule: {
              tunnel: TUNNEL,
              tunnelName: "web-1",
              hostname: "wiki.example.com",
              service: "http://127.0.0.1:8080",
              reachesProxy: false,
            },
          }),
        edits,
      ),
    );
    const { user, sheet } = await openSheet();
    await user.type(within(sheet).getByRole("textbox", { name: "Subdomain" }), "wiki");
    expect(
      await within(sheet).findByText(
        /A rule made in Cloudflare in web-1 sends it to http:\/\/127.0.0.1:8080/,
        {},
        { timeout: 3000 },
      ),
    ).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Move it to Dockwell" }));
    expect(sheet).toHaveTextContent(
      "Moves the rule made in Cloudflare in web-1 to the server's proxy. Undo puts it back",
    );
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits[0]?.spec?.kind.value).toMatchObject({
      publish: { takesOver: [{ tunnel: TUNNEL, hostname: "wiki.example.com" }] },
    });
  });

  it("closes the ways once one is picked, the one already picked too", async () => {
    renderAt(
      "/apps/svc_wordpress",
      publishApi(() => free(), []),
    );
    const { user, sheet } = await openSheet();
    await user.type(within(sheet).getByRole("textbox", { name: "Subdomain" }), "wiki");
    await within(sheet).findByText(/^Through the tunnel web-1 on /, {}, { timeout: 3000 });
    await user.click(within(sheet).getByRole("button", { name: "Change the way in" }));
    const ways = within(sheet).getByRole("radiogroup", { name: "Way in" });
    await user.click(within(ways).getByRole("radio", { name: /Through the tunnel web-1/ }));
    expect(within(sheet).queryByRole("radiogroup", { name: "Way in" })).toBeNull();
  });

  it("says the zone itself with www beside it, and leaves www out one step away", async () => {
    renderAt(
      "/apps/svc_wordpress",
      publishApi(() => free(), [], [], "www.example.com"),
    );
    const { user, sheet } = await openSheet();
    const sub = within(sheet).getByRole("textbox", { name: "Subdomain" });
    await user.type(sub, "x");
    await user.clear(sub);
    expect(
      await within(sheet).findByText(/example.com, with www.example.com beside it/, {}, { timeout: 3000 }),
    ).toBeVisible();
    // No checkbox up front: Change opens the choice.
    expect(within(sheet).queryByRole("checkbox")).toBeNull();
    await user.click(within(sheet).getByRole("button", { name: "Change" }));
    expect(await within(sheet).findByRole("checkbox", {}, { timeout: 3000 })).toBeVisible();
  });
});
