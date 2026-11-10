// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import {
  Connection,
  type DiskUsage,
  DiskUsageSchema,
  type Server,
  type ServerDiskUse,
  ServerService,
} from "@dockwell/api";
import { Panel } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";

import { sentence } from "../../lib/errors";
import { formatBytes, formatPercent } from "../../lib/format";
import "./DiskUse.css";

/** How long what takes the disk stands before the page asks again, while it's open. Docker adds up every volume to answer. */
const ASK_EVERY_MS = 5 * 60_000;

/** One part of a disk's bar and its legend. */
interface Part {
  key: string;
  label: string;
  bytes: number;
  note: string;
  /** How dark its swatch is, from ink down to the free space's well. */
  shade: "ink" | "s1" | "s2" | "s3" | "s4" | "free";
}

/**
 * What takes the room on a server's disks: the one Docker keeps its data on, by
 * images, volumes, containers, logs and everything else, as its agent adds it up
 * when this opens (ADR 0040); and any other disk by what's used and free.
 */
export function DiskUsePanel({ server }: { server: Server }) {
  const { t } = useLingui();
  const connected = server.connection === Connection.CONNECTED;
  // An agent that can't read Docker can't ask it either.
  const asking = connected && server.dockerUnreadableSince === undefined;
  const use = useQuery(
    ServerService.method.getServerDiskUse,
    { id: server.id },
    // Docker adds up every volume to answer, so coming back to the tab asks nothing.
    { enabled: asking, staleTime: 30_000, refetchInterval: ASK_EVERY_MS, refetchOnWindowFocus: false, retry: false },
  );
  const disks = server.stats?.disks.filter((disk) => disk.sizeBytes > 0n) ?? [];
  const docker = use.data?.diskUse;
  const dockerDisk = docker && docker.sizeBytes > 0n ? docker : undefined;
  // While Docker adds it up, each disk shows what's used and free, as when it can't say.
  const busy = asking && use.isPending;
  const others = disks.filter((disk) => disk.mountPoint !== dockerDisk?.mountPoint);
  if (!busy && !dockerDisk && disks.length === 0) return null;
  return (
    <Panel
      title={t`Disk usage`}
      actions={
        dockerDisk && !overCounted(dockerDisk) ? <code className="panel-meta">{dockerDisk.mountPoint}</code> : null
      }
      className="disk-use"
    >
      <div className="disk-use-body" aria-busy={busy}>
        {busy && disks.length === 0 ? <DiskSkeleton /> : null}
        {dockerDisk ? <DockerDisk use={dockerDisk} server={server} /> : null}
        {others.map((disk) => (
          <PlainDisk key={disk.mountPoint} disk={disk} server={server} />
        ))}
        {busy && disks.length > 0 ? (
          <p className="disk-use-note">
            <Trans>Adding up what takes the disk Docker keeps its data on.</Trans>
          </p>
        ) : null}
        {use.error && asking && !dockerDisk ? <p className="disk-use-note">{sentence(use.error)}</p> : null}
      </div>
    </Panel>
  );
}

