// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type FileEntry, FileService, FileType, type ReadFileResponse } from "@dockwell/api";
import { Button, ButtonLink, Callout, cx, Icon, Loading, Panel, Skeleton } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { CopyButton } from "../servers/CopyButton";
import { useControlPlaneNow } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { IMAGE_PREVIEW_MAX_BYTES, imageMimeOf, TEXT_PREVIEW_LINES, TEXT_PREVIEW_MAX_BYTES } from "./preferences";
import { archiveKind } from "./transfer";

export interface FileDetailsPaneProps {
  serverId: string;
  entry: FileEntry;
  path: string;
  isFolder: boolean;
  /** Download URL for a file or directory, passed from FilesPage. */
  downloadUrl?: string;
  fullWindow?: boolean;
  onToggleFullWindow?: () => void;
  onOpen: () => void;
  onRename: () => void;
  onMove: () => void;
  onCompress: () => void;
  onExtract?: () => void;
  onDelete: () => void;
  onClose: () => void;
  onPreviewRead?: (path: string, response: ReadFileResponse) => void;
  cachedRead?: ReadFileResponse;
}

type ImageState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "tooLarge" }
  | { kind: "ready"; url: string }
  | { kind: "error"; message: string };

type TextState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "ready"; read: ReadFileResponse }
  | { kind: "error"; message: string };

/**
 * Details and preview pane for a selected file or folder on the right side of
 * the file manager, replacing the old full-width button sheet.
 */
