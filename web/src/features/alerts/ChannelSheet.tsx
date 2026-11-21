// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import {
  type AlertChannelInfo,
  type AlertChannelSchema,
  type AlertDelivery,
  AlertService,
  ChangeService,
  type ConnectionInfo,
  LinkKind,
  AlertEmail_Security as Security,
} from "@dockwell/api";
import { Advanced, type BrandId, BrandMark, Button, Callout, cx, SelectField, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useId, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { useConnections } from "../connections/useConnections";
import { MailPicker } from "./MailPicker";
import { channelConnection, firstAvailableMailConnection, mailConnectionSender } from "./mail";
import { CHANNEL_KIND, CHANNEL_TYPES, type ChannelType, isChannelType, useChannels } from "./useAlerts";

type Spec = MessageInitShape<typeof AlertChannelSchema>;

/** What the form holds, for every type at once, so switching types keeps what was typed. */
interface Form {
  type: ChannelType;
  name: string;
  url: string;
  secret: string;
  connection: string;
  host: string;
  port: string;
  security: Security;
  username: string;
  password: string;
  from: string;
  to: string;
  server: string;
  topic: string;
  token: string;
  botToken: string;
  chatId: string;
}

/** A topic no one would guess, as ntfy.sh lets anyone who knows one read it. */
function makeTopic(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(8));
  return `dockwell-${Array.from(bytes, (b) => b.toString(36).padStart(2, "0").slice(-2)).join("")}`;
}

function blank(): Form {
  return {
    type: "email",
    name: "",
    url: "",
    secret: "",
    connection: "",
    host: MAIL.gmail.host,
    port: String(MAIL.gmail.port),
    security: MAIL.gmail.security,
    username: "",
    password: "",
    from: "",
    to: "",
    server: "",
    topic: makeTopic(),
    token: "",
    botToken: "",
    chatId: "",
  };
}

/** A channel's form, from what its list shows: everything but what's sealed. */
function fromInfo(info: AlertChannelInfo): Form {
  const form = { ...blank(), name: info.name, topic: "", type: isChannelType(info.type) ? info.type : "webhook" };
  const to = info.channel?.to;
  switch (to?.case) {
    case "email":
      return {
        ...form,
        connection: to.value.connection,
        host: to.value.host,
        port: to.value.port ? String(to.value.port) : "",
        security: to.value.security || Security.STARTTLS,
        username: to.value.username,
        from: to.value.from,
        to: to.value.to.join(", "),
      };
    case "ntfy":
      return { ...form, server: to.value.server };
    case "telegram":
      return { ...form, chatId: to.value.chatId };
    default:
      return form;
  }
}

/** The spec a form plans, with what's left empty for the control plane to keep. */
function specOf(f: Form): Spec {
  switch (f.type) {
    case "webhook":
      return { to: { case: "webhook", value: { url: f.url.trim(), secret: f.secret } } };
    case "slack":
      return { to: { case: "slack", value: { url: f.url.trim() } } };
    case "discord":
      return { to: { case: "discord", value: { url: f.url.trim() } } };
    case "ntfy":
      return { to: { case: "ntfy", value: { server: f.server.trim(), topic: f.topic.trim(), token: f.token.trim() } } };
    case "telegram":
      return { to: { case: "telegram", value: { botToken: f.botToken.trim(), chatId: f.chatId.trim() } } };
    case "email": {
      const recipients = f.to
        .split(/[,;\s]+/)
        .map((a) => a.trim())
        .filter((a) => a !== "");
      if (f.connection !== "") {
        return {
          to: {
            case: "email",
            value: {
              connection: f.connection,
              from: f.from.trim(),
              to: recipients,
            },
          },
        };
      }
      return {
        to: {
          case: "email",
          value: {
            host: f.host.trim(),
            port: Number(f.port) || 0,
            security: f.security,
            username: f.username.trim(),
            password: f.password,
            from: f.from.trim(),
            to: recipients,
          },
        },
      };
    }
  }
}

