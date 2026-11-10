// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import type { Website, WebsiteSchema } from "@dockwell/api";

import { validPort } from "../apps/form";

/** What a website serves, as its forms offer it. */
export type Serves = "folder" | "php" | "url" | "port";

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
};

/** The index files a form's words name, in order. */
export function indexOf(written: string): string[] {
  return written.split(/[\s,]+/).filter((name) => name !== "");
}

/** What a stored website comes to in a form. */
export function settingsOf(spec: Website | undefined): Settings {
  switch (spec?.serves.case) {
    case "proxy": {
      const { url, port, sendTargetHost } = spec.serves.value;
      return {
        ...BLANK,
        serves: port ? "port" : "url",
        url,
        port: port ? String(port) : "",
        targetHost: sendTargetHost,
      };
    }
    case "folder": {
      const { root, index, singlePageApp, owner } = spec.serves.value;
      return { ...BLANK, serves: "folder", root, index: index.join(" "), singlePageApp, owner };
    }
    case "php": {
      const { root, index, version, user, uploadLimitMb, filesOnly } = spec.serves.value;
      return {
        ...BLANK,
        serves: "php",
        root,
        index: index.join(" "),
        version,
        user,
        uploadMB: uploadLimitMb ? String(uploadLimitMb) : "",
        filesOnly,
      };
    }
    default:
      return BLANK;
  }
}

/** A form's settings as the API takes them: what the kind chosen needs, and nothing of the others. */
export function specOf(s: Settings): MessageInitShape<typeof WebsiteSchema> {
  switch (s.serves) {
    case "folder":
      return {
        serves: {
          case: "folder",
          value: { root: s.root.trim(), index: indexOf(s.index), singlePageApp: s.singlePageApp, owner: s.owner },
        },
      };
    case "php":
      return {
        serves: {
          case: "php",
          value: {
            root: s.root.trim(),
            index: indexOf(s.index),
            version: s.version,
            user: s.user.trim(),
            uploadLimitMb: Number(s.uploadMB) || 0,
            filesOnly: s.filesOnly,
          },
        },
      };
    case "port":
      return { serves: { case: "proxy", value: { port: Number(s.port) || 0 } } };
    case "url":
      return { serves: { case: "proxy", value: { url: s.url.trim(), sendTargetHost: s.targetHost } } };
  }
}

/**
 * What a form sends for a site it changes: what's typed, except where that means what's
 * stored, which goes as it's stored, so the plan lists no change the form doesn't make.
 * 64 MiB typed over a stored default, 0, stays 0.
 */
export function sendable(now: Settings, was: Settings): Settings {
  if (
    now.serves === "php" &&
    now.uploadMB !== was.uploadMB &&
    (Number(now.uploadMB) || 64) === (Number(was.uploadMB) || 64)
  ) {
    return { ...now, uploadMB: was.uploadMB };
  }
  return now;
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
      break;
    case "url":
      if (now.url.trim() !== was.url) out.push("url");
      if (now.targetHost !== was.targetHost) out.push("targetHost");
      break;
    case "port":
      if (now.port !== was.port) out.push("port");
      break;
  }
  return was.serves === now.serves ? out : out.filter((name) => name === "serves" || filled(now, name));
}

/** Whether a field was given something, which moving to its kind counts as a change of its own. */
function filled(s: Settings, name: string): boolean {
  return name === "singlePageApp" || name === "targetHost" || name === "filesOnly"
    ? s[name]
    : (s[name as keyof Settings] as string) !== "";
}

/** The field a form can say is wrong before the server is asked, and what's wrong with it. */
export interface Missing {
  field: "url" | "port" | "root" | "user" | "uploadMB";
  /** Which of the form's words say it. */
  why: "port" | "url" | "url-scheme" | "root" | "root-absolute" | "user" | "upload";
}

/** What a form can refuse by itself: a field the kind needs that's empty, or a port that isn't one. */
export function missing(s: Settings): Missing | undefined {
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
      return undefined;
  }
}

/** Which field a refusal from the server is about, by what it says, for the kind the form serves. */
export function refusedField(said: string, serves: Serves): Missing["field"] | "" {
  if (serves === "php" && /user's number|root, as anything/i.test(said)) return "user";
  if (serves === "php" && /upload limit/i.test(said)) return "uploadMB";
  if ((serves === "folder" || serves === "php") && /folder|path|absolute|index|file name/i.test(said)) return "root";
  if (serves === "url" && /\bURL\b|http:\/\/|https:\/\/|host name|path|query|container/i.test(said)) return "url";
  if (serves === "port" && /\bport\b/i.test(said)) return "port";
  return "";
}
