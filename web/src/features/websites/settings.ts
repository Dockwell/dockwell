// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import {
  type Website,
  WebsitePathRuleKind,
  WebsitePauseMode,
  type WebsiteRulesSchema,
  type WebsiteSchema,
} from "@dockwell/api";

import { validPort } from "../apps/form";

/** What a website serves, as its forms offer it. */
export type Serves = "folder" | "php" | "url" | "port";

export interface BasicAuthUserDraft {
  id: string;
  username: string;
  secret: string;
  /** Newly entered plaintext password; sent out-of-band in basicAuthPasswords, never stored on Project. */
  password: string;
}

export interface PathRuleDraft {
  id: string;
  from: string;
  to: string;
  kind: "redirect" | "rewrite";
  permanent: boolean;
}

export interface SubpathProxyDraft {
  id: string;
  path: string;
  targetKind: "url" | "service";
  url: string;
  serviceId: string;
  servicePort: string;
  stripPrefix: boolean;
  sendTargetHost: boolean;
}

export interface HeaderDraft {
  id: string;
  name: string;
  value: string;
  remove: boolean;
}

/** What a website's forms hold of what it serves: every kind's fields, so moving between them keeps what was typed. */
export interface Settings {
  serves: Serves;
  url: string;
  port: string;
  targetHost: boolean;
  root: string;
  /** Its index files, separated by spaces, as they're typed. */
  index: string;
  singlePageApp: boolean;
  /**
   * Who owns the folder Dockwell makes when it isn't there, as "uid:gid": the folder
   * above's owner, as the form finds it. Empty leaves it to Docker, which makes it root's.
   */
  owner: string;
  /** PHP's version, such as "8.3". Empty takes the newest. */
  version: string;
  /** Who PHP runs as, by number, as it's typed. */
  user: string;
  /** The most a request may send, in MB, as it's typed. Empty takes 64. */
  uploadMB: string;
  filesOnly: boolean;
  /** PHP tuning fields (empty keeps the runtime default). */
  memoryLimitMB: string;
  maxExecutionTimeS: string;
  postMaxSizeMB: string;
  fpmMaxChildren: string;
  disabledExtensions: string[];
  /** Pause state and remembered mode. */
  paused: boolean;
  pauseMode: WebsitePauseMode;
  /** Structured HTTP rules. */
  basicAuthEnabled: boolean;
  basicAuthPath: string;
  basicAuthRealm: string;
  basicAuthUsers: BasicAuthUserDraft[];
  corsEnabled: boolean;
  corsOrigins: string;
  corsMethods: string;
  corsHeaders: string;
  corsCredentials: boolean;
  corsMaxAge: string;
  rateLimitEnabled: boolean;
  rateLimitRpm: string;
  rateLimitBurst: string;
  pathRules: PathRuleDraft[];
  subpathProxies: SubpathProxyDraft[];
  headers: HeaderDraft[];
  staticCacheEnabled: boolean;
  staticCacheMaxAge: string;
  staticCacheExtensions: string;
  staticCacheImmutable: boolean;
  /** Advanced custom Caddy directives scoped to this website's server block. */
  customCaddy: string;
}

export const BLANK: Settings = {
  serves: "folder",
  url: "",
  port: "",
  targetHost: false,
  root: "",
  index: "",
  singlePageApp: false,
  owner: "",
  version: "",
  user: "",
  uploadMB: "",
  filesOnly: false,
  memoryLimitMB: "",
  maxExecutionTimeS: "",
  postMaxSizeMB: "",
  fpmMaxChildren: "",
  disabledExtensions: [],
  paused: false,
  pauseMode: WebsitePauseMode.MAINTENANCE,
  basicAuthEnabled: false,
  basicAuthPath: "",
  basicAuthRealm: "",
  basicAuthUsers: [],
  corsEnabled: false,
  corsOrigins: "",
  corsMethods: "",
  corsHeaders: "",
  corsCredentials: false,
  corsMaxAge: "",
  rateLimitEnabled: false,
  rateLimitRpm: "60",
  rateLimitBurst: "20",
  pathRules: [],
  subpathProxies: [],
  headers: [],
  staticCacheEnabled: false,
  staticCacheMaxAge: "86400",
  staticCacheExtensions: "",
  staticCacheImmutable: false,
  customCaddy: "",
};

/** The index files a form's words name, in order. */
export function indexOf(written: string): string[] {
  return written.split(/[\s,]+/).filter((name) => name !== "");
}

