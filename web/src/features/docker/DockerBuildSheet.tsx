// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  DockerArtifactKind,
  DockerArtifactState,
  DockerService,
  type Server,
  StreamError,
  uploadDockerArtifactPart,
} from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  CodeField,
  FolderPicker,
  Icon,
  Loading,
  SelectField,
  Sheet,
  SkeletonFacts,
  Tag,
  TextField,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { delay } from "../../lib/retry";
import { DeployKeySetup, useGitDeployKeys } from "../apps/DeployKeyPanel";
import { deployKeyNameOf, GITLAB_LOGIN_PREFIX, gitlabConnectionNameOf, isSSHRepository } from "../apps/gitHost";
import { RepositoryField, sameRepository } from "../apps/RepositoryField";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFolderList } from "../files/folders";
import { useProjects } from "../projects/useProjects";
import { useRegistryLogins } from "../registries/registries";
import { useGitLabProjects, useGitRepositories } from "../settings/github";
import { ARTIFACT_CHUNK, archiveHash, committedUploadAfterConflict } from "./archiveTransfer";

type Source = "folder" | "git" | "upload";

type RestoreBuild = { serverId: string; artifactId: string; kind: DockerArtifactKind };

type Transfer = { text: string; value?: number; size?: number; queued?: boolean };

