// SPDX-License-Identifier: AGPL-3.0-only

import type { HostToolboxPlan } from "@dockwell/api";
import { Trans, useLingui } from "@lingui/react/macro";

import { formatBytes } from "../../lib/format";

export function ToolboxPlanView({ plan }: { plan: HostToolboxPlan }) {
  const { i18n } = useLingui();
  const operation = plan.operation?.operation;
  const bindings = operation?.case === "service" ? operation.value.environment : [];
  return (
    <section className="plan-section toolbox-plan">
      <h3 className="dw-text-heading">
        <Trans>Host change</Trans>
      </h3>
      <dl className="facts">
        {plan.differences.map(({ subject, before, after }) => (
          <div key={subject}>
            <dt>{subject}</dt>
            <dd>
              {before ? <code>{before}</code> : null}
              {before && after && before !== after ? <span aria-hidden="true"> → </span> : null}
              {after && before !== after ? <code>{after}</code> : null}
            </dd>
          </div>
        ))}
      </dl>
      {plan.unitFile ? (
        <>
          <h4 className="dw-text-label">
            <Trans>Systemd unit file</Trans>
          </h4>
          <pre className="toolbox-unit">
            <code>{plan.unitFile}</code>
          </pre>
        </>
      ) : null}
      {bindings.length > 0 ? (
        <>
          <h4 className="dw-text-label">
            <Trans>Environment bindings</Trans>
          </h4>
          <ul className="toolbox-plan-files">
            {bindings.map((b) => (
              <li key={b.name}>
                <code>{b.name}</code>
                <span>
                  {b.sealedLiteral.length > 0 || b.literalValue !== undefined ? (
                    <Trans>Entered in this plan</Trans>
                  ) : (
                    <Trans>
                      {b.source} in {b.environmentName}
                    </Trans>
                  )}
                </span>
                {b.secret || b.literalValue !== undefined ? (
                  <span>
                    <Trans>Secret · Hidden</Trans>
                  </span>
                ) : (
                  <code style={{ whiteSpace: "pre-wrap" }}>{JSON.stringify(b.reviewedValue)}</code>
                )}
              </li>
            ))}
          </ul>
          {[
            ...new Set(
              bindings
                .filter((binding) => binding.environmentId && !binding.environmentProtected)
                .map((binding) => binding.environmentName),
            ),
          ].map((environment) => (
            <p key={environment} className="dw-text-caption">
              <Trans>Developers with access to {environment} can edit its source values.</Trans>
            </p>
          ))}
        </>
      ) : null}
      {plan.files.length > 0 ? (
        <>
          <h4 className="dw-text-label">
            <Trans>Files to remove</Trans>
          </h4>
          <ul className="toolbox-plan-files">
            {plan.files.map((file) => (
              <li key={file.path}>
                <code>{file.path}</code>
                <span>{formatBytes(file.bytes, i18n.locale)}</span>
              </li>
            ))}
          </ul>
        </>
      ) : null}
    </section>
  );
}
