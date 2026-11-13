// SPDX-License-Identifier: AGPL-3.0-only

import type { Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { inSentence, type UIStrings, UIStringsProvider } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { I18nProvider } from "@lingui/react";
import { Trans, useLingui } from "@lingui/react/macro";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import { i18n } from "./i18n";

/** Gives the design system's own words (status names, button labels, empty states) in the active locale. */
function TranslatedUIStrings({ children }: { children: ReactNode }) {
  const { t, i18n: active } = useLingui();
  const strings = useMemo<UIStrings>(
    () => ({
      status: {
        healthy: t`Healthy`,
        degraded: t`Degraded`,
        down: t`Down`,
        deploying: t`Deploying`,
        stopped: t`Stopped`,
        running: t`Running`,
        unknown: t`Unknown`,
      },
      remove: (what) => t`Remove ${what}`,
      optional: t`Optional`,
      close: t`Close`,
      dismiss: t`Dismiss`,
      scopeBar: {
        label: t`Scope`,
        protectedEnvironment: t`Protected environment`,
      },
      commandBar: {
        label: t`Command bar`,
        placeholder: t`Search or run a command`,
        results: t`Results`,
        empty: (query) => t`Nothing matches “${query}”. Try a server, app or action name.`,
        move: t`to move`,
        run: t`to run`,
        cancel: t`Cancel`,
        scope: (scope) => (
          <Trans>
            In <b>{scope}</b>
          </Trans>
        ),
      },
      sparkline: (samples, value) => {
        const latest = active.number(value, { maximumFractionDigits: 2 });
        return plural(samples, {
          one: `Trend over # sample, latest ${latest}`,
          other: `Trend over # samples, latest ${latest}`,
        });
      },
      trend: t({ message: "trend", comment: "Read after a metric's name to name its chart, as in “CPU trend”." }),
      tableEmpty: t`Nothing here yet.`,
      fleetGrid: {
        servers: (count) => plural(count, { one: "# server", other: "# servers" }),
        count: (count, status) => {
          switch (status) {
            case "healthy":
              return t`${count} healthy`;
            case "degraded":
              return t`${count} degraded`;
            case "down":
              return t`${count} down`;
            case "deploying":
              return t`${count} deploying`;
            case "stopped":
              return t`${count} stopped`;
            case "running":
              return t`${count} running`;
            case "unknown":
              return t`${count} unknown`;
          }
        },
        tile: (name, status, cpu) => (cpu == null ? t`${name}, ${status}` : t`${name}, ${status}, CPU ${cpu}%`),
        group: t`Servers`,
        hint: t`Hover or focus a server to inspect it.`,
        cpu: t`CPU`,
        memory: t`Memory`,
      },
      logStream: {
        live: t`Following`,
        paused: t`Paused`,
        follow: t`Follow`,
        pause: t`Pause`,
        level: { warn: t`Warning`, error: t`Error` },
        newLines: (count) => plural(count, { one: "# new line", other: "# new lines" }),
        manyNewLines: t`10k+ new lines`,
        showAll: t`Show all of this line`,
        expand: t`Set out`,
        collapse: t`Put back`,
        cut: (size) => t`${size} more not kept`,
      },
      planDiff: {
        action: {
          create: t`Create`,
          update: t`Update in place`,
          replace: t`Replace`,
          destroy: t`Delete`,
        },
        actionDone: {
          create: t`Created`,
          update: t`Updated in place`,
          replace: t`Replaced`,
          destroy: t`Deleted`,
        },
        restart: { action: t`Restart`, done: t`Restarted` },
        kind: {
          project: t`project`,
          environment: t`environment`,
          app: t`app`,
          service: t`service`,
          container: t`container`,
          domain: t`domain`,
          secret: t`secret`,
          variable: t`variable`,
          database: t`database`,
          job: t`job`,
          volume: t`volume`,
          network: t`network`,
          backup: t`backup`,
          package: t`package`,
          server: t`server`,
          firewall: t`firewall`,
          route: t`route`,
          resource: t`resource`,
        },
        summary: {
          create: t`to create`,
          change: t`to update`,
          replace: t`to replace`,
          restart: t`to restart`,
          destroy: t`to delete`,
          ifNewer: t`to update if newer`,
        },
        summaryDone: {
          create: t`created`,
          change: t`updated`,
          replace: t`replaced`,
          restart: t`restarted`,
          destroy: t`deleted`,
        },
        hidden: t`hidden`,
        from: t`from`,
        to: t`to`,
        copyWhole: (shown) => t`Copy ${shown} in full`,
        copiedWhole: t`Copied in full`,
      },
      undo: t`Undo`,
      dnsRecord: {
        copy: (value) => t`Copy ${value}`,
        copied: (value) => t`Copied ${value}`,
        check: t`Check again`,
      },
      folderPicker: {
        change: t`Change`,
        changeNamed: (path) => (path ? t`Change the folder, ${path}` : t`Choose a folder`),
        done: t`Done`,
        none: t`No folder chosen`,
        path: t`Path`,
        lookingIn: t`Looking in`,
        foldersIn: (path) => t`Folders in ${path}`,
        reading: t`Reading…`,
        empty: t`No folders here`,
        cantRead: t`Can't read the server's folders now`,
        newIn: (path) => t`New folder in ${path}`,
        newName: t({ message: "name", comment: "Placeholder of the field that names a new folder." }),
        use: t({ message: "Use", comment: "The button that takes the new folder's name as the folder to use." }),
        cut: (shownCount, totalCount) => {
          const shown = active.number(shownCount);
          const total = active.number(totalCount);
          return totalCount > 0
            ? t`Showing ${shown} of ${total}. Type a name to find one.`
            : t`Showing the first ${shown}. Type a name to find one.`;
        },
        findIn: (path) => t`Find in ${path}`,
        findName: t({ message: "part of its name", comment: "Placeholder of the field that finds a folder by name." }),
        noMatch: (name) => t`No folder here holds “${name}”`,
        noneShown: t`No folders among these`,
        moreMatch: t`More match than this shows. Type more of the name.`,
      },
      connection: {
        title: t({
          message: "Connect",
          comment: "The heading of a card that lists how to connect to a database or an app.",
        }),
        revealShort: t`Reveal`,
        reveal: (label) => {
          const what = inSentence(label);
          return t`Reveal ${what}`;
        },
        hide: t`Hide`,
        hidden: t`Hidden`,
        copy: (label) => {
          const what = inSentence(label);
          return t`Copy ${what}`;
        },
        copied: (label) => t`${label} copied`,
        copyWithout: (label, secret) => {
          const [what, without] = [inSentence(label), inSentence(secret)];
          return t`Copy ${what}, without the ${without}`;
        },
        copiedWithout: (label, secret) => {
          const without = inSentence(secret);
          return t`${label} copied, without the ${without}`;
        },
      },
      advanced: t`Advanced`,
      portField: {
        checking: t({ message: "Checking", context: "a port on the server" }),
        free: t({ message: "Free", context: "a port on the server" }),
        taken: t({ message: "Taken", context: "a port on the server" }),
        takenBy: (by) => <Trans>{by} uses it</Trans>,
        use: (port) => t`Use ${port}`,
        range: t`Use a port from 1 to 65535.`,
      },
    }),
    [t, active],
  );
  return <UIStringsProvider strings={strings}>{children}</UIStringsProvider>;
}

export interface ProvidersProps {
  transport: Transport;
  queryClient: QueryClient;
  children: ReactNode;
}

/** Everything a screen needs: translations, the API and the query cache. */
export function Providers({ transport, queryClient, children }: ProvidersProps) {
  return (
    <I18nProvider i18n={i18n}>
      <TranslatedUIStrings>
        <TransportProvider transport={transport}>
          <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
        </TransportProvider>
      </TranslatedUIStrings>
    </I18nProvider>
  );
}
