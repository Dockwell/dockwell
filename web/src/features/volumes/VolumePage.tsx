// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { BackupService, ChangeService, type VolumeInfo, VolumeService } from "@dockwell/api";
import { Button, Callout, Panel, PHONE_QUERY, Tag, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useMay } from "../auth/may";
import { BackUpSheet } from "../backups/BackUpSheet";
import { PlaceLink } from "../backups/BackupPage";
import { BackupRows } from "../backups/BackupsPage";
import { useDestinations } from "../backups/DestinationsPanel";
import { PoliciesPanel, usePolicies } from "../backups/PoliciesPanel";
import { PolicySheet } from "../backups/PolicySheet";
import { useBackups } from "../backups/useBackups";
import { useChangeSheet } from "../changes/ChangeSheet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { RefLink } from "../shell/RefLink";
import "../backups/Backups.css";
import "./VolumePage.css";

/**
 * A volume's own page: what it holds on each server, with its files a step away,
 * the apps that mount it, how it's backed up, and Delete, through a plan, once no
 * app mounts it. Every name on it opens its own page (#566).
 */
export function VolumePage({ volumeId }: { volumeId: string }) {
  const { t } = useLingui();
  const query = useQuery(VolumeService.method.getVolume, { id: volumeId });
  const volume = query.data?.volume;
  const name = volume?.resource?.name ?? "";
  const phone = useMediaQuery(PHONE_QUERY);
  if (query.error) {
    return (
      <Page title={t`Volume`} back="/more">
        <Callout tone="critical" title={t`Dockwell can't read this volume`}>
          {sentence(query.error)}
        </Callout>
      </Page>
    );
  }
  if (!volume) {
    return (
      <Page title={t`Volume`} back="/more">
        <p className="volume-note" aria-busy="true">
          <Trans>Reading the volume</Trans>
        </p>
      </Page>
    );
  }
  return (
    <Page
      title={name}
      machineTitle
      back="/more"
      breadcrumb={
        <nav className="volume-crumbs" aria-label={t`Breadcrumb`}>
          <Link to="/projects">
            <Trans>Projects</Trans>
          </Link>
          <span aria-hidden="true">/</span>
          <PlaceLink environment={volume.resource?.scope ?? ""} />
        </nav>
      }
      lead={<Lead volume={volume} />}
      phoneSub={<Lead volume={volume} />}
      actions={<DeleteVolume volume={volume} />}
    >
      <div className="volume-page">
        <OnServers volume={volume} />
        <MountedBy volume={volume} />
        <Backups volume={volume} />
        {/* A phone's header has no room for actions, so Delete comes last, as the page reads. */}
        {phone ? (
          <div className="volume-actionbar">
            <DeleteVolume volume={volume} size="lg" />
          </div>
        ) : null}
      </div>
    </Page>
  );
}

/** What it is in a line: who mounts it, and its driver where that's not Docker's own. */
function Lead({ volume }: { volume: VolumeInfo }) {
  const { t } = useLingui();
  const count = volume.mountedBy.length;
  const driver = volume.driver;
  const mounts =
    count === 0 ? t`No app mounts it` : plural(count, { one: "Mounted by # app", other: "Mounted by # apps" });
  return <>{driver === "local" ? mounts : t`${mounts} · Driver ${driver}`}</>;
}

/** Each server it's on: what it holds there, and its files, a step away. */
function OnServers({ volume }: { volume: VolumeInfo }) {
  const { t, i18n } = useLingui();
  return (
    <Panel title={t`On its servers`}>
      <ul className="volume-servers">
        {volume.servers.map((s) => (
          <li key={s.serverId} className="volume-server">
            <RefLink kind="server" id={s.serverId}>
              {s.serverName}
            </RefLink>
            {s.unread ? (
              <span className="volume-muted">{s.unread}</span>
            ) : (
              <span className="volume-size">
                {formatBytes(s.sizeBytes, i18n.locale)}
                {s.containers > 0 ? (
                  <span className="volume-muted">
                    {" · "}
                    {plural(s.containers, { one: "# container uses it", other: "# containers use it" })}
                  </span>
                ) : null}
              </span>
            )}
            <span className="volume-spacer" />
            {s.path ? (
              <RouterButtonLink
                size="sm"
                variant="ghost"
                icon="folder"
                to="/servers/$serverId/files"
                params={{ serverId: s.serverId }}
                search={{ path: s.path }}
              >
                <Trans>Open its files</Trans>
              </RouterButtonLink>
            ) : null}
          </li>
        ))}
      </ul>
      {volume.hasOptions ? (
        <p className="volume-note">
          <Trans>Its driver's options stay hidden, as they may hold a password.</Trans>
        </p>
      ) : null}
    </Panel>
  );
}