export function DockerBuildSheet({
  servers,
  restore,
  close,
  refresh,
}: {
  servers: readonly Pick<Server, "id" | "name">[];
  restore?: RestoreBuild | undefined;
  close: () => void;
  refresh: () => void;
}) {
  const { i18n, t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(DockerService, transport), [transport]);
  const changes = useChangeSheet();
  const [server, setServer] = useState(restore?.serverId ?? servers[0]?.id ?? "");
  const [source, setSource] = useState<Source>(
    restore?.kind === DockerArtifactKind.BUILD_CONTEXT ? "upload" : "folder",
  );
  const [folder, setFolder] = useState("");
  const [repository, setRepository] = useState("");
  const [gitLogin, setGitLogin] = useState("");
  const [glQuery, setGlQuery] = useState("");
  const [ref, setRef] = useState("");
  const [subdirectory, setSubdirectory] = useState("");
  const [project, setProject] = useState("");
  const [logins, setLogins] = useState<string[]>([]);
  const [addLogin, setAddLogin] = useState("");
  const [advanced, setAdvanced] = useState(false);
  const [output, setOutput] = useState("");
  const [dockerfile, setDockerfile] = useState("");
  const [editDockerfile, setEditDockerfile] = useState(false);
  const [override, setOverride] = useState("");
  const [file, setFile] = useState<File>();
  const [transfer, setTransfer] = useState<Transfer>();
  const [failure, setFailure] = useState("");
  const [dragging, setDragging] = useState(false);
  const [contextId, setContextId] = useState(
    restore?.kind === DockerArtifactKind.BUILD_CONTEXT ? restore.artifactId : "",
  );
  const [overrideId, setOverrideId] = useState(
    restore?.kind === DockerArtifactKind.DOCKERFILE ? restore.artifactId : "",
  );
  const active = useRef<AbortController>(undefined);
  const picker = useRef<HTMLInputElement>(null);
  const folders = useFolderList(server);
  const projects = useProjects();
  const registryLogins = useRegistryLogins({ project }, advanced);
  const gitProject = source === "git" ? project : "";
  const repos = useGitRepositories(gitProject);
  const glListing = useGitLabProjects(gitProject, glQuery);
  const deployKeys = useGitDeployKeys(gitProject, source === "git");
  const gitlabProjects = glListing.data?.projects ?? [];
  const gitlabConnections = glListing.data?.connections ?? [];
  const sshAddress = isSSHRepository(repository);
  const repo = repos.data?.repositories.find((r) => sameRepository(r.url, repository));
  const matchedGitLab = !sshAddress ? gitlabProjects.find((p) => sameRepository(p.url, repository)) : undefined;
  const keyName = deployKeyNameOf(gitLogin);
  const matchedKey = useMemo(() => {
    const list = deployKeys.data?.keys ?? [];
    if (keyName) {
      const byName = list.find((k) => k.name === keyName);
      if (byName && sameRepository(byName.repository, repository)) return byName;
    }
    return list.find((k) => sameRepository(k.repository, repository));
  }, [deployKeys.data?.keys, keyName, repository]);
  const preferredGitLabLogin = gitlabConnectionNameOf(gitLogin)
    ? gitLogin
    : matchedGitLab
      ? `${GITLAB_LOGIN_PREFIX}${matchedGitLab.connectionName}`
      : "";
  const effectiveGitLogin =
    preferredGitLabLogin ||
    gitLogin ||
    (matchedKey?.hostKeyPinned && !matchedKey.hostKeyChanged ? matchedKey.gitLogin : "");
  const activeGitLab = gitlabConnectionNameOf(effectiveGitLogin);
  const context = useQuery(
    DockerService.method.getDockerArtifact,
    { serverId: server, artifactId: contextId },
    { enabled: !!contextId && source === "upload" && !transfer, retry: false },
  );
  const native = t`Server platform`;
  const { data: overrideLease } = useQuery(
    DockerService.method.getDockerArtifact,
    { serverId: server, artifactId: overrideId },
    { enabled: !!restore && !!overrideId && !transfer, retry: false },
  );
  const overriding = editDockerfile || !!overrideId;
  const sourceReady =
    source === "folder"
      ? folder.trim().startsWith("/")
      : source === "git"
        ? !!repository.trim()
        : !!file ||
          context.data?.state === DockerArtifactState.READY ||
          context.data?.state === DockerArtifactState.CHECKING;
  const canReview = !!server && !!output.trim() && sourceReady && (!editDockerfile || !!override.trim());
  const uploadLabel = t`Uploading build input`;
  const checkingLabel = t`Checking build input`;
  const slotLabel = t`Waiting for a transfer slot`;

  useEffect(() => () => active.current?.abort(), []);

  const upload = async (
    uploadFile: File | undefined,
    kind: DockerArtifactKind,
    existing: string,
    keep: (id: string) => void,
    signal: AbortSignal,
  ) => {
    const options = { signal };
    let lease = existing
      ? await client.getDockerArtifact({ serverId: server, artifactId: existing }, options)
      : undefined;
    if (!lease) {
      if (!uploadFile) throw new Error(t`Choose a file for the build.`);
      lease = await client.beginDockerArtifactUpload(
        {
          serverId: server,
          kind,
          sizeBytes: BigInt(uploadFile.size),
          gzip: kind === DockerArtifactKind.BUILD_CONTEXT && /\.(tgz|gz)$/i.test(uploadFile.name),
          fileName: uploadFile.name,
        },
        options,
      );
      keep(lease.artifactId);
    }
    const artifactId = lease.artifactId;
    if (lease.state === DockerArtifactState.RECEIVING) {
      if (!uploadFile || BigInt(uploadFile.size) !== lease.sizeBytes)
        throw new Error(t`Choose the same file to resume the upload.`);
      const hash = archiveHash(uploadFile, signal);
      try {
        let offset = Number(lease.receivedBytes);
        let prefix = await hash.digest(offset);
        if (prefix !== lease.committedPrefixSha256) throw new Error(t`Choose the same file to resume the upload.`);
        while (offset < uploadFile.size) {
          let end = Math.min(offset + ARTIFACT_CHUNK, uploadFile.size);
          setTransfer({ text: uploadLabel, value: offset, size: uploadFile.size });
          try {
            lease = await uploadDockerArtifactPart({
              serverId: server,
              artifactId,
              offset,
              prefixSHA256: prefix,
              part: uploadFile.slice(offset, end),
              signal,
              onSent: (n) => setTransfer({ text: uploadLabel, value: offset + n, size: uploadFile.size }),
            });
          } catch (err) {
            if (err instanceof StreamError && err.reason === "artifact_transfer_slots") {
              setTransfer({ text: slotLabel, size: uploadFile.size, queued: true });
              await delay(1000, signal);
              signal.throwIfAborted();
              continue;
            }
            lease = await committedUploadAfterConflict(
              err,
              () => client.getDockerArtifact({ serverId: server, artifactId }, options),
              offset,
              uploadFile,
              hash.digest,
            );
            end = Number(lease.receivedBytes);
          }
          offset = end;
          prefix = await hash.digest(offset);
          if (lease.receivedBytes !== BigInt(offset) || lease.committedPrefixSha256 !== prefix)
            throw new Error(t`The received bytes differ from the selected file.`);
        }
      } finally {
        hash.close();
      }
      lease = await client.finishDockerArtifactUpload({ serverId: server, artifactId }, options);
    }
    while (lease.state === DockerArtifactState.CHECKING) {
      setTransfer({ text: checkingLabel });
      await delay(750, signal);
      lease = await client.getDockerArtifact({ serverId: server, artifactId }, options);
    }
    if (lease.state !== DockerArtifactState.READY) throw new Error(t`The server couldn't verify this build input.`);
    return artifactId;
  };

  const review = async () => {
    if (!canReview || active.current) return;
    const work = new AbortController();
    active.current = work;
    setFailure("");
    try {
      let contextArtifactId = "";
      let dockerfileOverrideArtifactId = "";
      if (source === "upload" || overriding) setTransfer({ text: uploadLabel });
      if (source === "upload")
        contextArtifactId = await upload(file, DockerArtifactKind.BUILD_CONTEXT, contextId, setContextId, work.signal);
      if (overriding) {
        const input = editDockerfile ? new File([override], "Dockerfile") : undefined;
        dockerfileOverrideArtifactId = await upload(
          input,
          DockerArtifactKind.DOCKERFILE,
          overrideId,
          setOverrideId,
          work.signal,
        );
      }
      work.signal.throwIfAborted();
      changes.open({
        kind: "plan",
        ...(source === "folder" ? { planningLabel: t`Preparing the build folder and checking base images` } : {}),
        onClose: refresh,
        make: async () => {
          const { change } = await client.planDockerMaintenance({
            serverIds: [server],
            operation: {
              case: "build",
              value: {
                output: output.trim(),
                dockerfilePath: dockerfile.trim(),
                credentialProject: project,
                registryLoginIds: logins,
                dockerfileOverrideArtifactId,
                folderPath: source === "folder" ? folder.trim() : "",
                contextArtifactId,
                ...(source === "git"
                  ? {
                      git: {
                        repository: repository.trim(),
                        ref: ref.trim(),
                        subdirectory: subdirectory.trim(),
                        gitLogin: effectiveGitLogin,
                        project,
                      },
                    }
                  : {}),
              },
            },
          });
          if (!change) throw new Error(t`The image build could not be reviewed.`);
          return { change };
        },
      });
      close();
    } catch (error) {
      if (!work.signal.aborted) setFailure(sentence(error));
    } finally {
      if (active.current === work) active.current = undefined;
      if (!work.signal.aborted) setTransfer(undefined);
    }
  };

  const choose = (next: File | undefined) => {
    if (!next || transfer) return;
    setFailure("");
    if (!/\.(tar|tar\.gz|tgz)$/i.test(next.name) || !next.size || next.size > 2 ** 30) {
      setFailure(t`Choose a .tar or .tar.gz archive up to 1 GiB.`);
      return;
    }
    if (context.data && context.data.sizeBytes !== BigInt(next.size)) setContextId("");
    setFile(next);
  };

  return (
    <Sheet
      open
      title={t`Build image`}
      onClose={close}
      footer={
        <>
          <Button variant="ghost" onClick={close}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" icon="commit" disabled={!canReview || !!transfer} onClick={() => void review()}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="docker-image-fields">
        {failure ? (
          <Callout tone="critical" title={t`Build input couldn't be prepared`}>
            {failure}
          </Callout>
        ) : null}
        {servers.length > 1 ? (
          <SelectField
            label={t`Server`}
            value={server}
            disabled={!!transfer}
            options={servers.map(({ id, name }) => ({ value: id, label: name }))}
            onChange={(event) => {
              setServer(event.target.value);
              setFolder("");
              setContextId("");
              setOverrideId("");
            }}
          />
        ) : null}
        <SelectField
          label={t`Source`}
          value={source}
          disabled={!!transfer}
          options={[
            { value: "folder", label: t`Server folder` },
            { value: "git", label: t`Git repository` },
            { value: "upload", label: t`Upload context` },
          ]}
          onChange={(event) => setSource(event.target.value as Source)}
        />
        {source === "folder" ? (
          <FolderPicker
            className="docker-build-folder"
            label={t`Server folder`}
            list={folders}
            value={folder}
            disabled={!!transfer}
            onChange={setFolder}
          />
        ) : source === "git" ? (
          <>
            <SelectField
              label={t`Git access`}
              value={project}
              disabled={!!transfer}
              options={[
                { label: t`Public repository`, value: "" },
                ...(projects.projects ?? []).map(({ id, label }) => ({ value: id, label })),
              ]}
              onChange={(event) => {
                setProject(event.target.value);
                setGitLogin("");
                setLogins([]);
              }}
            />
            {project && repos.isLoading && glListing.isLoading ? (
              <Loading label={t`Reading repositories`}>
                <SkeletonFacts layout="rows" count={1} />
              </Loading>
            ) : (
              <RepositoryField
                value={repository}
                onChange={(url, picked) => {
                  setRepository(url);
                  setGitLogin(picked?.gitLogin ?? "");
                }}
                repositories={repos.data?.repositories ?? []}
                gitlabProjects={gitlabProjects}
                gitlabConnections={gitlabConnections}
                deployKeys={deployKeys.data?.keys ?? []}
                onSearch={setGlQuery}
                hint={
                  activeGitLab
                    ? !sshAddress && matchedKey
                      ? t`Through GitLab (${activeGitLab}, preferred over deploy key ${matchedKey.name}).`
                      : t`Through GitLab (${activeGitLab}).`
                    : undefined
                }
              />
            )}
            {activeGitLab && !sshAddress ? null : (
              <DeployKeySetup
                project={project}
                repository={repository}
                activeKey={matchedKey}
                authRequired={false}
                sshAddress={sshAddress}
                onCreated={(k) => {
                  setRepository(k.repository);
                  setGitLogin(k.gitLogin);
                  void deployKeys.refetch();
                }}
                onVerified={(k) => {
                  setRepository(k.repository);
                  setGitLogin(k.gitLogin);
                  void deployKeys.refetch();
                }}
              />
            )}
            <TextField
              label={t`Branch, tag or commit`}
              mono
              value={ref}
              placeholder={repo?.defaultBranch || matchedGitLab?.defaultBranch || t`Default branch`}
              onChange={(event) => setRef(event.target.value)}
            />
            <TextField
              label={t`Subdirectory`}
              mono
              value={subdirectory}
              placeholder={t`Repository root`}
              onChange={(event) => setSubdirectory(event.target.value)}
            />
          </>
        ) : (
          <fieldset
            className="docker-artifact-drop"
            data-dragging={dragging || undefined}
            onDragOver={(event) => {
              event.preventDefault();
              if (!transfer) setDragging(true);
            }}
            onDragLeave={() => setDragging(false)}
            onDrop={(event) => {
              event.preventDefault();
              setDragging(false);
              choose(event.dataTransfer.files[0]);
            }}
          >
            <Icon name="archive" size={24} />
            <div>
              <strong>{file?.name ?? (context.data?.fileName || t`Drop a build context`)}</strong>
              <small>{file ? formatBytes(file.size, i18n.locale) : t`.tar or .tar.gz, up to 1 GiB`}</small>
            </div>
            <input
              type="file"
              hidden
              ref={picker}
              accept=".tar,.tar.gz,.tgz"
              aria-label={t`Build context archive`}
              onChange={(event) => choose(event.target.files?.[0])}
            />
            <Button variant="secondary" icon="upload" onClick={() => picker.current?.click()} disabled={!!transfer}>
              {file ? <Trans>Change file</Trans> : <Trans>Choose file</Trans>}
            </Button>
          </fieldset>
        )}
        <TextField
          label={t`Dockerfile path`}
          mono
          value={dockerfile}
          placeholder="Dockerfile"
          onChange={(event) => setDockerfile(event.target.value)}
        />
        <div>
          <Button
            variant="ghost"
            size="sm"
            icon={editDockerfile ? "x" : "edit"}
            onClick={() => {
              setEditDockerfile(!editDockerfile);
              setOverrideId("");
            }}
          >
            {editDockerfile ? <Trans>Use source Dockerfile</Trans> : <Trans>Override Dockerfile</Trans>}
          </Button>
        </div>
        {editDockerfile ? (
          <CodeField
            label={t`Dockerfile override`}
            lines={9}
            value={override}
            placeholder="FROM scratch"
            onChange={(event) => {
              setOverride(event.target.value);
              setOverrideId("");
            }}
          />
        ) : overrideLease ? (
          <Tag>{t`Uploaded Dockerfile`}</Tag>
        ) : null}
        <TextField
          label={t`Local image name`}
          mono
          value={output}
          placeholder="example/web"
          hint={t`Dockwell adds a unique tag.`}
          onChange={(event) => setOutput(event.target.value)}
        />
        <Advanced open={advanced} onOpenChange={setAdvanced} summary={native}>
          <div className="docker-image-fields">
            <SelectField label={t`Platform`} value="native" options={[{ value: "native", label: native }]} />
            {source !== "git" ? (
              <SelectField
                label={t`Credential project`}
                value={project}
                options={[
                  { value: "", label: t`Install-wide` },
                  ...(projects.projects ?? []).map(({ id, label }) => ({ value: id, label })),
                ]}
                onChange={(event) => {
                  setProject(event.target.value);
                  setLogins([]);
                  setAddLogin("");
                }}
              />
            ) : null}
            {registryLogins.isLoading ? (
              <Loading label={t`Reading registry logins`}>
                <SkeletonFacts layout="rows" count={1} />
              </Loading>
            ) : (
              <>
                <SelectField
                  label={t`Registry login`}
                  value={addLogin}
                  options={[
                    { value: "", label: t`Choose a registry login` },
                    ...(registryLogins.data?.logins ?? [])
                      .filter((item) => !logins.includes(item.id))
                      .map((item) => ({
                        value: item.id,
                        label: `${item.name} · ${item.registry}`,
                      })),
                  ]}
                  onChange={(event) => setAddLogin(event.target.value)}
                />
                <Button
                  variant="secondary"
                  size="sm"
                  icon="plus"
                  disabled={!addLogin}
                  onClick={() => {
                    setLogins([...logins, addLogin]);
                    setAddLogin("");
                  }}
                >
                  <Trans>Add login</Trans>
                </Button>
                <div className="plan-archive-tags">
                  {logins.map((key) => (
                    <Button
                      key={key}
                      variant="ghost"
                      size="sm"
                      icon="x"
                      onClick={() => setLogins(logins.filter((id) => id !== key))}
                    >
                      {registryLogins.data?.logins.find((item) => item.id === key)?.name ?? t`Registry login`}
                    </Button>
                  ))}
                </div>
              </>
            )}
          </div>
        </Advanced>
        {transfer?.size ? (
          <div className="docker-artifact-progress">
            <Loading label={transfer.text} slow={transfer.queued ? transfer.text : undefined} slowAfter={0}>
              <progress aria-label={uploadLabel} value={transfer.value} max={transfer.size} />
            </Loading>
          </div>
        ) : transfer ? (
          <Loading label={transfer.text}>
            <SkeletonFacts layout="rows" count={1} />
          </Loading>
        ) : null}
      </div>
    </Sheet>
  );
}
