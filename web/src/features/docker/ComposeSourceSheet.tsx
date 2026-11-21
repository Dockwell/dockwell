// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { FileService, FileType } from "@dockwell/api";
import { Button, Callout, Icon, SelectField, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";

const MAX_COMPOSE = 256 * 1024;
const yamlFile = /^[^/\\]+\.ya?ml$/i;

export interface ComposeSource {
  file: string;
  directory: string;
}

export function ComposeSourceSheet({
  servers,
  initialDirectory = "",
  onRead,
  onClose,
}: {
  servers: readonly { id: string; name: string }[];
  initialDirectory?: string;
  onRead: (source: ComposeSource, server: string) => void;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const [server, setServer] = useState(servers[0]?.id ?? "");
  const [typed, setTyped] = useState(initialDirectory || "/");
  const [directory, setDirectory] = useState(initialDirectory || "/");
  const [page, setPage] = useState("");
  const [reading, setReading] = useState(false);
  const [failure, setFailure] = useState<string>();
  const listing = useQuery(
    FileService.method.listFiles,
    { serverId: server, path: directory, page },
    {
      enabled: server !== "" && directory !== "",
      retry: false,
    },
  );
  const children =
    listing.data?.entries.filter(
      (entry) =>
        !entry.unreadableName &&
        !entry.refused &&
        (entry.type === FileType.DIRECTORY ||
          (entry.type === FileType.FILE && yamlFile.test(entry.name) && entry.sizeBytes <= BigInt(MAX_COMPOSE))),
    ) ?? [];
  const open = (path: string) => {
    setFailure(undefined);
    setTyped(path);
    setDirectory(path);
    setPage("");
  };
  const read = async (name: string, parent: string) => {
    setReading(true);
    setFailure(undefined);
    try {
      const path = parent === "/" ? `/${name}` : `${parent}/${name}`;
      const file = await createClient(FileService, transport).readFile({ serverId: server, path });
      if (
        file.path !== path ||
        file.file?.type !== FileType.FILE ||
        file.binary ||
        file.truncated ||
        file.file.sizeBytes > BigInt(MAX_COMPOSE)
      ) {
        setFailure(t`The source changed or is too large. Choose a regular Compose file in this folder.`);
        return;
      }
      onRead({ file: file.content, directory: parent }, server);
    } catch (error) {
      setFailure(sentence(error));
    } finally {
      setReading(false);
    }
  };
  const path = listing.data?.path ?? directory;
  return (
    <Sheet
      open
      title={t`Import from a server folder`}
      onClose={onClose}
      footer={
        <Button variant="ghost" onClick={onClose}>
          <Trans>Cancel</Trans>
        </Button>
      }
    >
      <div className="compose-sheet-fields">
        {servers.length > 1 ? (
          <SelectField
            label={t`Server`}
            value={server}
            options={servers.map((s) => ({ value: s.id, label: s.name }))}
            onChange={(event) => {
              setServer(event.target.value);
              open("/");
            }}
          />
        ) : null}
        <form
          className="compose-folder-bar"
          onSubmit={(event) => {
            event.preventDefault();
            open(typed.trim());
          }}
        >
          <TextField label={t`Folder path`} mono value={typed} onChange={(event) => setTyped(event.target.value)} />
          <Button type="submit" icon="folder" disabled={!typed.startsWith("/")}>
            <Trans>Open folder</Trans>
          </Button>
        </form>
        {failure || listing.error ? (
          <Callout tone="critical" title={t`Can't open this source`}>
            {failure ?? sentence(listing.error)}
          </Callout>
        ) : null}
        {listing.data ? (
          <ul className="compose-source-list">
            {path !== "/" ? (
              <li>
                <Button variant="ghost" icon="folder" onClick={() => open(path.slice(0, path.lastIndexOf("/")) || "/")}>
                  <Trans>Parent folder</Trans>
                </Button>
              </li>
            ) : null}
            {children.map((entry) => (
              <li key={entry.name}>
                <Button
                  variant="ghost"
                  icon={entry.type === FileType.DIRECTORY ? "folder" : "file"}
                  disabled={reading}
                  onClick={() =>
                    entry.type === FileType.DIRECTORY
                      ? open(path === "/" ? `/${entry.name}` : `${path}/${entry.name}`)
                      : void read(entry.name, path)
                  }
                >
                  {entry.name}
                </Button>
              </li>
            ))}
          </ul>
        ) : listing.isPending ? (
          <p aria-live="polite">
            <Icon name="refresh" size={14} className="dw-spin" /> <Trans>Reading folder</Trans>
          </p>
        ) : null}
        {listing.data && children.length === 0 ? (
          <p className="compose-muted">
            <Trans>No Compose files or folders on this page</Trans>
          </p>
        ) : null}
        {listing.data?.nextPage ? (
          <Button variant="ghost" onClick={() => setPage(listing.data?.nextPage ?? "")}>
            <Trans>Next page</Trans>
          </Button>
        ) : null}
      </div>
    </Sheet>
  );
}
