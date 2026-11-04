// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { SystemService } from "@dockwell/api";
import { CommandBar, type CommandItem, type ThemePreference, useThemePreference, useUIStrings } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useState } from "react";

import { fleetShape, primaryAddress, serverStatus } from "../servers/server";
import { useFleet } from "../servers/useFleet";
import { shortVersion } from "./version";

const OpenContext = createContext<() => void>(() => {});

/** Opens the command bar, for the buttons that offer it. */
export function useOpenCommandBar(): () => void {
  return useContext(OpenContext);
}

/** Apple keyboards say ⌘K; everyone else's say Ctrl K. */
function onApple(): boolean {
  const platform =
    (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform ?? navigator.platform;
  return /mac|iphone|ipad|ipod/i.test(platform ?? "");
}

/** The shortcut that opens the command bar, as key caps for this keyboard. */
export function useCommandShortcut(): string {
  return useMemo(() => (onApple() ? "⌘K" : "Ctrl K"), []);
}

/**
 * The command bar, open on ⌘K (Ctrl+K off Apple keyboards) from anywhere in
 * the shell and from the search buttons. For now it reaches every page and
 * server that exists, the control plane's status, and the theme.
 */
export function CommandMenu({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const openBar = useCallback(() => setOpen(true), []);

  useEffect(() => {
    const apple = onApple();
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.isComposing || !isK(event) || event.altKey || event.shiftKey) return;
      if (apple ? !event.metaKey || event.ctrlKey : !event.ctrlKey || event.metaKey) return;
      event.preventDefault();
      // Held keys repeat; the bar toggles once per press.
      if (event.repeat) return;
      setOpen((was) => !was);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  return (
    <OpenContext value={openBar}>
      {children}
      {open ? <Commands onClose={() => setOpen(false)} /> : null}
    </OpenContext>
  );
}

/**
 * Whether the key pressed is K: the letter on Latin layouts, wherever they
 * put it, and the key in K's place on layouts without Latin letters, such as
 * Russian, Greek or Hebrew, where the same key types л, κ or ל.
 */
function isK(event: KeyboardEvent): boolean {
  const key = event.key.toLowerCase();
  return key === "k" || (!/^[a-z]$/.test(key) && event.code === "KeyK");
}

type Run = () => void;

function Commands({ onClose }: { onClose: () => void }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const navigate = useNavigate();
  const { servers, fleet } = fleetShape(useFleet());
  const version = useQuery(SystemService.method.getVersion, {}).data?.build?.version;
  const [theme, setTheme] = useThemePreference();

  const commands = useMemo(() => {
    const list: (CommandItem & { run: Run })[] = [];
    const pages = t`Pages`;
    list.push({
      id: "overview",
      group: pages,
      icon: "home",
      label: t`Open the overview`,
      run: () => navigate({ to: "/" }),
    });
    if (fleet) {
      list.push({
        id: "servers",
        group: pages,
        icon: "server",
        label: t`Open the server list`,
        hint: String(servers?.length ?? ""),
        run: () => navigate({ to: "/servers" }),
      });
    }
    for (const server of servers ?? []) {
      const address = primaryAddress(server.inventory?.addresses ?? []);
      const name = server.name;
      list.push({
        id: `server:${server.id}`,
        group: fleet ? t`Servers` : pages,
        icon: "server",
        // Servers have no page of their own yet, so this says where it goes.
        label: t`Find ${name} in the server list`,
        hint: [strings.status[serverStatus(server)], address].filter(Boolean).join(" · "),
        run: () => navigate({ to: "/servers" }),
      });
    }
    list.push({
      id: "status",
      group: pages,
      icon: "gauge",
      label: t`Check the control plane's status`,
      hint: version ? shortVersion(version) : undefined,
      run: () => navigate({ to: "/status" }),
    });
    const themes: { id: ThemePreference; label: string; icon: CommandItem["icon"] }[] = [
      { id: "light", label: t`Use the light theme`, icon: "sun" },
      { id: "dark", label: t`Use the dark theme`, icon: "moon" },
      { id: "system", label: t`Match the system theme`, icon: "settings" },
    ];
    for (const option of themes) {
      list.push({
        id: `theme:${option.id}`,
        group: t`Theme`,
        icon: option.icon,
        label: option.label,
        hint: option.id === theme ? t`Current` : undefined,
        run: () => setTheme(option.id),
      });
    }
    return list;
  }, [t, strings, navigate, servers, fleet, version, theme, setTheme]);

  return (
    <CommandBar
      open
      items={commands}
      onClose={onClose}
      onSelect={(item) => {
        onClose();
        commands.find((command) => command.id === item.id)?.run();
      }}
    />
  );
}
