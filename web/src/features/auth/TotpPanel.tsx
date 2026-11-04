// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";
import { Button, Callout, Panel, Sheet, StatusBadge, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useMemo, useState } from "react";
import { encode } from "uqr";

import { useAuthError } from "./messages";

/** Code sign-in with an authenticator app, and its recovery codes. */
export function TotpPanel() {
  const { t } = useLingui();
  const totp = useQuery(AuthService.method.getTotp, {});
  const transport = useTransport();
  const explain = useAuthError();
  const [sheet, setSheet] = useState<"setup" | "codes" | undefined>();
  const [confirmOff, setConfirmOff] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const state = totp.data?.totp;
  const on = Boolean(state?.enabled);

  const turnOff = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(AuthService, transport).disableTotp({});
      setConfirmOff(false);
      await totp.refetch();
    } catch (err) {
      setProblem(explain(err));
      setConfirmOff(false);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel
      title={t`Authenticator app`}
      className="profile-panel"
      actions={state ? <StatusBadge status={on ? "healthy" : "stopped"} label={on ? t`On` : t`Off`} /> : null}
    >
      <div className="profile-totp">
        {on && state ? (
          <p>
            <Trans>
              Sign in with a 6-digit code when your passkey isn't at hand. {state.recoveryCodesLeft} of 10 recovery
              codes are left.
            </Trans>
          </p>
        ) : (
          <p>
            <Trans>
              Sign in with a 6-digit code when your passkey isn't at hand. Codes work at every address, so they also get
              you in after the panel moves to a new domain.
            </Trans>
          </p>
        )}
        {problem ? (
          <p className="profile-error" role="alert">
            {problem}
          </p>
        ) : null}
        <div className="profile-actions">
          {on ? (
            confirmOff ? (
              <>
                <Button size="sm" variant="danger" loading={busy} onClick={() => void turnOff()}>
                  <Trans>Turn off and delete codes</Trans>
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setConfirmOff(false)}>
                  <Trans>Keep it on</Trans>
                </Button>
              </>
            ) : (
              <>
                <Button size="sm" icon="refresh" onClick={() => setSheet("codes")}>
                  <Trans>New recovery codes</Trans>
                </Button>
                <Button size="sm" onClick={() => setSheet("setup")}>
                  <Trans>Move to another app</Trans>
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setConfirmOff(true)}>
                  <Trans>Turn off</Trans>
                </Button>
              </>
            )
          ) : (
            <Button size="sm" icon="lock" onClick={() => setSheet("setup")}>
              <Trans>Set up</Trans>
            </Button>
          )}
        </div>
      </div>
      <TotpSetupSheet
        open={sheet === "setup"}
        onClose={() => {
          setSheet(undefined);
          void totp.refetch();
        }}
      />
      <RegenerateSheet
        open={sheet === "codes"}
        left={state?.recoveryCodesLeft ?? 0}
        onClose={() => {
          setSheet(undefined);
          void totp.refetch();
        }}
      />
    </Panel>
  );
}

