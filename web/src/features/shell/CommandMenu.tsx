// SPDX-License-Identifier: AGPL-3.0-only

import { createClient, type Transport } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { ChangeService, EventKind, EventService, type Resource, ResourceService, SystemService } from "@dockwell/api";
import { CommandBar, type CommandItem, type ThemePreference, useThemePreference, useUIStrings } from "@dockwell/ui";
import type { I18n } from "@lingui/core";
import { msg } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useState } from "react";

import { onApple } from "../../lib/platform";
import { appState, imageOf, nameOf } from "../apps/app";
import { useServices } from "../apps/useServices";
import { useAppWords } from "../apps/words";
import { useSignedInUser } from "../auth/useAuthState";
import { type Opening, useChangeSheet } from "../changes/ChangeSheet";
import { isPrivateAddress, serverStatus } from "../servers/server";
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
 * the shell and from the search buttons. It reaches every page, server and
 * app, the activity of each server, project and environment, each environment's
 * variables and secrets, the latest changes' plans, the control plane's status,
 * your profile and the theme. Its first actions make an app, a project or an
 * environment. Other verbs come later.
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

/**
 * A command. Choosing one closes the bar, unless it keeps it open: a step
 * of a question, which asks the next one in an empty bar, or a note, such
 * as why what you typed can't be a name, which leaves what you typed.
 */
type Command = CommandItem & { run: Run; keepOpen?: "step" | "note" };

/** A name Dockwell takes for a project or environment: a DNS label, such as "api-v2". */
const NAME = /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/;

/**
 * What the bar is asking for, after an action that takes a name or a project. An
 * environment's project is asked for only once there is a choice.
 */
type Prompt =
  | { kind: "project" }
  | { kind: "environment"; project?: Resource | undefined }
  | { kind: "delete-project" };

/**
 * Plans deleting a project, with the volumes and networks left in it, as nothing else
 * in the panel deletes those, and "Delete this app" keeps its volumes. So the plan names
 * each, and a volume going needs a person's confirmation, as any data loss does.
 * Whatever else is left keeps the project, and the API says what.
 */
async function planDeletingProject(
  transport: Transport,
  project: Resource,
  environments: readonly Resource[],
  summary: string,
) {
  const inside = new Set(environments.filter((e) => e.resource?.scope === project.id).map((e) => e.id));
  const resources = createClient(ResourceService, transport);
  const lists = await Promise.all(["volume", "network"].map((kind) => resources.listResources({ kind })));
  const left = lists.flatMap((list) => list.resources).filter((r) => inside.has(r.resource?.scope ?? ""));
  return createClient(ChangeService, transport).proposeChange({
    summary,
    edits: [
      ...left.map((r) => ({ resource: r.resource, delete: true })),
      { resource: { kind: "project", name: project.resource?.name ?? project.id }, delete: true },
    ],
  });
}

/** The project a new environment is for: the one chosen, or the only one there is. */
function projectFor(prompt: Prompt, projects: readonly Resource[] | undefined): Resource | undefined {
  if (prompt.kind !== "environment") return undefined;
  return prompt.project ?? (projects?.length === 1 ? projects[0] : undefined);
}

