// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Container, ServiceInfo } from "@dockwell/api";
import { Button, Icon, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { formatBytes, formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { environmentOf } from "../apps/app";
import { BackUpSheet } from "./BackUpSheet";
import { latestDone, useBackups, useVolumes } from "./useBackups";
import { useMethodWord } from "./words";
import "./Backups.css";

/** The volumes an app mounts, each with its newest backup, and the way to take one. */
export function AppBackups({ service, app }: { service: ServiceInfo; app: Container | undefined }) {
  const { t, i18n } = useLingui();
  const environment = environmentOf(service);
  const all = useVolumes();
  const backups = useBackups().data?.backups ?? [];
  const method = useMethodWord();
  const [taking, setTaking] = useState<string>();
  const mounted = new Set((app?.mounts ?? []).flatMap((m) => (m.source.case === "volume" ? [m.source.value] : [])));
  const volumes = all.filter((v) => v.environment === environment && mounted.has(v.name));
  if (volumes.length === 0) return null;
  return (
    <Panel
      title={t`Backups`}
      flush
      actions={
        <RouterButtonLink to="/backups" variant="ghost" size="sm">
          <Trans>All backups</Trans>
        </RouterButtonLink>
      }
    >
      <ul className="app-backups">
        {volumes.map((volume) => {
          const latest = latestDone(backups, volume.id);
          return (
            <li key={volume.id} className="app-backup">
              <span className="app-backup-name">
                <Icon name="archive" size={14} />
                <code>{volume.name}</code>
              </span>
              <span className="app-backup-latest">
                {latest?.startedAt
                  ? [
                      formatDateTime(timestampDate(latest.startedAt), i18n.locale),
                      method(latest.method, latest.database),
                      formatBytes(latest.sizeBytes, i18n.locale),
                    ].join(" · ")
                  : t`Not backed up yet`}
              </span>
              <Button size="sm" icon="archive" onClick={() => setTaking(volume.id)}>
                <Trans>Back up now</Trans>
              </Button>
            </li>
          );
        })}
      </ul>
      {taking ? <BackUpSheet volumes={volumes} chosen={taking} onClose={() => setTaking(undefined)} /> : null}
    </Panel>
  );
}
