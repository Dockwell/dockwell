// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import {
  type AlertChannelInfo,
  type AlertChannelSchema,
  type AlertDelivery,
  AlertService,
  AlertEmail_Security as Security,
} from "@dockwell/api";
import { Button, Callout, SelectField, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { useChangeSheet } from "../changes/ChangeSheet";
import { CHANNEL_KIND, CHANNEL_TYPES, type ChannelType, isChannelType } from "./useAlerts";

type Spec = MessageInitShape<typeof AlertChannelSchema>;

/** What the form holds, for every type at once, so switching types keeps what was typed. */
interface Form {
  type: ChannelType;
  name: string;
  url: string;
  secret: string;
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
    host: "",
    port: "",
    security: Security.STARTTLS,
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
    case "email":
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
            to: f.to
              .split(/[,;\s]+/)
              .map((a) => a.trim())
              .filter((a) => a !== ""),
          },
        },
      };
  }
}

/** Whether a form has what its type needs, given what the channel keeps sealed already. */
function ready(f: Form, kept: ReadonlySet<string>): boolean {
  const has = (value: string, path: string) => value.trim() !== "" || kept.has(path);
  if (!/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(f.name)) return false;
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
      return (
        f.host.trim() !== "" &&
        f.from.trim() !== "" &&
        f.to.trim() !== "" &&
        (f.username.trim() === "" || has(f.password, "email.password"))
      );
  }
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
  const [form, setForm] = useState<Form>(() => (channel ? fromInfo(channel) : blank()));
  const [testing, setTesting] = useState(false);
  const [tested, setTested] = useState<{ delivery?: AlertDelivery; error?: unknown }>();
  const kept = useMemo(() => new Set(channel?.sealed ?? []), [channel]);
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
  const canSend = ready(form, kept);
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
      summary: channel ? t`Change the alert channel ${form.name}` : t`Add the alert channel ${form.name}`,
      edits: [
        {
          resource: { kind: "alert.channel", name: form.name },
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
      summary: t`Remove the alert channel ${form.name}`,
      edits: [{ resource: { kind: "alert.channel", name: form.name }, delete: true }],
      onClose: (change) => {
        if (change) onClose();
      },
    });

  return (
    <Sheet
      open
      onClose={onClose}
      title={channel ? t`Change ${channel.name}` : t`Add a channel`}
      footer={
        <>
          {channel ? (
            <Button variant="ghost" icon="trash" onClick={remove} className="alerts-sheet-remove">
              <Trans>Remove</Trans>
            </Button>
          ) : null}
          <Button variant="secondary" disabled={!canSend || testing} onClick={() => void test()}>
            {testing ? <Trans>Sending a test…</Trans> : <Trans>Send a test</Trans>}
          </Button>
          <Button variant="primary" disabled={!canSend} onClick={review}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="alerts-sheet">
        {channel ? (
          <p className="alerts-sheet-kind">{kindName}</p>
        ) : (
          <>
            <SelectField
              label={t`Sends to`}
              value={form.type}
              options={CHANNEL_TYPES.map((type) => ({ value: type, label: i18n._(CHANNEL_KIND[type].label) }))}
              onChange={(event) => set("type")(event.target.value as ChannelType)}
            />
            <TextField
              label={t`Name`}
              mono
              value={form.name}
              placeholder={form.type === "email" ? "ops-email" : `ops-${form.type}`}
              hint={t`Lowercase letters, digits and dashes. Rules name a channel by it.`}
              onChange={text("name")}
            />
          </>
        )}

        {form.type === "webhook" ? (
          <>
            <TextField
              label={t`URL`}
              mono
              autoComplete="off"
              value={form.url}
              placeholder={keptPlaceholder("webhook.url") ?? "https://hooks.example.com/dockwell"}
              hint={keptHint(
                "webhook.url",
                t`Dockwell posts each alert to it as JSON. It may hold a token, so it's kept sealed, and lists show only its host.`,
              )}
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
              hint={keptHint(
                "webhook.secret",
                t`Signs each request in a Dockwell-Signature header, so your endpoint can tell it came from this install.`,
              )}
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
                ? t`From your Slack app's Incoming Webhooks page. Anyone with it can post there, so it's kept sealed.`
                : t`From the channel's settings, under Integrations, then Webhooks. Anyone with it can post there, so it's kept sealed.`,
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
              hint={t`A group's ID, or a channel's name, such as @example_alerts. Add the bot to it first.`}
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
              hint={keptHint(
                "ntfy.topic",
                t`On ntfy.sh anyone who knows a topic can read it, so this one is hard to guess, and kept sealed. Subscribe to it in the ntfy app.`,
              )}
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
            <TextField
              label={t`To`}
              mono
              value={form.to}
              placeholder="ops@example.com, oncall@example.com"
              hint={t`Up to 10 addresses, separated by commas.`}
              onChange={text("to")}
            />
            <TextField
              label={t`From`}
              mono
              value={form.from}
              placeholder="Dockwell <alerts@example.com>"
              hint={t`An address your SMTP server sends as.`}
              onChange={text("from")}
            />
            <TextField
              label={t`SMTP server`}
              mono
              value={form.host}
              placeholder="smtp.example.com"
              onChange={text("host")}
            />
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
              hint={keptHint(
                "email.password",
                t`Sent only over TLS, and kept sealed. Changing the server, port, security or user asks for it again.`,
              )}
              onChange={text("password")}
            />
          </>
        ) : null}

        {tested?.delivery ? (
          tested.delivery.ok ? (
            <Callout tone="healthy" title={t`The test arrived`}>
              {t`${kindName} answered ${said}. Look for a message titled Dockwell test message.`}
            </Callout>
          ) : (
            <Callout tone="critical" title={t`The test didn't arrive`}>
              {t`It said: ${said}. Check what the form holds, then send another.`}
            </Callout>
          )
        ) : tested?.error ? (
          <Callout tone="critical" title={t`Dockwell didn't send it`}>
            {sentence(tested.error)}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}
