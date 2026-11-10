// SPDX-License-Identifier: AGPL-3.0-only

import {
  create,
  type DescEnum,
  type DescField,
  type DescMessage,
  type DescMethod,
  getOption,
  hasOption,
  type MessageInitShape,
  ScalarType,
  toJson,
} from "@bufbuild/protobuf";
import { type ReflectMessage, reflect } from "@bufbuild/protobuf/reflect";
import {
  command as commandOption,
  type Edit,
  EditSchema,
  FieldBehavior,
  field_behavior,
  flag as flagOption,
  from_file,
  http,
  SpecSchema,
  sensitive,
} from "@dockwell/api";

// The dockwell CLI is generated from the API (protoc-gen-dockwell-cli, in
// internal/cli/cligen): a command for each method, and create, update and
// delete for each kind of resource. These are its rules for names and flags,
// so the panel can say which command does what a form does. cli.test.ts holds
// them to the generated table, internal/cli/apicmd/apicmd.gen.go.

/** The CLI's own name. */
export const CLI = "dockwell";

/** A word as a shell reads it: as it is when it's plain, and in single quotes otherwise. */
export function shellWord(word: string): string {
  if (/^[A-Za-z0-9_@%+=:,./-]+$/.test(word)) return word;
  return `'${word.replaceAll("'", `'\\''`)}'`;
}

/** A field's flag: "page_size" is --page-size, and "app.image" --app-image. */
export function flagName(path: string): string {
  return path.replaceAll("_", "-").replaceAll(".", "-");
}

/** A name's words: "ListServers" is List, Servers; "HTTPRoute" is HTTP, Route. */
function camelWords(s: string): string[] {
  const out: string[] = [];
  let start = 0;
  for (let i = 1; i < s.length; i++) {
    const at = s.charAt(i);
    const upper = at !== at.toLowerCase();
    const before = s.charAt(i - 1);
    const after = s.charAt(i + 1);
    const lowerBefore = before !== before.toUpperCase();
    const lowerAfter = after !== "" && after !== after.toUpperCase();
    if (upper && (lowerBefore || lowerAfter)) {
      out.push(s.slice(start, i));
      start = i;
    }
  }
  out.push(s.slice(start));
  return out;
}

/** A name in kebab-case, as commands and flags are: "beginPasskeySignIn" is begin-passkey-sign-in. */
export function kebab(s: string): string {
  return s
    .split("_")
    .flatMap((part) => camelWords(part))
    .filter((w) => w !== "")
    .map((w) => w.toLowerCase())
    .join("-");
}

/** A noun's plural, keeping one that is plural already. */
function plural(s: string): string {
  if (s.endsWith("s")) return s;
  if (s.endsWith("y") && s.length > 1 && !"aeiou".includes(s.charAt(s.length - 2))) return `${s.slice(0, -1)}ies`;
  return `${s}s`;
}

/** PascalCase in snake_case, keeping its case: "TokenScope" is Token_Scope. */
function snake(s: string): string {
  let out = "";
  for (let i = 0; i < s.length; i++) {
    const c = s.charAt(i);
    if (i > 0 && c >= "A" && c <= "Z") out += "_";
    out += c;
  }
  return out;
}

/** An enum value as a flag takes it: without the enum's prefix, in lowercase, with dashes. */
export function enumWord(e: DescEnum, number: number): string {
  const value = e.values.find((v) => v.number === number);
  if (!value) return String(number);
  const prefix = `${snake(e.name).toUpperCase()}_`;
  const name = value.name.startsWith(prefix) ? value.name.slice(prefix.length) : value.name;
  return name.toLowerCase().replaceAll("_", "-");
}

/** A method's command, as dockwell <noun> <verb>, by the generator's rules. */
export function methodName(method: DescMethod): { noun: string; verb: string } {
  const parts = camelWords(method.name);
  let verb = (parts[0] ?? "").toLowerCase();
  let noun = "";
  const rule = hasOption(method, http) ? getOption(method, http) : undefined;
  const path = rule?.pattern.case === "custom" ? rule.pattern.value.path : (rule?.pattern.value as string | undefined);
  if (path === undefined) {
    // A stream has no REST path, so its noun is the rest of its name, plural.
    const rest = parts.slice(1);
    const last = rest.length - 1;
    rest[last] = plural(rest[last] ?? "");
    noun = kebab(rest.join(""));
  } else {
    const segments = path.replace(/^\/v1\//, "").split("/");
    noun = kebab((segments[0] ?? "").split(":")[0] ?? "");
    const custom = (segments[segments.length - 1] ?? "").split(":")[1];
    if (custom !== undefined && !custom.endsWith("}")) verb = kebab(custom);
  }
  if (hasOption(method, commandOption)) {
    const named = getOption(method, commandOption);
    if (named.noun !== "") noun = named.noun;
    if (named.verb !== "") verb = named.verb;
  }
  return { noun, verb };
}

/** A flag of a command, as the generator binds it to a field. */
export interface FlagField {
  /** The field's path from the request or the kind's spec, such as "app.image". */
  path: string;
  /** Its flag, without dashes. */
  flag: string;
  field: DescField;
  /** The argument rather than a flag: a request's id. */
  arg: boolean;
  /** It may hold a secret: a string comes from a file, and a map's values from the environment. */
  sensitive: boolean;
  /** Text that may come from a file instead, with @path. */
  fromFile: boolean;
}

function boundFlag(field: DescField, path: string, arg: boolean): FlagField {
  const isSensitive = hasOption(field, sensitive) && getOption(field, sensitive);
  const scalarText =
    field.fieldKind === "scalar" && (field.scalar === ScalarType.STRING || field.scalar === ScalarType.BYTES);
  const named = hasOption(field, flagOption) ? getOption(field, flagOption) : "";
  let flag = named || flagName(path);
  if (isSensitive && scalarText) flag += "-file";
  return {
    path,
    flag,
    field,
    arg,
    sensitive: isSensitive,
    fromFile: hasOption(field, from_file) && getOption(field, from_file),
  };
}

/** The flags of a method's command: one for each field of its request, and its id as the argument. */
export function methodFlags(method: DescMethod): FlagField[] {
  return method.input.fields.map((field) =>
    boundFlag(
      field,
      field.name,
      field.name === "id" && field.fieldKind === "scalar" && field.scalar === ScalarType.STRING,
    ),
  );
}

function outputOnly(field: DescField): boolean {
  return hasOption(field, field_behavior) && getOption(field, field_behavior).includes(FieldBehavior.OUTPUT_ONLY);
}

/** A kind's flags: its spec's fields, with a message that isn't repeated taken apart into its own, three deep. */
export function specFlags(spec: DescMessage, prefix = "", depth = 0): FlagField[] {
  const out: FlagField[] = [];
  for (const field of spec.fields) {
    const path = prefix + field.name;
    if (outputOnly(field)) continue;
    if (field.fieldKind === "message" && !field.message.typeName.startsWith("google.protobuf.") && depth < 3) {
      out.push(...specFlags(field.message, `${path}.`, depth + 1));
      continue;
    }
    out.push(boundFlag(field, path, false));
  }
  return out;
}

/** The kinds of resource the CLI has commands for, by the field of the spec's kind that holds each. */
export function kindNoun(kind: string): string {
  return kebab(plural(kind));
}

/** What a value of a sensitive map may carry on the command line: the rest come from the environment. */
export type Inline = (path: string, key: string) => boolean;

function words(msg: ReflectMessage, flag: FlagField, inline: Inline): string[] {
  const { field } = flag;
  // The message that holds the field, along its path: app.image is in app.
  let holder: ReflectMessage | undefined = msg;
  for (const name of flag.path.split(".").slice(0, -1)) {
    const f: DescField | undefined = holder.desc.fields.find((x) => x.name === name);
    if (f?.fieldKind !== "message" || !holder.isSet(f)) {
      holder = undefined;
      break;
    }
    holder = holder.get(f) as ReflectMessage;
  }
  if (!holder?.isSet(field)) return [];
  const one = (value: string) => [`--${flag.flag}`, shellWord(value)];
  switch (field.fieldKind) {
    case "scalar": {
      if (flag.sensitive && flag.flag.endsWith("-file")) return [`--${flag.flag}`, "-"];
      const value = holder.get(field);
      if (typeof value === "boolean") return value ? [`--${flag.flag}`] : [];
      return one(String(value));
    }
    case "enum":
      return one(enumWord(field.enum, holder.get(field) as number));
    case "message": {
      const value = holder.get(field) as ReflectMessage;
      return one(messageWord(value));
    }
    case "list": {
      const list = holder.get(field);
      const out: string[] = [];
      for (const item of list) {
        if (field.listKind === "message") out.push(...one(messageWord(item as ReflectMessage)));
        else if (field.listKind === "enum") out.push(...one(enumWord(field.enum, item as number)));
        else out.push(...one(String(item)));
      }
      return out;
    }
    case "map": {
      const map = holder.get(field);
      const out: string[] = [];
      for (const [key, value] of map) {
        const name = String(key);
        if (flag.sensitive && !inline(flag.path, name)) out.push(`--${flag.flag}`, shellWord(name));
        else {
          const text = field.mapKind === "message" ? messageWord(value as ReflectMessage) : String(value);
          out.push(...one(`${name}=${text}`));
        }
      }
      return out;
    }
  }
}

/** A message a flag takes: a time or a duration as words, anything else as JSON. */
function messageWord(value: ReflectMessage): string {
  const name = value.desc.typeName;
  if (name === "google.protobuf.Duration") {
    const seconds = Number(value.get(value.desc.fields[0] as DescField));
    if (seconds % 86400 === 0 && seconds > 0) return `${seconds / 86400}d`;
    if (seconds % 3600 === 0 && seconds > 0) return `${seconds / 3600}h`;
    if (seconds % 60 === 0 && seconds > 0) return `${seconds / 60}m`;
    return `${seconds}s`;
  }
  const json = toJson(value.desc, value.message);
  return typeof json === "string" ? json : JSON.stringify(json);
}

function flagsOf(msg: ReflectMessage, flags: FlagField[], inline: Inline): string[] {
  return flags.filter((f) => !f.arg).flatMap((f) => words(msg, f, inline));
}

const NONE: Inline = () => false;

/** The command that calls a method with a request, as the CLI takes it. Values that may hold a secret stay off it. */
export function methodCommand<I extends DescMessage>(
  method: DescMethod & { input: I },
  request: MessageInitShape<I>,
  inline: Inline = NONE,
): string {
  const { noun, verb } = methodName(method);
  const msg = reflect(method.input, create(method.input, request));
  const flags = methodFlags(method);
  const arg = flags.find((f) => f.arg);
  const id = arg && msg.isSet(arg.field) ? [shellWord(String(msg.get(arg.field)))] : [];
  return [CLI, noun, verb, ...id, ...flagsOf(msg, flags, inline)].join(" ");
}

/** Where a kind's command puts what it makes, each left out where there's no choice to make (PRD principle 4). */
export interface Placement {
  project?: string | undefined;
  environment?: string | undefined;
  /** The servers it runs on, by ID. */
  on?: readonly string[] | undefined;
}

/**
 * The command that makes or changes one resource as an edit would, such as
 * dockwell services create web --app-image nginx:1.27. A change of several
 * edits is a command for each, one to a line, which a shell runs in turn.
 */
export function editCommand(
  verb: "create" | "update",
  edit: MessageInitShape<typeof EditSchema> | Edit,
  placement: Placement = {},
  inline: Inline = NONE,
): string {
  const full = create(EditSchema, edit as MessageInitShape<typeof EditSchema>);
  const kind = full.spec?.kind;
  if (!kind?.case || !kind.value) return "";
  const field = SpecSchema.fields.find((f) => f.localName === kind.case);
  if (field?.fieldKind !== "message") return "";
  const msg = reflect(field.message, kind.value);
  const where: string[] = [];
  if (placement.project) where.push("--project", shellWord(placement.project));
  if (placement.environment) where.push("--environment", shellWord(placement.environment));
  for (const server of placement.on ?? []) where.push("--on", shellWord(server));
  const name = full.resource?.name ?? "";
  return [
    CLI,
    kindNoun(field.name),
    verb,
    shellWord(name),
    ...where,
    ...flagsOf(msg, specFlags(field.message), inline),
  ].join(" ");
}
