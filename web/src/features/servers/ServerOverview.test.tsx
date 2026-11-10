// SPDX-License-Identifier: AGPL-3.0-only

import { clone, create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  BuildInfoSchema,
  CloudProvider,
  Connection,
  DeploymentState,
  DiskSchema,
  DiskUsageSchema,
  HealthState,
  NetworkShape,
  PublicAddressSchema,
  PublicAddressSource,
  Reachable,
  ServerDiskUseSchema,
  ServerInventorySchema,
  ServerNetworkSchema,
  ServerSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { aHealth, anApp } from "../../test/apps";
import { authError } from "../../test/auth";
import { expectMetric } from "../../test/metrics";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, history, sample, snapshotComplete } from "../../test/servers";

const GiB = 1024 ** 3;
const NOW = new Date(2026, 10, 4, 12, 0);

const inventory = create(ServerInventorySchema, {
  hostname: "hz-1",
  osName: "Ubuntu 24.04.1 LTS",
  kernel: "6.8.0-48-generic",
  architecture: "amd64",
  cpuModel: "AMD EPYC 7763 64-Core Processor",
  cpuCount: 4,
  memoryBytes: BigInt(8 * GiB),
  disks: [
    create(DiskSchema, { mountPoint: "/", device: "/dev/sda1", filesystem: "ext4", sizeBytes: BigInt(160 * GiB) }),
  ],
  dockerVersion: "27.3.1",
  addresses: ["10.0.0.2", "49.12.33.7", "2a01:4f8:c012:9a3e::1", "172.17.0.1"],
});
const past = history(20, 22, 23.4);
const hz1 = aServer({
  id: "srv_1",
  name: "hz-1",
  controlServer: true,
  inventory,
  stats: past.at(-1),
  history: past,
  agentBuild: create(BuildInfoSchema, { version: "v0.4.0" }),
  createTime: timestampFromDate(new Date(2026, 8, 24, 9, 30)),
  connectTime: timestampFromDate(new Date(2026, 10, 4, 9, 0)),
  lastSeenTime: timestampFromDate(new Date(2026, 10, 4, 11, 59, 58)),
});

/** What takes hz-1's disk, as Server-Overview.dc.html has it. */
const diskUse = create(ServerDiskUseSchema, {
  time: timestampFromDate(NOW),
  dockerRoot: "/var/lib/docker",
  mountPoint: "/",
  sizeBytes: BigInt(160 * GiB),
  usedBytes: BigInt(61 * GiB),
  imagesBytes: BigInt(18 * GiB),
  imagesUnusedBytes: BigInt(4 * GiB),
  images: 9,
  volumesBytes: BigInt(22 * GiB),
  volumes: 4,
  containersBytes: BigInt(GiB / 2),
  containers: 7,
  logsBytes: BigInt(3 * GiB),
});

/** Renders the server's page at path once the snapshot holds servers, with more of the API as api says. */
async function renderServerWith(api: Api, path: string, ...servers: ReturnType<typeof aServer>[]) {
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  const rendered = renderAt(path, {
    ...api,
    servers: { watchServers: watch.stream, getServerDiskUse: () => ({ diskUse }), ...api.servers },
  });
  await screen.findByRole("heading", { level: 1 });
  return { ...rendered, watch };
}

/** Renders the server's page at path once the snapshot holds servers. */
function renderServer(path: string, ...servers: ReturnType<typeof aServer>[]) {
  return renderServerWith({}, path, ...servers);
}

const panel = (name: string) => screen.getByRole("region", { name });

afterEach(() => {
  vi.useRealTimers();
});

