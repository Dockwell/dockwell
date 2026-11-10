// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type FileEntry, FileService, FileType } from "@dockwell/api";
import { Callout, Icon, Panel, Table, type TableColumn } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useMemo } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes, formatDay } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { join } from "../files/paths";
import { EmptyState } from "../shell/EmptyState";
import { useWebsiteValue } from "./WebsiteLayout";
import { kindOf, targetOf } from "./website";

/** A folder's own entries first, then its files, each by name, as Files lists them. */
function sorted(entries: readonly FileEntry[]): FileEntry[] {
  const folder = (e: FileEntry) =>
    e.type === FileType.DIRECTORY || (e.type === FileType.SYMLINK && e.linkType === FileType.DIRECTORY);
  return [...entries].sort((a, b) => Number(folder(b)) - Number(folder(a)) || a.name.localeCompare(b.name));
}

/**
 * A website's files: what's at the top of its folder, as Files reads it on its server,
 * each opening there, where it's edited and saved. A website that sends its requests
 * on has no folder of its own.
 */
export function WebsiteFiles() {
  const { t, i18n } = useLingui();
  const { row } = useWebsiteValue();
  const { site } = row;
  const kind = kindOf(site.spec);
  const root = targetOf(site.spec);
  const has = kind === "folder" || kind === "php";
  const listed = useQuery(
    FileService.method.listFiles,
    { serverId: site.serverId, path: root },
    { enabled: has && site.serverId !== "", retry: false, refetchOnWindowFocus: false },
  );
  const columns = useMemo<TableColumn<FileEntry>[]>(
    () => [
      {
        key: "name",
        header: t`Name`,
        render: (e) => {
          const folder =
            e.type === FileType.DIRECTORY || (e.type === FileType.SYMLINK && e.linkType === FileType.DIRECTORY);
          return (
            <span className="website-file">
              <Icon name={folder ? "folder" : "file"} size={16} />
              <Link
                to="/servers/$serverId/files"
                params={{ serverId: site.serverId }}
                search={folder ? { path: join(root, e.name) } : { path: root, file: join(root, e.name) }}
                className="website-file-name"
              >
                {e.name}
              </Link>
            </span>
          );
        },
      },
      {
        key: "size",
        header: t`Size`,
        width: 120,
        align: "right",
        muted: true,
        render: (e) => (e.type === FileType.FILE ? formatBytes(e.sizeBytes, i18n.locale) : "—"),
      },
      {
        key: "mode",
        header: t`Mode`,
        width: 132,
        mono: true,
        muted: true,
        render: (e) => e.mode,
      },
      {
        key: "changed",
        header: t`Changed`,
        width: 104,
        align: "right",
        muted: true,
        render: (e) => (e.modifyTime ? formatDay(timestampDate(e.modifyTime), i18n.locale) : "—"),
      },
    ],
    [t, i18n, site.serverId, root],
  );

  if (!has) {
    return (
      <EmptyState icon="folder" title={<Trans>No folder of its own</Trans>}>
        <p>
          <Trans>This website sends its requests on, so it has no files here. What it sends to keeps its own.</Trans>
        </p>
      </EmptyState>
    );
  }
  const open = (
    <RouterButtonLink
      to="/servers/$serverId/files"
      params={{ serverId: site.serverId }}
      search={{ path: root }}
      size="sm"
    >
      <Trans>Open in Files</Trans>
    </RouterButtonLink>
  );
  return (
    <div className="website-tab">
      <Panel
        title={
          <span className="website-files-title">
            <Icon name="folder" size={16} />
            <code>{root}</code>
          </span>
        }
        actions={open}
        flush
      >
        {listed.isPending ? (
          <div className="website-files-loading" aria-busy="true">
            <span className="dw-sr">
              <Trans>Loading its files</Trans>
            </span>
          </div>
        ) : listed.error ? (
          reasonOf(listed.error) === "not_found" ? (
            <div className="website-files-note">
              <Callout tone="warning" title={t`The folder isn't on the server`} />
            </div>
          ) : (
            <div className="website-files-note">
              <Callout tone="warning">{sentence(listed.error)}</Callout>
            </div>
          )
        ) : listed.data.entries.length === 0 ? (
          <EmptyState icon="folder" title={<Trans>The folder is empty</Trans>}>
            <p>
              <Trans>Put the site's files in it with scp or rsync. Files edits files that are there.</Trans>
            </p>
          </EmptyState>
        ) : (
          <Table<FileEntry>
            className="website-files"
            columns={columns}
            rows={sorted(listed.data.entries)}
            rowKey="name"
            caption={t`What's in ${root}`}
          />
        )}
      </Panel>
    </div>
  );
}
