// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { CertificateState, type Domain, type DomainServer, DomainService, StepUpAction } from "@dockwell/api";
import { Button, Callout, Panel, Sheet, StatusBadge, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { DomainNote, useDomainState, useSilent } from "../apps/DomainNote";
import type { DomainLook } from "../apps/domains";
import { useMay } from "../auth/may";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useChangeSheet } from "../changes/ChangeSheet";
import { EmptyState } from "../shell/EmptyState";
import { RefLink } from "../shell/RefLink";
import { useWebsiteValue } from "./WebsiteLayout";

function downloadTextFile(filename: string, content: string) {
  const blob = new Blob([content], { type: "application/x-pem-file" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

/**
 * A website's certificate, as its domain's servers say it stands: issued and until
 * when, on its way, none as TLS ends in front, or failed, with why and the fix, plus
 * manual renew (respecting CA rate limits), custom cert upload, and download.
 */
export function WebsiteCertificate() {
  const { row } = useWebsiteValue();
  if (!row.domain || !row.look) {
    return (
      <EmptyState
        icon="shield"
        title={<Trans>No domain, so no certificate</Trans>}
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
  const { read, row } = useWebsiteValue();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay();
  const silent = useSilent(domain, look);
  const [status, word] = useDomainState(domain, look, silent, read);

  const client = createClient(DomainService, transport);
  const primaryCert = domain.servers[0]?.status?.certificate;
  const tlsOff = domain.routes[0]?.tls === "off";
  const isCustom = Boolean(primaryCert?.custom);
  const isIssued = primaryCert?.state === CertificateState.ISSUED;
  const coveredNames = primaryCert?.sans?.length ? primaryCert.sans : [domain.name];
  const isSharedOrMultiName =
    Boolean(primaryCert?.byDns) ||
    coveredNames.length > 1 ||
    coveredNames.some((n) => n.startsWith("*.") || n.toLowerCase() !== domain.name.toLowerCase());

  const mayRenew = may(DomainService.method.renewCertificate, { project: row.site.id });
  const mayCustom = may(DomainService.method.setCustomCertificate, { project: row.site.id });
  const mayInstallAdmin = may(DomainService.method.setCustomCertificate, { install: true });
  const mayDownload = may(DomainService.method.downloadCertificate, { project: row.site.id });
  const mayDownloadKey = isSharedOrMultiName ? mayInstallAdmin : mayCustom;

  const [busy, setBusy] = useState<"renew" | "cert" | "key" | null>(null);
  const [notice, setNotice] = useState<{ tone: "info" | "critical"; title: string; body: string }>();
  const [forcePrompt, setForcePrompt] = useState<string>();
  const [confirmSharedKey, setConfirmSharedKey] = useState(false);
  const [uploading, setUploading] = useState(false);

  const renew = async (force: boolean) => {
    setBusy("renew");
    setNotice(undefined);
    try {
      const res = await client.renewCertificate({ domain: domain.name, force });
      setForcePrompt(undefined);
      setNotice({
        tone: "info",
        title: t`Renewal requested`,
        body: res.message || t`Certificate renewal has been started for ${domain.name}.`,
      });
    } catch (err) {
      const msg = sentence(err);
      if (!force && /isn't due for renewal|not due/i.test(msg)) {
        setForcePrompt(msg);
      } else {
        setNotice({ tone: "critical", title: t`Could not renew certificate`, body: msg });
      }
    } finally {
      setBusy(null);
    }
  };

  const downloadCert = async () => {
    setBusy("cert");
    setNotice(undefined);
    try {
      const res = await client.downloadCertificate({ domain: domain.name, includeKey: false });
      downloadTextFile(`${domain.name}.crt.pem`, res.certPem);
    } catch (err) {
      setNotice({ tone: "critical", title: t`Could not download certificate`, body: sentence(err) });
    } finally {
      setBusy(null);
    }
  };

  const downloadKey = async () => {
    setBusy("key");
    setNotice(undefined);
    try {
      const res = await withStepUpIfAsked(transport, StepUpAction.DOWNLOAD_CERTIFICATE_KEY, domain.name, (proof) =>
        client.downloadCertificate({ domain: domain.name, includeKey: true, proof }),
      );
      if (res?.keyPem) {
        downloadTextFile(`${domain.name}.key.pem`, res.keyPem);
      }
    } catch (err) {
      setNotice({ tone: "critical", title: t`Could not download private key`, body: sentence(err) });
    } finally {
      setBusy(null);
    }
  };

  const useAutomatic = () => {
    sheet.open({
      kind: "plan",
      make: () =>
        client.setCustomCertificate({
          domain: domain.name,
          clear: true,
          summary: t`Use automatic certificate for ${domain.name}`,
        }),
    });
  };

  return (
    <div className="website-tab">
      <Panel
        title={
          <span className="website-files-title">
            <code>{domain.name}</code>
            {isCustom ? (
              <Tag tone="cobalt" words>
                <Trans>Custom</Trans>
              </Tag>
            ) : null}
          </span>
        }
        actions={<StatusBadge status={status} label={word} labelIsState />}
      >
        <div className="website-certificate">
          <DomainNote domain={domain} look={look} silent={silent} />
          {notice ? (
            <Callout tone={notice.tone} title={notice.title}>
              {notice.body}
            </Callout>
          ) : null}
          {domain.servers.map((server) => (
            <ServerCertificate key={server.serverId} server={server} several={domain.servers.length > 1} />
          ))}
          {!tlsOff ? (
            <div className="website-cert-actions">
              {!isCustom ? (
                <Button
                  size="sm"
                  variant="secondary"
                  icon="refresh"
                  loading={busy === "renew"}
                  onClick={() => void renew(false)}
                  {...mayRenew.off}
                >
                  <Trans>Renew now</Trans>
                </Button>
              ) : (
                <Button size="sm" variant="secondary" icon="refresh" onClick={useAutomatic} {...mayCustom.off}>
                  <Trans>Use automatic certificate</Trans>
                </Button>
              )}
              <Button size="sm" variant="secondary" icon="upload" onClick={() => setUploading(true)} {...mayCustom.off}>
                <Trans>Upload certificate</Trans>
              </Button>
              {isIssued ? (
                <>
                  <Button
                    size="sm"
                    variant="ghost"
                    icon="download"
                    loading={busy === "cert"}
                    onClick={() => void downloadCert()}
                    {...mayDownload.off}
                  >
                    <Trans>Download certificate</Trans>
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    icon="key"
                    loading={busy === "key"}
                    onClick={() => {
                      if (isSharedOrMultiName) {
                        setConfirmSharedKey(true);
                      } else {
                        void downloadKey();
                      }
                    }}
                    {...mayDownloadKey.off}
                  >
                    <Trans>Download key</Trans>
                  </Button>
                </>
              ) : null}
            </div>
          ) : null}
          <p className="websites-muted">
            {tlsOff
              ? t`TLS ends at a tunnel or a proxy in front, so there's no certificate here.`
              : isCustom
                ? t`Served with your uploaded certificate. Private keys stay sealed at rest and require a passkey to download.`
                : t`From Let's Encrypt once DNS points here, and renewed by itself.`}
          </p>
        </div>
      </Panel>

      {forcePrompt ? (
        <Sheet
          open
          onClose={() => setForcePrompt(undefined)}
          title={t`Renew certificate early?`}
          footer={
            <>
              <Button variant="secondary" onClick={() => setForcePrompt(undefined)}>
                <Trans>Keep current</Trans>
              </Button>
              <Button
                variant="primary"
                icon="refresh"
                loading={busy === "renew"}
                onClick={() => void renew(true)}
                {...mayCustom.off}
              >
                <Trans>Renew anyway</Trans>
              </Button>
            </>
          }
        >
          <div className="website-fields">
            <Callout tone="warning" title={t`Certificate is not due for renewal yet`}>
              {forcePrompt}
            </Callout>
            <p className="websites-muted">
              <Trans>
                Forced renewals count against certificate authority duplicate-certificate rate limits. Use this only
                when replacing a compromised key or testing renewal.
              </Trans>
            </p>
          </div>
        </Sheet>
      ) : null}

      {confirmSharedKey ? (
        <Sheet
          open
          onClose={() => setConfirmSharedKey(false)}
          title={t`Download shared certificate key?`}
          footer={
            <>
              <Button variant="secondary" onClick={() => setConfirmSharedKey(false)}>
                <Trans>Cancel</Trans>
              </Button>
              <Button
                variant="primary"
                icon="key"
                loading={busy === "key"}
                onClick={() => {
                  setConfirmSharedKey(false);
                  void downloadKey();
                }}
              >
                <Trans>Continue to passkey</Trans>
              </Button>
            </>
          }
        >
          <div className="website-fields">
            <Callout tone="warning" title={t`This private key covers multiple names`}>
              <Trans>
                Downloading this key grants TLS credentials for every name listed below, which may include other
                websites or projects on this server. A passkey check follows.
              </Trans>
            </Callout>
            <ul className="website-cert-names">
              {coveredNames.map((name) => (
                <li key={name}>
                  <code>{name}</code>
                </li>
              ))}
            </ul>
          </div>
        </Sheet>
      ) : null}

      {uploading ? <UploadCertSheet domain={domain.name} onClose={() => setUploading(false)} /> : null}
    </div>
  );
}

function UploadCertSheet({ domain, onClose }: { domain: string; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const [certPem, setCertPem] = useState("");
  const [keyPem, setKeyPem] = useState("");
  const [error, setError] = useState<string>();

  const loadFile = (setter: (v: string) => void) => (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    void file.text().then(setter);
  };

  const submit = () => {
    if (!certPem.trim() || !keyPem.trim()) {
      setError(t`Provide both a PEM certificate chain and its matching private key.`);
      return;
    }
    setError(undefined);
    const client = createClient(DomainService, transport);
    onClose();
    sheet.open({
      kind: "plan",
      make: () =>
        client.setCustomCertificate({
          domain,
          certPem: certPem.trim(),
          keyPem: keyPem.trim(),
          summary: t`Upload custom certificate for ${domain}`,
        }),
    });
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Upload certificate for ${domain}`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" icon="upload" disabled={!certPem.trim() || !keyPem.trim()} onClick={submit}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="website-fields">
        <p className="websites-muted">
          <Trans>
            Dockwell verifies that the certificate covers {domain}, is not expired, and matches the private key before
            planning. The private key is sealed like other secrets.
          </Trans>
        </p>
        {error ? (
          <Callout tone="critical" title={t`Missing PEM data`}>
            {error}
          </Callout>
        ) : null}
        <div className="website-textarea-field">
          <label className="dw-label" htmlFor="cert-pem-input">
            <Trans>Certificate chain (PEM)</Trans>
          </label>
          <input
            type="file"
            accept=".pem,.crt,.cer"
            aria-label={t`Load certificate file`}
            onChange={loadFile(setCertPem)}
          />
          <textarea
            id="cert-pem-input"
            className="website-textarea is-mono"
            rows={5}
            spellCheck={false}
            placeholder="-----BEGIN CERTIFICATE-----"
            value={certPem}
            onChange={(e) => setCertPem(e.target.value)}
          />
        </div>
        <div className="website-textarea-field">
          <label className="dw-label" htmlFor="key-pem-input">
            <Trans>Private key (PEM)</Trans>
          </label>
          <input type="file" accept=".pem,.key" aria-label={t`Load private key file`} onChange={loadFile(setKeyPem)} />
          <textarea
            id="key-pem-input"
            className="website-textarea is-mono"
            rows={5}
            spellCheck={false}
            placeholder="-----BEGIN PRIVATE KEY-----"
            value={keyPem}
            onChange={(e) => setKeyPem(e.target.value)}
          />
        </div>
      </div>
    </Sheet>
  );
}

function ServerCertificate({ server, several }: { server: DomainServer; several: boolean }) {
  const { t, i18n } = useLingui();
  const certificate = server.status?.certificate;
  const notBefore =
    certificate?.state === CertificateState.ISSUED && certificate.notBefore
      ? timestampDate(certificate.notBefore)
      : undefined;
  const expires =
    certificate?.state === CertificateState.ISSUED && certificate.expireTime
      ? timestampDate(certificate.expireTime)
      : undefined;
  const state = {
    [CertificateState.UNSPECIFIED]: t`Not known yet`,
    [CertificateState.PENDING]: t`On its way`,
    [CertificateState.ISSUED]: certificate?.custom ? t`Issued (custom)` : t`Issued`,
    [CertificateState.FAILED]: t`Failed`,
    [CertificateState.NONE]: t`None, as HTTPS ends in front`,
  }[certificate?.state ?? CertificateState.UNSPECIFIED];
  return (
    <dl className="website-facts">
      {several ? (
        <div className="website-fact">
          <dt>
            <Trans>Server</Trans>
          </dt>
          <dd>
            <code>
              <RefLink kind="server" id={server.serverId} look="plain">
                {server.serverName}
              </RefLink>
            </code>
          </dd>
        </div>
      ) : null}
      <div className="website-fact">
        <dt>
          <Trans>Certificate</Trans>
        </dt>
        <dd>{server.stale ? t`${state}, as ${server.serverName} last said` : state}</dd>
      </div>
      {certificate?.issuer ? (
        <div className="website-fact">
          <dt>
            <Trans>Issuer</Trans>
          </dt>
          <dd>{certificate.issuer}</dd>
        </div>
      ) : null}
      {certificate?.sans && certificate.sans.length > 0 ? (
        <div className="website-fact">
          <dt>
            <Trans>Covers</Trans>
          </dt>
          <dd>
            <code>{certificate.sans.join(", ")}</code>
          </dd>
        </div>
      ) : null}
      {notBefore ? (
        <div className="website-fact">
          <dt>
            <Trans>Valid from</Trans>
          </dt>
          <dd>
            <time dateTime={notBefore.toISOString()}>{formatDateTime(notBefore, i18n.locale)}</time>
          </dd>
        </div>
      ) : null}
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
