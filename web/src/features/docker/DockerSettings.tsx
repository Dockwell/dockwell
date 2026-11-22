// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  ChangeState,
  ContainerLoggingMode,
  DockerDaemonAction,
  DockerDaemonSchema,
  DockerLogRotationState,
  DockerMirrorReachability,
  type DockerObject,
  DockerObjectKind,
  DockerOwnership,
  DockerService,
  type DockerSettings as Settings,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Loading,
  Panel,
  SelectField,
  Skeleton,
  SkeletonCard,
  SkeletonFacts,
  SkeletonField,
  StatusBadge,
  Table,
  Tag,
  TextField,
} from "@dockwell/ui";
import { Plural, Trans, useLingui } from "@lingui/react/macro";
import { useInfiniteQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { useChangeSheet } from "../changes/ChangeSheet";
import { DockerTakeOn } from "./DockerTakeOn";
import { Fact } from "./parts";
import "./DockerSettings.css";

type Draft = MessageInitShape<typeof DockerDaemonSchema>;
type Fields = {
  mirrors: string[];
  driver: string;
  size: string;
  files: string;
  pools: { key: number; base: string; prefix: string }[];
};
type Group = "mirrors" | "logging" | "addressPools";

const mebibyte = 1_048_576;
const rotation = { driver: "json-file", maxSizeBytes: 10n * BigInt(mebibyte), maxFiles: 5 };

export function DockerSettings({ serverId }: { serverId: string }) {
  const { t } = useLingui();
  const query = useQuery(
    DockerService.method.getDockerSettings,
    { serverId },
    { retry: false, refetchOnWindowFocus: false },
  );
  const [showLogs, setShowLogs] = useState(false);
  const summary = useQuery(
    DockerService.method.getDockerSettings,
    { serverId, loggingSummary: true },
    { enabled: Boolean(query.data?.settings), retry: false, refetchOnWindowFocus: false },
  );
  const data = query.data?.settings;
  const refresh = () => {
    void query.refetch();
    void summary.refetch();
  };
  return (
    <section className="docker-settings">
      <div className="docker-section-heading">
        <h2>
          <Trans>Docker settings</Trans>
        </h2>
      </div>
      {data?.recoveryAttention ? (
        <Callout tone="warning" title={t`Docker settings need attention`}>
          {data.recoveryAttention}
        </Callout>
      ) : null}
      {query.isPending ? (
        <Loading
          label={t`Loading Docker settings`}
          slow={t`Docker is reading this server's settings`}
          className="docker-settings-loading"
        >
          <div className="docker-settings-layout">
            <div className="docker-settings-column">
              <SkeletonCard title={t`Registry mirrors`}>
                <SkeletonField />
              </SkeletonCard>
              <SkeletonCard title={t`Logging`}>
                <SkeletonField />
              </SkeletonCard>
              <SkeletonCard title={t`Default address pools`}>
                <SkeletonField />
              </SkeletonCard>
            </div>
            <div className="docker-settings-column">
              <SkeletonCard title={t`Engine`}>
                <SkeletonFacts count={6} layout="rows" />
              </SkeletonCard>
              <SkeletonCard title={t`Read-only settings`}>
                <SkeletonFacts count={5} layout="rows" />
              </SkeletonCard>
            </div>
          </div>
        </Loading>
      ) : data ? (
        <div className="docker-settings-layout">
          <div className="docker-settings-column">
            <SettingsEditor
              key={`${data.managedRevision}:${data.pendingFileReview}`}
              data={data}
              serverId={serverId}
              refresh={refresh}
              showLogs={showLogs}
              toggleLogs={() => setShowLogs(!showLogs)}
              mismatch={summary.data?.settings?.runningLoggingMismatch}
              summaryPending={summary.isPending}
            />
          </div>
          <div className="docker-settings-column">
            <Panel
              title={t`Engine`}
              actions={data.engineVersion ? <StatusBadge status="healthy" label={t`Running`} labelIsState /> : null}
            >
              <dl className="docker-fact-grid">
                <Fact label={t`Engine version`} value={data.engineVersion || t`Unknown`} mono />
                <Fact label={t`API version`} value={data.apiVersion || t`Unknown`} mono />
                {data.endpoint ? <Fact label={t`Endpoint`} value={data.endpoint} mono /> : null}
                {data.mode ? <Fact label={t`Mode`} value={data.mode} /> : null}
                {data.manager ? <Fact label={t`Manager`} value={data.manager} /> : null}
                {data.source ? <Fact label={t`Configuration source`} value={data.source} mono /> : null}
              </dl>
              <div className="docker-settings-service">
                <RouterButtonLink
                  variant="ghost"
                  size="sm"
                  to="/servers/$serverId/services"
                  params={{ serverId }}
                  search={{ unit: "docker.service" }}
                >
                  <Trans>Docker service and logs</Trans>
                </RouterButtonLink>
              </div>
            </Panel>
            <ReadOnlySettings data={data} />
          </div>
        </div>
      ) : (
        <Callout
          tone="critical"
          title={t`Docker settings unavailable`}
          actions={<Button onClick={refresh}>{t`Retry`}</Button>}
        >
          {sentence(query.error)}
        </Callout>
      )}
      {showLogs ? <UnboundedLogs serverId={serverId} logging={data?.logging} onChanged={refresh} /> : null}
    </section>
  );
}

function SettingsEditor({
  data,
  serverId,
  refresh,
  showLogs,
  toggleLogs,
  mismatch,
  summaryPending,
}: {
  data: Settings;
  serverId: string;
  refresh: () => void;
  showLogs: boolean;
  toggleLogs: () => void;
  mismatch?: number;
  summaryPending: boolean;
}) {
  const { t } = useLingui();
  const readonly = Boolean(data.editabilityReason);
  const editableLogging = !readonly && (data.logging?.driver === "json-file" || data.logging?.driver === "local");
  const transport = useTransport();
  const sheet = useChangeSheet();
  const [edited, setEdited] = useState<Partial<Record<Group, boolean>>>({});
  const [fields, setFields] = useState<Fields>(() => ({
    mirrors: data.mirrors,
    driver: data.logging?.driver === "local" ? "local" : "json-file",
    size: String(data.logging?.maxSizeBytes ? Number(data.logging.maxSizeBytes) / mebibyte : 10),
    files: String(data.logging?.maxFiles || 5),
    pools: data.pools.map((pool, key) => ({ key, base: pool.base, prefix: String(pool.prefixLength) })),
  }));
  function set<K extends keyof Fields>(key: K, value: Fields[K], group: Group) {
    setFields((before) => ({ ...before, [key]: value }));
    setEdited((before) => ({ ...before, [group]: true }));
  }
  const dirty = Object.values(edited).some(Boolean);
  const validLogging =
    !edited.logging ||
    (/^\d+$/.test(fields.size) &&
      /^\d+$/.test(fields.files) &&
      Number(fields.size) >= 1 &&
      Number(fields.size) <= 1024 &&
      Number(fields.files) >= 1 &&
      Number(fields.files) <= 100);
  const validPools =
    !edited.addressPools ||
    (fields.pools.length > 0 &&
      fields.pools.every(({ prefix }) => /^\d+$/.test(prefix) && Number(prefix) >= 8 && Number(prefix) <= 30));
  const validMirrors = fields.mirrors.every((mirror) => validMirrorURL(mirror.trim()));
  const valid = validMirrors && validLogging && validPools;
  const draft = useMemo(() => {
    const owns = (group: Group) =>
      edited[group] || data.provenance.some((entry) => entry.field === group && entry.sourceKind === "Dockwell");
    const next: Draft = {};
    if (owns("mirrors")) next.mirrors = { urls: fields.mirrors.map((value) => value.trim()) };
    if (owns("logging")) {
      next.logging = edited.logging
        ? {
            driver: fields.driver,
            maxSizeBytes: BigInt(Math.trunc(Math.min(1024, Number(fields.size) || 0))) * BigInt(mebibyte),
            maxFiles: Number(fields.files) || 0,
          }
        : data.logging;
    }
    if (owns("addressPools"))
      next.addressPools = {
        pools: fields.pools.map(({ base, prefix }) => ({
          base: base.trim(),
          prefixLength: validPools ? Number(prefix) : 0,
        })),
      };
    return create(DockerDaemonSchema, next);
  }, [data.logging, data.provenance, edited, fields, validPools]);
  const [checkedDraft, setCheckedDraft] = useState<typeof draft>();
  useEffect(() => {
    if (!dirty || !valid || readonly) return;
    const timer = window.setTimeout(() => setCheckedDraft(draft), 350);
    return () => window.clearTimeout(timer);
  }, [draft, dirty, valid, readonly]);
  const check = useQuery(
    DockerService.method.checkDockerSettings,
    { serverId, draft: checkedDraft },
    {
      enabled: dirty && valid && checkedDraft === draft && !readonly,
      retry: false,
      refetchOnWindowFocus: false,
    },
  );
  const checking = dirty && valid && (checkedDraft !== draft || check.isFetching);
  const finding = checkedDraft === draft && !checking ? check.data?.findings[0] : undefined;
  const ready = dirty && valid && !checking && !finding && check.isSuccess;
  function reviewSettings(currentFile = false) {
    const client = createClient(ChangeService, transport);
    const proposed = currentFile ? create(DockerDaemonSchema, { reviewCurrentFile: true }) : draft;
    sheet.open({
      kind: "plan",
      make: () =>
        client.proposeChange({
          summary: currentFile ? t`Activate the current Docker settings file` : t`Change Docker settings`,
          edits: [
            {
              resource: { kind: "docker.daemon", scope: serverId, name: "daemon" },
              baseRevision: data.managedRevision,
              spec: { kind: { case: "dockerDaemon", value: proposed } },
            },
          ],
        }),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) {
          if (!currentFile) setEdited({});
          refresh();
        }
      },
    });
  }
  const pendingFile = data.pendingFileReview || finding?.reviewCurrentFile;
  const limit = data.logging?.maxSizeBytes;
  const count = data.logging?.maxFiles;
  return (
    <>
      {readonly && data.editabilityReason !== data.recoveryAttention ? (
        <Callout title={t`Settings are read-only`}>{data.editabilityReason}</Callout>
      ) : null}
      {pendingFile && !readonly ? (
        <Callout
          tone="warning"
          actions={
            <Button onClick={() => reviewSettings(true)}>
              <Trans>Review the current file</Trans>
            </Button>
          }
        >
          <Trans>Docker has pending edits in daemon.json. Review them before changing settings.</Trans>
        </Callout>
      ) : null}
      <Panel
        title={t`Registry mirrors`}
        actions={
          !readonly && fields.mirrors.length < 16 ? (
            <Button size="sm" variant="secondary" onClick={() => set("mirrors", [...fields.mirrors, ""], "mirrors")}>
              <Trans>Add mirror</Trans>
            </Button>
          ) : null
        }
      >
        <div className="docker-setting-fields">
          {fields.mirrors.length === 0 ? (
            <span className="docker-cleanup-meta">
              <Trans>No registry mirrors</Trans>
            </span>
          ) : null}
          {fields.mirrors.map((mirror, index) => (
            // The editable list preserves sequence. Removal updates the positions of later fields.
            // biome-ignore lint/suspicious/noArrayIndexKey: mirrors are edited in place and may have duplicate values in a draft.
            <div className="docker-setting-mirror" key={index}>
              <TextField
                aria-label={t`Registry mirror URL ${index + 1}`}
                placeholder="https://registry.example"
                value={mirror}
                mono
                inputMode="url"
                error={
                  mirror && !validMirrorURL(mirror.trim())
                    ? t`Enter an HTTPS URL without credentials, a query or a fragment.`
                    : undefined
                }
                disabled={readonly}
                onChange={(event) =>
                  set(
                    "mirrors",
                    fields.mirrors.map((url, i) => (i === index ? event.target.value : url)),
                    "mirrors",
                  )
                }
              />
              {!readonly ? (
                <Button
                  variant="ghost"
                  size="sm"
                  aria-label={t`Remove mirror ${index + 1}`}
                  onClick={() =>
                    set(
                      "mirrors",
                      fields.mirrors.filter((_, i) => i !== index),
                      "mirrors",
                    )
                  }
                >
                  <Trans>Remove</Trans>
                </Button>
              ) : null}
              {edited.mirrors && checkedDraft === draft && !checking ? (
                <MirrorStatus status={check.data?.mirrorReachability[index]} />
              ) : null}
            </div>
          ))}
        </div>
      </Panel>
      <Panel title={t`Logging`}>
        <div className="docker-setting-fields">
          <div className="docker-setting-summary">
            <Tag>{data.logging?.driver || t`Unknown driver`}</Tag>
            {limit && count ? (
              <span>
                {Number(limit) / mebibyte} MiB × {count}
              </span>
            ) : null}
            {data.provenance.some((item) => item.field === "logging" && item.availability === "Configured") ? (
              <Tag>
                <Trans>Configured</Trans>
              </Tag>
            ) : null}
          </div>
          <span className="docker-cleanup-meta">
            <Trans>Defaults apply when Docker creates a container.</Trans>
          </span>
          <div className="docker-setting-summary">
            {summaryPending ? (
              <Loading label={t`Checking logging defaults`} slow={t`Docker is checking running containers`}>
                <Skeleton shape="line" width="12rem" />
              </Loading>
            ) : (
              <span className="docker-cleanup-meta">
                {mismatch === undefined ? (
                  <Trans>Container comparison unavailable</Trans>
                ) : (
                  <Plural value={mismatch} one="# running container differs" other="# running containers differ" />
                )}
              </span>
            )}
            <Button
              variant="ghost"
              size="sm"
              onClick={toggleLogs}
              aria-expanded={showLogs}
              aria-controls="docker-unbounded-logs"
            >
              {showLogs ? t`Hide containers` : t`Review containers`}
            </Button>
          </div>
          {editableLogging ? (
            <details>
              <summary className="dw-link">
                <Trans>Change logging defaults</Trans>
              </summary>
              <div className="docker-setting-fields">
                <SelectField
                  label={t`Driver`}
                  value={fields.driver}
                  options={[
                    { value: "json-file", label: "json-file" },
                    { value: "local", label: "local" },
                  ]}
                  onChange={(event) => set("driver", event.target.value, "logging")}
                />
                <div className="docker-setting-pair">
                  <TextField
                    label={t`Maximum size per file`}
                    value={fields.size}
                    suffix="MiB"
                    inputMode="numeric"
                    onChange={(event) => set("size", event.target.value, "logging")}
                  />
                  <TextField
                    label={t`Maximum files`}
                    value={fields.files}
                    inputMode="numeric"
                    onChange={(event) => set("files", event.target.value, "logging")}
                  />
                </div>
              </div>
            </details>
          ) : !readonly ? (
            <span className="docker-cleanup-meta">
              <Trans>The active log driver is managed outside Dockwell.</Trans>
            </span>
          ) : null}
        </div>
      </Panel>
      <Panel title={t`Default address pools`}>
        <div className="docker-setting-fields">
          {fields.pools.length === 0 ? (
            <span className="docker-cleanup-meta">
              <Trans>Docker reports no configured pools</Trans>
            </span>
          ) : null}
          {fields.pools.map((pool) => (
            <div className="docker-setting-pool" key={pool.key}>
              <TextField
                label={t`Base subnet`}
                value={pool.base}
                mono
                disabled={readonly}
                onChange={(event) =>
                  set(
                    "pools",
                    fields.pools.map((row) => (row.key === pool.key ? { ...row, base: event.target.value } : row)),
                    "addressPools",
                  )
                }
              />
              <TextField
                label={t`Allocation prefix length`}
                value={pool.prefix}
                inputMode="numeric"
                disabled={readonly}
                onChange={(event) =>
                  set(
                    "pools",
                    fields.pools.map((row) => (row.key === pool.key ? { ...row, prefix: event.target.value } : row)),
                    "addressPools",
                  )
                }
              />
              {!readonly ? (
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() =>
                    set(
                      "pools",
                      fields.pools.filter((row) => row.key !== pool.key),
                      "addressPools",
                    )
                  }
                >
                  <Trans>Remove</Trans>
                </Button>
              ) : null}
            </div>
          ))}
          {!readonly && fields.pools.length < 16 ? (
            <Button
              className="docker-setting-add-pool"
              variant="secondary"
              size="sm"
              onClick={() =>
                set(
                  "pools",
                  [
                    ...fields.pools,
                    { key: Math.max(-1, ...fields.pools.map(({ key }) => key)) + 1, base: "", prefix: "24" },
                  ],
                  "addressPools",
                )
              }
            >
              <Trans>Add address pool</Trans>
            </Button>
          ) : null}
        </div>
      </Panel>
      {dirty ? (
        <div className="docker-settings-review">
          {checking ? (
            <Loading label={t`Checking Docker settings`} slow={t`Docker is checking these defaults`}>
              <SkeletonField />
            </Loading>
          ) : null}
          {!validLogging ? (
            <Callout tone="critical">
              <Trans>Enter a size from 1 to 1,024 MiB and 1 to 100 files.</Trans>
            </Callout>
          ) : null}
          {!validPools ? (
            <Callout tone="critical">
              <Trans>Enter an address pool with a prefix length from 8 to 30.</Trans>
            </Callout>
          ) : null}
          {(finding && !finding.reviewCurrentFile) || (checkedDraft === draft && check.error) ? (
            <Callout tone="critical">{finding?.detail || sentence(check.error)}</Callout>
          ) : null}
          {ready ? (
            <Tag>
              {check.data?.impact?.action === DockerDaemonAction.RELOAD
                ? t`Reload Docker`
                : check.data?.impact?.action === DockerDaemonAction.RESTART
                  ? t`Restart Docker`
                  : t`No Docker restart`}
            </Tag>
          ) : null}
          <Button variant="primary" disabled={!ready || pendingFile} onClick={() => reviewSettings()}>
            <Trans>Review changes</Trans>
          </Button>
        </div>
      ) : null}
    </>
  );
}