export function FileDetailsPane({
  serverId,
  entry,
  path,
  isFolder,
  downloadUrl,
  fullWindow = false,
  onToggleFullWindow,
  onOpen,
  onRename,
  onMove,
  onCompress,
  onExtract,
  onDelete,
  onClose,
  onPreviewRead,
  cachedRead,
}: FileDetailsPaneProps) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(FileService, transport), [transport]);
  const now = useControlPlaneNow();
  const words = useTimeWords();

  const changeable = !entry.refused && !entry.unreadableName;
  const imageMime = !isFolder && entry.type === FileType.FILE ? imageMimeOf(entry.name) : undefined;
  const isArchive = !isFolder && entry.type === FileType.FILE && archiveKind(entry.name) !== undefined;
  const canPreviewText =
    changeable &&
    !isFolder &&
    !imageMime &&
    !isArchive &&
    (entry.type === FileType.FILE || (entry.type === FileType.SYMLINK && entry.linkType === FileType.FILE)) &&
    entry.sizeBytes <= BigInt(TEXT_PREVIEW_MAX_BYTES);

  const [imageState, setImageState] = useState<ImageState>({ kind: "idle" });
  const [imageDims, setImageDims] = useState<{ w: number; h: number }>();
  const [imageBroken, setImageBroken] = useState(false);
  const [textState, setTextState] = useState<TextState>({ kind: "idle" });

  // Read image bytes through the read path (allowBinary: true), never downloadFile,
  // so previews leave no "downloaded" entry in Activity and take no download slot.
  // Always display strictly inside <img> via a data: URL (compatible with img-src 'self' data: CSP, never inline SVG).
  useEffect(() => {
    setImageDims(undefined);
    setImageBroken(false);
    if (!imageMime || !changeable) {
      setImageState({ kind: "idle" });
      return;
    }
    if (entry.sizeBytes > BigInt(IMAGE_PREVIEW_MAX_BYTES)) {
      setImageState({ kind: "tooLarge" });
      return;
    }
    const bytesToDataUrl = (bytes: Uint8Array, mime: string): string => {
      let binary = "";
      const step = 0x8000;
      for (let i = 0; i < bytes.length; i += step) {
        binary += String.fromCharCode(...bytes.subarray(i, i + step));
      }
      return `data:${mime};base64,${btoa(binary)}`;
    };
    if (cachedRead && !cachedRead.truncated) {
      const bytes =
        cachedRead.rawContent.byteLength > 0 ? cachedRead.rawContent : new TextEncoder().encode(cachedRead.content);
      if (bytes.byteLength > IMAGE_PREVIEW_MAX_BYTES) {
        setImageState({ kind: "tooLarge" });
        return;
      }
      setImageState({ kind: "ready", url: bytesToDataUrl(bytes, imageMime) });
      return;
    }
    const abort = new AbortController();
    setImageState({ kind: "loading" });
    const timer = setTimeout(() => {
      void (async () => {
        try {
          const res = await client.readFile({ serverId, path, allowBinary: true }, { signal: abort.signal });
          if (abort.signal.aborted) return;
          if (res.truncated) {
            setImageState({ kind: "tooLarge" });
            return;
          }
          const bytes = res.rawContent.byteLength > 0 ? res.rawContent : new TextEncoder().encode(res.content);
          if (bytes.byteLength > IMAGE_PREVIEW_MAX_BYTES) {
            setImageState({ kind: "tooLarge" });
            return;
          }
          if (!res.binary && res.version) {
            onPreviewRead?.(path, res);
          }
          setImageState({ kind: "ready", url: bytesToDataUrl(bytes, imageMime) });
        } catch (err) {
          if (abort.signal.aborted) return;
          setImageState({ kind: "error", message: sentence(err) });
        }
      })();
    }, 150);
    return () => {
      clearTimeout(timer);
      abort.abort();
    };
  }, [client, serverId, path, imageMime, changeable, entry.sizeBytes, cachedRead, onPreviewRead]);

  // Load first lines of a small text file after a brief pause so double-clicking to edit doesn't double-read.
  useEffect(() => {
    if (!canPreviewText) {
      setTextState({ kind: "idle" });
      return;
    }
    if (cachedRead) {
      setTextState({ kind: "ready", read: cachedRead });
      return;
    }
    const abort = new AbortController();
    setTextState({ kind: "loading" });
    const timer = setTimeout(() => {
      void (async () => {
        try {
          const res = await client.readFile({ serverId, path }, { signal: abort.signal });
          if (abort.signal.aborted) return;
          setTextState({ kind: "ready", read: res });
          onPreviewRead?.(path, res);
        } catch (err) {
          if (abort.signal.aborted) return;
          setTextState({ kind: "error", message: sentence(err) });
        }
      })();
    }, 150);
    return () => {
      clearTimeout(timer);
      abort.abort();
    };
  }, [client, serverId, path, canPreviewText, cachedRead, onPreviewRead]);

  const icon = entry.type === FileType.SYMLINK ? "link" : isFolder ? "folder" : "file";
  const modifiedDate = entry.modifyTime ? timestampDate(entry.modifyTime) : undefined;
  const typeLabel =
    entry.type === FileType.SYMLINK
      ? isFolder
        ? t`Link to folder`
        : t`Link`
      : isFolder
        ? t`Folder`
        : imageMime
          ? t`Image`
          : isArchive
            ? t`Archive`
            : t`File`;

  const snippet = useMemo(() => {
    if (textState.kind !== "ready" || textState.read.binary) return undefined;
    const allLines = textState.read.content.replace(/\r\n?/g, "\n").split("\n");
    const cut = allLines.length > TEXT_PREVIEW_LINES;
    return {
      text: (cut ? allLines.slice(0, TEXT_PREVIEW_LINES) : allLines).join("\n"),
      cut,
      totalLines: allLines.length,
    };
  }, [textState]);

  const showEditAction = !isFolder && changeable && (!imageMime || imageMime === "image/svg+xml");
  const showOpenAction = isFolder && changeable;
  const mainIsExtract = !showOpenAction && !showEditAction && isArchive && Boolean(onExtract);
  const mainIsDownload = !showOpenAction && !showEditAction && !mainIsExtract && Boolean(downloadUrl);
  const canCompress = !isArchive && (entry.type === FileType.FILE || entry.type === FileType.DIRECTORY);

  const overflowActions = useMemo<MenuAction[]>(
    () => [
      {
        id: "move",
        label: t`Move`,
        icon: "arrow-right",
        onSelect: onMove,
      },
      {
        id: "copy-path",
        label: t`Copy path`,
        icon: "copy",
        onSelect: () => {
          void navigator.clipboard?.writeText(path).catch(() => {});
        },
      },
      {
        id: "delete",
        label: t`Delete`,
        icon: "trash",
        danger: true,
        onSelect: onDelete,
      },
    ],
    [onMove, onDelete, path, t],
  );

  return (
    <Panel
      flush
      className={cx("files-details", fullWindow && "is-fullwindow")}
      title={
        <span className="files-viewer-title" title={path}>
          <span className="files-icon">
            <Icon name={icon} size={16} />
          </span>
          <span className="files-viewer-name">{entry.name}</span>
        </span>
      }
      actions={
        <>
          {imageMime && onToggleFullWindow ? (
            <Button
              variant="ghost"
              size="sm"
              icon={fullWindow ? "minimize" : "maximize"}
              aria-label={fullWindow ? t`Exit full window` : t`Full window preview`}
              title={fullWindow ? t`Exit full window` : t`Full window`}
              onClick={onToggleFullWindow}
            />
          ) : null}
          <Button variant="ghost" size="sm" icon="x" aria-label={t`Close details`} onClick={onClose} />
        </>
      }
    >
      {changeable ? (
        <div className="files-details-actions" role="toolbar" aria-label={t`Actions for ${entry.name}`}>
          {showOpenAction ? (
            <Button size="sm" variant="secondary" icon="folder" onClick={onOpen}>
              <Trans>Open</Trans>
            </Button>
          ) : showEditAction ? (
            <Button size="sm" variant="secondary" icon="edit" onClick={onOpen}>
              <Trans>Edit</Trans>
            </Button>
          ) : mainIsExtract && onExtract ? (
            <Button size="sm" variant="secondary" icon="archive" onClick={onExtract}>
              <Trans>Extract</Trans>
            </Button>
          ) : mainIsDownload && downloadUrl ? (
            <ButtonLink
              size="sm"
              variant="secondary"
              icon="download"
              href={downloadUrl}
              download
              aria-label={t`Download ${entry.name}`}
              title={t`Download`}
            >
              <Trans>Download</Trans>
            </ButtonLink>
          ) : null}
          {downloadUrl && !mainIsDownload ? (
            <ButtonLink
              size="sm"
              variant="ghost"
              icon="download"
              href={downloadUrl}
              download
              aria-label={isFolder ? t`Download ${entry.name} as a .zip` : t`Download ${entry.name}`}
              title={isFolder ? t`Download as a .zip` : t`Download`}
            />
          ) : null}
          <Button
            size="sm"
            variant="ghost"
            icon="edit"
            aria-label={t`Rename ${entry.name}`}
            title={t`Rename`}
            onClick={onRename}
          />
          {isArchive && onExtract && !mainIsExtract ? (
            <Button
              size="sm"
              variant="ghost"
              icon="archive"
              aria-label={t`Extract ${entry.name}`}
              title={t`Extract`}
              onClick={onExtract}
            />
          ) : canCompress ? (
            <Button
              size="sm"
              variant="ghost"
              icon="archive"
              aria-label={t`Compress ${entry.name}`}
              title={t`Compress`}
              onClick={onCompress}
            />
          ) : null}
          <ActionMenu
            className="files-details-more"
            label={t`More options for ${entry.name}`}
            actions={overflowActions}
          />
        </div>
      ) : null}

      <div className="files-details-body">
        {entry.refused ? (
          <div className="files-note">
            <Callout tone="warning" title={t`Dockwell doesn't open this`}>
              <Trans>This path is protected on the server, so Dockwell doesn't preview or change it.</Trans>
            </Callout>
          </div>
        ) : null}

        {imageMime && changeable ? (
          <div className="files-details-preview">
            {imageState.kind === "loading" ? (
              <Loading label={t`Loading preview of ${entry.name}`} className="files-details-loading">
                <Skeleton shape="block" width="100%" height={180} />
              </Loading>
            ) : imageState.kind === "tooLarge" ? (
              <p className="files-details-note">
                <Trans>Images over 5 MB aren't previewed here. Download it to open it on your computer.</Trans>
              </p>
            ) : imageState.kind === "error" || imageBroken ? (
              <p className="files-details-note">
                <Trans>Can't preview this image here. Download it to open it on your computer.</Trans>
              </p>
            ) : imageState.kind === "ready" ? (
              <figure className="files-details-figure">
                <div className="files-details-stage">
                  <img
                    src={imageState.url}
                    alt={entry.name}
                    className="files-details-image"
                    onLoad={(event) => {
                      const img = event.currentTarget;
                      if (img.naturalWidth > 0 && img.naturalHeight > 0) {
                        setImageDims({ w: img.naturalWidth, h: img.naturalHeight });
                      }
                    }}
                    onError={() => setImageBroken(true)}
                  />
                </div>
                {imageDims ? (
                  <figcaption className="files-details-caption">
                    {imageDims.w} × {imageDims.h} px
                  </figcaption>
                ) : null}
              </figure>
            ) : null}
          </div>
        ) : null}

        {canPreviewText ? (
          <div className="files-details-preview">
            {textState.kind === "loading" ? (
              <Loading label={t`Previewing ${entry.name}`} className="files-details-loading">
                {["88%", "62%", "76%", "48%", "70%"].map((w) => (
                  <Skeleton key={w} text="code" width={w} />
                ))}
              </Loading>
            ) : textState.kind === "error" ? (
              <p className="files-details-note">{textState.message}</p>
            ) : textState.kind === "ready" && textState.read.binary ? (
              <p className="files-details-note">
                <Trans>This isn't a text file. Download it to open it on your computer.</Trans>
              </p>
            ) : snippet ? (
              <div className="files-details-text">
                <pre className="files-details-snippet">{snippet.text || t`(Empty file)`}</pre>
                {snippet.cut ? (
                  <p className="files-details-caption">
                    <Trans>
                      Showing first {TEXT_PREVIEW_LINES} of {snippet.totalLines} lines
                    </Trans>
                  </p>
                ) : null}
              </div>
            ) : null}
          </div>
        ) : null}

        <dl className="files-details-meta">
          <div className="files-details-row">
            <dt>
              <Trans>Path</Trans>
            </dt>
            <dd className="files-details-path">
              <code title={path}>{path}</code>
              <CopyButton text={path} label={t`Copy path ${path}`} />
            </dd>
          </div>
          <div className="files-details-row">
            <dt>
              <Trans>Type</Trans>
            </dt>
            <dd>
              {typeLabel}
              {entry.linkTarget ? (
                <span className="files-details-target">
                  {" "}
                  <code>→ {entry.linkTarget}</code>
                </span>
              ) : null}
            </dd>
          </div>
          {!isFolder && entry.type !== FileType.SYMLINK ? (
            <div className="files-details-row">
              <dt>
                <Trans>Size</Trans>
              </dt>
              <dd>{formatBytes(entry.sizeBytes, i18n.locale)}</dd>
            </div>
          ) : null}
          {modifiedDate ? (
            <div className="files-details-row">
              <dt>
                <Trans>Modified</Trans>
              </dt>
              <dd title={formatDateTime(modifiedDate, i18n.locale)}>
                {formatDateTime(modifiedDate, i18n.locale)} ({words.ago(modifiedDate, now)})
              </dd>
            </div>
          ) : null}
          {entry.owner ? (
            <div className="files-details-row">
              <dt>
                <Trans>Owner</Trans>
              </dt>
              <dd>
                <code>
                  {entry.owner}
                  {entry.group ? `:${entry.group}` : ""}
                </code>
              </dd>
            </div>
          ) : null}
          {entry.mode ? (
            <div className="files-details-row">
              <dt>
                <Trans>Permissions</Trans>
              </dt>
              <dd>
                <code>{entry.mode}</code>
              </dd>
            </div>
          ) : null}
        </dl>
      </div>
    </Panel>
  );
}
