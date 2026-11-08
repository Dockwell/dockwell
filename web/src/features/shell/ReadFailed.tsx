// SPDX-License-Identifier: AGPL-3.0-only

import { Callout } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import type { ComponentProps, ReactNode } from "react";

import { isRoleRefusal, sentence } from "../../lib/errors";

/**
 * Says a read failed, under the page's own title, with its way to try again.
 * When the caller's role doesn't cover the read, it says so under a title of
 * its own and drops the retry, since trying again can't help.
 */
export function ReadFailed({
  error,
  title,
  tone = "critical",
  actions,
  className,
  children,
}: {
  error: unknown;
  /** What couldn't be read, such as "Dockwell can't read the audit log". */
  title: string;
  tone?: ComponentProps<typeof Callout>["tone"];
  /** The way to try again. */
  actions?: ReactNode;
  className?: string;
  /** What follows the reason, such as what isn't affected. */
  children?: ReactNode;
}) {
  const { t } = useLingui();
  if (isRoleRefusal(error)) {
    return (
      <Callout tone="warning" title={t`Your role doesn't cover this`} className={className}>
        {sentence(error)}
      </Callout>
    );
  }
  return (
    <Callout tone={tone} title={title} actions={actions} className={className}>
      {sentence(error)}
      {children ? <> {children}</> : null}
    </Callout>
  );
}
