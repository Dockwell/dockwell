// SPDX-License-Identifier: AGPL-3.0-only

import type { DeployOrigin } from "@dockwell/api";
import { Trans, useLingui } from "@lingui/react/macro";

function safeHttps(value: string): URL | undefined {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && !url.username && !url.password ? url : undefined;
  } catch {
    return undefined;
  }
}

export function DeploySource({ origin }: { origin: DeployOrigin }) {
  const { t } = useLingui();
  const commitPage = safeHttps(origin.commitUrl);
  const run = safeHttps(origin.runUrl);
  const commit = origin.commit.slice(0, 7);
  const shortRef = origin.ref.replace(/^refs\/(heads|tags)\//, "");

  const link = commitPage ? (
    <a href={commitPage.href} target="_blank" rel="noreferrer" onClick={(event) => event.stopPropagation()}>
      {commit}
    </a>
  ) : (
    commit
  );
  return (
    <span className="ci-deploy-source">
      <span>
        <Trans>
          From {link} on {shortRef}
        </Trans>
      </span>
      {run ? (
        <a href={run.href} target="_blank" rel="noreferrer" onClick={(event) => event.stopPropagation()}>{t`CI run`}</a>
      ) : null}
    </span>
  );
}
