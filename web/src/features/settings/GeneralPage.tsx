// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, PanelServedBy, type Settings, SettingsService } from "@dockwell/api";
import { Button, Callout, Panel, SelectField, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";

import { chooseLocale, chosenLocale, LOCALES, type Locale, pickLocale } from "../../app/i18n";
import { reasonOf, sentence } from "../../lib/errors";
import { formatDay } from "../../lib/format";
import { Page } from "../shell/Page";
import "./Settings.css";

/** Where the install docs say how to change what dockwell run starts with. */
const CHANGE_SETTINGS = "https://github.com/Dockwell/dockwell/blob/main/docs/install.md#change-settings";

/** At most this many characters, as the control plane takes. */
const MAX_NAME = 64;

/**
 * General, as Settings-General.dc.html has it for an install: what it's called,
 * the time zone its schedules start in, how its panel is served, and the
 * language this browser shows it in (ADR 0042).
 */
export function GeneralPage() {
  const { t, i18n } = useLingui();
  const settings = useQuery(SettingsService.method.getSettings, {});
  const s = settings.data?.settings;
  const since = s?.createTime ? formatDay(timestampDate(s.createTime), i18n.locale) : "";
  return (
    <Page
      title={t`General`}
      lead={
        since
          ? t`What this install is called, its time zone and how its panel is served · set up ${since}`
          : t`What this install is called, its time zone and how its panel is served`
      }
      back="/settings"
      className="general"
    >
      {settings.error ? <Callout tone="critical">{sentence(settings.error)}</Callout> : null}
      <div className="general-panels">
        <InstallPanel settings={s} />
        <div className="general-column">
          <PanelServed settings={s} />
          <LanguagePanel />
        </div>
      </div>
    </Page>
  );
}

/** Every IANA zone this browser knows, UTC first. */
function zones(): string[] {
  const all = typeof Intl.supportedValuesOf === "function" ? Intl.supportedValuesOf("timeZone") : [];
  return ["UTC", ...all.filter((z) => z !== "UTC")];
}

/** The install's name and time zone, saved together, each on Activity when it changes. */
function InstallPanel({ settings }: { settings: Settings | undefined }) {
  const { t } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const [name, setName] = useState<string>();
  const [zone, setZone] = useState<string>();
  const [nameError, setNameError] = useState<string>();
  const [zoneError, setZoneError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const shownName = name ?? settings?.name ?? "";
  // Empty when nobody has chosen one, which new schedules read as UTC.
  const shownZone = zone ?? settings?.timeZone ?? "";
  const changedName = settings !== undefined && name !== undefined && name.trim() !== settings.name;
  const changedZone = settings !== undefined && zone !== undefined && zone !== settings.timeZone;

  const save = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || (!changedName && !changedZone)) return;
    setBusy(true);
    setFailure(undefined);
    setNameError(undefined);
    setZoneError(undefined);
    setSaved(false);
    const client = createClient(SettingsService, transport);
    try {
      if (changedName) {
        try {
          await client.setInstallName({ name: shownName });
          setName(undefined);
        } catch (err) {
          if (reasonOf(err) !== "invalid") throw err;
          setNameError(sentence(err));
        }
      }
      if (changedZone) {
        try {
          await client.setTimeZone({ timeZone: shownZone });
          setZone(undefined);
        } catch (err) {
          if (reasonOf(err) !== "invalid") throw err;
          setZoneError(sentence(err));
        }
      }
      setSaved(true);
    } catch (err) {
      setFailure(sentence(err));
    } finally {
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: createConnectQueryKey({ schema: SettingsService.method.getSettings, cardinality: undefined }),
        }),
        queryClient.invalidateQueries({
          queryKey: createConnectQueryKey({ schema: AuthService.method.getAuthState, cardinality: undefined }),
        }),
      ]);
      setBusy(false);
    }
  };

  const zoneOptions = [
    { value: "", label: t`Not set: UTC` },
    ...zones().map((z) => ({ value: z, label: z.replaceAll("_", " ") })),
  ];
  if (!zoneOptions.some((o) => o.value === shownZone)) zoneOptions.push({ value: shownZone, label: shownZone });
  return (
    <Panel title={t`This install`} className="general-install">
      <form className="general-form" onSubmit={(event) => void save(event)} aria-busy={settings === undefined}>
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        <TextField
          label={t`Name`}
          placeholder="Dockwell"
          value={shownName}
          maxLength={MAX_NAME}
          disabled={settings === undefined}
          error={nameError}
          hint={t`The sign-in screen and your browser's tabs say it. Empty, they say Dockwell.`}
          onChange={(event) => {
            setName(event.target.value);
            setNameError(undefined);
            setSaved(false);
          }}
        />
        <SelectField
          label={t`Time zone`}
          options={zoneOptions}
          value={shownZone}
          disabled={settings === undefined}
          error={zoneError}
          hint={t`A new schedule, such as a backup's, starts in it unless it names its own, from the panel, the CLI or the API. Each keeps the zone it was made with.`}
          onChange={(event) => {
            setZone(event.target.value);
            setZoneError(undefined);
            setSaved(false);
          }}
        />
        <div className="general-foot">
          <span className="general-note" role="status">
            {saved && !changedName && !changedZone
              ? t`Saved. It's on Activity.`
              : t`A change goes on Activity, with what it replaced.`}
          </span>
          <Button type="submit" size="sm" icon="check" loading={busy} disabled={!changedName && !changedZone}>
            <Trans>Save</Trans>
          </Button>
        </div>
      </form>
    </Panel>
  );
}

