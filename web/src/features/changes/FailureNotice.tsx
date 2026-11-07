// SPDX-License-Identifier: AGPL-3.0-only

import { Callout } from "@dockwell/ui";
import { Trans } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { factAndRest, withoutIds } from "./plan";

/**
 * A path, an image, a revision, a port or a command in a sentence: "/srv/uploads",
 * "acme/web:1.0", "nginx:1.27", "r12", "port 8080", "dockwell volumes delete data",
 * "--project". The panel's font joins "--" into a dash, so a command in it can't be read.
 */
const MACHINE =
  "(?<![\\w/.-])(?:dockwell(?: (?!and )[^\\s,]*[^\\s,.])+|--[a-z][\\w-]*|/[\\w.-]+(?:/[\\w.-]+)*|[\\w.-]+/[\\w./-]+(?::[\\w.-]+)?|[a-z][\\w.-]*:[\\w.-]*\\w|r\\d+(?![\\w.:-])|(?<=(?:port|端口) )\\d+)(?![\\w/-])";

const escaped = (name: string) => name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/**
 * Words from the server or a plan, with the machine values among them in mono,
 * as the brand book asks. A name reads like any word, so one only counts when
 * it is among names, such as the resources of the plan it is about.
 */
export function MachineWords({ children, names = [] }: { children?: ReactNode; names?: readonly string[] }) {
  if (typeof children !== "string") return <>{children}</>;
  const known = names
    .filter((name) => name !== "")
    .sort((a, b) => b.length - a.length)
    .map((name) => `(?<![\\w/.:-])${escaped(name)}(?![\\w/:-])`);
  const pattern = new RegExp([MACHINE, ...known].join("|"), "g");
  const out: ReactNode[] = [];
  let at = 0;
  for (const match of children.matchAll(pattern)) {
    if (match.index > at) out.push(children.slice(at, match.index));
    out.push(<code key={match.index}>{match[0]}</code>);
    at = match.index + match[0].length;
  }
  if (at < children.length) out.push(children.slice(at));
  return <>{out}</>;
}

/**
 * Why something failed, as the server says it in plain words: the fact leads,
 * the consequence and the fix follow, and what the server said in full waits
 * behind a disclosure, for whoever digs in. Only that has IDs and long names.
 */
export function FailureNotice({
  message,
  detail,
  fix,
  actions,
}: {
  message: string;
  /** The chain of errors, when there is one. */
  detail?: string | undefined;
  /** What to do about it, where this is shown, in place of what the server would have anyone do. */
  fix?: string | undefined;
  actions?: ReactNode;
}) {
  const [fact, rest] = factAndRest(withoutIds(message));
  return (
    <Callout tone="critical" title={<MachineWords>{fact}</MachineWords>} actions={actions} className="plan-failure">
      <MachineWords>{fix ?? rest}</MachineWords>
      {detail ? (
        <details className="plan-details">
          <summary>
            <Trans>Details</Trans>
          </summary>
          <pre className="plan-details-text">{detail}</pre>
        </details>
      ) : null}
    </Callout>
  );
}

/**
 * Why Dockwell made a change on its own, as nobody asked for it: what failed, in the
 * words the failed change has, with what its health check said where one failed.
 */
export function BecauseNotice({ because }: { because: string }) {
  return (
    <Callout
      tone="info"
      title={<Trans>Dockwell made this change because of what failed</Trans>}
      className="plan-because"
    >
      <MachineWords>{withoutIds(because)}</MachineWords>
    </Callout>
  );
}
