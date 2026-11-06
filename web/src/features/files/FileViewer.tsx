// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { FileService } from "@dockwell/api";
import { Button, Callout, Icon, Panel, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { useMemo } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { lineCount, lineEnding, lineNumbers, nameOf } from "./paths";

interface FileViewerProps {
  serverId: string;
  path: string;
  /** The folder on show, which closing the file goes back to. */
  folder: string;
}

/**
 * A file's text, as the server has it. Reading one goes on record, so it reads
 * once each time the file opens: never on its own when the window takes focus,
 * and nothing of it stays in the cache once it closes. What it shows is text in
 * a pre, whatever the file's name says: nothing in a file runs here.
 */
export function FileViewer({ serverId, path, folder }: FileViewerProps) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const navigate = useNavigate();
  const query = useQuery(
    FileService.method.readFile,
    { serverId, path },
    { retry: false, gcTime: 0, staleTime: Number.POSITIVE_INFINITY, refetchOnWindowFocus: false },
  );
  const name = nameOf(path);
  const read = query.data;
  const file = read?.file;
  const content = read?.content ?? "";
  const lines = useMemo(() => lineCount(content), [content]);
  const numbers = useMemo(() => lineNumbers(lines), [lines]);
  const close = () => {
    void navigate({ to: "/servers/$serverId/files", params: { serverId }, search: { path: folder } });
  };
  const size = file ? formatBytes(file.sizeBytes, i18n.locale) : "";
  const body = () => {
    if (query.error) {
      const reason = reasonOf(query.error);
      const refused = reason === "file_refused";
      const title = refused
        ? t`Dockwell doesn't open this file`
        : reason === "path_not_answering"
          ? t`This path isn't answering`
          : t`Can't read this file`;
      return (
        <div className="files-note">
          <Callout tone={refused ? "warning" : "critical"} title={title}>
            {sentence(query.error)}
          </Callout>
        </div>
      );
    }
    if (!read) {
      return (
        <p className="files-reading" aria-busy="true">
          <Trans>Reading {name}</Trans>
        </p>
      );
    }
    if (read.binary) {
      return (
        <div className="files-note">
          <Callout tone="info" title={t`This isn't a text file`}>
            <Trans>
              It's {size}, and it isn't UTF-8 text, so there's nothing to show here. Connect to the server over SSH to
              work with it.
            </Trans>
          </Callout>
        </div>
      );
    }
    return (
      <>
        {read.truncated ? (
          <div className="files-note">
            <Callout tone="warning" title={t`Showing the first 1 MB`}>
              <Trans>
                This file is {size}, so the rest of it stays on the server. Connect to the server over SSH to see it
                all.
              </Trans>
            </Callout>
          </div>
        ) : null}
        {/* biome-ignore lint/a11y/noNoninteractiveTabindex: it scrolls, so a keyboard has to reach it */}
        <section className="files-code" aria-label={t`Contents of ${name}`} tabIndex={0}>
          <pre aria-hidden="true" className="files-gutter">
            {numbers}
          </pre>
          <pre className="files-text">{content}</pre>
        </section>
        <div className="files-foot">
          <span>{lines === 1 ? <Trans>1 line</Trans> : <Trans>{lines} lines</Trans>}</span>
          <span>UTF-8</span>
          <span>{lineEnding(content)}</span>
        </div>
      </>
    );
  };
  return (
    <Panel
      flush
      className="files-viewer"
      title={
        phone ? undefined : (
          <span className="files-viewer-title" title={path}>
            <span className="files-icon">
              <Icon name="file" size={16} />
            </span>
            <span className="files-viewer-name">{name}</span>
          </span>
        )
      }
      actions={
        phone ? undefined : <Button variant="ghost" size="sm" icon="x" aria-label={t`Close ${name}`} onClick={close} />
      }
    >
      {file ? (
        <p className="files-facts">
          <code>{file.owner}</code>
          <code>{file.mode}</code>
          <span>{size}</span>
        </p>
      ) : null}
      {body()}
    </Panel>
  );
}
