// SPDX-License-Identifier: AGPL-3.0-only

/** Which kind of Git host a repository is on, by what deploy on push can tell of it. */
export type GitHostKind = "github" | "gitlab" | "bitbucket" | "gitea";

/** A repository's host, as deploy on push needs it: what it's called, and where its hooks are set. */
export interface GitHost {
  kind: GitHostKind;
  /** The host's name, such as GitHub, or "" for a host of a kind it can't tell, as a Gitea of your own. */
  name: string;
  /** The repository's path on the host, such as acme/web. */
  path: string;
  /** The page where the repository's webhooks are added. It never carries a login the URL had. */
  hooksPage: string;
}

/**
 * The host of a repository at url, as https://github.com/acme/web.git or
 * git@github.com:acme/web.git, or undefined for one that isn't a URL. A host
 * named for GitHub, GitLab or Bitbucket is taken to be one, and any other to
 * be a Gitea or a Forgejo, whose hooks are where Gitea keeps them.
 */
export function gitHost(url: string): GitHost | undefined {
  const raw = url.trim();
  const scp = /^[\w.-]+@([^:/]+):(.+)$/.exec(raw);
  let parsed: URL;
  try {
    parsed = new URL(
      scp ? `https://${scp[1]}/${scp[2]}` : /^[a-z][a-z0-9+.-]*:\/\//i.test(raw) ? raw : `https://${raw}`,
    );
  } catch {
    return undefined;
  }
  const host = parsed.hostname.toLowerCase();
  const path = parsed.pathname
    .replace(/^\/+/, "")
    .replace(/\/+$/, "")
    .replace(/\.git$/, "");
  if (!host || !path) return undefined;
  // A web page is https, or http where the repository itself is served so; never ssh's.
  const scheme = parsed.protocol === "http:" ? "http:" : "https:";
  const origin = `${scheme}//${parsed.host.toLowerCase()}`;
  const base = `${origin}/${path}`;
  if (host === "github.com" || host.startsWith("github.")) {
    return { kind: "github", name: "GitHub", path, hooksPage: `${base}/settings/hooks/new` };
  }
  if (host === "gitlab.com" || host.split(".").includes("gitlab")) {
    return { kind: "gitlab", name: "GitLab", path, hooksPage: `${base}/-/hooks` };
  }
  if (host === "bitbucket.org") {
    return { kind: "bitbucket", name: "Bitbucket", path, hooksPage: `${base}/admin/webhooks` };
  }
  return { kind: "gitea", name: host === "codeberg.org" ? "Codeberg" : "", path, hooksPage: `${base}/settings/hooks` };
}

/**
 * Whether a hook at url can't be reached by the repository's host: one on this
 * machine alone, unless the host is on it too, or on a private network when the
 * host is out on the internet, as GitHub, GitLab.com and Bitbucket are.
 */
export function hookOutOfReach(hookUrl: string, repositoryUrl: string): boolean {
  const hook = hostOf(hookUrl);
  if (hook === undefined) return false;
  const page = gitHost(repositoryUrl)?.hooksPage;
  const repo = page ? hostOf(page) : undefined;
  if (repo !== undefined && loopback(repo)) return false;
  if (loopback(hook)) return true;
  return privateHost(hook) && repo !== undefined && !privateHost(repo);
}

function hostOf(url: string): string | undefined {
  try {
    return new URL(url).hostname.toLowerCase().replace(/^\[|\]$/g, "");
  } catch {
    return undefined;
  }
}

function loopback(host: string): boolean {
  return host === "localhost" || host.endsWith(".localhost") || host === "::1" || host.startsWith("127.");
}

/** A host only a network of its own reaches: a private or shared address, or a name no public DNS has. */
function privateHost(host: string): boolean {
  const v4 = /^(\d+)\.(\d+)\.(\d+)\.(\d+)$/.exec(host);
  if (v4) {
    const [a, b] = [Number(v4[1]), Number(v4[2])];
    return (
      a === 10 ||
      (a === 172 && b >= 16 && b <= 31) ||
      (a === 192 && b === 168) ||
      (a === 100 && b >= 64 && b <= 127) ||
      (a === 169 && b === 254)
    );
  }
  if (host.includes(":")) return /^f[cd]/.test(host) || host.startsWith("fe80:");
  return !host.includes(".") || /\.(local|lan|internal|home\.arpa)$/.test(host);
}