/** The disk Docker keeps its data on, part by part. */
function DockerDisk({ use, server }: { use: ServerDiskUse; server: Server }) {
  const { t, i18n } = useLingui();
  const locale = i18n.locale;
  const bytes = (n: number | bigint) => formatBytes(n, locale);
  const size = Number(use.sizeBytes);
  const used = Number(use.usedBytes);
  const images = Number(use.imagesBytes + use.buildCacheBytes);
  const volumes = Number(use.volumesBytes);
  const containers = Number(use.containersBytes);
  const logs = Number(use.logsBytes);
  const counted = images + volumes + containers + logs;
  if (overCounted(use)) {
    const disk = create(DiskUsageSchema, {
      mountPoint: use.mountPoint,
      sizeBytes: use.sizeBytes,
      usedBytes: use.usedBytes,
    });
    return (
      <>
        <PlainDisk disk={disk} server={server} />
        <p className="disk-use-note">
          {t`Docker counts ${bytes(counted)} for its images, volumes, containers and logs, more than the ${bytes(used)} used on this disk, as some of it is on another disk. So this shows what's used and free.`}
        </p>
      </>
    );
  }
  const other = used - counted;
  const free = Math.max(0, size - used);
  const share = formatPercent(size > 0 ? (free / size) * 100 : 0, locale);
  const imageNotes = [
    plural(use.images, { one: "# image", other: "# images" }),
    use.imagesUnusedBytes > 0n ? t`${bytes(use.imagesUnusedBytes)} unused` : "",
    use.buildCacheBytes > 0n ? t`${bytes(use.buildCacheBytes)} build cache` : "",
  ];
  const volumeNotes = [
    plural(use.volumes, { one: "# volume", other: "# volumes" }),
    use.volumesUnusedBytes > 0n ? t`${bytes(use.volumesUnusedBytes)} unused` : "",
  ];
  const all: Part[] = [
    { key: "images", label: t`Images`, bytes: images, note: join(imageNotes), shade: "ink" },
    { key: "volumes", label: t`Volumes`, bytes: volumes, note: join(volumeNotes), shade: "s1" },
    {
      key: "containers",
      label: t`Containers`,
      bytes: containers,
      note: t`What they wrote outside volumes`,
      shade: "s2",
    },
    { key: "logs", label: t`Logs`, bytes: logs, note: t`What Docker keeps of what they print`, shade: "s3" },
    { key: "other", label: t`Other files`, bytes: other, note: t`The system and anything outside Docker`, shade: "s4" },
    {
      key: "free",
      label: t`Free`,
      bytes: free,
      note: size > 0 ? t`${share} of the disk` : "",
      shade: "free",
    },
  ];
  const parts = all.filter(
    (part) => part.bytes > 0 || part.key === "images" || part.key === "volumes" || part.key === "free",
  );
  const summary = parts.map((part) => `${part.label} ${bytes(part.bytes)}`).join(", ");
  return (
    <div className="disk-use-docker">
      <div
        className="disk-use-bar"
        role="img"
        aria-label={t`Disk usage of ${use.mountPoint}: ${summary}, of ${bytes(size)}`}
      >
        {parts
          .filter((part) => part.bytes > 0)
          .map((part) => (
            <span key={part.key} className={`disk-use-part is-${part.shade}`} style={{ flexGrow: part.bytes }} />
          ))}
      </div>
      <dl className="disk-use-legend">
        {parts.map((part) => (
          <div key={part.key} className="disk-use-item">
            <dt>
              <span className={`disk-use-swatch is-${part.shade}`} />
              {part.label}
            </dt>
            <dd className="disk-use-bytes">{bytes(part.bytes)}</dd>
            {part.note ? <dd className="disk-use-sub">{part.note}</dd> : null}
          </div>
        ))}
      </dl>
    </div>
  );
}

/** A disk Docker doesn't keep its data on, or any disk while Docker can't say: used and free. */
function PlainDisk({ disk, server }: { disk: DiskUsage; server: Server }) {
  const { t, i18n } = useLingui();
  const locale = i18n.locale;
  const info = server.inventory?.disks.find((d) => d.mountPoint === disk.mountPoint);
  const mount = disk.mountPoint;
  const usedBytes = Number(disk.usedBytes);
  const sizeBytes = Number(disk.sizeBytes);
  const used = formatBytes(usedBytes, locale);
  const free = formatBytes(sizeBytes - usedBytes, locale);
  const size = formatBytes(sizeBytes, locale);
  const share = formatPercent((usedBytes / sizeBytes) * 100, locale);
  return (
    <div className="disk">
      <div className="disk-head">
        <code className="disk-mount">{mount}</code>
        {info ? (
          <span className="disk-device">{[info.device, info.filesystem].filter(Boolean).join(" · ")}</span>
        ) : null}
        <span className="disk-share">{t`${share} of ${size} used`}</span>
      </div>
      <div className="disk-bar" role="img" aria-label={t`${mount}: ${used} used, ${free} free, of ${size}`}>
        <span className="disk-used" style={{ flexGrow: usedBytes }} />
        <span className="disk-free" style={{ flexGrow: sizeBytes - usedBytes }} />
      </div>
      <dl className="disk-legend">
        <div>
          <dt>
            <span className="disk-swatch is-used" />
            <Trans>Used</Trans>
          </dt>
          <dd>{used}</dd>
        </div>
        <div>
          <dt>
            <span className="disk-swatch is-free" />
            <Trans>Free</Trans>
          </dt>
          <dd>{free}</dd>
        </div>
      </dl>
    </div>
  );
}

/** Where the breakdown goes while Docker adds it up: its shape, and no numbers that could be wrong. */
function DiskSkeleton() {
  const { t } = useLingui();
  return (
    <div className="disk-use-docker" aria-busy="true">
      <span className="dw-sr">{t`Adding up what takes the disk`}</span>
      <div className="disk-use-bar is-skeleton" />
      <div className="disk-use-legend" aria-hidden="true">
        {["a", "b", "c", "d"].map((key) => (
          <div key={key} className="disk-use-item">
            <span className="disk-use-ghost is-label" />
            <span className="disk-use-ghost is-bytes" />
          </div>
        ))}
      </div>
    </div>
  );
}

/**
 * Whether Docker counts more for its data than its disk has used: some of it is on
 * another disk, such as a volume bound to one, so its parts can't be drawn as
 * shares of this one.
 */
function overCounted(use: ServerDiskUse): boolean {
  return use.imagesBytes + use.buildCacheBytes + use.volumesBytes + use.containersBytes + use.logsBytes > use.usedBytes;
}

function join(parts: readonly string[]): string {
  return parts.filter(Boolean).join(" · ");
}
