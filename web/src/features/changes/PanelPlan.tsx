// SPDX-License-Identifier: AGPL-3.0-only

import type { HostPanel } from "@dockwell/api";
import { Trans, useLingui } from "@lingui/react/macro";

/** A panel's name as people call it, by the word its plan gives it. */
export function panelName(kind: string): string {
  switch (kind) {
    case "coolify":
      return "Coolify";
    case "dokploy":
      return "Dokploy";
  }
  return kind;
}

/** What stopping or starting the panel an import came from does, in its step's line (ADR 0089). */
export function PanelLine({ panel, server }: { panel: HostPanel; server: string }) {
  const name = panelName(panel.kind);
  if (panel.apps) {
    const services = panel.parts.map((p) => p.name).join(", ");
    return panel.start ? (
      <Trans>
        Scale {services} back up in {name} on <code>{server}</code>
      </Trans>
    ) : (
      <Trans>
        Scale {services} to none in {name} on <code>{server}</code>
      </Trans>
    );
  }
  const proxy = panel.parts[0]?.name ?? "";
  if (panel.proxy) {
    return panel.start ? (
      <Trans>
        Start {proxy} again on <code>{server}</code>
      </Trans>
    ) : (
      <Trans>
        Stop {proxy} on <code>{server}</code>, so Dockwell takes ports 80 and 443
      </Trans>
    );
  }
  return panel.start ? (
    <Trans>
      Start {name} again on <code>{server}</code>
    </Trans>
  ) : (
    <Trans>
      Stop {name} on <code>{server}</code>
    </Trans>
  );
}

/**
 * What stopping the panel takes with it, under its step: its own containers and
 * services, that each stays stopped, what goes on running, and that nothing is
 * deleted. A start says what it puts back.
 */
export function PanelPlan({ panel, server }: { panel: HostPanel; server: string }) {
  const { t, i18n } = useLingui();
  const list = (names: string[]) =>
    new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" }).format(names);
  const name = panelName(panel.kind);
  const containers = list(panel.parts.filter((p) => !p.swarm).map((p) => p.name));
  const services = list(panel.parts.filter((p) => p.swarm).map((p) => p.name));
  const lines: { key: string; text: string }[] = [];
  if (panel.apps) {
    lines.push(
      panel.start
        ? { key: "start", text: t`Runs once Dockwell's copies are gone, as many replicas as each had` }
        : {
            key: "stops",
            text: t`First, so each is down for a moment until Dockwell's copy starts on the same volumes`,
          },
    );
  } else if (panel.proxy) {
    lines.push(
      panel.start
        ? { key: "start", text: t`Starts once the routes that hold ports 80 and 443 are gone` }
        : {
            key: "stops",
            text: t`Stays stopped when Docker or ${server} restarts, and its domains come as the routes below`,
          },
    );
    if (!panel.start && panel.domains.length > 0) {
      const chosen = list(panel.domains);
      lines.push({
        key: "checks",
        text: t`Just before the stop, the agent checks ${chosen} again, and leaves it running if anything changed`,
      });
    }
  } else if (panel.start) {
    lines.push({ key: "start", text: t`Starts ${list(panel.parts.map((p) => p.name))} as each stood before the stop` });
  } else {
    if (containers !== "") {
      lines.push({ key: "stops", text: t`Stops ${containers}, which stay stopped when Docker or ${server} restarts` });
    }
    if (services !== "") {
      lines.push({ key: "scales", text: t`Scales the Swarm services ${services} to none` });
    }
    const stays = panel.stays;
    if (stays !== "") {
      lines.push({ key: "stays", text: t`${stays} keeps serving the domains of what was taken on` });
    }
    lines.push({ key: "apps", text: t`What ${name} ran keeps running, and nothing of ${name} is deleted` });
  }
  return (
    <div className="plan-install">
      <span className="plan-step-meta">
        {lines.map((l) => (
          <span key={l.key}>{l.text}</span>
        ))}
      </span>
    </div>
  );
}
