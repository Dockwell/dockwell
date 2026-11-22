// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type DockerArtifactInfo,
  DockerArtifactKind,
  DockerArtifactState,
  DockerObjectKind,
  DockerService,
  type Server,
  StreamError,
  uploadDockerArtifactPart,
} from "@dockwell/api";
import { Button, Callout, Icon, Loading, SelectField, Sheet, SkeletonFacts, Tag, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQuery as useCachedQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { delay } from "../../lib/retry";
import { useChangeSheet } from "../changes/ChangeSheet";
import { registryOf } from "../registries/registries";
import { CopyButton } from "../servers/CopyButton";
import { ARTIFACT_CHUNK, archiveHash, MAX_ARTIFACT_BYTES } from "./archiveTransfer";
import { shortImageId } from "./imageIdentity";

type ServerChoice = Pick<Server, "id" | "name">;

function tagKey(alias: string) {
  const host = registryOf(alias);
  const first = alias.split("/")[0] ?? "";
  const qualified = first.includes(".") || (first.includes(":") && alias.includes("/")) || first === "localhost";
  let name = qualified && alias.includes("/") ? alias.slice(first.length + 1) : alias;
  if (host === "docker.io" && !name.includes("/")) name = `library/${name}`;
  return `${host}/${name}`;
}

function suggestTag(alias: string, taken: (key: string) => boolean): string {
  const colon = alias.lastIndexOf(":");
  if (colon <= alias.lastIndexOf("/") || colon < 0) return "";
  const repo = alias.slice(0, colon);
  const label = alias.slice(colon + 1, colon + 111);
  for (let n = 1; n <= 128; n++) {
    const suggested = `${repo}:${label}-imported${n > 1 ? `-${n}` : ""}`;
    if (!taken(tagKey(suggested))) return suggested;
  }
  return "";
}

type SelectedTag = { imageId: string; alias: string; original: string };
function leaseInSession(server: string, id?: string): string {
  const key = `docker-image-import:${server}`;
  try {
    if (id === "") sessionStorage.removeItem(key);
    else if (id !== undefined) sessionStorage.setItem(key, id);
    return sessionStorage.getItem(key) ?? "";
  } catch {
    return "";
  }
}

function archiveTags(info: DockerArtifactInfo): SelectedTag[] {
  return info.images.flatMap((image) =>
    image.proposedTags.map((alias) => ({ imageId: image.imageId, alias, original: alias })),
  );
}

type ImportPhase = "upload" | "checking" | "pause";

export function DockerImportSheet({
  servers,
  close,
  refresh,
  restore,
}: {
  servers: readonly ServerChoice[];
  close: () => void;
  refresh: () => void;
  restore?: { serverId: string; artifactId: string } | undefined;
}) {
  const { t, i18n } = useLingui();
  const client = createClient(DockerService, useTransport());
  const changes = useChangeSheet();
  const [server, setServer] = useState(restore?.serverId ?? servers[0]?.id ?? "");
  const [file, setFile] = useState<File>();
  const [info, setInfo] = useState<DockerArtifactInfo>();
  const [progress, setProgress] = useState(0);
  const [phase, setPhase] = useState<ImportPhase>();
  const [failure, setFailure] = useState("");
  const [tags, setTags] = useState<SelectedTag[]>([]);
  const run = useRef<AbortController | undefined>(undefined);
  const choice = useRef<HTMLInputElement>(null);
  const pickButton = useRef<HTMLButtonElement>(null);
  const [dragging, setDragging] = useState(false);
  const [speed, setSpeed] = useState(0);
  const [waitingForSlot, setWaitingForSlot] = useState(false);
  const [starting, setStarting] = useState(false);
  const ready = info?.state === DockerArtifactState.READY;
  const leaseId = info?.artifactId ?? restore?.artifactId ?? leaseInSession(server);
  const status = useQuery(
    DockerService.method.getDockerArtifact,
    { serverId: server, artifactId: leaseId },
    {
      enabled: !!leaseId && phase !== "upload",
      refetchInterval: phase === "checking" || !info ? 800 : 30_000,
      retry: false,
    },
  );
  useEffect(() => {
    const prior = status.data;
    if (!prior || phase === "upload") return;
    setInfo(prior);
    setProgress(Number(prior.receivedBytes));
    if (prior.state === DockerArtifactState.READY) {
      setTags((edited) => (edited.length ? edited : archiveTags(prior)));
      setPhase(undefined);
    } else if (prior.state === DockerArtifactState.CHECKING) {
      setPhase("checking");
    } else if (prior.state === DockerArtifactState.FAILED) {
      setPhase(undefined);
    } else {
      setPhase((old) => old ?? "pause");
    }
  }, [status.data, phase]);
  useEffect(() => {
    if (status.error && ConnectError.from(status.error).code === Code.NotFound) {
      leaseInSession(server, "");
      setInfo(undefined);
      setPhase(undefined);
    }
  }, [status.error, server]);

  const installed = useCachedQuery({
    queryKey: ["docker-archive-tags", server, info?.artifactId],
    enabled: ready,
    retry: false,
    refetchOnWindowFocus: false,
    queryFn: async ({ signal }) => {
      const names = new Map<string, string>();
      let pageToken = "";
      do {
        const page = await client.listDockerObjects(
          { serverIds: [server], kind: DockerObjectKind.IMAGE, pageSize: 24, pageToken },
          { signal },
        );
        if (page.unavailable.length) throw new Error(t`Docker could not list the local tags.`);
        for (const { detail, key } of page.objects) {
          if (detail.case === "image") for (const tag of detail.value.tags) names.set(tagKey(tag), key?.engineId ?? "");
        }
        pageToken = page.nextPageToken;
      } while (pageToken);
      return names;
    },
  });
  const clashes = new Set<string>();
  const used = new Set<string>();
  for (const { alias } of tags) {
    if (!alias.trim()) continue;
    const key = tagKey(alias.trim());
    if (used.has(key) || installed.data?.has(key)) clashes.add(key);
    used.add(key);
  }
  useEffect(() => () => run.current?.abort(), []);

  const choose = (picked: File | undefined) => {
    if (!picked) return;
    const valid = picked.size > 0 && picked.size <= MAX_ARTIFACT_BYTES;
    setFile(valid ? picked : undefined);
    setFailure(valid ? "" : t`Choose an archive up to 8 GiB.`);
  };

  const update = (next: DockerArtifactInfo) => {
    setInfo(next);
    setProgress(Number(next.receivedBytes));
  };
  const transfer = async () => {
    if (!server || starting || phase === "upload" || phase === "checking") return;
    if (info && info.receivedBytes === info.sizeBytes) {
      setStarting(true);
      setFailure("");
      try {
        const next = await client.finishDockerArtifactUpload({ serverId: server, artifactId: info.artifactId });
        update(next);
        setPhase(next.state === DockerArtifactState.READY ? undefined : "checking");
        void status.refetch();
      } catch (error) {
        setFailure(sentence(error));
        void status.refetch();
      } finally {
        setStarting(false);
      }
      return;
    }
    if (!file || info?.state === DockerArtifactState.FAILED) return;
    const current = new AbortController();
    run.current = current;
    const hash = archiveHash(file, current.signal);
    setFailure("");
    setSpeed(0);
    setWaitingForSlot(false);
    setPhase("upload");
    try {
      const next = info
        ? await client.getDockerArtifact({ serverId: server, artifactId: info.artifactId }, { signal: current.signal })
        : await client.beginDockerArtifactUpload(
            {
              serverId: server,
              kind: DockerArtifactKind.IMAGE_IMPORT,
              sizeBytes: BigInt(file.size),
              gzip: /\.(?:tar\.gz|tgz)$/i.test(file.name),
              fileName: file.name,
            },
            { signal: current.signal },
          );
      update(next);
      leaseInSession(server, next.artifactId);
      const id = next.artifactId;
      let offset = Number(next.receivedBytes);
      let prefix = await hash.digest(offset);
      if (file.size !== Number(next.sizeBytes) || prefix !== next.committedPrefixSha256) {
        throw new Error(t`Choose the same file to resume this upload.`);
      }
      const started = performance.now();
      const startOffset = offset;
      const measureSpeed = (sent: number) => {
        const elapsed = (performance.now() - started) / 1000;
        if (elapsed > 0.3) setSpeed(Math.max(0, (sent - startOffset) / elapsed));
      };
      while (offset < file.size) {
        const end = Math.min(file.size, offset + ARTIFACT_CHUNK);
        let accepted: DockerArtifactInfo;
        for (;;) {
          try {
            accepted = await uploadDockerArtifactPart({
              serverId: server,
              artifactId: id,
              offset,
              prefixSHA256: prefix,
              part: file.slice(offset, end),
              signal: current.signal,
              onSent: (bytes) => {
                const sent = offset + bytes;
                setProgress(sent);
                measureSpeed(sent);
              },
            });
            break;
          } catch (error) {
            if (!(error instanceof StreamError) || error.reason !== "artifact_transfer_slots") throw error;
            setWaitingForSlot(true);
            setProgress(offset);
            await delay(1000, current.signal);
            current.signal.throwIfAborted();
          }
        }
        setWaitingForSlot(false);
        update(accepted);
        offset = end;
        measureSpeed(offset);
        prefix = await hash.digest(offset);
        if (accepted.receivedBytes !== BigInt(offset) || accepted.committedPrefixSha256 !== prefix) {
          throw new Error(t`The received bytes differ from the selected file.`);
        }
      }
      setPhase("checking");
      const sealed = await client.finishDockerArtifactUpload(
        { serverId: server, artifactId: id },
        { signal: current.signal },
      );
      update(sealed);
      if (sealed.state === DockerArtifactState.READY) {
        setTags(archiveTags(sealed));
        setPhase(undefined);
      }
      void status.refetch();
    } catch (error) {
      if (run.current === current) {
        setWaitingForSlot(false);
        if (!current.signal.aborted) setFailure(sentence(error));
        setPhase("pause");
      }
    } finally {
      hash.close();
    }
  };
  const cancel = async () => {
    run.current?.abort();
    if (info) {
      try {
        await client.abortDockerArtifact({ serverId: server, artifactId: info.artifactId });
        leaseInSession(server, "");
      } catch (error) {
        setFailure(sentence(error));
        setPhase("pause");
        return;
      }
    }
    close();
  };
  const review = () => {
    if (!info || clashes.size) return;
    changes.open({
      kind: "plan",
      onClose: () => {
        refresh();
      },
      make: async () => {
        const { change } = await client.planDockerMaintenance({
          serverIds: [server],
          operation: {
            case: "load",
            value: {
              artifactId: info.artifactId,
              selectedImageIds: info.images.map(({ imageId }) => imageId),
              tags: tags
                .filter(({ alias }) => alias.trim())
                .map(({ imageId, alias }) => ({ imageId, alias: alias.trim() })),
            },
          },
        });
        if (!change) throw new Error(t`The archive could not be reviewed.`);
        return { change };
      },
    });
    close();
  };

  const failed = info?.state === DockerArtifactState.FAILED;
  const busy = phase === "upload" || phase === "checking" || starting;
  const needFile = !info || info.receivedBytes < info.sizeBytes;
  const fileName = file?.name ?? info?.fileName;
  const size = Number(info?.sizeBytes ?? file?.size ?? 0);
  const percent = size > 0 ? Math.min(100, Math.floor((100 * progress) / size)) : 0;
  const queued = phase === "upload" && waitingForSlot;
  const remaining = speed > 0 ? Math.max(0, Math.ceil((size - progress) / speed)) : 0;
  const timeLeft = remaining >= 60 ? t`${Math.ceil(remaining / 60)} min left` : t`${remaining} s left`;

  return (
    <Sheet
      open
      title={t`Import image`}
      onClose={close}
      initialFocusRef={pickButton}
      footer={
        <>
          <Button variant="ghost" onClick={() => void cancel()}>
            <Trans>Cancel</Trans>
          </Button>
          {ready ? (
            <Button
              variant="primary"
              icon="commit"
              disabled={clashes.size > 0 || !installed.isSuccess}
              onClick={review}
            >
              <Trans>Review plan</Trans>
            </Button>
          ) : phase === "upload" ? (
            <Button variant="secondary" icon="pause" onClick={() => run.current?.abort()}>
              <Trans>Pause</Trans>
            </Button>
          ) : (
            <Button
              variant="primary"
              icon="upload"
              disabled={(needFile && !file) || !server || busy || failed || (status.isLoading && !info)}
              onClick={() => void transfer()}
            >
              {phase === "pause" ? <Trans>Resume</Trans> : <Trans>Upload archive</Trans>}
            </Button>
          )}
        </>
      }
    >
      <div className="docker-image-fields">
        {!info && servers.length > 1 ? (
          <SelectField
            label={t`Server`}
            value={server}
            options={servers.map(({ id, name }) => ({ value: id, label: name }))}
            onChange={(e) => setServer(e.target.value)}
          />
        ) : null}
        {!ready && !failed && needFile && phase !== "upload" ? (
          <fieldset
            className="docker-artifact-drop"
            aria-label={t`Image archive`}
            data-dragging={dragging || undefined}
            onDragOver={(event) => {
              event.preventDefault();
              setDragging(true);
            }}
            onDragLeave={() => setDragging(false)}
            onDrop={(event) => {
              event.preventDefault();
              setDragging(false);
              if (!busy) choose(event.dataTransfer.files[0]);
            }}
          >
            <Icon name="archive" size={24} />
            <div>
              <strong>{file ? file.name : t`Drop a Docker image archive`}</strong>
              <small>{file ? formatBytes(file.size, i18n.locale) : t`.tar or .tar.gz, up to 8 GiB`}</small>
            </div>
            <input
              ref={choice}
              type="file"
              aria-label={t`Docker image archive`}
              accept=".tar,.tar.gz,.tgz"
              hidden
              onChange={(event) => choose(event.target.files?.[0])}
            />
            <Button
              ref={pickButton}
              variant="secondary"
              icon="upload"
              disabled={busy}
              onClick={() => choice.current?.click()}
            >
              {file ? <Trans>Change file</Trans> : <Trans>Choose file</Trans>}
            </Button>
          </fieldset>
        ) : null}
        {(phase || info) && size > 0 && !ready && !failed ? (
          <div className="docker-artifact-progress">
            <div className="docker-artifact-progress-line">
              <label htmlFor="docker-archive-upload">{fileName || t`Image archive`}</label>
              <strong>{percent}%</strong>
            </div>
            {queued ? (
              <Loading label={t`Waiting for a transfer slot`} slow={t`Waiting for a transfer slot`} slowAfter={0}>
                <progress id="docker-archive-upload" max={size} />
              </Loading>
            ) : (
              <progress id="docker-archive-upload" value={progress} max={size} />
            )}
            <div className="docker-artifact-progress-line">
              <small>{t`${formatBytes(progress, i18n.locale)} of ${formatBytes(size, i18n.locale)}`}</small>
              {queued ? null : (
                <small>
                  {phase === "upload" && speed > 0
                    ? t`${formatBytes(speed, i18n.locale)}/s · ${timeLeft}`
                    : phase === "upload"
                      ? t`Estimating time`
                      : phase === "pause"
                        ? t`Paused`
                        : t`Checking`}
                </small>
              )}
            </div>
          </div>
        ) : null}
        {phase === "checking" || (ready && installed.isPending) || (status.isLoading && !info) ? (
          <Loading label={t`Checking the archive`}>
            <SkeletonFacts layout="rows" count={3} />
          </Loading>
        ) : ready ? (
          <section className="docker-artifact-preview" aria-label={t`Images in the archive`}>
            <h3>
              <Trans>Images in the archive</Trans>
            </h3>
            {info.images.map((image) => (
              <div key={image.imageId} className="docker-artifact-image">
                <div className="plan-archive-identity">
                  <Icon name="archive" size={16} />
                  <code title={image.imageId}>{shortImageId(image.imageId)}</code>
                  <CopyButton text={image.imageId} label={t`Copy full image ID`} />
                  <Tag>{image.platform}</Tag>
                </div>
                {tags.map((tag, index) => {
                  if (tag.imageId !== image.imageId) return null;
                  const taken = clashes.has(tagKey(tag.alias.trim()));
                  const holder = installed.data?.get(tagKey(tag.alias.trim()));
                  const suggested = taken
                    ? suggestTag(tag.alias, (name) => used.has(name) || !!installed.data?.has(name))
                    : "";
                  const changeTag = (alias: string) =>
                    setTags((old) => old.map((value, at) => (at === index ? { ...value, alias } : value)));
                  return (
                    <div className="docker-artifact-tag" key={tag.original}>
                      {holder ? (
                        <small>
                          <Trans>
                            On this server, this tag points to <code>{shortImageId(holder)}</code>.
                          </Trans>
                        </small>
                      ) : null}
                      <TextField
                        label={taken ? t`Choose another tag` : t`Tag`}
                        mono
                        value={tag.alias}
                        onChange={(event) => changeTag(event.target.value)}
                      />
                      {suggested ? (
                        <Button variant="secondary" size="sm" onClick={() => changeTag(suggested)}>
                          <Trans>
                            Use <code>{suggested}</code>
                          </Trans>
                        </Button>
                      ) : null}
                    </div>
                  );
                })}
              </div>
            ))}
          </section>
        ) : null}
        {ready && installed.isError ? (
          <Callout tone="warning" title={t`Docker could not list the local tags.`}>
            <Button variant="ghost" onClick={() => void installed.refetch()}>
              <Trans>Retry</Trans>
            </Button>
          </Callout>
        ) : null}
        {failed ? (
          <Callout tone="warning" title={t`This archive couldn't be verified`}>
            {info?.failureSpace ? (
              <Trans>
                The archive needs {formatBytes(info.failureSpace.requiredBytes, i18n.locale)}, with{" "}
                {formatBytes(info.failureSpace.availableBytes, i18n.locale)} available.
              </Trans>
            ) : (
              <Trans>Choose a Docker image archive and start a new import.</Trans>
            )}
          </Callout>
        ) : null}
        {!info && status.isError && ConnectError.from(status.error).code !== Code.NotFound ? (
          <Callout tone="warning" title={t`The upload status couldn't be read`}>
            <Button variant="ghost" onClick={() => void status.refetch()}>
              <Trans>Retry</Trans>
            </Button>
          </Callout>
        ) : null}
        {failure ? (
          <Callout tone="warning" title={t`Image import needs attention`}>
            {failure}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}

export function DockerExportSheet({
  server,
  imageId,
  tags,
  platform,
  close,
}: {
  server: string;
  imageId: string;
  tags: readonly string[];
  platform: string;
  close: () => void;
}) {
  const { t } = useLingui();
  const client = createClient(DockerService, useTransport());
  const changes = useChangeSheet();
  const [tag, setTag] = useState(tags[0] ?? "");
  const review = () => {
    changes.open({
      kind: "plan",
      make: async () => {
        const { change } = await client.planDockerMaintenance({
          serverIds: [server],
          operation: {
            case: "export",
            value: { sourceKey: { serverId: server, imageId }, platform, tags: tag ? [tag] : [] },
          },
        });
        if (!change) throw new Error(t`The image export could not be reviewed.`);
        return { change };
      },
    });
    close();
  };
  return (
    <Sheet
      open
      title={t`Export image`}
      onClose={close}
      footer={
        <>
          <Button variant="ghost" onClick={close}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" icon="commit" onClick={review}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="docker-image-fields">
        <div className="docker-artifact-image">
          <code>{imageId}</code>
          {platform ? <Tag>{platform}</Tag> : null}
        </div>
        {tags.length ? (
          <SelectField
            label={t`Tag to include`}
            value={tag}
            options={[...tags.map((value) => ({ value, label: value })), { value: "", label: t`Image only` }]}
            onChange={(event) => setTag(event.target.value)}
          />
        ) : null}
      </div>
    </Sheet>
  );
}
