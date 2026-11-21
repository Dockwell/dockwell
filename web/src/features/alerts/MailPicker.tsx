// SPDX-License-Identifier: AGPL-3.0-only

import { type ConnectionInfo, SmtpSecurity } from "@dockwell/api";
import { Button, cx } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useId } from "react";

import type { Offer } from "../auth/may";
import { defaultPort } from "../connections/mail";
import { ConnectionMark, MailMark } from "../connections/ProviderMark";
import { mailPickerStatus } from "./mail";
import "./Alerts.css";

/**
 * Picks where alert emails send through: usable mail connections first, then
 * connections serving an app, shared, or whose password an app received
 * (disabled with why and the way out), then Another server one step away
 * (ADR 0099, #833).
 */
export function MailPicker({
  connections,
  value,
  currentConnection,
  onChange,
  mayPlanInstall,
  onNewFrom,
  onRotatePassword,
}: {
  connections: readonly ConnectionInfo[];
  value: string;
  currentConnection?: string | undefined;
  onChange: (connectionName: string) => void;
  mayPlanInstall: Offer;
  onNewFrom: (connection: ConnectionInfo) => void;
  onRotatePassword: (connection: ConnectionInfo) => void;
}) {
  const { t, i18n } = useLingui();
  const groupId = useId();
  const listFormat = new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" });

  const usable = connections.filter((c) => mailPickerStatus(c, currentConnection).state === "available");
  const blocked = connections.filter((c) => mailPickerStatus(c, currentConnection).state !== "available");
  const ordered = [...usable, ...blocked];
  const singleUsable = usable.length === 1;

  const securityWord = (s: SmtpSecurity) =>
    s === SmtpSecurity.TLS ? t`TLS` : s === SmtpSecurity.NONE ? t`No TLS` : t`STARTTLS`;

  return (
    <fieldset className="alerts-mail-picker">
      <legend className="dw-field-label">
        <Trans>Send through</Trans>
      </legend>
      <div className="alerts-mail-options">
        {ordered.map((conn) => {
          const status = mailPickerStatus(conn, currentConnection);
          const label = conn.displayName || conn.name;
          const host = conn.host;
          const port = conn.port || defaultPort(conn.security);
          const endpoint = host ? `${host}:${port}` : "";
          const sec = host ? securityWord(conn.security) : "";

          const isSingleChoice = singleUsable && status.state === "available";
          const title = isSingleChoice
            ? t`Use the mail connection`
            : host
              ? t`Use ${label} (${host})`
              : t`Use ${label}`;

          let disabled = false;
          let detail = "";
          let action: React.ReactNode = null;

          if (status.state === "available") {
            detail = isSingleChoice
              ? [label, endpoint, sec].filter(Boolean).join(" · ")
              : [endpoint || conn.name, sec].filter(Boolean).join(" · ");
          } else if (status.state === "serves-app") {
            disabled = true;
            const appNames = status.apps.map((a) => a.name);
            const who = appNames.length > 0 ? listFormat.format(appNames) : t`an app`;
            detail = t`Serves ${who}, which could send mail as the alert sender`;
            action = (
              <Button size="sm" variant="secondary" onClick={() => onNewFrom(conn)} {...mayPlanInstall.off}>
                <Trans>New connection from it</Trans>
              </Button>
            );
          } else if (status.state === "shared") {
            disabled = true;
            detail = t`Shared for apps, which could send mail as the alert sender`;
            action = (
              <Button size="sm" variant="secondary" onClick={() => onNewFrom(conn)} {...mayPlanInstall.off}>
                <Trans>New connection from it</Trans>
              </Button>
            );
          } else if (status.state === "held") {
            disabled = true;
            detail = t`An app received this connection's password`;
            action = (
              <Button size="sm" variant="secondary" onClick={() => onRotatePassword(conn)} {...mayPlanInstall.off}>
                <Trans>Paste a password made for alerts</Trans>
              </Button>
            );
          } else if (status.state === "no-tls") {
            disabled = true;
            detail = t`Uses no TLS away from this network; alerts require TLS`;
          }

          const picked = value === conn.name && !disabled;
          return (
            <div key={conn.id} className={cx("alerts-mail-option", picked && "is-picked", disabled && "is-off")}>
              <label className="alerts-mail-choice">
                <input
                  type="radio"
                  name={groupId}
                  value={conn.name}
                  checked={picked}
                  disabled={disabled}
                  onChange={() => onChange(conn.name)}
                />
                <ConnectionMark connection={conn} />
                <span className="alerts-mail-text">
                  <span className="alerts-mail-name">{title}</span>
                  {detail ? <span className="alerts-mail-why">{detail}</span> : null}
                </span>
              </label>
              {action ? <div className="alerts-mail-action">{action}</div> : null}
            </div>
          );
        })}

        <div className={cx("alerts-mail-option", value === "" && "is-picked")}>
          <label className="alerts-mail-choice">
            <input type="radio" name={groupId} value="" checked={value === ""} onChange={() => onChange("")} />
            <MailMark />
            <span className="alerts-mail-text">
              <span className="alerts-mail-name">
                {t({ message: "Another server", context: "another mail server for alert email" })}
              </span>
              <span className="alerts-mail-why">
                <Trans>Enter an SMTP server and password for alerts alone</Trans>
              </span>
            </span>
          </label>
        </div>
      </div>
    </fieldset>
  );
}
