// SPDX-License-Identifier: AGPL-3.0-only

import type { Server } from "@dockwell/api";
import { TabNav, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { Fragment, type ReactNode } from "react";

import { RouterTabLink } from "../../lib/links";
import { EmptyState } from "../shell/EmptyState";
import { Page, type PageProps } from "../shell/Page";
import { Connecting, usePageReconnecting } from "./FleetNotes";
import { publicAddressOf } from "./network";
import { ServerAddress } from "./ServerAddress";
import { primaryAddress, useServerWords } from "./server";
import { useFleet, useFleetUnread } from "./useFleet";
import "./ServerFrame.css";

/** What a tab changes of the phone's header, for a screen of its own within it. */
export type PhoneHead = Pick<PageProps, "phoneTitle" | "phoneMachineTitle" | "phoneSub" | "backLink">;

interface ServerFrameProps {
  id: string;
  /** What the phone's header says instead of the server's name. */
  phoneHead?: PhoneHead | undefined;
  /** Leaves the tabs out on a phone, for a screen that takes all of it. */
  noTabs?: boolean;
  /** The tab's own screen, for the server. */
  children: (server: Server) => ReactNode;
}

/**
 * One server's own page, which exists from the first server on, whichever tab
 * is open: its name, what it is, and its tabs, each a route of its own.
 */
export function ServerFrame({ id, phoneHead, noTabs = false, children }: ServerFrameProps) {
  const { t } = useLingui();
  const state = useFleet();
  const status = usePageReconnecting(state.status);
  if (state.status === "connecting") {
    return (
      <Page title={t`Server`}>
        <Connecting error={state.error} />
      </Page>
    );
  }
  const server = state.servers.find((s) => s.id === id);
  if (!server) {
    return (
      <Page title={t`Server`} status={status}>
        <EmptyState
          icon="server"
          title={<Trans>No server with this ID</Trans>}
          action={
            <Link to="/" className="dw-btn dw-btn-secondary dw-btn-lg">
              <Trans>Go to the overview</Trans>
            </Link>
          }
        >
          <p>
            <Trans>
              <code>{id}</code> may have been removed, or the link may be wrong.
            </Trans>
          </p>
        </EmptyState>
      </Page>
    );
  }
  return (
    <ServerPage server={server} fleet={state.servers.length > 1} status={status} phoneHead={phoneHead} noTabs={noTabs}>
      {children(server)}
    </ServerPage>
  );
}

interface ServerPageProps {
  server: Server;
  fleet: boolean;
  status: ReactNode;
  phoneHead: PhoneHead | undefined;
  noTabs: boolean;
  children: ReactNode;
}

function ServerPage({ server, fleet, status, phoneHead, noTabs, children }: ServerPageProps) {
  const { t } = useLingui();
  const unread = useFleetUnread();
  const { up, phoneSub } = useServerWords(server);
  const inv = server.inventory;
  const address = publicAddressOf(server) ?? primaryAddress(inv?.addresses ?? []);
  // The hostname only when it says something the name and address don't.
  const hostname = inv?.hostname && inv.hostname !== server.name && inv.hostname !== address ? inv.hostname : "";
  const parts: { key: string; node: ReactNode }[] = [
    { key: "hostname", node: hostname },
    { key: "address", node: address ? <ServerAddress address={address} className="server-address" /> : "" },
    { key: "up", node: up },
  ].filter((part) => part.node);
  const lead =
    parts.length > 0
      ? parts.map(({ key, node }, i) => (
          <Fragment key={key}>
            {i > 0 ? " · " : null}
            {node}
          </Fragment>
        ))
      : undefined;
  return (
    <Page
      title={server.name}
      machineTitle
      badge={server.controlServer ? <Tag>{t`Runs the control plane`}</Tag> : null}
      lead={lead}
      phoneSub={unread ? t`Unknown` : phoneSub}
      back={fleet ? "/servers" : undefined}
      status={status}
      className="server-page"
      {...phoneHead}
    >
      {noTabs ? null : (
        <TabNav label={t`Server ${server.name}`}>
          <RouterTabLink to="/servers/$serverId" params={{ serverId: server.id }} activeOptions={{ exact: true }}>
            <Trans>Overview</Trans>
          </RouterTabLink>
          <RouterTabLink to="/servers/$serverId/files" params={{ serverId: server.id }}>
            <Trans>Files</Trans>
          </RouterTabLink>
          <RouterTabLink to="/servers/$serverId/terminal" params={{ serverId: server.id }}>
            <Trans>Terminal</Trans>
          </RouterTabLink>
        </TabNav>
      )}
      {children}
    </Page>
  );
}