function Commands({ onClose }: { onClose: () => void }) {
  const { t, i18n } = useLingui();
  const strings = useUIStrings();
  const navigate = useNavigate();
  const sheet = useChangeSheet();
  const transport = useTransport();
  // The view keeps its identity between samples, so the list isn't rebuilt while you type.
  const { servers, fleet } = useFleetView();
  const version = useQuery(SystemService.method.getVersion, {}).data?.build?.version;
  const [theme, setTheme] = useThemePreference();
  const email = useSignedInUser()?.email;
  const projects = useQuery(ResourceService.method.listResources, { kind: "project" }).data?.resources;
  const environments = useQuery(ResourceService.method.listResources, { kind: "environment" }).data?.resources;
  const recent = useQuery(EventService.method.listEvents, { kinds: [EventKind.CHANGE], pageSize: 8 }).data?.events;
  const apps = useServices().data;
  const appWords = useAppWords();
  const [prompt, setPrompt] = useState<Prompt>();
  const [query, setQuery] = useState("");

  const commands = useMemo(() => {
    const list: Command[] = [];
    if (prompt)
      return promptItems(
        prompt,
        query.trim(),
        projects,
        i18n,
        setPrompt,
        (opening) => {
          onClose();
          sheet.open(opening);
        },
        (project, summary) => planDeletingProject(transport, project, environments ?? [], summary),
      );
    const pages = t`Pages`;
    list.push({
      id: "overview",
      group: pages,
      icon: "home",
      label: t`Open the overview`,
      run: () => navigate({ to: "/" }),
    });
    list.push({
      id: "apps",
      group: pages,
      icon: "service",
      label: t`Open the apps`,
      hint: apps ? String(apps.length) : undefined,
      run: () => navigate({ to: "/apps" }),
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
      const address = server.address;
      const where = address && isPrivateAddress(address) ? t`${address}, private` : address;
      list.push({
        id: `server:${server.id}`,
        group: fleet ? t`Servers` : pages,
        icon: "server",
        label: t`Open ${name}`,
        hint: [strings.status[serverStatus(server)], where].filter(Boolean).join(" · "),
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
      id: "variables",
      group: pages,
      icon: "key",
      label: t`Open the variables and secrets`,
      run: () => navigate({ to: "/variables" }),
    });
    list.push({
      id: "domains",
      group: pages,
      icon: "link",
      label: t`Open the domains`,
      run: () => navigate({ to: "/domains" }),
    });
    list.push({
      id: "databases",
      group: pages,
      icon: "database",
      label: t`Open the databases`,
      run: () => navigate({ to: "/databases" }),
    });
    list.push({
      id: "status",
      group: pages,
      icon: "gauge",
      label: t`Check the control plane's status`,
      hint: version ? shortVersion(version) : undefined,
      run: () => navigate({ to: "/settings/control-plane" }),
    });
    list.push({
      id: "profile",
      group: pages,
      icon: "user",
      label: t`Open your profile and security settings`,
      hint: email,
      run: () => navigate({ to: "/profile" }),
    });

    for (const app of apps ?? []) {
      const name = nameOf(app);
      list.push({
        id: `app:${app.id}`,
        group: t`Apps`,
        icon: "service",
        label: t`Open ${name}`,
        hint: [appWords.state(appState(app, servers)), imageOf(app)].join(" · "),
        run: () => navigate({ to: "/apps/$serviceId", params: { serviceId: app.id } }),
      });
    }

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
      list.push({
        id: `environment-variables:${env.id}`,
        group: t`Environments`,
        icon: "key",
        label: t`Open the variables and secrets of ${name}`,
        hint: env.displayName || undefined,
        run: () => navigate({ to: "/variables", search: { environment: env.id } }),
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
      id: "new-app",
      group: actions,
      icon: "plus",
      label: t`New app`,
      hint: t`Opens a plan`,
      run: () => navigate({ to: "/apps/new" }),
    });
    list.push({
      id: "create-project",
      group: actions,
      icon: "plus",
      label: t`Create a project`,
      hint: t`Opens a plan`,
      keepOpen: "step",
      run: () => setPrompt({ kind: "project" }),
    });
    list.push({
      id: "create-environment",
      group: actions,
      icon: "plus",
      label: t`Create an environment`,
      hint: t`Opens a plan`,
      keepOpen: "step",
      // Which project it is for is asked once the projects are in, and only if there is a choice.
      run: () => setPrompt({ kind: "environment" }),
    });
    // The organization keeps a project, so the only one has nothing to offer.
    if ((projects?.length ?? 0) > 1) {
      list.push({
        id: "delete-project",
        group: actions,
        icon: "trash",
        label: t`Delete a project`,
        hint: t`Opens a plan`,
        keepOpen: "step",
        run: () => setPrompt({ kind: "delete-project" }),
      });
    }

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
    apps,
    appWords,
    prompt,
    query,
    sheet,
    onClose,
    transport,
  ]);

  const project = prompt ? projectFor(prompt, projects) : undefined;
  const placeholder = !prompt
    ? undefined
    : prompt.kind === "project"
      ? t`Name the new project, such as shop`
      : prompt.kind === "delete-project"
        ? t`Which project do you want to delete?`
        : project
          ? t`Name the new environment, such as staging`
          : projects
            ? t`Which project is it for?`
            : t`Finding your projects`;

  // Each question has a bar of its own, which starts empty.
  const question = prompt ? `${prompt.kind}:${project?.id ?? (projects ? "choose" : "loading")}` : "commands";
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new question starts with an empty query, as its bar does
  useEffect(() => setQuery(""), [question]);

  return (
    <CommandBar
      key={question}
      open
      items={commands}
      placeholder={placeholder}
      onQueryChange={setQuery}
      onClose={onClose}
      onSelect={(item) => {
        const command = commands.find((c) => c.id === item.id);
        // A step asks its next question in a new, empty bar; a note leaves what you typed as it is.
        if (command?.keepOpen === "step") setQuery("");
        else if (!command?.keepOpen) onClose();
        command?.run();
      }}
    />
  );
}

/** The items while the bar asks for a name, or which project, for a new project or environment, or one to delete. */
function promptItems(
  prompt: Prompt,
  name: string,
  loaded: readonly Resource[] | undefined,
  i18n: I18n,
  setPrompt: (p: Prompt) => void,
  propose: (opening: Opening) => void,
  planDeleting: (project: Resource, summary: string) => ReturnType<typeof planDeletingProject>,
): Command[] {
  const projects = loaded ?? [];
  if (prompt.kind === "delete-project") {
    // Its plan says what goes with it, and the API says what keeps it, by name.
    return projects.map((project) => {
      const projectName = project.resource?.name ?? project.id;
      const label = i18n._(msg`Delete the project ${projectName}`);
      return {
        id: `prompt:delete:${project.id}`,
        icon: "trash",
        label,
        hint: i18n._(msg`Opens a plan`),
        run: () => propose({ kind: "plan", make: () => planDeleting(project, label) }),
      };
    });
  }
  if (prompt.kind === "environment" && !projectFor(prompt, loaded)) {
    // Until the projects are in, there is nothing to choose from yet.
    if (!loaded) return [];
    return projects.map((project) => {
      const projectName = project.resource?.name ?? project.id;
      return {
        id: `prompt:project:${project.id}`,
        icon: "folder",
        label: i18n._(msg`In ${projectName}`),
        hint: project.displayName || undefined,
        keepOpen: "step",
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
        keepOpen: "note",
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
  const project = projectFor(prompt, loaded);
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