/** A channel's name as the control plane takes it: lowercase letters, digits and dashes. */
const NAME = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

/** What's typed as a name, as one could be: lowercase, with a dash for each run of anything else. */
function nameOf(typed: string): string {
  return typed
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+/, "");
}

/** The port a mail server is reached on, as the control plane reads it (alert.EmailPort). */
function portOf(f: Form): number {
  return Number(f.port) || (f.security === Security.TLS ? 465 : f.security === Security.NONE ? 25 : 587);
}

/**
 * Whether a mail channel now sends to a server, port, security or user its kept password hasn't
 * been sent to, which the control plane then asks for again (alert.Channel's Keep).
 */
function mailMoved(f: Form, was: Form | undefined): boolean {
  if (!was || f.type !== "email" || f.connection !== "" || was.connection !== "") return false;
  return (
    f.host.trim().toLowerCase() !== was.host.trim().toLowerCase() ||
    portOf(f) !== portOf(was) ||
    f.security !== was.security ||
    f.username.trim() !== was.username.trim()
  );
}

/** Whether a form has what its type needs, given what the channel keeps sealed already. */
function ready(f: Form, kept: ReadonlySet<string>): boolean {
  const has = (value: string, path: string) => value.trim() !== "" || kept.has(path);
  if (!NAME.test(f.name)) return false;
  switch (f.type) {
    case "webhook":
      return has(f.url, "webhook.url");
    case "slack":
      return has(f.url, "slack.url");
    case "discord":
      return has(f.url, "discord.url");
    case "ntfy":
      return has(f.topic, "ntfy.topic");
    case "telegram":
      return has(f.botToken, "telegram.bot_token") && f.chatId.trim() !== "";
    case "email":
      if (f.connection !== "") return f.from.trim() !== "" && f.to.trim() !== "";
      return (
        f.host.trim() !== "" &&
        f.from.trim() !== "" &&
        f.to.trim() !== "" &&
        (f.username.trim() === "" || has(f.password, "email.password"))
      );
  }
}

/** Mail services a person picks, each with the SMTP settings it takes. */
type Mail = "gmail" | "microsoft" | "fastmail" | "ses" | "sendgrid" | "mailgun" | "postmark" | "other";

const MAIL: Record<
  Exclude<Mail, "other">,
  { label: string; host: string; port: number; security: Security; user?: string; brand?: BrandId }
> = {
  gmail: { label: "Gmail", host: "smtp.gmail.com", port: 587, security: Security.STARTTLS, brand: "gmail" },
  microsoft: {
    label: "Microsoft 365 or Outlook.com",
    host: "smtp.office365.com",
    port: 587,
    security: Security.STARTTLS,
  },
  fastmail: { label: "Fastmail", host: "smtp.fastmail.com", port: 465, security: Security.TLS, brand: "fastmail" },
  ses: {
    label: "Amazon SES",
    host: "email-smtp.us-east-1.amazonaws.com",
    port: 587,
    security: Security.STARTTLS,
    brand: "amazon-ses",
  },
  sendgrid: { label: "SendGrid", host: "smtp.sendgrid.net", port: 587, security: Security.STARTTLS, user: "apikey" },
  mailgun: { label: "Mailgun", host: "smtp.mailgun.org", port: 587, security: Security.STARTTLS, brand: "mailgun" },
  postmark: {
    label: "Postmark",
    host: "smtp.postmarkapp.com",
    port: 587,
    security: Security.STARTTLS,
    brand: "postmark",
  },
};

/**
 * The mail services, each a card with its own mark where Dockwell has one, then another
 * server: one tap picks it and fills in its server, as a select would, with each
 * service recognisable at a glance.
 */