describe("ServerOverview", () => {
  it("names the server, its role and where it is", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    await renderServer("/servers/srv_1", hz1);
    expect(await screen.findByRole("heading", { level: 1, name: "hz-1" })).toBeVisible();
    expect(screen.getByText("Runs the control plane")).toBeVisible();
    expect(screen.getByText("up 3 days", { exact: false })).toHaveTextContent("49.12.33.7 · up 3 days");
    expect(document.title).toBe("hz-1 · Dockwell");
    expect(screen.getByRole("link", { name: "Server hz-1" })).toHaveAttribute("aria-current", "page");
  });

  it("names a hostname that is the address once", async () => {
    const errors = vi.spyOn(console, "error");
    const named = aServer({
      ...hz1,
      inventory: create(ServerInventorySchema, { ...inventory, hostname: "49.12.33.7" }),
    });
    await renderServer("/servers/srv_1", named);
    const lead = screen.getByText("up 3 days", { exact: false });
    expect(lead).toHaveTextContent(/^49\.12\.33\.7 · up 3 days$/);
    expect(lead.querySelectorAll(".server-address")).toHaveLength(1);
    expect(errors.mock.calls.flat().join(" ")).not.toContain("same key");
  });

  it("tags a private address shown as the server's", async () => {
    const behindNAT = aServer({ ...hz1, inventory: create(ServerInventorySchema, { addresses: ["10.0.0.25"] }) });
    await renderServer("/servers/srv_1", behindNAT);
    await screen.findAllByText("10.0.0.25");
    expect(document.querySelector(".server-address")).toHaveTextContent(/^10\.0\.0\.25Private$/);
    expect(document.querySelector(".topbar-address")).toHaveTextContent(/^10\.0\.0\.25Private$/);
  });

  it("tags a TUN's address, as it's no way in", async () => {
    const tunnelled = aServer({
      ...hz1,
      inventory: create(ServerInventorySchema, { addresses: ["198.18.0.1"] }),
    });
    await renderServer("/servers/srv_1", tunnelled);
    await screen.findAllByText("198.18.0.1");
    expect(document.querySelector(".server-address")).toHaveTextContent(/^198\.18\.0\.1Private$/);
  });

  it("shows live CPU, memory, disk and load", async () => {
    await renderServer("/servers/srv_1", hz1);
    await screen.findByText("Load average");
    expectMetric("CPU", "23.4", "%");
    expectMetric("Memory", "2.4", "of 8 GB");
    expectMetric("Disk", "61", "of 160 GB", "99 GB free");
    expectMetric("Load average", "0.84", "/ 0.91 / 0.88", "1, 5 and 15 min, on 4 vCPU");
    expect(screen.queryByText("Network", { selector: ".dw-metric-label span" })).toBeNull();
  });

  it("says what takes the disk Docker keeps its data on, part by part", async () => {
    await renderServer("/servers/srv_1", hz1);
    const disk = await screen.findByRole("region", { name: "Disk usage" });
    await within(disk).findByText("Images");
    expect(
      within(disk).getByRole("img", {
        name: "Disk usage of /: Images 18 GB, Volumes 22 GB, Containers 512 MB, Logs 3 GB, Other files 18 GB, Free 99 GB, of 160 GB",
      }),
    ).toBeVisible();
    expect(disk).toHaveTextContent("Images18 GB9 images · 4 GB unused");
    expect(disk).toHaveTextContent("Volumes22 GB4 volumes");
    expect(disk).toHaveTextContent("Free99 GB62% of the disk");
  });

  it("draws each disk's used and free space while Docker can't say what takes it", async () => {
    const getServerDiskUse = () => {
      throw new ConnectError("hz-1 can't say what takes its disk now: Docker isn't running", Code.Unavailable);
    };
    await renderServerWith({ servers: { getServerDiskUse } }, "/servers/srv_1", hz1);
    const disks = await screen.findByRole("region", { name: "Disk usage" });
    await within(disks).findByText("Docker isn't running", { exact: false });
    expect(within(disks).getByText("/")).toBeVisible();
    expect(disks).toHaveTextContent("/dev/sda1 · ext4");
    expect(disks).toHaveTextContent("38% of 160 GB used");
    expect(within(disks).getByRole("img", { name: "/: 61 GB used, 99 GB free, of 160 GB" })).toBeVisible();
    expect(disks).toHaveTextContent("Used61 GBFree99 GB");
  });

  it("asks a server whose agent isn't connected nothing of its disk", async () => {
    const getServerDiskUse = vi.fn(() => ({ diskUse }));
    await renderServerWith(
      { servers: { getServerDiskUse } },
      "/servers/srv_1",
      aServer({ ...hz1, connection: Connection.UNREACHABLE, connectTime: undefined }),
    );
    const disks = await screen.findByRole("region", { name: "Disk usage" });
    expect(disks).toHaveTextContent("38% of 160 GB used");
    expect(getServerDiskUse).not.toHaveBeenCalled();
  });

  it("draws Docker's disk part by part, and another disk beside it by used and free, each once", async () => {
    const two = clone(ServerSchema, hz1);
    two.inventory?.disks.push(
      create(DiskSchema, { mountPoint: "/mnt", device: "/dev/sdb1", filesystem: "ext4", sizeBytes: BigInt(75 * GiB) }),
    );
    two.stats?.disks.push(
      create(DiskUsageSchema, { mountPoint: "/mnt", sizeBytes: BigInt(75 * GiB), usedBytes: BigInt(5 * GiB) }),
    );
    await renderServerWith({}, "/servers/srv_1", two);
    const disks = await screen.findByRole("region", { name: "Disk usage" });
    await within(disks).findByText("Images");
    expect(within(disks).getAllByRole("img", { name: /^Disk usage of \/:/ })).toHaveLength(1);
    expect(within(disks).getAllByRole("img", { name: /^\/mnt: / })).toHaveLength(1);
    // / is drawn once, part by part, and not again by used and free.
    expect(within(disks).queryByRole("img", { name: /^\/: / })).toBeNull();
    // Each has its own Free.
    expect(within(disks).getAllByText("Free", { exact: true })).toHaveLength(2);
  });

  it("draws each disk's used and free space while Docker adds up what takes it", async () => {
    let answer: (v: { diskUse: typeof diskUse }) => void = () => {};
    const getServerDiskUse = () => new Promise<{ diskUse: typeof diskUse }>((resolve) => (answer = resolve));
    await renderServerWith({ servers: { getServerDiskUse } }, "/servers/srv_1", hz1);
    const disks = await screen.findByRole("region", { name: "Disk usage" });
    expect(await within(disks).findByRole("img", { name: "/: 61 GB used, 99 GB free, of 160 GB" })).toBeVisible();
    expect(disks).toHaveTextContent("Adding up what takes the disk Docker keeps its data on.");
    act(() => answer({ diskUse }));
    await within(disks).findByText("Images");
    expect(disks).not.toHaveTextContent("Adding up");
  });

  it("asks nothing of a disk while its agent can't read Docker", async () => {
    const getServerDiskUse = vi.fn(() => ({ diskUse }));
    await renderServerWith(
      { servers: { getServerDiskUse } },
      "/servers/srv_1",
      aServer({ ...hz1, dockerUnreadableSince: timestampFromDate(new Date(2026, 10, 4, 11, 50)) }),
    );
    const disks = await screen.findByRole("region", { name: "Disk usage" });
    expect(disks).toHaveTextContent("38% of 160 GB used");
    expect(getServerDiskUse).not.toHaveBeenCalled();
  });

  it("doesn't ask again for the disk when the tab comes back into focus", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const getServerDiskUse = vi.fn(() => ({ diskUse }));
    await renderServerWith({ servers: { getServerDiskUse } }, "/servers/srv_1", hz1);
    await within(await screen.findByRole("region", { name: "Disk usage" })).findByText("Images");
    // Long enough that the answer is old, and short of the next ask.
    await act(() => vi.advanceTimersByTimeAsync(60_000));
    act(() => {
      window.dispatchEvent(new Event("visibilitychange"));
      window.dispatchEvent(new Event("focus"));
    });
    await act(() => vi.advanceTimersByTimeAsync(100));
    expect(getServerDiskUse).toHaveBeenCalledTimes(1);
  });

  it("says when Docker counts more than the disk has used, and draws used and free alone", async () => {
    const elsewhere = create(ServerDiskUseSchema, { ...diskUse, volumesBytes: BigInt(80 * GiB) });
    await renderServerWith({ servers: { getServerDiskUse: () => ({ diskUse: elsewhere }) } }, "/servers/srv_1", hz1);
    const disks = await screen.findByRole("region", { name: "Disk usage" });
    expect(
      await within(disks).findByText(
        "Docker counts 102 GB for its images, volumes, containers and logs, more than the 61 GB used on this disk, as some of it is on another disk. So this shows what's used and free.",
      ),
    ).toBeVisible();
    expect(within(disks).getByRole("img", { name: "/: 61 GB used, 99 GB free, of 160 GB" })).toBeVisible();
    expect(within(disks).queryByText("Images")).toBeNull();
    // The disk says its mount point once.
    expect(within(disks).getAllByText("/")).toHaveLength(1);
  });

  it("counts the apps and domains on the server, and names what needs a look", async () => {
    const api: Api = {
      services: {
        listServices: () => ({
          services: [
            anApp("web", "nginx:1.27"),
            anApp("api", "ghcr.io/acme/api:2"),
            anApp("worker", "ghcr.io/acme/worker:1"),
            anApp("elsewhere", "redis:7", {}, { deployments: [{ serverId: "srv_2", serverName: "hz-2" }] }),
          ],
        }),
      },
      health: {
        listHealth: () => ({
          services: [
            aHealth("svc_web", { state: HealthState.HEALTHY }),
            aHealth("svc_api", { state: HealthState.HEALTHY }),
            aHealth("svc_worker", { state: HealthState.UNHEALTHY }),
          ],
        }),
      },
      domains: {
        listDomains: () => ({
          domains: [
            { name: "shop.example.com", servers: [{ serverId: "srv_1" }] },
            { name: "api.example.com", servers: [{ serverId: "srv_1" }] },
            { name: "status.example.com", servers: [{ serverId: "srv_1" }] },
            { name: "other.example.com", servers: [{ serverId: "srv_2" }] },
          ],
        }),
      },
    };
    await renderServerWith(api, "/servers/srv_1", hz1);
    const runs = await screen.findByRole("region", { name: "What runs here" });
    const apps = await within(runs).findByRole("link", { name: /^3 apps/ });
    await waitFor(() => expect(apps).toHaveTextContent("3 apps2 healthy · worker unhealthy"));
    expect(apps).toHaveAttribute("href", "/apps");
    const domains = await within(runs).findByRole("link", { name: /^3 domains/ });
    expect(domains).toHaveTextContent("3 domainsapi.example.com, shop.example.com and 1 more");
  });

  it("says how each app's deployment went on this server, not on another", async () => {
    const at = timestampFromDate(new Date(2026, 10, 4, 11, 0));
    const on = (state: DeploymentState, serverId = "srv_1") => ({
      serverId,
      serverName: serverId,
      state,
      updateTime: at,
    });
    const api: Api = {
      services: {
        listServices: () => ({
          services: [
            anApp("web", "nginx:1.27", {}, { deployments: [on(DeploymentState.FAILED)] }),
            anApp(
              "api",
              "ghcr.io/acme/api:2",
              {},
              { deployments: [on(DeploymentState.LIVE), on(DeploymentState.FAILED, "srv_2")] },
            ),
            anApp("worker", "ghcr.io/acme/worker:1", {}, { deployments: [on(DeploymentState.PENDING)] }),
          ],
        }),
      },
      health: {
        listHealth: () => ({
          services: [
            aHealth("svc_web", { state: HealthState.HEALTHY }),
            aHealth("svc_api", { state: HealthState.HEALTHY }),
            aHealth("svc_worker", { state: HealthState.HEALTHY }),
          ],
        }),
      },
    };
    await renderServerWith(api, "/servers/srv_1", hz1);
    const runs = await screen.findByRole("region", { name: "What runs here" });
    const apps = await within(runs).findByRole("link", { name: /^3 apps/ });
    await waitFor(() => expect(apps).toHaveTextContent("3 apps1 healthy · web failed · worker deploying"));
  });

  it("counts every app it can't read the health of as unknown, so the counts add up", async () => {
    const api: Api = {
      services: {
        listServices: () => ({
          services: [
            anApp("web", "nginx:1.27"),
            anApp("api", "ghcr.io/acme/api:2"),
            anApp("worker", "ghcr.io/acme/worker:1"),
          ],
        }),
      },
      health: {
        listHealth: () => {
          throw new ConnectError("the control plane is busy", Code.Unavailable);
        },
      },
    };
    await renderServerWith(api, "/servers/srv_1", hz1);
    const runs = await screen.findByRole("region", { name: "What runs here" });
    const apps = await within(runs).findByRole("link", { name: /^3 apps/ });
    await waitFor(() => expect(apps).toHaveTextContent("3 apps3 unknown"));
  });

  it("invites an app and a domain on a server with neither, and opens the one app it has", async () => {
    await renderServer("/servers/srv_1", hz1);
    const runs = await screen.findByRole("region", { name: "What runs here" });
    expect(await within(runs).findByRole("link", { name: /^No apps/ })).toHaveAttribute("href", "/apps/new");
    expect(await within(runs).findByRole("link", { name: /^No domains/ })).toHaveAttribute("href", "/domains");
  });

  it("opens the one app a server has, by name", async () => {
    const api: Api = {
      services: { listServices: () => ({ services: [anApp("web", "nginx:1.27")] }) },
      health: { listHealth: () => ({ services: [aHealth("svc_web", { state: HealthState.NO_CHECK })] }) },
    };
    await renderServerWith(api, "/servers/srv_1", hz1);
    const runs = await screen.findByRole("region", { name: "What runs here" });
    const app = await within(runs).findByRole("link", { name: /^1 app/ });
    await waitFor(() => expect(app).toHaveTextContent("1 appweb, running"));
    expect(app).toHaveAttribute("href", "/apps/svc_web");
  });

  it("says what the server is", async () => {
    await renderServer("/servers/srv_1", hz1);
    const system = await screen.findByRole("region", { name: "System" });
    expect(system).toHaveTextContent("amd64");
    expect(system).toHaveTextContent("Operating systemUbuntu 24.04.1 LTS");
    expect(system).toHaveTextContent("Kernel6.8.0-48-generic");
    expect(system).toHaveTextContent("ProcessorAMD EPYC 7763 64-Core Processor · 4 vCPU");
    expect(system).toHaveTextContent("Memory8 GB");
    expect(system).toHaveTextContent("Docker27.3.1");
    expect(system).toHaveTextContent("Hostnamehz-1");
  });

  describe("its network", () => {
    const behindNAT = (network: Parameters<typeof create<typeof ServerNetworkSchema>>[1]) =>
      aServer({
        ...hz1,
        inventory: create(ServerInventorySchema, { addresses: ["10.0.0.25"] }),
        network: create(ServerNetworkSchema, network),
      });
    const known = { address: "203.0.113.50", knownTime: timestampFromDate(new Date()) };

    it("keeps the private address, the public one and what's proved apart", async () => {
      await renderServer(
        "/servers/srv_1",
        behindNAT({
          ipv4: create(PublicAddressSchema, {
            ...known,
            source: PublicAddressSource.PROVED_BY_DOMAIN,
            domain: "app.example.com",
          }),
          provider: CloudProvider.ORACLE,
          shape: NetworkShape.CLOUD_NAT,
          reachable: Reachable.YES,
          reachableDomain: "app.example.com",
          reachableTime: timestampFromDate(new Date()),
        }),
      );
      const network = await screen.findByRole("region", { name: "Network" });
      expect(network).toHaveTextContent("ConnectionBehind Oracle Cloud's NAT");
      expect(network).toHaveTextContent("Public IPv4203.0.113.50");
      expect(network).toHaveTextContent("Proved by the certificate for app.example.com, as of");
      expect(network).toHaveTextContent("ReachableYes");
      expect(network).toHaveTextContent("A certificate came for app.example.com on port 80 or 443, as of");
      expect(network).toHaveTextContent("Private addresses10.0.0.25");
      // The public address is the server's address everywhere else, with no tag.
      expect(document.querySelector(".server-address")).toHaveTextContent(/^203\.0\.113\.50$/);
    });

    it("says a looked-up address is a way out, and leaves reachable not checked", async () => {
      await renderServer(
        "/servers/srv_1",
        behindNAT({
          ipv4: create(PublicAddressSchema, { ...known, source: PublicAddressSource.LOOKED_UP, unproven: true }),
          shape: NetworkShape.LOCAL_NAT,
        }),
      );
      const network = await screen.findByRole("region", { name: "Network" });
      expect(network).toHaveTextContent("ConnectionLikely a home or office network");
      expect(network).toHaveTextContent("Looked up");
      expect(network).toHaveTextContent("your router's address, a way out and not a way in");
      expect(network).toHaveTextContent("ReachableNot checked");
      // A router's address isn't the server's, so the header goes on saying what it knows.
      expect(document.querySelector(".server-address")).toHaveTextContent(/^10\.0\.0\.25Private$/);
    });

    it("names no way out as the server's address on a cloud either, and says so", async () => {
      await renderServer(
        "/servers/srv_1",
        behindNAT({
          ipv4: create(PublicAddressSchema, { ...known, source: PublicAddressSource.LOOKED_UP, unproven: true }),
          provider: CloudProvider.AZURE,
          shape: NetworkShape.CLOUD_NAT,
        }),
      );
      const network = await screen.findByRole("region", { name: "Network" });
      expect(network).toHaveTextContent("That's its way out, and may not be a way in");
      expect(network).not.toHaveTextContent("usually its way in");
      expect(document.querySelector(".server-address")).toHaveTextContent(/^10\.0\.0\.25Private$/);
    });

    it("says metadata the machine doesn't back up may not be a way in", async () => {
      await renderServer(
        "/servers/srv_1",
        behindNAT({
          ipv4: create(PublicAddressSchema, { ...known, source: PublicAddressSource.CLOUD_METADATA, unproven: true }),
          shape: NetworkShape.LOCAL_NAT,
        }),
      );
      const network = await screen.findByRole("region", { name: "Network" });
      expect(network).toHaveTextContent("this machine doesn't name that cloud, so it may not be a way in");
      expect(document.querySelector(".server-address")).toHaveTextContent(/^10\.0\.0\.25Private$/);
    });

    it("keeps IPv4 and IPv6 apart, each with how it's known, and clears the one it's asked to", async () => {
      const set = vi.fn(() => ({ server: hz1 }));
      const watch = feed<WatchServersResponse>();
      watch.push(
        changed(
          behindNAT({
            ipv4: create(PublicAddressSchema, { ...known, source: PublicAddressSource.LOOKED_UP, unproven: true }),
            ipv6: create(PublicAddressSchema, {
              address: "2001:db8::5",
              source: PublicAddressSource.SET_BY_PERSON,
              knownTime: known.knownTime,
            }),
            shape: NetworkShape.LOCAL_NAT,
          }),
        ),
        snapshotComplete,
      );
      renderAt("/servers/srv_1", { servers: { watchServers: watch.stream, setServerPublicAddress: set } });
      const network = await screen.findByRole("region", { name: "Network" });
      expect(network).toHaveTextContent("Public IPv4203.0.113.50");
      expect(network).toHaveTextContent("your router's address, a way out and not a way in");
      expect(network).toHaveTextContent("Public IPv62001:db8::5");
      expect(network).toHaveTextContent("Set by a person");
      // A way in of one family is the server's address, whatever's known of the other.
      expect(document.querySelector(".server-address")).toHaveTextContent(/^2001:db8::5$/);

      const [, six] = within(network).getAllByRole("button", { name: "Change" });
      if (six) await userEvent.click(six);
      const sheet = await screen.findByRole("dialog", { name: "Set the public address of hz-1" });
      expect(within(sheet).getByLabelText("Public address")).toHaveValue("2001:db8::5");
      await userEvent.click(within(sheet).getByRole("button", { name: "Clear" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(set.mock.calls[0]?.at(0)).toMatchObject({ id: "srv_1", address: "", ipv6: true });
    });

    it("offers no clearing of what no person set", async () => {
      await renderServer(
        "/servers/srv_1",
        behindNAT({
          ipv4: create(PublicAddressSchema, { ...known, source: PublicAddressSource.LOOKED_UP, unproven: true }),
          shape: NetworkShape.LOCAL_NAT,
        }),
      );
      const network = await screen.findByRole("region", { name: "Network" });
      await userEvent.click(within(network).getByRole("button", { name: "Change" }));
      const sheet = await screen.findByRole("dialog", { name: "Set the public address of hz-1" });
      expect(within(sheet).getByLabelText("Public address")).toHaveValue("");
      expect(within(sheet).queryByRole("button", { name: "Clear" })).toBeNull();
    });

    it("says when nothing is known, and takes the address from a person", async () => {
      const set = vi.fn(() => ({ server: hz1 }));
      const watch = feed<WatchServersResponse>();
      watch.push(changed(behindNAT({ shape: NetworkShape.CLOUD_NAT })), snapshotComplete);
      renderAt("/servers/srv_1", { servers: { watchServers: watch.stream, setServerPublicAddress: set } });
      const network = await screen.findByRole("region", { name: "Network" });
      expect(network).toHaveTextContent("Public addressNot known");
      expect(network).toHaveTextContent("ReachableNot checked");

      await userEvent.click(within(network).getByRole("button", { name: "Set" }));
      const sheet = await screen.findByRole("dialog", { name: "Set the public address of hz-1" });
      await userEvent.click(within(sheet).getByRole("button", { name: "Set address" }));
      expect(await within(sheet).findByText("Name the address, such as 203.0.113.7.")).toBeVisible();
      expect(set).not.toHaveBeenCalled();

      await userEvent.type(within(sheet).getByLabelText("Public address"), "203.0.113.50{Enter}");
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(set).toHaveBeenCalledOnce();
      expect(set.mock.calls[0]?.at(0)).toMatchObject({ id: "srv_1", address: "203.0.113.50" });
    });

    it("says at the field why it refused an address", async () => {
      const set = vi.fn(() => {
        throw authError(Code.InvalidArgument, "invalid", "10.0.0.5 is private, so the internet can't reach it there.");
      });
      const watch = feed<WatchServersResponse>();
      watch.push(changed(behindNAT({ shape: NetworkShape.CLOUD_NAT })), snapshotComplete);
      renderAt("/servers/srv_1", { servers: { watchServers: watch.stream, setServerPublicAddress: set } });
      const network = await screen.findByRole("region", { name: "Network" });
      await userEvent.click(within(network).getByRole("button", { name: "Set" }));
      const sheet = await screen.findByRole("dialog", { name: "Set the public address of hz-1" });
      await userEvent.type(within(sheet).getByLabelText("Public address"), "10.0.0.5{Enter}");
      expect(
        await within(sheet).findByText("10.0.0.5 is private, so the internet can't reach it there."),
      ).toBeVisible();
      expect(screen.getByRole("dialog")).toBeVisible();
    });
  });

  it("lists public addresses to copy, and private ones apart", async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    await renderServer("/servers/srv_1", hz1);
    const network = await screen.findByRole("region", { name: "Network" });
    expect(network).toHaveTextContent("12 Mbit/s now");
    expect(network).toHaveTextContent("Public IPv449.12.33.7");
    expect(network).toHaveTextContent("Public IPv62a01:4f8:c012:9a3e::1");
    expect(network).toHaveTextContent("Private addresses10.0.0.2172.17.0.1");
    expect(network).toHaveTextContent("Traffic8 Mbit/s in · 4.4 Mbit/s out");

    await userEvent.click(within(network).getByRole("button", { name: "Copy 49.12.33.7" }));
    expect(writeText).toHaveBeenCalledWith("49.12.33.7");
    expect(await within(network).findByRole("button", { name: "Copied 49.12.33.7" })).toBeVisible();
  });

  it("follows the agent: its build, its connection and its heartbeat", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    await renderServer("/servers/srv_1", hz1);
    const agent = await screen.findByRole("region", { name: "Agent" });
    expect(agent).toHaveTextContent("Connected");
    expect(await within(agent).findByText("· same as the control plane")).toBeVisible();
    expect(agent).toHaveTextContent("Versionv0.4.0");
    expect(agent).toHaveTextContent("Control planeRuns on this server");
    expect(agent).toHaveTextContent("ConnectedFor 3 hours");
    expect(agent).toHaveTextContent("Last heartbeat2 s ago");
    expect(agent).toHaveTextContent("Joined24 Sept 2026, 09:30");

    await act(() => vi.advanceTimersByTimeAsync(3_000));
    expect(agent).toHaveTextContent("Last heartbeat5 s ago");
  });

  it("measures the agent's times on the control plane's clock, not the browser's", async () => {
    // The control plane's clock is 10 min behind the browser's.
    const skew = 10 * 60_000;
    const controlNow = () => new Date(Date.now() - skew);
    const ago = (ms: number) => timestampFromDate(new Date(controlNow().getTime() - ms));
    const server = aServer({ ...hz1, connectTime: ago(30 * 60_000), lastSeenTime: ago(3_000) });
    const { watch } = await renderServer("/servers/srv_1", server);

    // A sample arrives, stamped by the control plane as it hears it.
    act(() => watch.push(changed(aServer({ ...server, history: [], stats: sample(40), lastSeenTime: ago(0) }))));

    const agent = await screen.findByRole("region", { name: "Agent" });
    await vi.waitFor(() => expect(agent).toHaveTextContent("ConnectedFor 30 min"));
    expect(agent).toHaveTextContent("Last heartbeat1 s ago");
  });

  it("says when the agent's build differs from the control plane's", async () => {
    const hz2 = aServer({
      id: "srv_2",
      name: "hz-2",
      stats: sample(5),
      agentBuild: create(BuildInfoSchema, { version: "v0.3.9" }),
    });
    await renderServer("/servers/srv_2", hz1, hz2);
    const agent = await screen.findByRole("region", { name: "Agent" });
    expect(await within(agent).findByText("· the control plane runs v0.4.0")).toBeVisible();
    expect(agent).toHaveTextContent("Control planeRuns on another server");
  });

  it("explains a server whose agent dropped, and keeps what it knew", async () => {
    await renderServer(
      "/servers/srv_1",
      aServer({ ...hz1, connection: Connection.UNREACHABLE, connectTime: undefined }),
    );
    expect(await screen.findByRole("alert")).toHaveTextContent("hz-1 isn't reporting");
    const agent = screen.getByRole("region", { name: "Agent" });
    expect(agent).toHaveTextContent("Not connected");
    expect(agent).not.toHaveTextContent("For ");
    expect(screen.getByRole("region", { name: "Network" })).not.toHaveTextContent("Traffic");
    expect(screen.getByRole("region", { name: "System" })).toHaveTextContent("Ubuntu 24.04.1 LTS");
  });

  it("explains a server that hasn't reported yet", async () => {
    await renderServer(
      "/servers/srv_1",
      aServer({ id: "srv_1", name: "hz-1", connection: Connection.PENDING, inventory: undefined }),
    );
    expect(await screen.findByText("hz-1 is waiting for its agent")).toBeVisible();
    expect(screen.getByRole("region", { name: "System" })).toHaveTextContent(
      "The agent hasn't said what this server is yet.",
    );
    expect(screen.getByRole("region", { name: "Agent" })).toHaveTextContent("Waiting");
    expect(screen.getByRole("region", { name: "Agent" })).toHaveTextContent("Last heartbeatNever");
    expect(screen.queryByRole("region", { name: "Disk usage" })).toBeNull();
  });

  it("says when there's no such server", async () => {
    await renderServer("/servers/srv_9", hz1);
    expect(await screen.findByRole("heading", { level: 2, name: "No server with this ID" })).toBeVisible();
    expect(screen.getByText("srv_9").tagName).toBe("CODE");
    expect(within(screen.getByRole("main")).getByRole("link", { name: "Go to the overview" })).toHaveAttribute(
      "href",
      "/",
    );
  });

  it("is reached from the server list in a fleet, with a way back on phones", async () => {
    onPhone();
    const user = userEvent.setup();
    await renderServer("/servers", hz1, aServer({ id: "srv_2", name: "hz-2", stats: sample(8) }));
    await user.click(screen.getByRole("link", { name: "hz-2" }));

    expect(await screen.findByRole("heading", { level: 1, name: "hz-2" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/servers");
    const tabs = screen.getByRole("navigation", { name: "Main" });
    expect(within(tabs).getByRole("link", { name: "Servers" })).toHaveAttribute("aria-current", "page");
  });

  it("passes axe, wide and on a phone", async () => {
    const { container, unmount } = await renderServer("/servers/srv_1", hz1);
    await screen.findByRole("region", { name: "Agent" });
    await expectAccessible(container);
    unmount();

    onPhone();
    const phone = await renderServer("/servers/srv_1", hz1);
    expect(screen.getByRole("heading", { level: 1, name: "hz-1" })).toBeVisible();
    expect(screen.getByText("Healthy · up 3 days")).toBeVisible();
    await expectAccessible(phone.container);
  });

  it("speaks Simplified Chinese", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    await activateLocale("zh-CN");
    await renderServer("/servers/srv_1", hz1);
    expect(await screen.findByText("运行控制平面")).toBeVisible();
    expectMetric("平均负载", "0.84", "1、5 和 15 分钟，共 4 个 vCPU");
    expect(await screen.findByText("镜像")).toBeVisible();
    expect(panel("磁盘占用")).toHaveTextContent("可用99 GB占磁盘的 62%");
    expect(panel("系统")).toHaveTextContent("操作系统Ubuntu 24.04.1 LTS");
    expect(panel("网络")).toHaveTextContent("公网 IPv449.12.33.7");
    expect(panel("代理")).toHaveTextContent("已连接 3 小时");
    expect(panel("代理")).toHaveTextContent("最后心跳2 秒前");
  });
});
