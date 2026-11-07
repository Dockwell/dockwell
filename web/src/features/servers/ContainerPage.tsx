// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { ChangeState, ComposeService, ContainerService, ContainerState, type ServerContainer } from "@dockwell/api";
import { Button, Icon, Panel, SelectField, StatusBadge, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { useRef, useState } from "react";
import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { usePlaces } from "../apps/useServices";
import { useChangeSheet } from "../changes/ChangeSheet";
import { LogsPage, type LogsSearch } from "../logs/LogsPage";
import { LogsSearchButton } from "../logs/phoneSearch";
import { Page } from "../shell/Page";
import { useMadeWords } from "./ContainersPanel";
import {
  labelFor,
  mountSource,
  portWords,
  READ_EVERY_MS,
  serviceNameFor,
  statusOf,
  useContainerStateWord,
} from "./containers";
import { useFleet } from "./useFleet";
import "./ContainersPanel.css";

/**
 * A container Dockwell didn't make: what it is and what it prints, which reads like an
 * app's logs. Dockwell only reads it, and this page has nothing that changes it.
 */
export function ContainerPage({
  serverId,
  name,
  search,
  onSearch,
}: {
  serverId: string;
  name: string;
  search: LogsSearch;
  onSearch: (next: LogsSearch) => void;
}) {
  const { t } = useLingui();
  const fleet = useFleet();
  const serverName = (fleet.status === "connecting" ? [] : fleet.servers).find((s) => s.id === serverId)?.name ?? "";
  const stateWord = useContainerStateWord();
  const list = useQuery(
    ContainerService.method.listContainers,
    { server: serverId },
    { refetchInterval: READ_EVERY_MS, retry: false },
  );
  // What it last read can't stand once it can't be read, as on the server's page.
  const container = list.error ? undefined : list.data?.containers.find((c) => c.name === name);
  const searchButton = useRef<(() => void) | null>(null);
  const back = (
    <Link
      to="/servers/$serverId"
      params={{ serverId }}
      className="mobile-header-button"
      aria-label={t`Back to the server`}
    >
      <Icon name="arrow-left" size={20} />
    </Link>
  );
  return (
    <LogsSearchButton value={searchButton}>
      <Page
        title={name}
        machineTitle
        phoneMachineTitle
        breadcrumb={
          <nav className="container-crumbs" aria-label={t`Breadcrumb`}>
            <Link to="/servers/$serverId" params={{ serverId }}>
              {serverName || t`Server`}
            </Link>
            <span aria-hidden="true">/</span>
            <span aria-current="page">{name}</span>
          </nav>
        }
        badge={
          container ? (
            <StatusBadge status={statusOf(container.state)} label={stateWord(container.state)} labelIsState />
          ) : null
        }
        lead={container ? <code>{container.image}</code> : undefined}
        actions={
          container?.state === ContainerState.RUNNING ? (
            <RouterButtonLink
              to="/servers/$serverId/terminal"
              params={{ serverId }}
              search={{ container: name }}
              icon="terminal"
            >
              <Trans>Open a shell</Trans>
            </RouterButtonLink>
          ) : undefined
        }
        phoneSub={container ? stateWord(container.state) : undefined}
        backLink={back}
        phoneAction={
          <button
            type="button"
            className="mobile-header-button"
            aria-label={t`Search logs`}
            onClick={() => searchButton.current?.()}
          >
            <Icon name="search" size={20} />
          </button>
        }
      >
        {list.error ? (
          <Panel flush title={t`What Docker says of it`}>
            <p className="containers-note">{sentence(list.error)}</p>
          </Panel>
        ) : container ? (
          <Facts container={container} />
        ) : null}
        {container ? (
          <TakeItOn
            serverId={serverId}
            container={container}
            inProject={
              list.data?.containers.filter(
                (c) => c.composeProject !== "" && c.composeProject === container.composeProject && !c.oneOff,
              ).length ?? 0
            }
          />
        ) : null}
        <LogsPage id={name} server={serverId} search={search} onSearch={onSearch} />
      </Page>
    </LogsSearchButton>
  );
}

/**
 * Taking a container on: a name for the service, where it goes when there is a choice,
 * and a plan like any other. What it says first is what a person worries about: it
 * doesn't restart, and nothing of its environment shows.
 */
function TakeItOn({
  serverId,
  container,
  inProject,
}: {
  serverId: string;
  container: ServerContainer;
  /** How many containers of its Compose project are here, which can be taken on together. */
  inProject: number;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const places = usePlaces();
  const [name, setName] = useState(() => serviceNameFor(container));
  const [environment, setEnvironment] = useState("");
  const project = container.composeProject;
  // What came late to a project that was taken on belongs with the rest of it.
  const joins = project ? places.environments.find((e) => e.project === labelFor(project)) : undefined;
  const chosen = environment || joins?.id || places.defaultEnvironment?.id || "";
  const openProject = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(ComposeService, transport).adoptCompose({ name: project, server: serverId }),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) void navigate({ to: "/apps" });
      },
    });
  const open = () =>
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(ContainerService, transport).adoptContainer({
          id: container.name,
          server: serverId,
          service: name,
          environment: chosen,
        }),
      onClose: (change) => {
        const made = change?.plan?.resources[0]?.id;
        if (change?.state === ChangeState.APPLIED && made) {
          void navigate({ to: "/apps/$serviceId", params: { serviceId: made } });
        }
      },
    });
  return (
    <Panel title={t`Take it on`}>
      <div className="container-take-on">
        <p>
          <Trans>
            Dockwell runs it as a service from then on, so changing its image is a plan you can undo, and it keeps its
            volumes. Taking it on doesn't restart it, and its environment comes in sealed, so no plan or log shows it.
          </Trans>
        </p>
        {joins ? (
          <p>
            <Trans>Its Compose project was taken on already, so this joins it.</Trans>
          </p>
        ) : null}
        <TextField label={t`Name as a service`} value={name} onChange={(event) => setName(event.target.value)} />
        {places.environments.length > 1 ? (
          <SelectField
            label={places.showsEnvironment ? t`Environment` : t`Project`}
            value={chosen}
            options={places.environments.map((e) => ({ value: e.id, label: e.label }))}
            onChange={(event) => setEnvironment(event.target.value)}
          />
        ) : null}
        <div className="container-take-on-actions">
          <Button onClick={open} disabled={name === ""}>
            <Trans>Plan taking it on</Trans>
          </Button>
          {inProject > 1 ? (
            <Button variant="secondary" onClick={openProject}>
              {t`Plan taking on all ${inProject} of ${project}`}
            </Button>
          ) : null}
        </div>
      </div>
    </Panel>
  );
}

