// SPDX-License-Identifier: AGPL-3.0-only

import { type HostUnit, SystemServiceAction, SystemServiceGuard } from "@dockwell/api";
import { Trans, useLingui } from "@lingui/react/macro";

import { DockerStops } from "./DockerStops";

/** What an action on a system service does, in its step's line (ADR 0082). */
export function UnitLine({ unit, server }: { unit: HostUnit; server: string }) {
  const name = unit.name;
  switch (unit.action) {
    case SystemServiceAction.START:
      return (
        <Trans>
          Start <code>{name}</code> on {server}
        </Trans>
      );
    case SystemServiceAction.STOP:
      return (
        <Trans>
          Stop <code>{name}</code> on {server}
        </Trans>
      );
    case SystemServiceAction.RESTART:
      return (
        <Trans>
          Restart <code>{name}</code> on {server}
        </Trans>
      );
    case SystemServiceAction.RELOAD:
      return (
        <Trans>
          Reload <code>{name}</code> on {server}
        </Trans>
      );
    case SystemServiceAction.ENABLE:
      return (
        <Trans>
          Start <code>{name}</code> at boot on {server}
        </Trans>
      );
    case SystemServiceAction.DISABLE:
      return (
        <Trans>
          Stop <code>{name}</code> starting at boot on {server}
        </Trans>
      );
    case SystemServiceAction.MASK:
      return (
        <Trans>
          Mask <code>{name}</code> on {server}
        </Trans>
      );
    case SystemServiceAction.UNMASK:
      return (
        <Trans>
          Unmask <code>{name}</code> on {server}
        </Trans>
      );
  }
  return <code>{name}</code>;
}

/**
 * What an action on a system service takes with it, under its step: what stops or
 * restarts with it, what starts it again, what stops while Docker restarts, and how
 * it stood, which an undo puts back.
 */
export function UnitPlan({ unit, server, serverId }: { unit: HostUnit; server: string; serverId: string }) {
  const { t, i18n } = useLingui();
  const list = (names: string[]) =>
    new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" }).format(names);
  const restart = unit.action === SystemServiceAction.RESTART;
  const stop = unit.action === SystemServiceAction.STOP;
  const withIt = list(unit.with);
  const triggers = list(unit.triggers);
  const atBoot = unit.wasAtBoot;
  const lines: { key: string; text: string; warn?: boolean }[] = [];
  if (unit.with.length > 0) {
    lines.push({
      key: "with",
      text: restart ? t`Restarts ${withIt} with it, as they need it` : t`Stops ${withIt} with it, as they need it`,
      warn: stop,
    });
  }
  if (stop && unit.triggers.length > 0) {
    lines.push({ key: "triggers", text: t`${triggers} starts it again when it's asked for` });
  }
  if (unit.action === SystemServiceAction.DISABLE) {
    lines.push({ key: "boot", text: t`Another unit that needs it can still start it` });
  }
  if (unit.action === SystemServiceAction.MASK) {
    lines.push({
      key: "mask",
      text: t`Nothing can start it, by hand or as another unit needs it, until it's unmasked`,
    });
  }
  if (unit.action === SystemServiceAction.ENABLE || unit.action === SystemServiceAction.DISABLE) {
    if (atBoot) lines.push({ key: "was", text: t`At boot it reads ${atBoot} now` });
  }
  if (unit.replacesRules) {
    lines.push({
      key: "rules",
      text: t`Its own rules load, and can replace Docker's and Dockwell's on ${server}`,
      warn: true,
    });
    lines.push({
      key: "back",
      text: t`Dockwell puts its own table back after; Docker's rules come back when Docker restarts`,
    });
  } else if (unit.confirm && unit.guard === SystemServiceGuard.FIREWALL) {
    lines.push({
      key: "firewall",
      text: t`Every port the firewall closes on ${server} opens while it's stopped`,
      warn: true,
    });
  }
  if (unit.confirm && unit.guard === SystemServiceGuard.NETWORK) {
    lines.push({ key: "network", text: t`${server} may drop off the network for a moment`, warn: true });
  }
  return (
    <div className="plan-install">
      <span className="plan-step-meta">
        {lines.map((l) => (
          <span key={l.key} className={l.warn ? "plan-step-warn" : undefined}>
            {l.text}
          </span>
        ))}
        {unit.dockerRestarts ? <DockerStops serverId={serverId} server={server} /> : null}
      </span>
    </div>
  );
}
