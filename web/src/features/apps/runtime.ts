// SPDX-License-Identifier: AGPL-3.0-only

import type { Runtime, ServiceInfo, WebsiteRuntime } from "@dockwell/api";

import { appOf } from "./app";

/** The languages an app from a folder runs on, as a spec names them, in the order the panel offers them (ADR 0058). */
export const APP_LANGUAGES = ["node", "python", "go", "java"] as const;

/** A language's two letters, as a runtime's card marks it. */
export const LANGUAGE_MARKS: Readonly<Record<string, string>> = {
  php: "Ph",
  node: "No",
  python: "Py",
  go: "Go",
  java: "Ja",
};

/** What an app from a folder runs on, or undefined for an app from an image. */
export function runtimeOf(service: ServiceInfo): Runtime | undefined {
  return service.spec?.runtime;
}

/** The versions on offer for a language, newest first, as ListWebsiteRuntimes lists them: not the retired ones. */
export function versionsOf(language: string, runtimes: readonly WebsiteRuntime[] | undefined): WebsiteRuntime[] {
  return (runtimes ?? []).filter((r) => r.id === language && !r.retired);
}

/** A version Dockwell knows, on offer or retired. */
export function knownVersion(
  language: string,
  version: string,
  runtimes: readonly WebsiteRuntime[] | undefined,
): WebsiteRuntime | undefined {
  return (runtimes ?? []).find((r) => r.id === language && r.version === version);
}

/** Whether an app from a folder runs on a version Dockwell no longer offers. */
export function onRetired(service: ServiceInfo, runtimes: readonly WebsiteRuntime[] | undefined): boolean {
  const rt = runtimeOf(service);
  return rt !== undefined && knownVersion(rt.language, rt.version, runtimes)?.retired === true;
}

/** What people call a language, such as "Node.js", or its name in a spec while the list loads. */
export function languageName(language: string, runtimes: readonly WebsiteRuntime[] | undefined): string {
  return (runtimes ?? []).find((r) => r.id === language)?.language ?? language;
}

/** What an app runs on, in words, such as "Node.js 24". */
export function runtimeWords(runtime: Runtime, runtimes: readonly WebsiteRuntime[] | undefined): string {
  return `${languageName(runtime.language, runtimes)} ${runtime.version}`.trim();
}

/**
 * What an app from a folder's runtime wrote into its environment, by name, with
 * each value, which isn't secret: what the app sets of its own isn't here, even by
 * the same name. Empty for an app from an image.
 */
export function runtimeEnv(service: ServiceInfo): ReadonlyMap<string, string> {
  return new Map(Object.entries(runtimeOf(service)?.wrote ?? {}));
}

/** The build of its version on offer now, when an app runs an older one, which a plan of the same runtime updates it to. */
export function newerBuild(
  service: ServiceInfo,
  runtimes: readonly WebsiteRuntime[] | undefined,
): WebsiteRuntime | undefined {
  const rt = runtimeOf(service);
  const image = appOf(service)?.image ?? "";
  if (!rt || image === "") return undefined;
  const offered = versionsOf(rt.language, runtimes).find((r) => r.version === rt.version);
  return offered && offered.image !== image ? offered : undefined;
}
