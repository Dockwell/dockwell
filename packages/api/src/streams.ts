// SPDX-License-Identifier: Apache-2.0

// The routes that stream a file's bytes, for any client: an upload's parts sent as
// they're read, and a download saved as it comes (docs/adr/0045-a-file-manager-on-a-server.md).
// A browser can't send a stream's body over Connect, and a download is a link it
// saves, so these use the REST routes the control plane streams, which the OpenAPI
// document describes. Their method, path, query and media types come from that
// document, through gen/streams.ts, and nothing here writes a path of its own.

import { fromJson, type JsonValue } from "@bufbuild/protobuf";

import { type UploadDatabaseImportResponse, UploadDatabaseImportResponseSchema } from "./gen/dockwell/v1/database_pb";
import { type DockerArtifactInfo, DockerArtifactInfoSchema } from "./gen/dockwell/v1/docker_pb";
import { type UploadFileResponse, UploadFileResponseSchema } from "./gen/dockwell/v1/file_pb";
import { type StreamRoute, streamRoutes } from "./gen/streams";

/** The header an error's reason comes in, as for every REST path. */
const reasonHeader = "Dockwell-Error";

/** A route's URL, from baseUrl, with its path and query filled in. */
function routeURL(
  route: StreamRoute,
  path: Record<string, string>,
  query: Record<string, string | number | readonly string[] | undefined>,
  baseUrl = "",
): string {
  const filled = route.path.replace(/\{(\w+)\}/g, (_, name: string) => {
    const value = path[name];
    if (value === undefined) throw new Error(`${route.operationId} needs ${name}`);
    return encodeURIComponent(value);
  });
  const params = new URLSearchParams();
  for (const name of route.query) {
    const value = query[name];
    if (value === undefined) continue;
    if (Array.isArray(value)) {
      for (const item of value) params.append(name, item);
    } else {
      params.set(name, String(value));
    }
  }
  const search = params.toString();
  return baseUrl + filled + (search ? `?${search}` : "");
}

/**
 * A streaming call that didn't work: the status it came back with, 0 where the
 * connection dropped before anything came back, the server's words, and the
 * reason it gave, such as "upload_offset".
 */
export class StreamError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly reason: string,
  ) {
    super(message);
    this.name = "StreamError";
  }
}

/** What an error's body says, or nothing where it isn't a REST error. */
function messageOf(body: string): string {
  try {
    const parsed: unknown = JSON.parse(body);
    if (parsed && typeof parsed === "object" && "message" in parsed && typeof parsed.message === "string") {
      return parsed.message;
    }
  } catch {
    // Not JSON: a proxy's page, say.
  }
  return "";
}

/** One part of an upload, as UploadFile takes it. */
export interface UploadPart {
  serverId: string;
  uploadId: string;
  /** Where the part starts in the file: how far the upload got. */
  offset: number;
  /** The bytes. A Blob is read as it's sent, so a whole file is never in memory. */
  part: Blob;
  /** How much of the part has gone, as it goes. */
  onSent?: (bytes: number) => void;
  signal?: AbortSignal;
  /** Where the API is, when it isn't this page's origin. */
  baseUrl?: string;
}

/**
 * Sends one part of an upload, and answers how far the upload got. It uses
 * XMLHttpRequest, as fetch can't say how much of a request has gone.
 */
function sendPart<T>(
  route: StreamRoute,
  url: string,
  p: Pick<UploadPart, "part" | "onSent" | "signal">,
  decode: (value: JsonValue) => T,
  prefix?: string,
): Promise<T> {
  return new Promise((resolve, reject) => {
    if (p.signal?.aborted) {
      reject(p.signal.reason);
      return;
    }
    const xhr = new XMLHttpRequest();
    const stop = () => xhr.abort();
    p.signal?.addEventListener("abort", stop, { once: true });
    const done = () => p.signal?.removeEventListener("abort", stop);
    xhr.open(route.method, url);
    if (route.requestType) xhr.setRequestHeader("Content-Type", route.requestType);
    if (prefix !== undefined) xhr.setRequestHeader("Dockwell-Artifact-Prefix-SHA256", prefix);
    xhr.upload.onprogress = (event) => p.onSent?.(event.loaded);
    xhr.onload = () => {
      done();
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          resolve(decode(JSON.parse(xhr.responseText || "{}") as JsonValue));
        } catch (err) {
          reject(new StreamError(xhr.status, `The answer to an upload can't be read: ${String(err)}`, ""));
        }
        return;
      }
      reject(new StreamError(xhr.status, messageOf(xhr.responseText), xhr.getResponseHeader(reasonHeader) ?? ""));
    };
    xhr.onerror = () => {
      done();
      reject(new StreamError(0, "", ""));
    };
    xhr.onabort = () => {
      done();
      reject(new DOMException("The upload stopped", "AbortError"));
    };
    xhr.send(p.part);
  });
}

export function uploadFilePart(p: UploadPart): Promise<UploadFileResponse> {
  const route: StreamRoute = streamRoutes.FileService_UploadFile;
  const url = routeURL(route, { serverId: p.serverId, uploadId: p.uploadId }, { offset: p.offset }, p.baseUrl);
  return sendPart(route, url, p, (value) => fromJson(UploadFileResponseSchema, value, { ignoreUnknownFields: true }));
}

/** One chunk of a database dump import upload, sent to DatabaseService.UploadDatabaseImport. */
export interface DatabaseImportPart {
  id: string;
  importId: string;
  offset: number;
  part: Blob;
  project?: string;
  environment?: string;
  onSent?: (bytes: number) => void;
  signal?: AbortSignal;
  baseUrl?: string;
}

export function uploadDatabaseImportPart(p: DatabaseImportPart): Promise<UploadDatabaseImportResponse> {
  const route: StreamRoute = streamRoutes.DatabaseService_UploadDatabaseImport;
  const url = routeURL(
    route,
    { id: p.id, importId: p.importId },
    { offset: p.offset, project: p.project, environment: p.environment },
    p.baseUrl,
  );
  return sendPart(route, url, p, (value) =>
    fromJson(UploadDatabaseImportResponseSchema, value, { ignoreUnknownFields: true }),
  );
}

export type ArtifactPart = Omit<UploadPart, "uploadId"> & { artifactId: string; prefixSHA256: string };

export function uploadDockerArtifactPart(p: ArtifactPart): Promise<DockerArtifactInfo> {
  const route: StreamRoute = streamRoutes.DockerService_UploadDockerArtifact;
  const url = routeURL(route, { serverId: p.serverId, artifactId: p.artifactId }, { offset: p.offset }, p.baseUrl);
  return sendPart(
    route,
    url,
    p,
    (value) => fromJson(DockerArtifactInfoSchema, value, { ignoreUnknownFields: true }),
    p.prefixSHA256,
  );
}

export function downloadDockerArtifactURL(p: { serverId: string; artifactId: string; baseUrl?: string }): string {
  return routeURL(
    streamRoutes.DockerService_DownloadDockerArtifact,
    { serverId: p.serverId, artifactId: p.artifactId },
    {},
    p.baseUrl,
  );
}

/** Where a file, a folder, or several selected paths as a .zip download from, for a link a browser saves. */
export function downloadFileURL(
  p:
    | { serverId: string; path: string; paths?: undefined; baseUrl?: string }
    | { serverId: string; path?: undefined; paths: readonly string[]; baseUrl?: string },
): string {
  return routeURL(
    streamRoutes.FileService_DownloadFile,
    { serverId: p.serverId },
    p.paths ? { paths: p.paths } : { path: p.path },
    p.baseUrl,
  );
}