/** How the panel is served, as dockwell run was started, and how to change that. */
function PanelServed({ settings }: { settings: Settings | undefined }) {
  const { t } = useLingui();
  const panel = settings?.panel;
  const domain = panel?.domain ?? "";
  const ca = panel?.certificateAuthority || "Let's Encrypt";
  const url = panel?.url || (domain ? `https://${domain}` : "");
  let served: string;
  switch (panel?.servedBy) {
    case PanelServedBy.DOCKWELL:
      served = t`Dockwell itself, with a certificate from ${ca} that it renews`;
      break;
    case PanelServedBy.INGRESS:
      served = t`Caddy, the server's ingress, which holds ports 80 and 443 while the server has routes`;
      break;
    default:
      served = t`Nothing yet: the panel has no domain`;
  }
  return (
    <Panel title={t`Panel address`} actions={domain ? <span className="settings-meta">{t`HTTPS`}</span> : null}>
      <dl className="settings-facts" aria-busy={settings === undefined}>
        <dt>{t`Address`}</dt>
        <dd>
          {domain ? (
            <a href={url} className="settings-mono settings-link">
              {url}
            </a>
          ) : (
            <span className="settings-muted">{t`Loopback only`}</span>
          )}
        </dd>
        <dt>{t`Served by`}</dt>
        <dd>{served}</dd>
        <dt>{t`Loopback`}</dt>
        <dd>
          {panel?.listen ? <code>{panel.listen}</code> : <span className="settings-muted">–</span>}
          <span className="settings-sub">{t`Always on, through an SSH tunnel to the server`}</span>
        </dd>
      </dl>
      <div className="settings-how">
        <p>
          {domain ? (
            <Trans>
              The domain is set where Dockwell runs, as <code>DOCKWELL_DOMAIN</code>, and read as it starts. Change it
              there and restart Dockwell:
            </Trans>
          ) : (
            <Trans>
              To reach the panel over HTTPS on a domain of yours, point the domain's DNS at this server, set{" "}
              <code>DOCKWELL_DOMAIN=panel.example.com</code> where Dockwell runs, and restart it:
            </Trans>
          )}
        </p>
        <ul>
          <li>
            <Trans>
              With systemd, <code>sudo systemctl edit dockwell</code> adds the line as{" "}
              <code>Environment=DOCKWELL_DOMAIN=…</code>, then <code>sudo systemctl restart dockwell</code>.
            </Trans>
          </li>
          <li>
            <Trans>
              With OpenRC, <code>export DOCKWELL_DOMAIN=…</code> in <code>/etc/conf.d/dockwell</code>, then{" "}
              <code>sudo rc-service dockwell restart</code>.
            </Trans>
          </li>
          <li>
            <Trans>
              Run by hand, <code>dockwell run --domain …</code>.
            </Trans>
          </li>
        </ul>
        <p>
          <Trans>
            Ports 80 and 443 must reach this server for the certificate, and Dockwell won't start with a domain one of
            this server's routes already serves. Passkeys belong to the address they were made on, so those for an old
            domain won't work on the new one: over SSH, run <code>dockwell recover</code> on the server, open its link
            on the new domain, and add a passkey there.
          </Trans>{" "}
          <a href={CHANGE_SETTINGS} target="_blank" rel="noreferrer" className="settings-link">
            <Trans>How to change settings</Trans>
          </a>
        </p>
      </div>
    </Panel>
  );
}

/** The language this browser shows the panel in. */
function LanguagePanel() {
  const { t } = useLingui();
  const [chosen, setChosen] = useState<Locale | "">(() => chosenLocale() ?? "");
  const browser = LOCALES[pickLocale(navigator.languages)];
  const options = [
    { value: "", label: t`Same as this browser (${browser})` },
    ...(Object.keys(LOCALES) as Locale[]).map((l) => ({ value: l, label: LOCALES[l] })),
  ];
  return (
    <Panel title={t`Language`} actions={<span className="settings-meta">{t`This browser`}</span>}>
      <SelectField
        label={t`Show the panel in`}
        options={options}
        value={chosen}
        hint={t`Only this browser changes. Your other browsers, and the command line, keep their own.`}
        onChange={(event) => {
          const next = event.target.value as Locale | "";
          setChosen(next);
          void chooseLocale(next === "" ? undefined : next);
        }}
      />
    </Panel>
  );
}