/** Scan, confirm with a code, then save the recovery codes. */
function TotpSetupSheet({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const [setup, setSetup] = useState<{ secret: string; uri: string }>();
  const [code, setCode] = useState("");
  const [codes, setCodes] = useState<string[]>();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();

  useEffect(() => {
    if (!open) {
      setSetup(undefined);
      setCode("");
      setCodes(undefined);
      setProblem(undefined);
      return;
    }
    let live = true;
    createClient(AuthService, transport)
      .beginTotpSetup({})
      .then((res) => live && setSetup({ secret: res.secret, uri: res.uri }))
      .catch((err: unknown) => live && setProblem(explain(err)));
    return () => {
      live = false;
    };
  }, [open, transport, explain]);

  const confirm = async (event: FormEvent) => {
    event.preventDefault();
    if (!/^\d{6}$/.test(code.replace(/\s/g, ""))) {
      setProblem(t`Enter the 6-digit code your authenticator app shows for Dockwell.`);
      return;
    }
    setBusy(true);
    setProblem(undefined);
    try {
      const res = await createClient(AuthService, transport).confirmTotpSetup({ code });
      setCodes(res.recoveryCodes);
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };

  if (codes) {
    return (
      <Sheet
        open={open}
        onClose={onClose}
        title={t`Save your recovery codes`}
        footer={
          <Button variant="primary" onClick={onClose}>
            <Trans>I saved them</Trans>
          </Button>
        }
      >
        <RecoveryCodes codes={codes} />
      </Sheet>
    );
  }
  return (
    <Sheet
      open={open}
      onClose={onClose}
      title={t`Set up an authenticator app`}
      subtitle={t`Such as 1Password, Google Authenticator or Aegis`}
      footer={
        <Button type="submit" form="totp-setup" variant="primary" loading={busy} disabled={!setup}>
          <Trans>Turn on</Trans>
        </Button>
      }
    >
      <form id="totp-setup" className="totp-setup" onSubmit={(e) => void confirm(e)} noValidate>
        <ol className="totp-steps">
          <li>
            <span>
              <Trans>Scan this code with the app.</Trans>
            </span>
            {setup ? (
              <QRCode text={setup.uri} label={t`QR code for your authenticator app`} />
            ) : (
              <div className="totp-qr" />
            )}
            {setup ? (
              <p className="totp-secret">
                <Trans>
                  Can't scan it? Enter this key instead: <code>{groupSecret(setup.secret)}</code>
                </Trans>
              </p>
            ) : null}
          </li>
          <li>
            <TextField
              label={t`Enter the 6-digit code it shows`}
              value={code}
              onChange={(e) => setCode(e.target.value)}
              autoComplete="one-time-code"
              inputMode="numeric"
              mono
              size="lg"
            />
          </li>
        </ol>
        {problem ? (
          <Callout tone="critical" title={t`The authenticator app isn't set up yet`}>
            {problem}
          </Callout>
        ) : null}
      </form>
    </Sheet>
  );
}

/** Replaces the recovery codes, after saying the old ones stop working. */
function RegenerateSheet({ open, left, onClose }: { open: boolean; left: number; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const [codes, setCodes] = useState<string[]>();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();

  useEffect(() => {
    if (!open) {
      setCodes(undefined);
      setProblem(undefined);
    }
  }, [open]);

  const regenerate = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      const res = await createClient(AuthService, transport).regenerateRecoveryCodes({});
      setCodes(res.recoveryCodes);
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Sheet
      open={open}
      onClose={onClose}
      title={codes ? t`Save your recovery codes` : t`Make new recovery codes`}
      footer={
        codes ? (
          <Button variant="primary" onClick={onClose}>
            <Trans>I saved them</Trans>
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose}>
              <Trans>Cancel</Trans>
            </Button>
            <Button variant="danger" loading={busy} onClick={() => void regenerate()}>
              <Trans>Replace codes</Trans>
            </Button>
          </>
        )
      }
    >
      {codes ? (
        <RecoveryCodes codes={codes} />
      ) : (
        <p className="totp-text">
          <Trans>
            Ten new codes replace the {left} you have now, and the old ones stop working at once. You see the new ones
            only once.
          </Trans>
        </p>
      )}
      {problem ? (
        <p className="profile-error" role="alert">
          {problem}
        </p>
      ) : null}
    </Sheet>
  );
}

function RecoveryCodes({ codes }: { codes: string[] }) {
  const { t } = useLingui();
  const [copied, setCopied] = useState(false);
  const text = codes.join("\n");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
    } catch {
      // Clipboard access was refused; the codes stay selectable.
    }
  };
  return (
    <div className="totp-codes">
      <p className="totp-text">
        <Trans>
          Each code signs you in once, with your email, if you lose your authenticator app. They're shown only now. Keep
          them somewhere safe, apart from your phone.
        </Trans>
      </p>
      <ul className="totp-code-list" aria-label={t`Recovery codes`}>
        {codes.map((c) => (
          <li key={c}>
            <code>{c}</code>
          </li>
        ))}
      </ul>
      <div className="profile-actions">
        <Button size="sm" icon={copied ? "check" : "copy"} onClick={() => void copy()}>
          {copied ? t`Codes copied` : t`Copy codes`}
        </Button>
        <a
          className="dw-btn dw-btn-secondary dw-btn-sm"
          href={`data:text/plain;charset=utf-8,${encodeURIComponent(`${text}\n`)}`}
          download="dockwell-recovery-codes.txt"
        >
          <span className="dw-btn-label">
            <Trans>Download</Trans>
          </span>
        </a>
      </div>
    </div>
  );
}

/** "JBSW Y3DP EHPK 3PXP …": a base32 secret in groups of four, easier to type. */
export function groupSecret(secret: string): string {
  return secret.replace(/(.{4})/g, "$1 ").trim();
}

/**
 * A QR code, drawn as one SVG path. It stays black on white in both themes,
 * because not every scanner reads inverted codes.
 */
function QRCode({ text, label }: { text: string; label: string }) {
  const { path, size } = useMemo(() => {
    const qr = encode(text, { ecc: "M", border: 2 });
    let d = "";
    qr.data.forEach((row, y) => {
      row.forEach((dark, x) => {
        if (dark) d += `M${x} ${y}h1v1h-1z`;
      });
    });
    return { path: d, size: qr.size };
  }, [text]);
  return (
    <svg className="totp-qr" viewBox={`0 0 ${size} ${size}`} role="img" aria-label={label} shapeRendering="crispEdges">
      <rect width={size} height={size} fill="#fff" />
      <path d={path} fill="#000" />
    </svg>
  );
}