/** The apps that mount it, each a link, with where. */
function MountedBy({ volume }: { volume: VolumeInfo }) {
  const { t } = useLingui();
  return (
    <Panel title={t`Mounted by`}>
      {volume.mountedBy.length === 0 ? (
        <p className="volume-muted">
          <Trans>No app mounts it, so deleting it stops nothing.</Trans>
        </p>
      ) : (
        <ul className="volume-mounts">
          {volume.mountedBy.map((m) => (
            <li key={`${m.serviceId}:${m.target}`} className="volume-mount">
              <RefLink kind="app" id={m.serviceId}>
                {m.service}
              </RefLink>
              <code className="volume-target">{m.target}</code>
              {m.readOnly ? <Tag words>{t`Read-only`}</Tag> : null}
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

/** How it's backed up: its schedules, its backups, and a backup now. */
function Backups({ volume }: { volume: VolumeInfo }) {
  const { t } = useLingui();
  const backups = useBackups(volume.id).data?.backups ?? [];
  const env = volume.resource?.scope ?? "";
  const name = volume.resource?.name ?? "";
  const policies = (usePolicies().data?.policies ?? []).filter(
    (p) => p.environmentId === env && p.spec?.volume === name,
  );
  const destinations = useDestinations().data?.destinations ?? [];
  const [taking, setTaking] = useState(false);
  const [scheduling, setScheduling] = useState(false);
  const may = useMay();
  const choice = [{ id: volume.id, name, environment: env }];
  const backUp = (
    <Button
      size="sm"
      icon="archive"
      onClick={() => setTaking(true)}
      {...may(BackupService.method.createBackup, { environment: env }).off}
    >
      <Trans>Back up now</Trans>
    </Button>
  );
  return (
    <section className="volume-backups" aria-labelledby="volume-backups">
      <div className="volume-section-head">
        <h2 className="dw-text-heading" id="volume-backups">
          <Trans>Backups</Trans>
        </h2>
        <span className="volume-spacer" />
        {backups.length > 0 ? backUp : null}
      </div>
      <PoliciesPanel policies={policies} onAdd={() => setScheduling(true)} />
      {backups.length === 0 ? (
        <EmptyState icon="archive" title={t`No backups of ${name} yet`} action={backUp} />
      ) : (
        <BackupRows backups={backups} caption={t`Backups of ${name}, newest first`} />
      )}
      {taking ? (
        <BackUpSheet volumes={choice} chosen={volume.id} destinations={destinations} onClose={() => setTaking(false)} />
      ) : null}
      {scheduling ? (
        <PolicySheet volumes={choice} destinations={destinations} onClose={() => setScheduling(false)} />
      ) : null}
    </section>
  );
}

/** Delete, through a plan, which says it destroys the data; off, with why, while an app mounts it. */
function DeleteVolume({ volume, size }: { volume: VolumeInfo; size?: "lg" }) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const may = useMay();
  const name = volume.resource?.name ?? "";
  const users = new Intl.ListFormat(i18n.locale, { type: "conjunction" }).format([
    ...new Set(volume.mountedBy.map((m) => m.service)),
  ]);
  const off =
    volume.mountedBy.length > 0
      ? t`${users} mounts it. Take it off first, then delete it.`
      : may(ChangeService.method.proposeChange, { environment: volume.resource?.scope ?? "" }).reason;
  return (
    <Button
      variant="danger"
      icon="trash"
      {...(size ? { size } : {})}
      off={off || undefined}
      onClick={() =>
        sheet.open({
          kind: "propose",
          summary: t`Delete the volume ${name}`,
          edits: [{ resource: { kind: "volume", scope: volume.resource?.scope ?? "", name }, delete: true }],
        })
      }
    >
      <Trans>Delete</Trans>
    </Button>
  );
}
