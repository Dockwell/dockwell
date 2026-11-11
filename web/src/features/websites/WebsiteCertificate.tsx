// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { CertificateState, type Domain, type DomainServer } from "@dockwell/api";
import { Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { DomainNote, useDomainState, useSilent } from "../apps/DomainNote";
import type { DomainLook } from "../apps/domains";
import { EmptyState } from "../shell/EmptyState";
import { useWebsiteValue } from "./WebsiteLayout";

/**
 * A website's certificate, as its domain's servers say it stands: issued and until
 * when, on its way, none as TLS ends in front, or failed, with why and the fix, in the
 * words the Domains list has for it.
 */
export function WebsiteCertificate() {
  const { row } = useWebsiteValue();
  if (!row.domain || !row.look) {
    return (
      <EmptyState
        icon="shield"
        title={<Trans>No domain, so no certificate</Trans>}
        // Where to put it right: its configuration, where it can be deleted and made again.
        action={
          <RouterButtonLink to="/websites/$websiteId/configuration" params={{ websiteId: row.site.id }} size="lg">
            <Trans>Open its configuration</Trans>
          </RouterButtonLink>
        }
      />
    );
  }
  return <Certificates domain={row.domain} look={row.look} />;
}

function Certificates({ domain, look }: { domain: Domain; look: DomainLook }) {
  const { t } = useLingui();
  const { read } = useWebsiteValue();
  const silent = useSilent(domain, look);
  const [status, word] = useDomainState(domain, look, silent, read);
  return (
    <div className="website-tab">
      <Panel
        title={
          <span className="website-files-title">
            <code>{domain.name}</code>
          </span>
        }
        actions={<StatusBadge status={status} label={word} labelIsState />}
      >
        <div className="website-certificate">
          <DomainNote domain={domain} look={look} silent={silent} />
          {domain.servers.map((server) => (
            <ServerCertificate key={server.serverId} server={server} several={domain.servers.length > 1} />
          ))}
          <p className="websites-muted">
            {domain.routes[0]?.tls === "off"
              ? t`TLS ends at a tunnel or a proxy in front, so there's no certificate here.`
              : t`From Let's Encrypt once DNS points here, and renewed by itself.`}
          </p>
        </div>
      </Panel>
    </div>
  );
}

function ServerCertificate({ server, several }: { server: DomainServer; several: boolean }) {
  const { t, i18n } = useLingui();
  const certificate = server.status?.certificate;
  const expires =
    certificate?.state === CertificateState.ISSUED && certificate.expireTime
      ? timestampDate(certificate.expireTime)
      : undefined;
  const state = {
    [CertificateState.UNSPECIFIED]: t`Not known yet`,
    [CertificateState.PENDING]: t`On its way`,
    [CertificateState.ISSUED]: t`Issued`,
    [CertificateState.FAILED]: t`Failed`,
    [CertificateState.NONE]: t`None, as TLS ends in front`,
  }[certificate?.state ?? CertificateState.UNSPECIFIED];
  return (
    <dl className="website-facts">
      {several ? (
        <div className="website-fact">
          <dt>
            <Trans>Server</Trans>
          </dt>
          <dd>
            <code>{server.serverName}</code>
          </dd>
        </div>
      ) : null}
      <div className="website-fact">
        <dt>
          <Trans>Certificate</Trans>
        </dt>
        <dd>{server.stale ? t`${state}, as ${server.serverName} last said` : state}</dd>
      </div>
      {expires ? (
        <div className="website-fact">
          <dt>
            <Trans>Expires</Trans>
          </dt>
          <dd>
            <time dateTime={expires.toISOString()}>{formatDateTime(expires, i18n.locale)}</time>
          </dd>
        </div>
      ) : null}
      {certificate?.message ? (
        <div className="website-fact">
          <dt>
            <Trans>It says</Trans>
          </dt>
          <dd>{certificate.message}</dd>
        </div>
      ) : null}
    </dl>
  );
}