function splitList(written: string): string[] {
  return written
    .split(/[\n,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

/** What a stored website comes to in a form. */
export function settingsOf(spec: Website | undefined): Settings {
  const rules = spec?.rules;
  const basicAuth = rules?.basicAuth;
  const cors = rules?.cors;
  const rateLimit = rules?.rateLimit;
  const staticCache = rules?.staticCache;
  const common: Partial<Settings> = {
    paused: spec?.paused ?? false,
    pauseMode: spec?.pauseMode || WebsitePauseMode.MAINTENANCE,
    basicAuthEnabled: (basicAuth?.users.length ?? 0) > 0,
    basicAuthPath: basicAuth?.path ?? "",
    basicAuthRealm: basicAuth?.realm ?? "",
    basicAuthUsers: (basicAuth?.users ?? []).map((u, idx) => ({
      id: `ba-${idx}-${u.username}`,
      username: u.username,
      secret: u.secret,
      password: "",
    })),
    corsEnabled: cors?.enabled ?? false,
    corsOrigins: (cors?.allowedOrigins ?? []).join("\n"),
    corsMethods: (cors?.allowedMethods ?? []).join(", "),
    corsHeaders: (cors?.allowedHeaders ?? []).join(", "),
    corsCredentials: cors?.allowCredentials ?? false,
    corsMaxAge: cors?.maxAgeSeconds ? String(cors.maxAgeSeconds) : "",
    rateLimitEnabled: rateLimit?.enabled ?? false,
    rateLimitRpm: rateLimit?.requestsPerMinute ? String(rateLimit.requestsPerMinute) : "60",
    rateLimitBurst: rateLimit?.burst ? String(rateLimit.burst) : "20",
    pathRules: (rules?.pathRules ?? []).map((r, idx) => ({
      id: `pr-${idx}`,
      from: r.from,
      to: r.to,
      kind: r.kind === WebsitePathRuleKind.REWRITE ? "rewrite" : "redirect",
      permanent: r.permanent,
    })),
    subpathProxies: (rules?.subpathProxies ?? []).map((sp, idx) => ({
      id: `sp-${idx}`,
      path: sp.path,
      targetKind: sp.serviceId ? "service" : "url",
      url: sp.url,
      serviceId: sp.serviceId,
      servicePort: sp.servicePort ? String(sp.servicePort) : "",
      stripPrefix: sp.stripPrefix,
      sendTargetHost: sp.sendTargetHost,
    })),
    headers: (rules?.headers ?? []).map((h, idx) => ({
      id: `hd-${idx}`,
      name: h.name,
      value: h.value,
      remove: h.remove,
    })),
    staticCacheEnabled: staticCache?.enabled ?? false,
    staticCacheMaxAge: staticCache?.maxAgeSeconds ? String(staticCache.maxAgeSeconds) : "86400",
    staticCacheExtensions: (staticCache?.extensions ?? []).join(" "),
    staticCacheImmutable: staticCache?.immutable ?? false,
    customCaddy: spec?.customCaddy ?? "",
  };

  switch (spec?.serves.case) {
    case "proxy": {
      const { url, port, sendTargetHost } = spec.serves.value;
      return {
        ...BLANK,
        ...common,
        serves: port ? "port" : "url",
        url,
        port: port ? String(port) : "",
        targetHost: sendTargetHost,
      };
    }
    case "folder": {
      const { root, index, singlePageApp, owner } = spec.serves.value;
      return { ...BLANK, ...common, serves: "folder", root, index: index.join(" "), singlePageApp, owner };
    }
    case "php": {
      const {
        root,
        index,
        version,
        user,
        uploadLimitMb,
        filesOnly,
        memoryLimitMb,
        maxExecutionTimeS,
        postMaxSizeMb,
        fpmMaxChildren,
        disabledExtensions,
      } = spec.serves.value;
      return {
        ...BLANK,
        ...common,
        serves: "php",
        root,
        index: index.join(" "),
        version,
        user,
        uploadMB: uploadLimitMb ? String(uploadLimitMb) : "",
        filesOnly,
        memoryLimitMB: memoryLimitMb ? String(memoryLimitMb) : "",
        maxExecutionTimeS: maxExecutionTimeS ? String(maxExecutionTimeS) : "",
        postMaxSizeMB: postMaxSizeMb ? String(postMaxSizeMb) : "",
        fpmMaxChildren: fpmMaxChildren ? String(fpmMaxChildren) : "",
        disabledExtensions: [...disabledExtensions],
      };
    }
    default:
      return { ...BLANK, ...common };
  }
}

function rulesOf(s: Settings): MessageInitShape<typeof WebsiteRulesSchema> | undefined {
  const basicAuth =
    s.basicAuthEnabled && s.basicAuthUsers.length > 0
      ? {
          path: s.basicAuthPath.trim(),
          realm: s.basicAuthRealm.trim(),
          users: s.basicAuthUsers
            .filter((u) => u.username.trim() !== "")
            .map((u) => ({
              username: u.username.trim(),
              secret: u.secret,
            })),
        }
      : undefined;
  const cors = s.corsEnabled
    ? {
        enabled: true,
        allowedOrigins: splitList(s.corsOrigins),
        allowedMethods: splitList(s.corsMethods),
        allowedHeaders: splitList(s.corsHeaders),
        allowCredentials: s.corsCredentials,
        maxAgeSeconds: Number(s.corsMaxAge) || 0,
      }
    : undefined;
  const rateLimit = s.rateLimitEnabled
    ? {
        enabled: true,
        requestsPerMinute: Number(s.rateLimitRpm) || 60,
        burst: Number(s.rateLimitBurst) || 0,
      }
    : undefined;
  const pathRules = s.pathRules
    .filter((r) => r.from.trim() !== "" || r.to.trim() !== "")
    .map((r) => ({
      from: r.from.trim(),
      to: r.to.trim(),
      kind: r.kind === "rewrite" ? WebsitePathRuleKind.REWRITE : WebsitePathRuleKind.REDIRECT,
      permanent: r.permanent,
    }));
  const subpathProxies = s.subpathProxies
    .filter((sp) => sp.path.trim() !== "" || sp.url.trim() !== "" || sp.serviceId !== "")
    .map((sp) => ({
      path: sp.path.trim(),
      url: sp.targetKind === "url" ? sp.url.trim() : "",
      serviceId: sp.targetKind === "service" ? sp.serviceId : "",
      servicePort: sp.targetKind === "service" ? Number(sp.servicePort) || 0 : 0,
      stripPrefix: sp.stripPrefix,
      sendTargetHost: sp.sendTargetHost,
    }));
  const headers = s.headers
    .filter((h) => h.name.trim() !== "")
    .map((h) => ({
      name: h.name.trim(),
      value: h.remove ? "" : h.value.trim(),
      remove: h.remove,
    }));
  const staticCache = s.staticCacheEnabled
    ? {
        enabled: true,
        maxAgeSeconds: Number(s.staticCacheMaxAge) || 86400,
        extensions: indexOf(s.staticCacheExtensions),
        immutable: s.staticCacheImmutable,
      }
    : undefined;

  if (
    !basicAuth &&
    !cors &&
    !rateLimit &&
    pathRules.length === 0 &&
    subpathProxies.length === 0 &&
    headers.length === 0 &&
    !staticCache
  ) {
    return undefined;
  }
  return {
    basicAuth,
    cors,
    rateLimit,
    pathRules,
    subpathProxies,
    headers,
    staticCache,
  };
}

/** Newly entered Basic Auth plaintext passwords, keyed by username, for UpdateWebsiteRequest.basicAuthPasswords. */
export function basicAuthPasswordsOf(s: Settings): Record<string, string> {
  if (!s.basicAuthEnabled) return {};
  const out: Record<string, string> = {};
  for (const u of s.basicAuthUsers) {
    const name = u.username.trim();
    if (name && u.password) out[name] = u.password;
  }
  return out;
}

/** A form's settings as the API takes them: what the kind chosen needs, and nothing of the others. */
export function specOf(s: Settings): MessageInitShape<typeof WebsiteSchema> {
  const rules = rulesOf(s);
  const common = {
    ...(s.paused ? { paused: true } : {}),
    ...(s.pauseMode !== WebsitePauseMode.UNSPECIFIED && (s.paused || s.pauseMode !== WebsitePauseMode.MAINTENANCE)
      ? { pauseMode: s.pauseMode }
      : {}),
    ...(rules ? { rules } : {}),
    ...(s.customCaddy.trim() !== "" ? { customCaddy: s.customCaddy } : {}),
  };
  switch (s.serves) {
    case "folder":
      return {
        ...common,
        serves: {
          case: "folder",
          value: { root: s.root.trim(), index: indexOf(s.index), singlePageApp: s.singlePageApp, owner: s.owner },
        },
      };
    case "php":
      return {
        ...common,
        serves: {
          case: "php",
          value: {
            root: s.root.trim(),
            index: indexOf(s.index),
            version: s.version,
            user: s.user.trim(),
            uploadLimitMb: Number(s.uploadMB) || 0,
            filesOnly: s.filesOnly,
            memoryLimitMb: Number(s.memoryLimitMB) || 0,
            maxExecutionTimeS: Number(s.maxExecutionTimeS) || 0,
            postMaxSizeMb: Number(s.postMaxSizeMB) || 0,
            fpmMaxChildren: Number(s.fpmMaxChildren) || 0,
            disabledExtensions: [...s.disabledExtensions].sort(),
          },
        },
      };
    case "port":
      return { ...common, serves: { case: "proxy", value: { port: Number(s.port) || 0 } } };
    case "url":
      return { ...common, serves: { case: "proxy", value: { url: s.url.trim(), sendTargetHost: s.targetHost } } };
  }
}

/**
 * What a form sends for a site it changes: what's typed, except where that means what's
 * stored, which goes as it's stored, so the plan lists no change the form doesn't make.
 * 64 MiB typed over a stored default, 0, stays 0.
 */
export function sendable(now: Settings, was: Settings): Settings {
  let next = now;
  if (
    now.serves === "php" &&
    now.uploadMB !== was.uploadMB &&
    (Number(now.uploadMB) || 64) === (Number(was.uploadMB) || 64)
  ) {
    next = { ...next, uploadMB: was.uploadMB };
  }
  return next;
}

/** Which of a form's fields differ from what's stored, by name, for the kind it serves now. */
export function changed(was: Settings, now: Settings): string[] {
  const out: string[] = [];
  if (now.serves !== was.serves) out.push("serves");
  switch (now.serves) {
    case "folder":
      if (now.root.trim() !== was.root) out.push("root");
      if (indexOf(now.index).join(" ") !== indexOf(was.index).join(" ")) out.push("index");
      if (now.singlePageApp !== was.singlePageApp) out.push("singlePageApp");
      break;
    case "php":
      if (now.root.trim() !== was.root) out.push("root");
      if (indexOf(now.index).join(" ") !== indexOf(was.index).join(" ")) out.push("index");
      if (now.version !== was.version) out.push("version");
      if (now.user.trim() !== was.user) out.push("user");
      // Empty, or 0 as stored, is the default, 64 MiB, so typing 64 changes nothing.
      if ((Number(now.uploadMB) || 64) !== (Number(was.uploadMB) || 64)) out.push("uploadMB");
      if (now.filesOnly !== was.filesOnly) out.push("filesOnly");
      if ((Number(now.memoryLimitMB) || 128) !== (Number(was.memoryLimitMB) || 128)) out.push("memoryLimitMB");
      if ((Number(now.maxExecutionTimeS) || 30) !== (Number(was.maxExecutionTimeS) || 30))
        out.push("maxExecutionTimeS");
      if ((Number(now.postMaxSizeMB) || 0) !== (Number(was.postMaxSizeMB) || 0)) out.push("postMaxSizeMB");
      if ((Number(now.fpmMaxChildren) || 5) !== (Number(was.fpmMaxChildren) || 5)) out.push("fpmMaxChildren");
      if ([...now.disabledExtensions].sort().join(",") !== [...was.disabledExtensions].sort().join(","))
        out.push("disabledExtensions");
      break;
    case "url":
      if (now.url.trim() !== was.url) out.push("url");
      if (now.targetHost !== was.targetHost) out.push("targetHost");
      break;
    case "port":
      if (now.port !== was.port) out.push("port");
      break;
  }
  if (
    JSON.stringify(rulesOf(now) ?? null) !== JSON.stringify(rulesOf(was) ?? null) ||
    Object.keys(basicAuthPasswordsOf(now)).length > 0
  ) {
    out.push("rules");
  }
  if (now.customCaddy.trim() !== was.customCaddy.trim()) {
    out.push("customCaddy");
  }
  return was.serves === now.serves
    ? out
    : out.filter((name) => name === "serves" || name === "rules" || name === "customCaddy" || filled(now, name));
}

/** Whether a field was given something, which moving to its kind counts as a change of its own. */
function filled(s: Settings, name: string): boolean {
  if (name === "singlePageApp" || name === "targetHost" || name === "filesOnly") return s[name];
  if (name === "disabledExtensions") return s.disabledExtensions.length > 0;
  return (s[name as keyof Settings] as string) !== "";
}

/** The field a form can say is wrong before the server is asked, and what's wrong with it. */
export interface Missing {
  field:
    | "url"
    | "port"
    | "root"
    | "user"
    | "uploadMB"
    | "memoryLimitMB"
    | "maxExecutionTimeS"
    | "postMaxSizeMB"
    | "fpmMaxChildren"
    | "rules"
    | "customCaddy";
  /** Which of the form's words say it. */
  why:
    | "port"
    | "url"
    | "url-scheme"
    | "root"
    | "root-absolute"
    | "user"
    | "upload"
    | "memoryLimit"
    | "maxExecutionTime"
    | "postMaxSize"
    | "fpmMaxChildren"
    | "basicAuthPassword";
}

/** What a form can refuse by itself: a field the kind needs that's empty, or a port that isn't one. */
export function missing(s: Settings): Missing | undefined {
  if (s.basicAuthEnabled) {
    for (const u of s.basicAuthUsers) {
      if (u.username.trim() !== "" && !u.secret && !u.password) {
        return { field: "rules", why: "basicAuthPassword" };
      }
    }
  }
  switch (s.serves) {
    case "port":
      return validPort(s.port) ? undefined : { field: "port", why: "port" };
    case "url":
      if (s.url.trim() === "") return { field: "url", why: "url" };
      return /^https?:\/\/\S+$/i.test(s.url.trim()) ? undefined : { field: "url", why: "url-scheme" };
    case "folder":
    case "php":
      if (s.root.trim() === "") return { field: "root", why: "root" };
      if (!s.root.trim().startsWith("/")) return { field: "root", why: "root-absolute" };
      // By number, as the server takes it: a user alone, or a user and a group, as it stores them.
      if (s.serves === "php" && s.user.trim() !== "" && !/^\d{1,10}(:\d{1,10})?$/.test(s.user.trim()))
        return { field: "user", why: "user" };
      if (s.serves === "php" && s.uploadMB.trim() !== "" && !/^\d{1,4}$/.test(s.uploadMB.trim()))
        return { field: "uploadMB", why: "upload" };
      if (s.serves === "php" && s.memoryLimitMB.trim() !== "") {
        const n = Number(s.memoryLimitMB);
        if (!Number.isInteger(n) || n < 32 || n > 4096) return { field: "memoryLimitMB", why: "memoryLimit" };
      }
      if (s.serves === "php" && s.maxExecutionTimeS.trim() !== "") {
        const n = Number(s.maxExecutionTimeS);
        if (!Number.isInteger(n) || n < 5 || n > 3600) return { field: "maxExecutionTimeS", why: "maxExecutionTime" };
      }
      if (s.serves === "php" && s.postMaxSizeMB.trim() !== "") {
        const n = Number(s.postMaxSizeMB);
        if (!Number.isInteger(n) || n < 1 || n > 4096) return { field: "postMaxSizeMB", why: "postMaxSize" };
      }
      if (s.serves === "php" && s.fpmMaxChildren.trim() !== "") {
        const n = Number(s.fpmMaxChildren);
        if (!Number.isInteger(n) || n < 1 || n > 200) return { field: "fpmMaxChildren", why: "fpmMaxChildren" };
      }
      return undefined;
  }
}

/** Which field a refusal from the server is about, by what it says, for the kind the form serves. */
export function refusedField(said: string, serves: Serves): Missing["field"] | "" {
  if (/custom caddy|caddyfile|caddy rejected/i.test(said)) return "customCaddy";
  if (/basic auth|cors|rate limit|redirect|rewrite|sub-path|header|static cache/i.test(said)) return "rules";
  if (serves === "php" && /memory_limit|memory limit/i.test(said)) return "memoryLimitMB";
  if (serves === "php" && /max_execution_time|execution time/i.test(said)) return "maxExecutionTimeS";
  if (serves === "php" && /post_max_size|post max/i.test(said)) return "postMaxSizeMB";
  if (serves === "php" && /max_children|pool size/i.test(said)) return "fpmMaxChildren";
  if (serves === "php" && /user's number|root, as anything/i.test(said)) return "user";
  if (serves === "php" && /upload limit/i.test(said)) return "uploadMB";
  if ((serves === "folder" || serves === "php") && /folder|path|absolute|index|file name/i.test(said)) return "root";
  if (serves === "url" && /\bURL\b|http:\/\/|https:\/\/|host name|path|query|container/i.test(said)) return "url";
  if (serves === "port" && /\bport\b/i.test(said)) return "port";
  return "";
}