function MailServices({ value, onPick }: { value: Mail; onPick: (next: Mail) => void }) {
  const { t } = useLingui();
  const name = useId();
  const options: { id: Mail; label: string; brand?: BrandId | undefined }[] = [
    ...(Object.keys(MAIL) as Exclude<Mail, "other">[]).map((m) => ({
      id: m,
      label: MAIL[m].label,
      brand: MAIL[m].brand,
    })),
    { id: "other", label: t`Another SMTP server` },
  ];
  return (
    <fieldset className="alerts-mail-services" aria-label={t`Mail service`}>
      <legend className="dw-field-label" aria-hidden="true">
        {t`Mail service`}
      </legend>
      <div className="alerts-mail-service-grid">
        {options.map((o) => (
          <label key={o.id} className={cx("alerts-mail-service", value === o.id && "is-picked")}>
            <input type="radio" name={name} value={o.id} checked={value === o.id} onChange={() => onPick(o.id)} />
            <BrandMark brand={o.brand} icon={o.id === "other" ? "server" : "mail"} size={24} />
            <span className="alerts-mail-service-name">{o.label}</span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

/** Amazon SES's regions, whose SMTP endpoints differ by region. */
const SES_REGIONS = [
  "us-east-1",
  "us-east-2",
  "us-west-2",
  "ca-central-1",
  "eu-west-1",
  "eu-west-2",
  "eu-central-1",
  "ap-south-1",
  "ap-southeast-1",
  "ap-southeast-2",
  "ap-northeast-1",
];

/** Which mail service an SMTP server is, or other. */
function mailOf(host: string): Mail {
  const h = host.trim().toLowerCase();
  if (/^email-smtp\.[a-z0-9-]+\.amazonaws\.com$/.test(h)) return "ses";
  const found = (Object.keys(MAIL) as Exclude<Mail, "other" | "ses">[]).find((m) => MAIL[m].host === h);
  return found ?? (h === "" ? "gmail" : "other");
}

/** A channel's name, not yet taken: its type's, then numbered. */
function freeName(base: string, taken: ReadonlySet<string>): string {
  if (!taken.has(base)) return base;
  for (let i = 2; ; i++) if (!taken.has(`${base}-${i}`)) return `${base}-${i}`;
}

/**
 * Adds a channel, or changes one, then its plan. "Send a test" sends what the form
 * holds before anything is planned, and says what the far end answered. What
 * signs in goes into the plan sealed; a channel's own is never shown, and leaving
 * it empty keeps it.
 */
export function ChannelSheet({ channel, onClose }: { channel?: AlertChannelInfo; onClose: () => void }) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const client = useMemo(() => createClient(AlertService, transport), [transport]);
  // A channel is the install's, in no project, so planning one is an admin's: off, with why, otherwise (ADR 0059).
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  // Sending a test is an admin's too: it reaches the far end with what signs in.
  const mayTest = may(AlertService.method.testAlertChannel, { install: true });
  const connectionsQuery = useConnections();
  const allConnections = connectionsQuery.data?.connections ?? [];
  const mailConnections = useMemo(() => allConnections.filter((c) => c.kind === LinkKind.SMTP), [allConnections]);
  const [form, setForm] = useState<Form>(() => {
    if (channel) return fromInfo(channel);
    const initial = blank();
    const first = firstAvailableMailConnection(mailConnections);
    if (first !== "") {
      const conn = mailConnections.find((c) => c.name === first);
      return {
        ...initial,
        connection: first,
        from: conn ? mailConnectionSender(conn) : "",
      };
    }
    return initial;
  });
  const [pickedSource, setPickedSource] = useState(
    () =>
      channel !== undefined ||
      (connectionsQuery.data !== undefined && firstAvailableMailConnection(mailConnections) !== ""),
  );
  const [fromTouched, setFromTouched] = useState(channel !== undefined);
  const [connSheet, setConnSheet] = useState<
    { mode: "new-from"; from: ConnectionInfo } | { mode: "rotate"; connection: ConnectionInfo }
  >();
  // When mail connections arrive after opening Add a channel, pre-select the first usable one.
  useEffect(() => {
    if (pickedSource || channel || !connectionsQuery.data) return;
    const first = firstAvailableMailConnection(mailConnections);
    setPickedSource(true);
    if (first === "") return;
    const conn = mailConnections.find((c) => c.name === first);
    setForm((f) => ({
      ...f,
      connection: first,
      from: !fromTouched && f.from === "" && conn ? mailConnectionSender(conn) : f.from,
    }));
  }, [pickedSource, channel, connectionsQuery.data, mailConnections, fromTouched]);
  const [testing, setTesting] = useState(false);
  const [tested, setTested] = useState<{ delivery?: AlertDelivery; error?: unknown }>();
  const was = useMemo(() => (channel ? fromInfo(channel) : undefined), [channel]);
  // What the channel keeps sealed, less a password its mail server, port, security or user moved away from.
  const moved = mailMoved(form, was);
  const kept = useMemo(
    () => new Set((channel?.sealed ?? []).filter((path) => !(moved && path === "email.password"))),
    [channel, moved],
  );
  const others = useChannels().data?.channels;
  // A new channel's name comes from its type, unless a person gives one under Advanced.
  const taken = useMemo(
    () => new Set((others ?? []).filter((c) => c.name !== channel?.name).map((c) => c.name)),
    [others, channel],
  );
  // What a person typed, which may be empty while they type; until then, the type's name.
  const [typedName, setTypedName] = useState<string>();
  const name = channel ? form.name : (typedName ?? freeName(form.type, taken));
  const nameError =
    channel || NAME.test(name)
      ? taken.has(name) && !channel
        ? t`Another channel has this name. Rules name a channel by it, so give this one another.`
        : undefined
      : name === ""
        ? t`A channel needs a name: lowercase letters, digits and dashes.`
        : t`Lowercase letters, digits and dashes, starting and ending with a letter or digit.`;
  const [mail, setMail] = useState<Mail>(() => mailOf(channel ? fromInfo(channel).host : MAIL.gmail.host));
  // Advanced won't close on a name that can't be taken, so what holds the buttons back stays in view.
  const [advanced, setAdvanced] = useState(false);
  const selectConnection = (next: string) => {
    setPickedSource(true);
    setTested(undefined);
    const conn = next ? mailConnections.find((c) => c.name === next) : undefined;
    if (next === "" && form.host.trim() === "") {
      setMail("gmail");
      setForm((f) => ({
        ...f,
        connection: "",
        host: MAIL.gmail.host,
        port: String(MAIL.gmail.port),
        security: MAIL.gmail.security,
      }));
      return;
    }
    setForm((f) => ({
      ...f,
      connection: next,
      from: !fromTouched && conn ? mailConnectionSender(conn) || f.from : f.from,
    }));
  };
  const pickMail = (next: Mail, region = "us-east-1") => {
    setMail(next);
    setTested(undefined);
    if (next === "other") {
      // A service's server isn't another's: the person types theirs.
      setForm((f) => ({ ...f, host: "", port: "" }));
      return;
    }
    const preset = MAIL[next];
    setForm((f) => ({
      ...f,
      host: next === "ses" ? `email-smtp.${region}.amazonaws.com` : preset.host,
      port: String(preset.port),
      security: preset.security,
      username: preset.user ?? (f.username === "apikey" ? "" : f.username),
    }));
  };
  const set =
    <K extends keyof Form>(key: K) =>
    (value: Form[K]) => {
      setForm((f) => ({ ...f, [key]: value }));
      setTested(undefined);
    };
  const text = (key: keyof Form) => (event: { target: { value: string } }) => set(key)(event.target.value as never);
  const keptHint = (path: string, otherwise: string) =>
    kept.has(path) ? t`Dockwell keeps it sealed. Leave it empty to keep it.` : otherwise;
  const keptPlaceholder = (path: string) => (kept.has(path) ? t`Kept, sealed` : undefined);
  const canSend = ready({ ...form, name }, kept) && nameError === undefined;
  const kind = CHANNEL_KIND[form.type];
  const kindName = i18n._(kind.label);
  const said = tested?.delivery?.said ?? "";

  const test = async () => {
    setTesting(true);
    setTested(undefined);
    try {
      const res = await client.testAlertChannel({ name: channel?.name ?? "", channel: specOf(form) });
      setTested({ delivery: res.delivery });
      if (channel) {
        void queryClient.invalidateQueries({
          queryKey: createConnectQueryKey({ schema: AlertService, cardinality: undefined }),
        });
      }
    } catch (error) {
      setTested({ error });
    } finally {
      setTesting(false);
    }
  };

  const review = () =>
    sheet.open({
      kind: "propose",
      summary: channel ? t`Change the alert channel ${name}` : t`Add the alert channel ${name}`,
      edits: [
        {
          resource: { kind: "alert.channel", name },
          spec: { kind: { case: "alertChannel", value: specOf(form) } },
        },
      ],
      onClose: (change) => {
        if (change) onClose();
      },
    });

  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: t`Remove the alert channel ${name}`,
      edits: [{ resource: { kind: "alert.channel", name }, delete: true }],
      onClose: (change) => {
        if (change) onClose();
      },
    });

  const smtpServer = (
    <>
      <TextField label={t`SMTP server`} mono value={form.host} placeholder="smtp.example.com" onChange={text("host")} />
      <div className="alerts-sheet-row">
        <SelectField
          label={t`Security`}
          value={String(form.security)}
          options={[
            { value: String(Security.STARTTLS), label: t`STARTTLS` },
            { value: String(Security.TLS), label: t`TLS` },
            { value: String(Security.NONE), label: t`None, on this network` },
          ]}
          onChange={(event) => set("security")(Number(event.target.value) as Security)}
        />
        <TextField
          label={t`Port`}
          mono
          inputMode="numeric"
          optional
          value={form.port}
          placeholder={form.security === Security.TLS ? "465" : form.security === Security.NONE ? "25" : "587"}
          onChange={text("port")}
        />
      </div>
    </>
  );
  const usingConnection = form.type === "email" && form.connection !== "";
  const savedConnection = channel ? channelConnection(channel) : "";
  const securityWord =
    form.security === Security.TLS ? "TLS" : form.security === Security.NONE ? t`no TLS` : "STARTTLS";
  // What Advanced holds, in a line: the name a new channel takes, and a mail service's server.
  const summary = [
    channel ? "" : t`Named ${name}`,
    form.type === "email" && !usingConnection && mail !== "other"
      ? `${form.host}:${form.port || "587"} · ${securityWord}`
      : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const advancedHolds = !channel || (form.type === "email" && !usingConnection && mail !== "other");

  return (
    <>
      <Sheet
        open
        onClose={onClose}
        title={channel ? t`Change ${channel.name}` : t`Add a channel`}
        footer={
          <>
            {channel ? (
              <Button variant="ghost" icon="trash" onClick={remove} className="alerts-sheet-remove" {...mayPlan.off}>
                <Trans>Remove</Trans>
              </Button>
            ) : null}
            <Button variant="ghost" disabled={!canSend || testing} onClick={() => void test()} {...mayTest.off}>
              {testing ? <Trans>Sending a test…</Trans> : <Trans>Send a test</Trans>}
            </Button>
            <Button variant="secondary" onClick={onClose}>
              <Trans>Cancel</Trans>
            </Button>
            <Button variant="primary" disabled={!canSend} onClick={review} {...mayPlan.off}>
              <Trans>Review plan</Trans>
            </Button>
          </>
        }
      >
        <div className="alerts-sheet">
          {channel ? (
            <p className="alerts-sheet-kind">
              {kindName}
              {savedConnection ? (
                <>
                  {" · "}
                  <Trans>
                    via{" "}
                    <Link
                      to="/connections/$name"
                      params={{ name: savedConnection }}
                      className="alerts-channel-link"
                      onClick={onClose}
                    >
                      <code>{savedConnection}</code>
                    </Link>
                  </Trans>
                </>
              ) : null}
            </p>
          ) : (
            <SelectField
              label={t`Sends to`}
              value={form.type}
              options={CHANNEL_TYPES.map((type) => ({ value: type, label: i18n._(CHANNEL_KIND[type].label) }))}
              onChange={(event) => set("type")(event.target.value as ChannelType)}
            />
          )}

          {form.type === "webhook" ? (
            <>
              <TextField
                label={t`URL`}
                mono
                autoComplete="off"
                value={form.url}
                placeholder={keptPlaceholder("webhook.url") ?? "https://hooks.example.com/dockwell"}
                hint={keptHint("webhook.url", t`Each alert is posted to it as JSON. Kept sealed; lists show its host.`)}
                onChange={text("url")}
              />
              <TextField
                label={t`Signing secret`}
                mono
                type="password"
                autoComplete="off"
                optional
                value={form.secret}
                placeholder={keptPlaceholder("webhook.secret")}
                hint={keptHint("webhook.secret", t`Signs each request, in a Dockwell-Signature header.`)}
                onChange={text("secret")}
              />
            </>
          ) : null}

          {form.type === "slack" || form.type === "discord" ? (
            <TextField
              label={t`Webhook URL`}
              mono
              autoComplete="off"
              value={form.url}
              placeholder={
                keptPlaceholder(`${form.type}.url`) ??
                (form.type === "slack"
                  ? "https://hooks.slack.com/services/T000/B000/XXXX"
                  : "https://discord.com/api/webhooks/0000/XXXX")
              }
              hint={keptHint(
                `${form.type}.url`,
                form.type === "slack"
                  ? t`From your Slack app's Incoming Webhooks. Kept sealed.`
                  : t`From the channel's Integrations, then Webhooks. Kept sealed.`,
              )}
              onChange={text("url")}
            />
          ) : null}

          {form.type === "telegram" ? (
            <>
              <TextField
                label={t`Bot token`}
                mono
                type="password"
                autoComplete="off"
                value={form.botToken}
                placeholder={keptPlaceholder("telegram.bot_token")}
                hint={keptHint("telegram.bot_token", t`From @BotFather, when you make the bot. It's kept sealed.`)}
                onChange={text("botToken")}
              />
              <TextField
                label={t`Chat`}
                mono
                value={form.chatId}
                placeholder="-1001234567890"
                hint={t`A group's ID or a channel's @name, with the bot added to it.`}
                onChange={text("chatId")}
              />
            </>
          ) : null}

          {form.type === "ntfy" ? (
            <>
              <TextField
                label={t`Server`}
                mono
                optional
                value={form.server}
                placeholder="https://ntfy.sh"
                hint={t`ntfy.sh unless you give a server of your own.`}
                onChange={text("server")}
              />
              <TextField
                label={t`Topic`}
                mono
                autoComplete="off"
                value={form.topic}
                placeholder={keptPlaceholder("ntfy.topic")}
                hint={keptHint("ntfy.topic", t`Made hard to guess, and kept sealed. Subscribe to it in the ntfy app.`)}
                onChange={text("topic")}
              />
              <TextField
                label={t`Access token`}
                mono
                type="password"
                autoComplete="off"
                optional
                value={form.token}
                placeholder={keptPlaceholder("ntfy.token")}
                hint={keptHint("ntfy.token", t`For a server or topic that asks for one.`)}
                onChange={text("token")}
              />
            </>
          ) : null}

          {form.type === "email" ? (
            <>
              {mailConnections.length > 0 ? (
                <MailPicker
                  connections={mailConnections}
                  value={form.connection}
                  currentConnection={savedConnection || undefined}
                  onChange={selectConnection}
                  mayPlanInstall={mayPlan}
                  onNewFrom={(from) => setConnSheet({ mode: "new-from", from })}
                  onRotatePassword={(connection) => setConnSheet({ mode: "rotate", connection })}
                />
              ) : null}
              {!usingConnection ? (
                <>
                  <MailServices value={mail} onPick={(next) => pickMail(next)} />
                  {mail === "ses" ? (
                    <SelectField
                      label={t`Region`}
                      value={/^email-smtp\.([a-z0-9-]+)\.amazonaws\.com$/.exec(form.host)?.[1] ?? "us-east-1"}
                      options={SES_REGIONS.map((r) => ({ value: r, label: r }))}
                      onChange={(event) => pickMail("ses", event.target.value)}
                    />
                  ) : null}
                </>
              ) : null}
              <TextField
                label={t`To`}
                mono
                value={form.to}
                placeholder="ops@example.com, oncall@example.com"
                hint={t`Up to 10, separated by commas.`}
                onChange={text("to")}
              />
              <TextField
                label={t`From`}
                mono
                value={form.from}
                placeholder="Dockwell <alerts@example.com>"
                hint={t`An address the mail service sends as.`}
                onChange={(event) => {
                  setFromTouched(true);
                  set("from")(event.target.value);
                }}
              />
              {!usingConnection ? (
                <>
                  {mail === "other" ? smtpServer : null}
                  <div className="alerts-sheet-row">
                    <TextField
                      label={t`User`}
                      mono
                      autoComplete="off"
                      optional
                      value={form.username}
                      onChange={text("username")}
                    />
                    <TextField
                      label={t`Password`}
                      mono
                      type="password"
                      autoComplete="off"
                      optional={form.username.trim() === ""}
                      value={form.password}
                      placeholder={keptPlaceholder("email.password")}
                      hint={
                        moved && channel?.sealed.includes("email.password") && form.username.trim() !== ""
                          ? t`Its server changed, so give the password again.`
                          : keptHint("email.password", t`Sent only over TLS, and kept sealed.`)
                      }
                      onChange={text("password")}
                    />
                  </div>
                </>
              ) : null}
            </>
          ) : null}

          {advancedHolds ? (
            <Advanced
              open={advanced}
              onOpenChange={(open) => setAdvanced(open || nameError !== undefined)}
              summary={summary}
            >
              {channel ? null : (
                <TextField
                  label={t`Name`}
                  mono
                  value={name}
                  hint={t`Rules name a channel by it.`}
                  error={nameError}
                  onChange={(event) => setTypedName(nameOf(event.target.value))}
                />
              )}
              {form.type === "email" && !usingConnection && mail !== "other" ? smtpServer : null}
            </Advanced>
          ) : null}

          {tested?.delivery ? (
            tested.delivery.ok ? (
              <Callout tone="healthy" title={t`The test arrived`}>
                {t`${kindName} answered ${said}.`}
              </Callout>
            ) : (
              <Callout tone="critical" title={t`The test didn't arrive`}>
                {t`It said: ${said}.`}
              </Callout>
            )
          ) : tested?.error ? (
            <Callout tone="critical" title={t`Dockwell didn't send it`}>
              {sentence(tested.error)}
            </Callout>
          ) : null}
        </div>
      </Sheet>
      {connSheet ? (
        <ConnectionSheet
          kind="mail"
          existing={allConnections}
          editing={connSheet.mode === "rotate" ? connSheet.connection : undefined}
          rotateForAlerts={connSheet.mode === "rotate"}
          mailPrefill={
            connSheet.mode === "new-from"
              ? {
                  host: connSheet.from.host,
                  port: connSheet.from.port,
                  security: connSheet.from.security,
                }
              : undefined
          }
          onClose={() => setConnSheet(undefined)}
          onSaved={(savedName) => {
            setConnSheet(undefined);
            void connectionsQuery.refetch().then((res) => {
              const saved = res.data?.connections.find((c) => c.name === savedName);
              setPickedSource(true);
              setTested(undefined);
              setForm((f) => ({
                ...f,
                connection: savedName,
                from: !fromTouched && saved ? mailConnectionSender(saved) || f.from : f.from,
              }));
            });
          }}
        />
      ) : null}
    </>
  );
}