function Facts({ container }: { container: ServerContainer }) {
  const { t } = useLingui();
  const made = useMadeWords()(container);
  const ports = container.ports.map(portWords);
  const { mounts, morePorts, moreMounts } = container;
  return (
    <Panel title={t`What Docker says of it`}>
      <dl className="container-facts">
        <dt>
          <Trans>Status</Trans>
        </dt>
        <dd>
          <code>{container.status}</code>
        </dd>
        {made ? (
          <>
            <dt>
              <Trans>Made</Trans>
            </dt>
            <dd>{made}</dd>
          </>
        ) : null}
        {container.composeProject ? (
          <>
            <dt>
              <Trans>Compose project</Trans>
            </dt>
            <dd>
              <code>{container.composeProject}</code>
              {container.composeService ? (
                <>
                  {" · "}
                  <code>{container.composeService}</code>
                </>
              ) : null}
            </dd>
          </>
        ) : null}
        <dt>
          <Trans>Ports</Trans>
        </dt>
        <dd>
          {ports.length > 0 ? (
            <ul>
              {ports.map((port) => (
                <li key={port}>
                  <code>{port}</code>
                </li>
              ))}
              {morePorts > 0 ? <li>{t`and ${morePorts} more`}</li> : null}
            </ul>
          ) : (
            <Trans>It publishes none now.</Trans>
          )}
        </dd>
        <dt>
          <Trans>Volumes</Trans>
        </dt>
        <dd>
          {mounts.length > 0 ? (
            <ul>
              {mounts.map((mount) => (
                <li key={mount.target}>
                  <code>
                    {mountSource(mount)}→{mount.target}
                  </code>
                  {mount.readOnly ? ` (${t`read-only`})` : ""}
                </li>
              ))}
              {moreMounts > 0 ? <li>{t`and ${moreMounts} more`}</li> : null}
            </ul>
          ) : (
            <Trans>It mounts none.</Trans>
          )}
        </dd>
      </dl>
    </Panel>
  );
}
