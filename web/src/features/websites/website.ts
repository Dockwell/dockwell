// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type Website, type WebsiteInfo, WebsiteService } from "@dockwell/api";
import { useMemo } from "react";

/** What a website does, as its settings say. */
export type WebsiteKind = "folder" | "php" | "url" | "port" | "unknown";

export function kindOf(spec: Website | undefined): WebsiteKind {
  if (spec?.serves.case === "folder") return "folder";
  if (spec?.serves.case === "php") return "php";
  if (spec?.serves.case !== "proxy") return "unknown";
  return spec.serves.value.port ? "port" : "url";
}

/** What a website serves from: its folder, a URL, or a port on the server. Empty for one that says nothing. */
export function targetOf(spec: Website | undefined): string {
  if (spec?.serves.case === "folder" || spec?.serves.case === "php") return spec.serves.value.root;
  if (spec?.serves.case !== "proxy") return "";
  const { url, port } = spec.serves.value;
  return port ? `127.0.0.1:${port}` : url;
}

/**
 * Every website, as the API lists them. The plan sheet reads them again once a change
 * applies, as it does every list of what a change can make.
 */
export function useWebsites() {
  return useQuery(WebsiteService.method.listWebsites, {}, { select: (res) => res.websites });
}

/**
 * Which website an environment is, by its ID, so what lists services can leave a
 * website's own out, or lead to the website (ADR 0039). Undefined until the websites
 * are read.
 */
export function useWebsiteOfEnvironment(): ((environmentId: string) => WebsiteInfo | undefined) | undefined {
  const query = useWebsites();
  const data = query.data;
  return useMemo(() => {
    if (!data) return undefined;
    const byEnvironment = new Map(data.map((w) => [w.environmentId, w]));
    return (environmentId: string) => byEnvironment.get(environmentId);
  }, [data]);
}

/** The versions of PHP a website can run, newest first, as the control plane offers them. */
export function useRuntimes() {
  return useQuery(
    WebsiteService.method.listWebsiteRuntimes,
    {},
    { select: (res) => res.runtimes, staleTime: Number.POSITIVE_INFINITY },
  );
}

/** One website, by its project's ID, from the list every website screen shares. */
export function useWebsite(id: string): {
  website: WebsiteInfo | undefined;
  isPending: boolean;
  error: Error | null;
  refetch: () => void;
} {
  const query = useWebsites();
  return {
    website: query.data?.find((w) => w.id === id),
    isPending: query.isPending,
    error: query.error,
    refetch: () => void query.refetch(),
  };
}

/**
 * The lines of a config with whether each differs from the one before: a line is new
 * when the old config has no line like it in the same order, as the longest run they
 * share says.
 */
export function changedLines(before: string, after: string): { n: number; text: string; changed: boolean }[] {
  const a = lines(before);
  const b = lines(after);
  if (before === "") return b.map((text, i) => ({ n: i + 1, text, changed: false }));
  // The longest common subsequence, from the end, which configs this short afford.
  const keep: number[][] = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      const row = keep[i] as number[];
      const next = keep[i + 1] as number[];
      row[j] = a[i] === b[j] ? (next[j + 1] as number) + 1 : Math.max(next[j] as number, row[j + 1] as number);
    }
  }
  const out: { n: number; text: string; changed: boolean }[] = [];
  let i = 0;
  let j = 0;
  while (j < b.length) {
    if (i < a.length && a[i] === b[j]) {
      out.push({ n: j + 1, text: b[j] as string, changed: false });
      i++;
      j++;
    } else if (i < a.length && (keep[i + 1]?.[j] ?? 0) >= (keep[i]?.[j + 1] ?? 0)) {
      i++;
    } else {
      out.push({ n: j + 1, text: b[j] as string, changed: true });
      j++;
    }
  }
  return out;
}

function lines(text: string): string[] {
  const all = text.split("\n");
  if (all[all.length - 1] === "") all.pop();
  return all;
}