export function validMirrorURL(value: string) {
  if (!/^https:\/\/[^/]/.test(value) || /[\s?#\\]/.test(value) || value.length > 512) return false;
  try {
    const url = new URL(value);
    return Boolean(url.hostname && !url.username && !url.password);
  } catch {
    return false;
  }
}

function MirrorStatus({ status }: { status?: DockerMirrorReachability }) {
  return (
    <div className="docker-mirror-status" aria-live="polite">
      {status === DockerMirrorReachability.REACHABLE ? (
        <Tag>
          <Trans>Reachable from server</Trans>
        </Tag>
      ) : status === DockerMirrorReachability.UNREACHABLE ? (
        <Tag>
          <Trans>Server could not reach registry</Trans>
        </Tag>
      ) : status === DockerMirrorReachability.NOT_CHECKED ? (
        <Tag>
          <Trans>Reachability unverified</Trans>
        </Tag>
      ) : null}
    </div>
  );
}

function ReadOnlySettings({ data }: { data: Settings }) {
  const { t } = useLingui();
  return (
    <Panel title={t`Read-only settings`}>
      <div className="docker-setting-fields">
        <dl className="docker-fact-grid">
          <Fact label={t`Live restore`} value={state(data.liveRestore, t`On`, t`Off`, t`Unknown`)} />
          <Fact label="IPv6" value={state(data.ipv6, t`On`, t`Off`, t`Unknown`)} />
          {data.dataRoot ? <Fact label={t`Data root`} value={data.dataRoot} mono /> : null}
          {data.imageStore ? <Fact label={t`Image store`} value={data.imageStore} /> : null}
          <Fact label={t`Insecure registries`} value={data.insecureEntries.join(", ") || t`None reported`} mono />
          {Object.entries(data.knownFeatures)
            .sort(([a], [b]) => a.localeCompare(b))
            .map(([feature, enabled]) => (
              <Fact key={feature} label={feature} value={enabled ? t`On` : t`Off`} />
            ))}
        </dl>
        <span className="docker-cleanup-meta">
          <Trans>Managed outside Dockwell, with migration for storage changes.</Trans>
        </span>
        <details>
          <summary className="dw-link">
            <Trans>View supported configuration</Trans>
          </summary>
          <pre className="docker-settings-json">
            {JSON.stringify(
              {
                "registry-mirrors": data.mirrors,
                "log-driver": data.logging?.driver,
                "log-opts":
                  data.logging?.maxSizeBytes && data.logging.maxFiles
                    ? { "max-size": String(data.logging.maxSizeBytes), "max-file": String(data.logging.maxFiles) }
                    : undefined,
                "default-address-pools": data.pools.map((pool) => ({ base: pool.base, size: pool.prefixLength })),
              },
              null,
              2,
            )}
          </pre>
        </details>
      </div>
    </Panel>
  );
}

function state(value: boolean | undefined, yes: string, no: string, unknown: string) {
  return value === undefined ? unknown : value ? yes : no;
}

function UnboundedLogs({
  serverId,
  logging,
  onChanged,
}: {
  serverId: string;
  logging?: Settings["logging"];
  onChanged: () => void;
}) {
  const { t } = useLingui();
  const client = createClient(DockerService, useTransport());
  const sheet = useChangeSheet();
  const [outside, setOutside] = useState<DockerObject>();
  const query = useInfiniteQuery({
    queryKey: ["docker-unbounded-logs", serverId],
    queryFn: ({ pageParam, signal }) =>
      client.listDockerObjects(
        {
          serverIds: [serverId],
          kind: DockerObjectKind.CONTAINER,
          pageToken: pageParam,
          pageSize: 20,
          filter: { logRotation: DockerLogRotationState.UNBOUNDED },
        },
        { signal },
      ),
    initialPageParam: "",
    getNextPageParam: (page) => page.nextPageToken || undefined,
    retry: false,
  });
  const rows = query.data?.pages.flatMap((page) => page.objects) ?? [];
  const unavailable = query.data?.pages.some((page) => page.unavailable.length > 0);
  const suggested =
    logging?.maxSizeBytes && logging.maxFiles && (logging.driver === "json-file" || logging.driver === "local")
      ? logging
      : rotation;
  useEffect(() => {
    document.getElementById("docker-unbounded-logs")?.scrollIntoView({ block: "start" });
  }, []);
  return (
    <div id="docker-unbounded-logs">
      <Panel title={t`Containers with unbounded logs`} flush>
        <Table
          caption={t`Containers with unbounded logs`}
          rows={rows}
          rowKey="name"
          loading={query.isPending}
          loadingRows={4}
          loadingLabel={t`Checking container log limits`}
          slow={t`Docker is checking the containers’ log limits`}
          columns={[
            {
              key: "container",
              header: t`Container`,
              render: (row) => (
                <Link
                  className="dw-link docker-row-link"
                  to="/servers/$serverId/docker/containers/$containerId"
                  params={{ serverId, containerId: row.key?.engineId || row.name }}
                >
                  {row.name}
                </Link>
              ),
            },
            {
              key: "limits",
              header: t`Log limits`,
              render: () => (
                <Tag>
                  <Trans>Unbounded</Trans>
                </Tag>
              ),
            },
            {
              key: "review",
              header: t`Action`,
              render: (row) =>
                row.ownership === DockerOwnership.MANAGED && row.key ? (
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() =>
                      sheet.open({
                        kind: "plan",
                        make: () =>
                          client.planDockerContainerLogging({
                            container: row.key,
                            baseRevision: row.managedRevision,
                            logging: { mode: ContainerLoggingMode.LIMITS, limits: suggested },
                          }),
                        onClose: (change) => {
                          if (change?.state === ChangeState.APPLIED) {
                            void query.refetch();
                            onChanged();
                          }
                        },
                      })
                    }
                  >
                    <Trans>Review log rotation</Trans>
                  </Button>
                ) : row.ownership === DockerOwnership.EXTERNAL ? (
                  <Button variant="secondary" size="sm" onClick={() => setOutside(row)}>
                    <Trans>Take on</Trans>
                  </Button>
                ) : null,
            },
          ]}
        />
        {unavailable || query.isError ? (
          <Callout tone="critical" title={t`Some log limits are unavailable`}>
            {query.error ? sentence(query.error) : t`Open Docker settings again when the server responds.`}
          </Callout>
        ) : null}
        {query.isSuccess && !unavailable && rows.length === 0 && !query.hasNextPage ? (
          <div className="docker-setting-empty">
            <Trans>No unbounded logs found</Trans>
          </div>
        ) : null}
        {query.hasNextPage ? (
          <div className="docker-setting-empty">
            <Button variant="secondary" disabled={query.isFetching} onClick={() => void query.fetchNextPage()}>
              <Trans>Check more containers</Trans>
            </Button>
          </div>
        ) : null}
        {outside ? <DockerTakeOn row={outside} onClose={() => setOutside(undefined)} /> : null}
      </Panel>
    </div>
  );
}
