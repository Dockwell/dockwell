// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { EventKind, EventService, type Resource, ResourceService, SystemService } from "@dockwell/api";
import { CommandBar, type CommandItem, type ThemePreference, useThemePreference, useUIStrings } from "@dockwell/ui";
import type { I18n } from "@lingui/core";
import { msg } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useState } from "react";

import { onApple } from "../../lib/platform";
import { useSignedInUser } from "../auth/useAuthState";
import { type Opening, useChangeSheet } from "../changes/ChangeSheet";
import { serverStatus } from "../servers/server";
import { useFleetView } from "../servers/useFleet";
import { shortVersion } from "./version";

const OpenContext = createContext<() => void>(() => {});

/** Opens the command bar, for the buttons that offer it. */
export function useOpenCommandBar(): () => void {
  return useContext(OpenContext);
}

/** The shortcut that opens the command bar, as key caps for this keyboard. */
export function useCommandShortcut(): string {
  return useMemo(() => (onApple() ? "⌘K" : "Ctrl K"), []);
}

/**
 * The command bar, open on ⌘K (Ctrl+K off Apple keyboards) from anywhere in
 * the shell and from the search buttons. It reaches every page and server,
 * the activity of each server, project and environment, the latest changes'
 * plans, the control plane's status, your profile and the theme. Its first
 * actions make a project or an environment: it asks for the name, then
 * opens the plan. Other verbs come later.
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

/** A command, and whether choosing it keeps the bar open, as for a step of a question. */
type Command = CommandItem & { run: Run; keepOpen?: boolean };

/** A name Dockwell takes for a project or environment: a DNS label, such as "api-v2". */
const NAME = /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** What the bar is asking for, after an action that takes a name. */
type Prompt = { kind: "project" } | { kind: "environment"; project?: Resource | undefined };

