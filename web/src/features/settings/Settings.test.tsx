// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import { PanelServedBy, SettingsSchema } from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale, chosenLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState } from "../../test/auth";
import { type Api, onPhone, renderAt } from "../../test/render";

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

  it("says how the panel is served, and how to change that", async () => {
    renderSettings("/settings/general");
    const panel = await screen.findByRole("region", { name: "Panel address" });
    await waitFor(() => expect(panel).toHaveTextContent("AddressLoopback only"));
    expect(panel).toHaveTextContent("Loopback127.0.0.1:7600");
    expect(panel).toHaveTextContent("DOCKWELL_DOMAIN=panel.example.com");
    expect(panel).toHaveTextContent("Environment=DOCKWELL_DOMAIN=…");
  });

  it("names the domain at its port, what serves it and who issues its certificate", async () => {
    const served = settings({
      panel: {
        listen: "127.0.0.1:7600",
        domain: "panel.example.com",
        servedBy: PanelServedBy.DOCKWELL,
        // As a listener on every address names itself.
        httpsListen: "[::]:8443",
        url: "https://panel.example.com:8443",
        certificateAuthority: "acme.example.com",
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
    expect(panel).toHaveTextContent("The domain is set where Dockwell runs");
    for (const how of [
      "sudo systemctl edit dockwell",
      "/etc/conf.d/dockwell",
      "dockwell run --domain",
      "dockwell recover",
    ]) {
      expect(panel).toHaveTextContent(how);
    }
    expect(within(panel).getByRole("link", { name: "How to change settings" })).toHaveClass("general-link");
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

  it("shows the panel in the language chosen for this browser, and goes back to the browser's", async () => {
    const user = userEvent.setup();
    renderSettings("/settings/general");
    const language = await screen.findByRole("region", { name: "Language" });
    await user.selectOptions(within(language).getByRole("combobox", { name: "Show the panel in" }), "zh-CN");
    expect(await screen.findByRole("heading", { level: 1, name: "常规" })).toBeVisible();
    expect(chosenLocale()).toBe("zh-CN");
    await user.selectOptions(within(screen.getByRole("region", { name: "语言" })).getByRole("combobox"), "");
    expect(await screen.findByRole("heading", { level: 1, name: "General" })).toBeVisible();
    expect(chosenLocale()).toBeUndefined();
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
