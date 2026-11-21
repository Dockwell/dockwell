// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useTransport } from "@connectrpc/connect-query";
import { type DockerObject, ServerService } from "@dockwell/api";
import { useQueries } from "@tanstack/react-query";
import { useMay } from "../auth/may";
import { diskMeasurementOptions } from "../servers/diskMeasurement";

// Inventory opens immediately. The disk request shares the Overview's query
// and the agent's coalesced measurement, even when several volumes are shown.
export function useVolumeMeasurements(rows: readonly DockerObject[]) {
  const transport = useTransport();
  const allowed = useMay()(ServerService.method.getServerDiskUse).allowed;
  const ids = [
    ...new Set(
      rows.flatMap((row) =>
        row.key &&
        row.detail.case === "volume" &&
        row.detail.value.driver === "local" &&
        row.detail.value.createdAt &&
        row.detail.value.sizeBytes === undefined
          ? [row.key.serverId]
          : [],
      ),
    ),
  ];
  const reads = useQueries({
    queries: ids.map((id) => ({
      ...diskMeasurementOptions(id, transport),
      enabled: allowed,
    })),
  });

  return (row: DockerObject) => {
    if (row.detail.case !== "volume") return undefined;
    const volume = row.detail.value;
    if (volume.sizeBytes !== undefined && volume.sizeObservedAt) {
      return { bytes: volume.sizeBytes, time: volume.sizeObservedAt };
    }
    const read = reads[ids.indexOf(row.key?.serverId ?? "")];
    const disk = read?.data?.diskUse;
    if (allowed && read && !read.error && (read.isPending || (disk && !disk.time))) return "pending";
    if (!volume.createdAt || !disk?.time || timestampMs(disk.time) < timestampMs(volume.createdAt)) return undefined;
    const measured = disk.volumeUse.find((item) => item.name === row.name && item.mountpoint === volume.mountpoint);
    return measured && measured.sizeBytes >= 0n ? { bytes: measured.sizeBytes, time: disk.time } : undefined;
  };
}