function Commands({ onClose }: { onClose: () => void }) {
  const { t, i18n } = useLingui();
  const strings = useUIStrings();
  const navigate = useNavigate();
  const sheet = useChangeSheet();
  // The view keeps its identity between samples, so the list isn't rebuilt while you type.
  const { servers, fleet } = useFleetView();
  const version = useQuery(SystemService.method.getVersion, {}).data?.build?.version;
  const [theme, setTheme] = useThemePreference();
  const email = useSignedInUser()?.email;
  const projects = useQuery(ResourceService.method.listResources, { kind: "project" }).data?.resources;
  const environments = useQuery(ResourceService.method.listResources, { kind: "environment" }).data?.resources;
  const recent = useQuery(EventService.method.listEvents, { kinds: [EventKind.CHANGE], pageSize: 8 }).data?.events;
  const [prompt, setPrompt] = useState<Prompt>();
  const [query, setQuery] = useState("");

  const commands = useMemo(() => {
    const list: Command[] = [];
    if (prompt)
      return promptItems(prompt, query.trim(), projects ?? [], i18n, setPrompt, (opening) => {
        onClose();
        sheet.open(opening);
      });
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
      const name = server.name;
      list.push({
        id: `server:${server.id}`,
        group: fleet ? t`Servers` : pages,
        icon: "server",
        label: t`Open ${name}`,
        hint: [strings.status[serverStatus(server)], server.address].filter(Boolean).join(" · "),
        run: () => navigate({ to: "/servers/$serverId", params: { serverId: server.id } }),
      });
      list.push({
        id: `server-activity:${server.id}`,
        group: fleet ? t`Servers` : pages,
        icon: "activity",
        label: t`Show what happened on ${name}`,
        run: () => navigate({ to: "/activity", search: { server: server.id } }),
      });
    }
    list.push({
      id: "activity",
      group: pages,
      icon: "activity",
      label: t`Open the activity`,
      run: () => navigate({ to: "/activity" }),
    });
    list.push({
      id: "status",
      group: pages,
      icon: "gauge",
      label: t`Check the control plane's status`,
      hint: version ? shortVersion(version) : undefined,
      run: () => navigate({ to: "/status" }),
    });
    list.push({
      id: "profile",
      group: pages,
      icon: "user",
      label: t`Open your profile and security settings`,
      hint: email,
      run: () => navigate({ to: "/profile" }),
    });

    // Projects and environments, once there is a choice of them (PRD principle 4).
    const shownProjects = (projects?.length ?? 0) > 1 ? (projects ?? []) : [];
    for (const project of shownProjects) {
      const name = project.resource?.name ?? project.id;
      list.push({
        id: `project:${project.id}`,
        group: t`Projects`,
        icon: "folder",
        label: t`Show the activity of ${name}`,
        hint: project.displayName || undefined,
        run: () => navigate({ to: "/activity", search: { resource: project.id } }),
      });
    }
    const perProject = new Map<string, number>();
    for (const env of environments ?? [])
      perProject.set(env.resource?.scope ?? "", (perProject.get(env.resource?.scope ?? "") ?? 0) + 1);
    for (const env of environments ?? []) {
      const scope = env.resource?.scope ?? "";
      if ((perProject.get(scope) ?? 0) < 2) continue;
      const projectName = projects?.find((p) => p.id === scope)?.resource?.name;
      const envName = env.resource?.name ?? env.id;
      const name = shownProjects.length > 0 && projectName ? `${projectName} / ${envName}` : envName;
      list.push({
        id: `environment:${env.id}`,
        group: t`Environments`,
        icon: "layers",
        label: t`Show the activity of ${name}`,
        hint: env.displayName || undefined,
        run: () => navigate({ to: "/activity", search: { resource: env.id } }),
      });
    }

    for (const event of recent ?? []) {
      const change = event.change;
      if (!change) continue;
      const summary = change.summary;
      list.push({
        id: `change:${change.id}`,
        group: t`Recent changes`,
        icon: change.reverts ? "undo" : "commit",
        label: t`Open the plan: ${summary}`,
        hint: event.actor?.name || undefined,
        run: () => sheet.open({ kind: "change", id: change.id }),
      });
    }

    const actions = t`Actions`;
    list.push({
      id: "create-project",
      group: actions,
      icon: "plus",
      label: t`Create a project`,
      hint: t`Opens a plan`,
      keepOpen: true,
      run: () => setPrompt({ kind: "project" }),
    });
    list.push({
      id: "create-environment",
      group: actions,
      icon: "plus",
      label: t`Create an environment`,
      hint: t`Opens a plan`,
      keepOpen: true,
      run: () =>
        setPrompt(
          (projects?.length ?? 0) > 1 ? { kind: "environment" } : { kind: "environment", project: projects?.[0] },
        ),
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
  }, [
    t,
    i18n,
    strings,
    navigate,
    servers,
    fleet,
    version,
    email,
    theme,
    setTheme,
    projects,
    environments,
    recent,
    prompt,
    query,
    sheet,
    onClose,
  ]);

  const placeholder = !prompt
    ? undefined
    : prompt.kind === "project"
      ? t`Name the new project, such as shop`
      : prompt.project
        ? t`Name the new environment, such as staging`
        : t`Which project is it for?`;

  return (
    <CommandBar
      // A new question starts with an empty query.
      key={prompt ? `${prompt.kind}:${prompt.kind === "environment" ? (prompt.project?.id ?? "") : ""}` : "commands"}
      open
      items={commands}
      placeholder={placeholder}
      onQueryChange={setQuery}
      onClose={onClose}
      onSelect={(item) => {
        const command = commands.find((c) => c.id === item.id);
        // Choosing a step of a question keeps the bar open for the next one.
        if (command?.keepOpen) setQuery("");
        else onClose();
        command?.run();
      }}
    />
  );
}

/** The items while the bar asks for a name, or which project, for a new project or environment. */
function promptItems(
  prompt: Prompt,
  name: string,
  projects: readonly Resource[],
  i18n: I18n,
  setPrompt: (p: Prompt) => void,
  propose: (opening: Opening) => void,
): Command[] {
  if (prompt.kind === "environment" && !prompt.project) {
    return projects.map((project) => {
      const projectName = project.resource?.name ?? project.id;
      return {
        id: `prompt:project:${project.id}`,
        icon: "folder",
        label: i18n._(msg`In ${projectName}`),
        hint: project.displayName || undefined,
        keepOpen: true,
        run: () => setPrompt({ kind: "environment", project }),
      };
    });
  }
  if (name === "") return [];
  if (!NAME.test(name)) {
    return [
      {
        id: "prompt:invalid",
        icon: "alert",
        label: i18n._(msg`${name} can't be a name. Use lowercase letters, digits and dashes, starting with a letter`),
        keepOpen: true,
        run: () => {},
      },
    ];
  }
  if (prompt.kind === "project") {
    return [
      {
        id: "create",
        icon: "plus",
        label: i18n._(msg`Create the project ${name}`),
        hint: i18n._(msg`Opens a plan`),
        run: () =>
          propose({
            kind: "propose",
            summary: i18n._(msg`Create the project ${name}`),
            edits: [{ resource: { kind: "project", name }, spec: { kind: { case: "project", value: {} } } }],
          }),
      },
    ];
  }
  const project = prompt.project;
  const projectName = project?.resource?.name ?? "";
  // With one project it goes unnamed, as everywhere until there is a choice.
  const label =
    projects.length > 1
      ? i18n._(msg`Create the environment ${name} in ${projectName}`)
      : i18n._(msg`Create the environment ${name}`);
  return [
    {
      id: "create",
      icon: "plus",
      label,
      hint: i18n._(msg`Opens a plan`),
      run: () =>
        propose({
          kind: "propose",
          summary: label,
          edits: [
            {
              resource: { kind: "environment", name, scope: projects.length > 1 ? (project?.id ?? "") : "" },
              spec: { kind: { case: "environment", value: {} } },
            },
          ],
        }),
    },
  ];
}
