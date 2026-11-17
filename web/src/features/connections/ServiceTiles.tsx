// SPDX-License-Identifier: AGPL-3.0-only

import { cx } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { MAIL_PROVIDERS, type MailProviderId } from "./mail";
import { PROVIDERS, type ProviderId } from "./providers";

/** What a connection reaches: storage or a mail server, or either. */
export type ConnectionKindPick = "storage" | "mail";

/**
 * The services a connection is made for, as tiles in two groups, storage and mail:
 * picking one starts the form at it (ADR 0099). Only one group shows where the
 * connection's kind is settled, as for an install's need or a saved connection.
 */
export function ServiceTiles({
  picked,
  onPick,
  only,
  disabled,
  pressed = true,
}: {
  picked?: ProviderId | MailProviderId | undefined;
  onPick: (id: ProviderId | MailProviderId) => void;
  only?: ConnectionKindPick | undefined;
  disabled?: boolean;
  /** Tiles say which is picked, as in a form; on a page they only start one. */
  pressed?: boolean;
}) {
  const { t } = useLingui();
  const groups: {
    kind: ConnectionKindPick;
    label: string;
    tiles: { id: ProviderId | MailProviderId; mark: string; name: string }[];
  }[] = [
    {
      kind: "storage",
      label: t`Storage`,
      tiles: PROVIDERS.map((p) => ({ id: p.id, mark: p.mark, name: p.name || t`Another S3-compatible store` })),
    },
    {
      kind: "mail",
      label: t`Mail`,
      tiles: MAIL_PROVIDERS.map((p) => ({ id: p.id, mark: p.mark, name: p.name || t`Another mail server` })),
    },
  ];
  const shown = groups.filter((g) => only === undefined || g.kind === only);
  return (
    <div className="connection-service-groups">
      {shown.map((g) => (
        <fieldset key={g.kind} className="connection-stores" disabled={disabled}>
          <legend>{g.label}</legend>
          <div className="connection-store-grid">
            {g.tiles.map((x) => (
              <button
                key={x.id}
                type="button"
                className={cx("connection-store", pressed && picked === x.id && "is-picked")}
                aria-pressed={pressed ? picked === x.id : undefined}
                onClick={() => onPick(x.id)}
              >
                <span className="connection-mark" aria-hidden>
                  {x.mark}
                </span>
                <span>{x.name}</span>
              </button>
            ))}
          </div>
        </fieldset>
      ))}
    </div>
  );
}
