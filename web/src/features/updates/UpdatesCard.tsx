// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { PackageEffect, type Server, UpdateService } from "@dockwell/api";
import { Glyph, type GlyphShape, Icon, Panel } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { useControlPlaneNow } from "../servers/useFleet";
import { RebootButton } from "./reboot";
import { distributionOf, managerName, plainVersion, releaseStanding, summarize, useServerUpdates } from "./updates";
import { useWhyNot } from "./why";
import "./Updates.css";

interface Line {
  shape: GlyphShape;
  tone: "warning" | "healthy" | "unknown";
  title: string;
  sub: string;
}

/** What the card says of a server's updates, and of Dockwell's version. */
function useLines(
  server: Server,
): { updates: Line; dockwell: Line | undefined; checked: string; needsReboot: boolean } | undefined {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow();
  const whyNot = useWhyNot();
  // A server's updates are an admin's to read: the card asks for nobody else, and isn't drawn (ADR 0059).
  const ask = useMay()(UpdateService.method.getServerUpdates).allowed;
  const query = useServerUpdates(server.id, { ask });
  const u = query.data?.updates;
  if (!u) return undefined;
  const checked = u.listsTime ? t`Checked ${words.ago(timestampDate(u.listsTime), now)}` : "";
  const release = query.data?.dockwell;
  const standing = releaseStanding(release);
  const version = plainVersion(release?.controlPlane ?? "");
  const both = !release?.agent || plainVersion(release.agent) === version;
  const dockwell: Line | undefined =
    standing === "current"
      ? {
          shape: "circle",
          tone: "healthy",
          title: t`Dockwell ${version} is current`,
          sub: both ? t`Agent and control plane` : t`Control plane`,
        }
      : standing === "behind"
        ? {
            shape: "triangle",
            tone: "warning",
            title: t`Dockwell ${plainVersion(release?.newest ?? "")} is out`,
            sub: t`This runs ${version}. Its Updates tab says how to update`,
          }
        : undefined;
  const why = whyNot(server, u, release);
  // Nothing read yet, or nothing to read here: said as it is, never as up to date.
  if (u.unavailable || !u.readTime) {
    const updates: Line = why
      ? { shape: why.shape, tone: why.tone === "warning" ? "warning" : "unknown", title: why.title, sub: why.short }
      : { shape: "dashed-ring", tone: "unknown", title: t`Checking for updates`, sub: t`Asking ${server.name}` };
    return { updates, dockwell, checked, needsReboot: false };
  }
  const s = summarize(u);
  const distro = distributionOf(server.inventory?.osName);
  const reboot = s.security.some((p) => p.effect === PackageEffect.REBOOT) || u.rebootRequired;
  const names = (ps: { name: string }[]) => {
    const shown = ps.slice(0, 3).map((p) => p.name);
    return ps.length > shown.length ? t`${shown.join(", ")} and ${ps.length - shown.length} more` : shown.join(", ");
  };
  let updates: Line;
  if (s.security.length > 0) {
    const n = s.security.length;
    updates = {
      shape: "triangle",
      tone: "warning",
      title: distro
        ? plural(n, { one: `# security update for ${distro}`, other: `# security updates for ${distro}` })
        : plural(n, { one: "# security update", other: "# security updates" }),
      sub: reboot ? t`${names(s.security)}. Needs a reboot.` : `${names(s.security)}.`,
    };
  } else if (u.rebootRequired) {
    updates = {
      shape: "triangle",
      tone: "warning",
      title: t`${server.name} needs a reboot`,
      sub: t`For what was installed to take effect`,
    };
  } else if (s.total > 0 && !u.marksSecurity) {
    // Where the distribution doesn't mark security fixes, as Alpine doesn't, none is
    // known to be one, and none known not to be.
    const manager = managerName(u.manager);
    updates = {
      shape: "circle",
      tone: "unknown",
      title: plural(s.total, { one: "# update", other: "# updates" }),
      sub: t`${manager} doesn't mark security fixes. ${names([...s.other, ...(s.docker ? [s.docker] : [])])}.`,
    };
  } else if (s.total > 0) {
    updates = {
      shape: "circle",
      tone: "healthy",
      title: plural(s.total, { one: "# update, not for security", other: "# updates, none for security" }),
      sub: `${names([...s.other, ...(s.docker ? [s.docker] : [])])}.`,
    };
  } else {
    updates = { shape: "circle", tone: "healthy", title: t`Up to date`, sub: t`Nothing waits in its package lists` };
  }
  return { updates, dockwell, checked, needsReboot: u.rebootRequired };
}

/** The overview's Updates panel, on a desktop. */
export function UpdatesCard({ server }: { server: Server }) {
  const { t } = useLingui();
  const lines = useLines(server);
  if (!lines) return null;
  return (
    <Panel title={t`Updates`} actions={lines.checked ? <span className="panel-meta">{lines.checked}</span> : null}>
      <div className="updates-card-body">
        {[lines.updates, lines.dockwell].map((line) =>
          line ? (
            <div key={line.title} className="updates-card-row">
              <span className={`updates-card-glyph updates-tone-${line.tone}`}>
                <Glyph shape={line.shape} size={10} />
              </span>
              <div className="updates-card-text">
                <span className="updates-card-title">{line.title}</span>
                <span className="updates-card-sub">{line.sub}</span>
              </div>
            </div>
          ) : null,
        )}
        <div className="updates-card-actions">
          {/* A reboot the server needs is one step away, from where it says so. */}
          {lines.needsReboot ? <RebootButton server={server} /> : null}
          <RouterButtonLink to="/servers/$serverId/updates" params={{ serverId: server.id }} size="sm" icon="commit">
            <Trans>Review updates</Trans>
          </RouterButtonLink>
        </div>
      </div>
    </Panel>
  );
}

/** The overview's Updates link, on a phone: one line for what waits, and Review. */
export function UpdatesPhoneCard({ server }: { server: Server }) {
  const lines = useLines(server);
  if (!lines) return null;
  const line = lines.updates;
  return (
    <Link to="/servers/$serverId/updates" params={{ serverId: server.id }} className="dw-panel updates-phone-card">
      <span className={`updates-card-glyph updates-tone-${line.tone}`}>
        <Glyph shape={line.shape} size={10} />
      </span>
      <span className="updates-card-text">
        <span className="updates-phone-title">{line.title}</span>
        <span className="updates-phone-sub">{line.sub}</span>
      </span>
      <span className="updates-phone-review">
        <Trans>Review</Trans>
        <span>
          <Icon name="chevron-right" size={16} />
        </span>
      </span>
    </Link>
  );
}
