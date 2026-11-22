// SPDX-License-Identifier: AGPL-3.0-only

import { type DockerArtifactInfo, StreamError } from "@dockwell/api";

export const ARTIFACT_CHUNK = 64 << 20;
export const MAX_ARTIFACT_BYTES = 8 * 2 ** 30;

export function archiveHash(file: File, signal: AbortSignal) {
  const worker = new Worker(new URL("./artifactHash.worker.ts", import.meta.url), { type: "module" });
  worker.postMessage(file);
  return {
    digest: (count: number) =>
      new Promise<string>((resolve, reject) => {
        const stop = () => reject(signal.reason);
        const finish = () => signal.removeEventListener("abort", stop);
        if (signal.aborted) return stop();
        signal.addEventListener("abort", stop, { once: true });
        worker.onmessage = (event: MessageEvent<string | null>) => {
          finish();
          if (event.data) resolve(event.data);
          else reject(new Error("The selected file could not be read."));
        };
        worker.onerror = () => {
          finish();
          reject(new Error("The selected file could not be read."));
        };
        worker.postMessage(count);
      }),
    close: () => worker.terminate(),
  };
}

/** A paused request may commit after the next viewer reads its offset. */
export async function committedUploadAfterConflict(
  error: unknown,
  read: () => Promise<DockerArtifactInfo>,
  offset: number,
  file: File,
  digest: (end: number) => Promise<string>,
) {
  if (!(error instanceof StreamError) || error.status !== 412) throw error;
  const current = await read();
  const end = Number(current.receivedBytes);
  if (end <= offset || end > file.size || current.sizeBytes !== BigInt(file.size)) throw error;
  if ((await digest(end)) !== current.committedPrefixSha256) throw error;
  return current;
}
