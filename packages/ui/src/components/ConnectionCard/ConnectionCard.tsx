// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { cx, hasContent } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import { Button } from "../Button/Button";
import { CopyButton } from "../CopyButton/CopyButton";
import { Panel } from "../Panel/Panel";
import "./ConnectionCard.css";

/** One thing a client needs to connect, such as the host or the user. */
export interface ConnectionItem {
  /** What it is, in sentence case, such as "Host". It names the copy button too. */
  label: string;
  /** The value, which shows in mono and copies as it is. */
  value: string;
  /** One short line under it, such as where the host answers. Most items need none. */
  hint?: ReactNode;
}

/** The secret a client signs in with, hidden until someone reveals it. */
export interface ConnectionSecret {
  /** What it is, such as "Password". */
  label: string;
  /** The value once revealed. Undefined while it's hidden. */
  value?: string | undefined;
  /** True while the reveal is on its way, such as while the browser asks for a passkey. */
  busy?: boolean;
  /** Why it couldn't be revealed, in place of the hint. */
  problem?: ReactNode;
  /** Asks for the value: the card never holds a secret it wasn't given. */
  onReveal: () => void;
  /** Hides it again. */
  onHide: () => void;
  /** Beside the reveal, such as a button that changes the secret. */
  action?: ReactNode;
  /** One short line under it, such as which secret holds it. */
  hint?: ReactNode;
}

/** A ready-made string a client takes whole, such as a database URL. */
export interface ConnectionString {
  /** What it is, such as "Connection string". */
  label: string;
  /**
   * Builds the string with the secret's value, or without the secret while it's
   * hidden (undefined), as the clipboard gets it then. The card shows it with the
   * secret masked until it's revealed.
   */
  build: (secret: string | undefined) => string;
}

export interface ConnectionCardProps {
  /** The card's heading. Defaults to the UI strings' "Connect". */
  title?: ReactNode;
  /** Controls on the right of the heading, such as a status. */
  actions?: ReactNode;
  /** What a client needs, in the order a client form asks: host, port, user. */
  items: readonly ConnectionItem[];
  /** The secret, which shows masked and takes a reveal. */
  secret?: ConnectionSecret;
  /** A ready-made string that holds everything above. */
  connectionString?: ConnectionString;
  /** One short line under the details, for a state they don't show, such as a password kept where nothing reveals it. */
  note?: ReactNode;
  className?: string;
}

/** What stands in for a hidden secret: the same length whatever its own, which it doesn't give away. */
const MASK = "••••••••••••";
/**
 * What a connection string is built with while the secret is hidden, then swapped for
 * the mask: letters and digits alone, so whatever the string escapes leaves it whole.
 */
const STAND_IN = "dwHiddenSecret7f3a9c";

/**
 * Everything a client needs to connect, each with a copy button, and the secret
 * masked until someone reveals it, so a person copies the details rather than
 * reading where to find them. The card holds no secret of its own: revealing
 * asks the page, which confirms it as it must (such as with a passkey), and
 * hands the value back while it shows.
 */
export function ConnectionCard({
  title,
  actions,
  items,
  secret,
  connectionString,
  note,
  className,
}: ConnectionCardProps) {
  const strings = useUIStrings().connection;
  const revealed = secret?.value;
  const shownString =
    secret && revealed === undefined
      ? connectionString?.build(STAND_IN).replaceAll(STAND_IN, MASK)
      : connectionString?.build(revealed);
  const copiedString = connectionString?.build(revealed);
  // Without the secret: the string copies with it left out, and says so.
  const partial = secret !== undefined && revealed === undefined;
  return (
    <Panel
      title={hasContent(title) ? title : strings.title}
      actions={actions}
      className={cx("dw-conn-card", className)}
    >
      <dl className="dw-conn">
        {items.map((item) => (
          <div key={item.label} className="dw-conn-row">
            <dt className="dw-conn-label">{item.label}</dt>
            <dd className="dw-conn-value">
              <div className="dw-conn-line">
                <code className="dw-conn-code">{item.value}</code>
                <CopyButton
                  text={item.value}
                  label={strings.copy(item.label)}
                  copiedLabel={strings.copied(item.label)}
                  iconOnly
                  variant="ghost"
                />
              </div>
              {hasContent(item.hint) ? <div className="dw-conn-hint">{item.hint}</div> : null}
            </dd>
          </div>
        ))}
        {secret ? (
          <div className="dw-conn-row">
            <dt className="dw-conn-label">{secret.label}</dt>
            <dd className="dw-conn-value">
              <div className="dw-conn-line">
                {revealed !== undefined ? (
                  <code className="dw-conn-code is-revealed">{revealed}</code>
                ) : (
                  <code className="dw-conn-code is-masked">
                    <span aria-hidden="true">{MASK}</span>
                    <span className="dw-sr">{strings.hidden}</span>
                  </code>
                )}
                {revealed !== undefined ? (
                  <>
                    <CopyButton
                      text={revealed}
                      label={strings.copy(secret.label)}
                      copiedLabel={strings.copied(secret.label)}
                      iconOnly
                      variant="ghost"
                    />
                    <Button size="sm" variant="ghost" onClick={secret.onHide}>
                      {strings.hide}
                    </Button>
                  </>
                ) : (
                  <Button
                    size="sm"
                    variant="ghost"
                    icon="eye"
                    loading={secret.busy}
                    aria-label={strings.reveal(secret.label)}
                    onClick={secret.onReveal}
                  >
                    {strings.revealShort}
                  </Button>
                )}
                {hasContent(secret.action) ? <span className="dw-conn-action">{secret.action}</span> : null}
              </div>
              {hasContent(secret.problem) ? (
                <div className="dw-conn-hint is-problem" role="alert">
                  {secret.problem}
                </div>
              ) : hasContent(secret.hint) ? (
                <div className="dw-conn-hint">{secret.hint}</div>
              ) : null}
            </dd>
          </div>
        ) : null}
      </dl>
      {hasContent(note) ? <p className="dw-conn-note">{note}</p> : null}
      {connectionString && shownString !== undefined && copiedString !== undefined ? (
        <div className="dw-conn-string">
          <span className="dw-conn-label">{connectionString.label}</span>
          <div className="dw-conn-line is-well">
            <code className="dw-conn-code">{shownString}</code>
            <CopyButton
              text={copiedString}
              label={
                partial && secret
                  ? strings.copyWithout(connectionString.label, secret.label)
                  : strings.copy(connectionString.label)
              }
              copiedLabel={
                partial && secret
                  ? strings.copiedWithout(connectionString.label, secret.label)
                  : strings.copied(connectionString.label)
              }
              iconOnly
              variant="ghost"
            />
          </div>
        </div>
      ) : null}
    </Panel>
  );
}
