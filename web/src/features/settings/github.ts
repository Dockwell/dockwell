// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  type GitHubAppInfo,
  type GitHubDelivery,
  GitHubDelivery_State,
  type GitHubInstallation,
  GitProviderService,
} from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";

import { useMay } from "../auth/may";

/** The GitHub Apps Dockwell made, with their installations and whether GitHub reaches the panel (ADR 0098). */
export function useGitHubApps(enabled = true) {
  const allowed = useMay()(GitProviderService.method.listGitProviders).allowed;
  return useQuery(GitProviderService.method.listGitProviders, {}, { enabled: enabled && allowed });
}

/** The repositories a project's apps may build from through a GitHub App, as GitHub last listed them. */
export function useGitRepositories(project: string) {
  const allowed = useMay()(GitProviderService.method.listGitRepositories, project ? { project } : undefined).allowed;
  return useQuery(
    GitProviderService.method.listGitRepositories,
    { project },
    { enabled: allowed && project !== "", staleTime: 60_000, refetchOnWindowFocus: false },
  );
}

/** The App and installation a build's git login names, as "<App name>/<installation ID>", where they're listed. */
export function installationOf(
  apps: readonly GitHubAppInfo[] | undefined,
  login: string,
): { app: GitHubAppInfo; installation: GitHubInstallation | undefined } | undefined {
  const [name, id] = login.split("/");
  const app = apps?.find((a) => a.name === name);
  if (!app) return undefined;
  return { app, installation: app.installations.find((i) => String(i.id) === id) };
}

/** Where GitHub installs an App on more repositories or accounts. */
export function installUrl(app: GitHubAppInfo): string {
  return `${app.htmlUrl}/installations/new`;
}

/** What a delivery line says: GitHub reaches the panel, or Dockwell checks for pushes itself, and how often. */
export interface DeliveryWords {
  text: string;
  /** True while GitHub's deliveries don't reach the panel: a domain would end it. */
  unreached: boolean;
}

/**
 * Whether GitHub's pushes reach the panel, as the App's own deliveries last said, in
 * one line: where they don't, Dockwell checks for pushes itself, or waits as long as
 * GitHub asked (ADR 0098).
 */
export function useDeliveryWords(): (delivery: GitHubDelivery | undefined) => DeliveryWords {
  const { t, i18n } = useLingui();
  return (delivery) => {
    switch (delivery?.state) {
      case GitHubDelivery_State.POLLING: {
        const minutes = Math.max(1, Math.round((delivery.checkEverySeconds || 60) / 60));
        return {
          text:
            minutes === 1
              ? t`GitHub can't reach this panel, so Dockwell checks for pushes every minute.`
              : t`GitHub can't reach this panel, so Dockwell checks for pushes every ${minutes} minutes.`,
          unreached: true,
        };
      }
      case GitHubDelivery_State.BACKING_OFF: {
        const until = delivery.backoffUntil
          ? i18n.date(timestampDate(delivery.backoffUntil), { hour: "numeric", minute: "2-digit" })
          : "";
        return {
          text: until
            ? t`GitHub asked Dockwell to slow down, so pushes aren't checked until ${until}.`
            : t`GitHub asked Dockwell to slow down, so pushes aren't checked for now.`,
          unreached: true,
        };
      }
      default:
        return { text: t`Pushes come through the GitHub App.`, unreached: false };
    }
  };
}
