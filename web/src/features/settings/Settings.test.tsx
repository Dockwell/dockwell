// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  AutoUpdate_Level,
  PanelDomainFrom,
  PanelServedBy,
  Role,
  type SetAutoUpdateDefaultRequest,
  SettingsSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, mira } from "../../test/auth";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { leave } from "./PanelDomain";

const settings = (init: MessageInitShape<typeof SettingsSchema> = {}) =>
  create(SettingsSchema, {
    timeZone: "",
    createTime: timestampFromDate(new Date(2026, 10, 4, 9, 0)),
    panel: { listen: "127.0.0.1:7600", servedBy: PanelServedBy.NONE },
    ...init,
  } as MessageInitShape<typeof SettingsSchema>);

function renderSettings(path: string, api: Api = {}) {
  return renderAt(path, { ...api, settings: { getSettings: () => ({ settings: settings() }), ...api.settings } });
}

afterEach(() => {
  localStorage.clear();
});

describe("Settings", () => {
  it("opens on General beside its sections, from the rail", async () => {
    const user = userEvent.setup();
    renderSettings("/");
    await user.click(await screen.findByRole("link", { name: "Settings" }));
    expect(await screen.findByRole("heading", { level: 1, name: "General" })).toBeVisible();
    const nav = screen.getByRole("navigation", { name: "Settings" });
    expect(within(nav).getByRole("link", { name: "General" })).toHaveAttribute("aria-current", "page");
    await user.click(within(nav).getByRole("link", { name: "Profile and security" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Profile and security" })).toBeVisible();
    expect(
      within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: "Profile and security" }),
    ).toHaveAttribute("aria-current", "page");
  });

  it("names the install and sets its time zone, each once, and says it's on Activity", async () => {
    const user = userEvent.setup();
    const setInstallName = vi.fn(({ name }: { name: string }) => ({ settings: settings({ name }) }));
    const setTimeZone = vi.fn(({ timeZone }: { timeZone: string }) => ({ settings: settings({ timeZone }) }));
    renderSettings("/settings/general", { settings: { setInstallName, setTimeZone } });
    const install = await screen.findByRole("region", { name: "This install" });
    const save = within(install).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    const name = within(install).getByRole("textbox", { name: "Name" });
    // Until the settings come, there's nothing to change.
    await waitFor(() => expect(name).toBeEnabled());
    await user.type(name, "Acme servers");
    await user.selectOptions(within(install).getByRole("combobox", { name: "Time zone" }), "Europe/Amsterdam");
    await user.click(save);
    await waitFor(() => expect(setTimeZone).toHaveBeenCalledTimes(1));
    expect(setInstallName).toHaveBeenCalledTimes(1);
    expect(setInstallName.mock.calls[0]?.[0]).toMatchObject({ name: "Acme servers" });
    expect(setTimeZone.mock.calls[0]?.[0]).toMatchObject({ timeZone: "Europe/Amsterdam" });
    expect(await within(install).findByText("Saved. It's on Activity.")).toBeVisible();
  });

  it("sets what new apps' automatic updates start as, and says apps made already keep their own", async () => {
    const user = userEvent.setup();
    const setAutoUpdateDefault = vi.fn((req: SetAutoUpdateDefaultRequest) => ({
      settings: settings({
        autoUpdate: req.autoUpdate ? { level: req.autoUpdate.level, at: "03:00", minutes: 120 } : undefined,
      }),
    }));
    renderSettings("/settings/general", { settings: { setAutoUpdateDefault } });
    const panel = await screen.findByRole("region", { name: "New apps" });
    expect(panel).toHaveTextContent("Apps made already keep their own.");
    const pick = within(panel).getByRole("combobox", { name: "Automatic updates" });
    await waitFor(() => expect(pick).toBeEnabled());
    expect(pick).toHaveValue(String(AutoUpdate_Level.UNSPECIFIED));
    await user.selectOptions(pick, "Patch updates, daily from 03:00");
    await user.click(within(panel).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(setAutoUpdateDefault).toHaveBeenCalledTimes(1));
    expect(setAutoUpdateDefault.mock.calls[0]?.[0].autoUpdate?.level).toBe(AutoUpdate_Level.PATCH);
    expect(await within(panel).findByText("Saved. It's on Activity.")).toBeVisible();
  });

  it("says at the field why a name was refused", async () => {
    const user = userEvent.setup();
    const setInstallName = () => {
      throw authError(
        Code.InvalidArgument,
        "invalid",
        "a name can't hold a line break or another control character. Write it on one line",
      );
    };
    renderSettings("/settings/general", { settings: { setInstallName } });
    const install = await screen.findByRole("region", { name: "This install" });
    const name = within(install).getByRole("textbox", { name: "Name" });
    await waitFor(() => expect(name).toBeEnabled());
    await user.type(name, "Acme");
    await user.click(within(install).getByRole("button", { name: "Save" }));
    expect(await within(install).findByText(/Write it on one line/)).toBeVisible();
  });

  it("says how the panel is served, and checks a domain for it as it's typed", async () => {
    const user = userEvent.setup();
    const checkPanelDomain = vi.fn(({ domain }: { domain: string }) =>
      domain === "shop.example.com"
        ? { refusal: "the route www serves shop.example.com, so the panel can't take shop.example.com" }
        : {
            addresses: ["198.51.100.7"],
            publicAddresses: ["192.0.2.10"],
            pointsHere: false,
            portsHeld: [{ port: 80, program: "nginx" }],
          },
    );
    renderSettings("/settings/general", { settings: { checkPanelDomain } });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await waitFor(() => expect(panel).toHaveTextContent("AddressLoopback only"));
    expect(panel).toHaveTextContent("Loopback127.0.0.1:7600");
    expect(panel).not.toHaveTextContent("systemctl edit");
    const field = within(panel).getByRole("textbox", { name: "Domain" });
    const save = within(panel).getByRole("button", { name: "Serve the panel there" });
    await user.type(field, "shop.example.com");
    expect(await within(panel).findByText(/the route www serves shop.example.com/)).toBeVisible();
    expect(save).toBeDisabled();
    await user.clear(field);
    await user.type(field, "panel.example.com");
    // Where its record leads, with the record to make, and what holds a port it needs.
    expect(await within(panel).findByText("DNS doesn't point here yet")).toBeVisible();
    expect(panel).toHaveTextContent("192.0.2.10");
    expect(panel).toHaveTextContent("It leads to 198.51.100.7");
    expect(panel).toHaveTextContent("nginx holds port 80 on this server, so no certificate can come until it's freed.");
    await waitFor(() => expect(save).toBeEnabled());
    expect(checkPanelDomain).toHaveBeenLastCalledWith(
      expect.objectContaining({ domain: "panel.example.com" }),
      expect.anything(),
    );
  });

  it("says where a domain leads while this server's public address isn't known, and takes it there", async () => {
    const user = userEvent.setup();
    let known = false;
    const checkPanelDomain = vi.fn(() =>
      known
        ? { addresses: ["203.0.113.10"], publicAddresses: ["203.0.113.10"], pointsHere: true }
        : { dnsProblem: "it has no A or AAAA record" },
    );
    const setServerPublicAddress = vi.fn(() => {
      known = true;
      return { server: control };
    });
    const control = aServer({ id: "srv_1", name: "hz-1", controlServer: true });
    const watch = feed<WatchServersResponse>();
    watch.push(changed(control), snapshotComplete);
    renderSettings("/settings/general", {
      settings: { checkPanelDomain },
      servers: { watchServers: watch.stream, setServerPublicAddress },
    });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await user.type(await within(panel).findByRole("textbox", { name: "Domain" }), "panel.example.com");
    expect(await within(panel).findByText("Dockwell can't tell whether panel.example.com leads here")).toBeVisible();
    expect(panel).toHaveTextContent(
      "It has no A or AAAA record. This server's public address isn't known: set it, and Dockwell compares the two.",
    );
    await user.click(within(panel).getByRole("button", { name: "Set public address" }));
    const sheet = await screen.findByRole("dialog", { name: "Set the public address of hz-1" });
    await user.type(within(sheet).getByLabelText("Public address"), "203.0.113.10{Enter}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(setServerPublicAddress.mock.calls[0]?.at(0)).toMatchObject({ id: "srv_1", address: "203.0.113.10" });
    // The check runs again with it, and the record reads as one that leads here.
    expect(await within(panel).findByText("Points here")).toBeVisible();
    expect(panel).not.toHaveTextContent("Dockwell can't tell");
  });

  it("saves a domain the owner confirms, waits through the restart and the certificate, then opens it", async () => {
    const user = userEvent.setup();
    let now = settings();
    const setPanelDomain = vi.fn(({ domain }: { domain: string }) => {
      now = settings({
        panel: {
          listen: "127.0.0.1:7600",
          domain,
          setDomain: domain,
          domainFrom: PanelDomainFrom.SETTING,
          servedBy: PanelServedBy.DOCKWELL,
          url: `https://${domain}`,
          certified: false,
        },
      });
      return { settings: now, restarting: true };
    });
    const createPanelSignInLink = vi.fn(() => ({ url: "https://panel.example.com/recover#t0ken" }));
    const went = vi.spyOn(leave, "to").mockImplementation(() => {});
    renderSettings("/settings/general", {
      settings: {
        getSettings: () => ({ settings: now }),
        checkPanelDomain: () => ({ addresses: ["192.0.2.10"], publicAddresses: ["192.0.2.10"], pointsHere: true }),
        setPanelDomain,
        createPanelSignInLink,
      },
    });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await user.type(await within(panel).findByRole("textbox", { name: "Domain" }), "panel.example.com");
    expect(await within(panel).findByText("Points here")).toBeVisible();
    const save = within(panel).getByRole("button", { name: "Serve the panel there" });
    await waitFor(() => expect(save).toBeEnabled());
    await user.click(save);
    expect(within(panel).getByText("Dockwell restarts to serve panel.example.com")).toBeVisible();
    await user.click(within(panel).getByRole("button", { name: "Restart and serve it" }));
    await waitFor(() => expect(setPanelDomain).toHaveBeenCalledTimes(1));
    expect(
      await within(panel).findByText("Getting a certificate for panel.example.com…", undefined, { timeout: 5000 }),
    ).toBeVisible();
    now = settings({ panel: { ...now.panel, certified: true } as MessageInitShape<typeof SettingsSchema>["panel"] });
    await user.click(within(panel).getByRole("button", { name: "Check again" }));
    await user.click(await within(panel).findByRole("button", { name: "Open panel.example.com" }));
    await waitFor(() => expect(went).toHaveBeenCalledWith("https://panel.example.com/recover#t0ken"));
    expect(createPanelSignInLink).toHaveBeenCalledTimes(1);
    went.mockRestore();
  });

  it("offers the tunnel first for a domain on Cloudflare, checks the way in, and serves it behind the tunnel", async () => {
    const user = userEvent.setup();
    let now = settings();
    let connected = false;
    const checkPanelReach = vi.fn(() =>
      connected
        ? { reached: true, target: "http://127.0.0.1:7600" }
        : {
            problem:
              "cloudflare answered 530 for panel.example.com: its tunnel isn't connected. Start cloudflared on this server",
            target: "http://127.0.0.1:7600",
          },
    );
    const setPanelDomain = vi.fn(({ domain }: { domain: string }) => {
      now = settings({
        panel: {
          listen: "127.0.0.1:7600",
          domain,
          setDomain: domain,
          domainFrom: PanelDomainFrom.SETTING,
          servedBy: PanelServedBy.PROXY,
          url: `https://${domain}`,
          certified: true,
        },
      });
      return { settings: now, restarting: true };
    });
    renderSettings("/settings/general", {
      settings: {
        getSettings: () => ({ settings: now }),
        checkPanelDomain: () => ({ addresses: ["104.16.0.10"], publicAddresses: ["192.0.2.10"], cloudflare: true }),
        checkPanelReach,
        setPanelDomain,
      },
    });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await user.type(await within(panel).findByRole("textbox", { name: "Domain" }), "panel.example.com");
    const ways = await within(panel).findByRole("radiogroup", { name: "How it reaches this server" });
    const options = within(ways).getAllByRole("radio");
    // The choices keep one order, whatever the check finds (#734's review, L6).
    expect(options.map((o) => o.textContent)).toEqual(["Dockwell serves it", "Behind a tunnel or proxy"]);
    // Cloudflare alone isn't a tunnel: the check that didn't reach leaves Dockwell serving it.
    await waitFor(() => expect(checkPanelReach).toHaveBeenCalled());
    expect(within(ways).getByRole("radio", { name: "Dockwell serves it" })).toBeChecked();
    await user.click(within(ways).getByRole("radio", { name: "Behind a tunnel or proxy" }));
    // No certificate comes through a tunnel, so nothing says DNS must lead here, or ports.
    expect(await within(panel).findByText("Not reached yet")).toBeVisible();
    expect(panel).not.toHaveTextContent("DNS doesn't point here yet");
    expect(panel).toHaveTextContent("http://127.0.0.1:7600");
    expect(panel).toHaveTextContent("Cloudflare answered 530 for panel.example.com: its tunnel isn't connected.");
    connected = true;
    await user.click(within(panel).getByRole("button", { name: "Check again" }));
    expect(await within(panel).findByText("Reached through your tunnel")).toBeVisible();
    await user.click(within(panel).getByRole("button", { name: "Serve the panel there" }));
    await user.click(within(panel).getByRole("button", { name: "Restart and serve it" }));
    await waitFor(() =>
      expect(setPanelDomain).toHaveBeenCalledWith(
        expect.objectContaining({ domain: "panel.example.com", behindProxy: true }),
        expect.anything(),
      ),
    );
    // Served behind the tunnel: nothing to wait for, and the way onto it.
    expect(
      await within(panel).findByRole("button", { name: "Open panel.example.com" }, { timeout: 5000 }),
    ).toBeVisible();
    expect(panel).toHaveTextContent("Your tunnel or proxy, which ends TLS and sends it here");
    expect(panel).not.toHaveTextContent("Getting a certificate");
    expect(panel).toHaveTextContent("Reached through your tunnel");
    await expectAccessible(panel);
  });

  it("serves a domain waiting for a certificate behind Cloudflare's tunnel instead, in a click", async () => {
    const user = userEvent.setup();
    const setPanelDomain = vi.fn(() => ({ settings: settings(), restarting: true }));
    const waiting = settings({
      panel: {
        listen: "127.0.0.1:7600",
        domain: "panel.example.com",
        setDomain: "panel.example.com",
        domainFrom: PanelDomainFrom.SETTING,
        servedBy: PanelServedBy.DOCKWELL,
        url: "https://panel.example.com",
        certified: false,
      },
    });
    renderSettings("/settings/general", {
      settings: {
        getSettings: () => ({ settings: waiting }),
        checkPanelDomain: () => ({ addresses: ["104.16.0.10"], publicAddresses: ["192.0.2.10"], cloudflare: true }),
        checkPanelReach: () => ({ reached: true, target: "http://127.0.0.1:7600" }),
        setPanelDomain,
      },
    });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    expect(await within(panel).findByText("panel.example.com reaches this panel through a tunnel")).toBeVisible();
    expect(panel).not.toHaveTextContent("Getting a certificate");
    await user.click(within(panel).getByRole("button", { name: "Serve it behind a tunnel" }));
    expect(within(panel).getByText("Dockwell restarts to serve panel.example.com behind your tunnel")).toBeVisible();
    await user.click(within(panel).getByRole("button", { name: "Restart and serve it" }));
    await waitFor(() =>
      expect(setPanelDomain).toHaveBeenCalledWith(
        expect.objectContaining({ domain: "panel.example.com", behindProxy: true }),
        expect.anything(),
      ),
    );
  });

  it("leaves a domain on Cloudflare's proxy without a tunnel waiting for its certificate, the tunnel one step away", async () => {
    const waiting = settings({
      panel: {
        listen: "127.0.0.1:7600",
        domain: "panel.example.com",
        setDomain: "panel.example.com",
        domainFrom: PanelDomainFrom.SETTING,
        servedBy: PanelServedBy.DOCKWELL,
        url: "https://panel.example.com",
        certified: false,
      },
    });
    const checkPanelReach = vi.fn(() => ({
      problem:
        "cloudflare answered 525 for panel.example.com: its proxy connects to this server's public address, not a tunnel",
      target: "http://127.0.0.1:7600",
    }));
    renderSettings("/settings/general", {
      settings: {
        getSettings: () => ({ settings: waiting }),
        checkPanelDomain: () => ({ addresses: ["104.16.0.10"], publicAddresses: ["192.0.2.10"], cloudflare: true }),
        checkPanelReach,
      },
    });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await waitFor(() => expect(checkPanelReach).toHaveBeenCalled());
    expect(await within(panel).findByText("Getting a certificate for panel.example.com…")).toBeVisible();
    expect(panel).not.toHaveTextContent("through a tunnel");
    // Not the primary action, as nothing says a tunnel is there.
    expect(within(panel).getByRole("button", { name: "Serve it behind a tunnel" })).not.toHaveClass("dw-btn-primary");
  });

  it("moves a domain from the tunnel back to Dockwell's own certificate as one change, its record and ports first", async () => {
    const user = userEvent.setup();
    const setPanelDomain = vi.fn(() => ({ settings: settings(), restarting: true }));
    const tunnelled = settings({
      panel: {
        listen: "127.0.0.1:7600",
        domain: "panel.example.com",
        setDomain: "panel.example.com",
        domainFrom: PanelDomainFrom.SETTING,
        servedBy: PanelServedBy.PROXY,
        url: "https://panel.example.com",
        certified: true,
      },
    });
    renderSettings("/settings/general", {
      settings: {
        getSettings: () => ({ settings: tunnelled }),
        checkPanelReach: () => ({ reached: true, target: "http://127.0.0.1:7600" }),
        checkPanelDomain: () => ({
          addresses: ["198.51.100.7"],
          publicAddresses: ["192.0.2.10"],
          portsHeld: [{ port: 443, program: "nginx" }],
        }),
        setPanelDomain,
      },
    });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await user.click(await within(panel).findByRole("button", { name: "Serve it with its own certificate" }));
    expect(await within(panel).findByText("DNS doesn't point here yet")).toBeVisible();
    expect(panel).toHaveTextContent("192.0.2.10");
    expect(panel).toHaveTextContent(
      "nginx holds port 443 on this server, so no certificate can come until it's freed.",
    );
    expect(
      within(panel).getByText("Dockwell restarts to serve panel.example.com with its own certificate"),
    ).toBeVisible();
    await user.click(within(panel).getByRole("button", { name: "Restart and serve it" }));
    await waitFor(() =>
      expect(setPanelDomain).toHaveBeenCalledWith(
        expect.objectContaining({ domain: "panel.example.com", behindProxy: false }),
        expect.anything(),
      ),
    );
  });

  it("says why a domain set here isn't served, with the way to stop using it", async () => {
    const user = userEvent.setup();
    const setPanelDomain = vi.fn(() => ({ settings: settings(), restarting: true }));
    const refused = settings({
      panel: {
        listen: "127.0.0.1:7600",
        setDomain: "panel.example.com",
        domainFrom: PanelDomainFrom.SETTING,
        servedBy: PanelServedBy.NONE,
        problem:
          "Dockwell couldn't hold the ports panel.example.com needs: listen tcp :443: bind: address already in use.",
      },
    });
    renderSettings("/settings/general", { settings: { getSettings: () => ({ settings: refused }), setPanelDomain } });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    expect(await within(panel).findByText("panel.example.com isn't served")).toBeVisible();
    expect(panel).toHaveTextContent("address already in use");
    await user.click(within(panel).getByRole("button", { name: "Stop using it" }));
    await waitFor(() =>
      expect(setPanelDomain).toHaveBeenCalledWith(expect.objectContaining({ domain: "" }), expect.anything()),
    );
  });

  it("waits after Stop using it for the control plane that started since, not the one about to restart", async () => {
    const user = userEvent.setup();
    const before = timestampFromDate(new Date(2026, 10, 10, 9, 0));
    let now = settings({
      panel: {
        listen: "127.0.0.1:7600",
        setDomain: "panel.example.com",
        domainFrom: PanelDomainFrom.SETTING,
        servedBy: PanelServedBy.NONE,
        problem: "Dockwell couldn't hold the ports panel.example.com needs: listen tcp :443: bind: permission denied.",
        startTime: before,
      },
    });
    const setPanelDomain = vi.fn(() => {
      // The control plane that answers until it restarts says the domain is cleared already.
      now = settings({ panel: { listen: "127.0.0.1:7600", servedBy: PanelServedBy.NONE, startTime: before } });
      return { settings: now, restarting: true };
    });
    renderSettings("/settings/general", { settings: { getSettings: () => ({ settings: now }), setPanelDomain } });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await user.click(await within(panel).findByRole("button", { name: "Stop using it" }));
    expect(
      await within(panel).findByText("Dockwell restarts to answer on loopback alone. Apps and websites keep running…"),
    ).toBeVisible();
    // Reading the cleared domain from the old control plane isn't the restart.
    await new Promise((resolve) => setTimeout(resolve, 1500));
    expect(within(panel).queryByRole("textbox", { name: "Domain" })).toBeNull();
    now = settings({
      panel: { listen: "127.0.0.1:7600", servedBy: PanelServedBy.NONE, startTime: timestampFromDate(new Date()) },
    });
    expect(await within(panel).findByRole("textbox", { name: "Domain" }, { timeout: 3000 })).toBeVisible();
  });

  it("leaves a domain dockwell run was given where it was given", async () => {
    const served = settings({
      panel: {
        listen: "127.0.0.1:7600",
        domain: "panel.example.com",
        domainFrom: PanelDomainFrom.RUN,
        servedBy: PanelServedBy.DOCKWELL,
        // As a listener on every address names itself.
        httpsListen: "[::]:8443",
        url: "https://panel.example.com:8443",
        certificateAuthority: "acme.example.com",
        certified: true,
      },
    });
    renderSettings("/settings/general", { settings: { getSettings: () => ({ settings: served }) } });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    expect(await within(panel).findByRole("link", { name: "https://panel.example.com:8443" })).toHaveAttribute(
      "href",
      "https://panel.example.com:8443",
    );
    expect(panel).toHaveTextContent("Dockwell itself, with a certificate from acme.example.com that it renews");
    expect(panel).not.toHaveTextContent("[::]");
    expect(panel).toHaveTextContent("dockwell run was started with this domain");
    expect(within(panel).queryByRole("textbox", { name: "Domain" })).toBeNull();
    expect(within(panel).getByRole("link", { name: "How to change settings" })).toHaveClass("settings-link");
  });

  it("offers an admin the checks, and the save to the owner alone", async () => {
    const user = userEvent.setup();
    renderSettings("/settings/general", {
      auth: { getAuthState: () => authState({ user: { ...mira, role: Role.ADMIN } }) },
      settings: {
        checkPanelDomain: () => ({ addresses: ["192.0.2.10"], publicAddresses: ["192.0.2.10"], pointsHere: true }),
      },
    });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await user.type(await within(panel).findByRole("textbox", { name: "Domain" }), "panel.example.com");
    expect(await within(panel).findByText("Points here")).toBeVisible();
    expect(within(panel).getByRole("button", { name: "Serve the panel there" })).toBeDisabled();
    expect(panel).toHaveTextContent("Only the owner sets it.");
  });

  it("says when the server's ingress serves the domain, while it has routes", async () => {
    const served = settings({
      panel: {
        listen: "127.0.0.1:7600",
        domain: "panel.example.com",
        servedBy: PanelServedBy.INGRESS,
        url: "https://panel.example.com",
        certificateAuthority: "Let's Encrypt",
      },
    });
    renderSettings("/settings/general", { settings: { getSettings: () => ({ settings: served }) } });
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await waitFor(() =>
      expect(panel).toHaveTextContent(
        "Caddy, the server's ingress, which holds ports 80 and 443 while the server has routes",
      ),
    );
  });

  it("tells a time zone nobody chose from UTC chosen", async () => {
    const user = userEvent.setup();
    const setTimeZone = vi.fn(({ timeZone }: { timeZone: string }) => ({ settings: settings({ timeZone }) }));
    renderSettings("/settings/general", { settings: { setTimeZone } });
    const install = await screen.findByRole("region", { name: "This install" });
    const zone = within(install).getByRole("combobox", { name: "Time zone" });
    await waitFor(() => expect(zone).toBeEnabled());
    expect(zone).toHaveValue("");
    expect(within(zone).getByRole("option", { name: "Not set: UTC" })).toBeInTheDocument();
    await user.selectOptions(zone, "UTC");
    await user.click(within(install).getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(setTimeZone).toHaveBeenCalledWith(expect.objectContaining({ timeZone: "UTC" }), expect.anything()),
    );
  });

  it("leaves the language to Profile, where each browser chooses its own", async () => {
    renderSettings("/settings/general");
    await screen.findByRole("heading", { level: 1, name: "General" });
    expect(await screen.findByRole("region", { name: "Panel address" })).toBeVisible();
    expect(screen.queryByRole("region", { name: "Language" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Show the panel in" })).toBeNull();
  });

  it("names the install in the browser's tab", async () => {
    renderSettings("/settings/general", { auth: { getAuthState: () => authState({ installName: "Acme servers" }) } });
    await screen.findByRole("heading", { level: 1, name: "General" });
    await waitFor(() => expect(document.title).toBe("General · Acme servers"));
  });

  it("heads the sign-in screen with the install's name, and names the server to run dockwell recover on", async () => {
    renderAt("/sign-in", {
      auth: { getAuthState: () => authState({ user: undefined, installName: "Acme servers", serverName: "hz-1" }) },
    });
    expect(await screen.findByText(/the panel for Acme servers/)).toBeVisible();
    expect(screen.getByText("Run this on hz-1 over SSH. It prints a sign-in link that works once.")).toBeVisible();
    expect(screen.queryByText(/Run this on Acme servers/)).toBeNull();
  });

  it("lists its sections on a phone, and passes axe there and wide", async () => {
    const { container, unmount } = renderSettings("/settings/general");
    await screen.findByRole("region", { name: "Panel address" });
    await expectAccessible(container);
    unmount();

    onPhone();
    const phone = renderSettings("/settings");
    expect(await screen.findByRole("heading", { level: 1, name: "Settings" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/more");
    expect(screen.getByRole("heading", { level: 2, name: "Install" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 2, name: "Account" })).toBeVisible();
    expect(screen.getByRole("link", { name: "General" })).toHaveAttribute("href", "/settings/general");
    expect(screen.getByRole("link", { name: "Profile and security" })).toHaveAttribute("href", "/profile");
    // Each group is a region its heading names, by an ID of its place rather than its translated label.
    expect(
      within(screen.getByRole("region", { name: "Account" })).getByRole("link", { name: "Profile and security" }),
    ).toBeVisible();
    await expectAccessible(phone.container);
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderSettings("/settings/general");
    expect(await screen.findByRole("heading", { level: 1, name: "常规" })).toBeVisible();
    expect(screen.getByRole("region", { name: "此安装" })).toBeVisible();
    expect(screen.getByRole("region", { name: "面板地址" })).toHaveTextContent("仅本机回环");
  });
});
