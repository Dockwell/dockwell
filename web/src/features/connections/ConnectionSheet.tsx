// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type CheckConnectionResponse,
  type ConnectionInfo,
  ConnectionReach,
  ConnectionService,
  LinkKind,
} from "@dockwell/api";
import { Advanced, Button, Callout, cx, SelectField, Sheet, StatusBadge, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAllResources } from "../projects/useProjects";
import { useFleetView } from "../servers/useFleet";
import { BUCKET, freeConnectionName, PROVIDERS, type ProviderId, placeOf, provider, R2_ACCOUNT } from "./providers";
import { checkServer, SILENT, useCheckWords, useConnections } from "./useConnections";
import "./Connections.css";

/** How long typing pauses before a connection is checked. */
const CHECK_AFTER_MS = 700;

/**
 * Adds a connection to S3-compatible storage, or changes one, as a plan (ADR 0099).
 * A person picks the store, types what only they know (an account ID or a region,
 * the bucket and the keys), and the bucket is checked from a server as they type:
 * the check writes, reads and deletes a small object, and says Reached before
 * anything is planned. Its name, which projects may use it, its public address
 * and whether its bucket goes in the path are under Advanced, at defaults.
 */
export function ConnectionSheet({
  onClose,
  onSaved,
  existing = [],
  editing,
  project,
  start,
}: {
  onClose: () => void;
  /** Where to go once the plan applies, with the connection's name. */
  onSaved?: (name: string) => void;
  /** The connections there are, so a new one's name isn't taken. */
  existing?: readonly ConnectionInfo[];
  /** The connection to change, if any. */
  editing?: ConnectionInfo | undefined;
  /** The project a new one reaches by default, as when it's added for an app there. */
  project?: string | undefined;
  /** The store a new one starts at, as a tile picked on Connections. */
  start?: ProviderId | undefined;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(ConnectionService, transport), [transport]);
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  const mayCheck = may(ConnectionService.method.checkConnection).allowed;
  const words = useCheckWords();
  // What the pages list, read again once a change applies, so the page a save lands
  // on, or one a removal leaves, says what's there now.
  const listed = useConnections();
  const fleet = useFleetView();
  const resources = useAllResources();
  const projects = (resources.data ?? [])
    .filter((r) => r.resource?.kind === "project")
    .map((r) => ({ id: r.id, label: r.displayName || r.resource?.name || r.id }));

  const was = editing ? placeOf(editing.endpoint, editing.region) : undefined;
  const [store, setStore] = useState<ProviderId>(was?.provider ?? start ?? "r2");
  const p = provider(store);
  const [account, setAccount] = useState(was?.account ?? "");
  const [region, setRegion] = useState(was?.region || p.regions[0] || "");
  const [typedEndpoint, setTypedEndpoint] = useState(was?.provider === "other" ? (editing?.endpoint ?? "") : "");
  const [bucket, setBucket] = useState(editing?.bucket ?? "");
  const [keyId, setKeyId] = useState("");
  const [secret, setSecret] = useState("");
  const [typedName, setTypedName] = useState<string>();
  const [label, setLabel] = useState(editing?.displayName ?? "");
  const [publicUrl, setPublicUrl] = useState(editing?.publicUrl ?? "");
  const [pathStyle, setPathStyle] = useState<boolean>(editing?.pathStyle ?? p.pathStyle);
  const [pathTouched, setPathTouched] = useState(editing !== undefined);
  const startReach = editing?.reach ?? (project ? ConnectionReach.PROJECTS : ConnectionReach.EVERY);
  const [reach, setReach] = useState(
    startReach === ConnectionReach.UNSPECIFIED ? ConnectionReach.PROJECTS : startReach,
  );
  const [picked, setPicked] = useState<string[]>(editing?.projects ?? (project ? [project] : []));
  const [server, setServer] = useState("");
  const [answer, setAnswer] = useState<CheckConnectionResponse>();
  const [checking, setChecking] = useState(false);
  const [failure, setFailure] = useState<string>();

  // A store's own default for the path, until a person sets it.
  const pick = (id: ProviderId) => {
    const next = provider(id);
    setStore(id);
    setRegion(next.regions[0] ?? "");
    if (!pathTouched) setPathStyle(next.pathStyle);
  };
  const endpoint = store === "other" ? typedEndpoint.trim().replace(/\/+$/, "") : p.endpoint({ account, region });
  const signRegion = p.region({ region: store === "other" ? region.trim() : region });
  const name =
    editing?.name ??
    typedName ??
    freeConnectionName(
      bucket || p.name || "storage",
      existing.map((c) => c.name),
    );
  const servers = fleet.servers ?? [];
  const from = server || checkServer(servers);
  const bucketOk = BUCKET.test(bucket) && !bucket.includes("..");
  const typedKeys = keyId !== "" && secret !== "";
  // Moving a saved connection, or its keys, needs its keys again: the server keeps
  // what it was saved with only where they'd go to the same place.
  const moved =
    editing !== undefined &&
    (endpoint !== editing.endpoint ||
      signRegion !== editing.region ||
      bucket !== editing.bucket ||
      (editing.pathStyle && !pathStyle));
  // A key is its ID and its secret together: typing either asks for both, or a new
  // secret would be saved beside no ID, and every app reading it would start with none.
  const keysNeeded = !editing || moved || keyId !== "" || secret !== "";
  const keysKept = editing !== undefined && !moved && keyId === "" && secret === "";
  const complete = endpoint !== "" && bucketOk && (keysNeeded ? typedKeys : true);
  const reaching = reach === ConnectionReach.EVERY || picked.length > 0;
  const passed = answer?.passed === true;
  // A bucket that answered and refused is wrong as typed, as is an address Dockwell
  // keeps every check and app from. One this server can't reach from a network of the
  // check's own may still be one an app reaches from its own, as a store on an app's
  // network is: each app's link checks it again from there before anything is planned.
  const unreached = answer !== undefined && !passed && SILENT.includes(answer.code);
  // A change that moves nothing the bucket sees, as a label or reach, needs no check:
  // one runs only when asked, so opening Change sends the saved keys nowhere.
  const ready = complete && reaching && name !== "" && (mayCheck && keysNeeded ? passed || unreached : true);
  const [askedToCheck, setAskedToCheck] = useState(0);

  // Each change to what's checked moves this on, so an answer to what changed since is dropped.
  const asked = useRef(0);
  const what = JSON.stringify([endpoint, signRegion, bucket, keyId, secret, pathStyle, from, askedToCheck]);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it checks again whenever what's checked changes, which `what` holds
  useEffect(() => {
    const mine = ++asked.current;
    setAnswer(undefined);
    setFailure(undefined);
    if (!complete || !mayCheck || (!keysNeeded && askedToCheck === 0)) {
      setChecking(false);
      return;
    }
    setChecking(true);
    // A check still running when what's checked changes, or the sheet closes, stops:
    // its container on the server goes with it.
    const stop = new AbortController();
    const timer = setTimeout(async () => {
      try {
        const said = await client.checkConnection(
          {
            name: editing?.name ?? "",
            s3: { endpoint, region: signRegion, bucket, accessKeyId: keyId, secretKey: secret, pathStyle },
            server: from,
          },
          { signal: stop.signal },
        );
        if (mine === asked.current) setAnswer(said);
      } catch (err) {
        if (mine === asked.current) setFailure(sentence(err));
      } finally {
        if (mine === asked.current) setChecking(false);
      }
    }, CHECK_AFTER_MS);
    return () => {
      clearTimeout(timer);
      stop.abort();
    };
  }, [what, complete, mayCheck, keysNeeded]);

  const spec = () => ({
    displayName: label.trim() || (p.name ? `${p.name} · ${bucket}` : bucket),
    kind: LinkKind.S3,
    s3: {
      endpoint,
      region: signRegion,
      bucket,
      accessKeyId: keyId,
      secretKey: secret,
      pathStyle,
      publicUrl: publicUrl.trim(),
    },
    reach,
    projects: reach === ConnectionReach.PROJECTS ? picked : [],
  });
  const review = () =>
    sheet.open({
      kind: "propose",
      summary: editing ? t`Change the connection ${name}` : t`Connect to the bucket ${bucket}`,
      edits: [{ resource: { kind: "connection", name }, spec: { kind: { case: "connection", value: spec() } } }],
      onClose: (change) => {
        if (!change) return;
        void listed.refetch().then(() => {
          onClose();
          onSaved?.(name);
        });
      },
    });
  // A connection a link names can't go: Used by disconnects each first, as a plan.
  const links = editing?.uses.length ?? 0;
  const inUse =
    links > 0
      ? plural(links, {
          one: "# link names it. Disconnect it under Used by first.",
          other: "# links name it. Disconnect them under Used by first.",
        })
      : undefined;
  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: t`Remove the connection ${name}`,
      edits: [{ resource: { kind: "connection", name }, delete: true }],
      onClose: (change) => {
        if (change) void listed.refetch().then(onClose);
      },
    });

  const who =
    reach === ConnectionReach.EVERY
      ? t`every project`
      : picked.length === 1
        ? (projects.find((x) => x.id === picked[0])?.label ?? t`1 project`)
        : plural(picked.length, { one: "# project", other: "# projects" });
  const summary = [t`Named ${name}`, t`for ${who}`, pathStyle ? t`bucket in the path` : t`bucket in the host`].join(
    " · ",
  );
  const check = answer ? words(answer) : undefined;

  return (
    <Sheet
      open
      onClose={onClose}
      title={editing ? t`Connection ${editing.name}` : t`Add a connection`}
      subtitle={editing ? undefined : t`A bucket of S3-compatible storage, for apps to keep files in`}
      footer={
        <>
          {editing ? (
            <Button variant="ghost" className="connection-remove" onClick={remove} off={inUse} {...mayPlan.off}>
              <Trans>Remove</Trans>
            </Button>
          ) : null}
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!ready} onClick={review} {...mayPlan.off}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="connection-sheet">
        <fieldset className="connection-stores" disabled={editing !== undefined}>
          <legend>
            <Trans>Store</Trans>
          </legend>
          <div className="connection-store-grid">
            {PROVIDERS.map((x) => (
              <button
                key={x.id}
                type="button"
                className={cx("connection-store", store === x.id && "is-picked")}
                aria-pressed={store === x.id}
                onClick={() => pick(x.id)}
              >
                <span className="connection-mark" aria-hidden>
                  {x.mark}
                </span>
                <span>{x.name || t`Another store`}</span>
              </button>
            ))}
          </div>
        </fieldset>
        {p.account ? (
          <TextField
            label={t`Account ID`}
            mono
            autoComplete="off"
            spellCheck={false}
            value={account}
            placeholder="0123456789abcdef0123456789abcdef"
            hint={
              account !== "" && !R2_ACCOUNT.test(account.trim())
                ? t`32 letters and digits, as Cloudflare shows it`
                : undefined
            }
            onChange={(event) => setAccount(event.target.value.trim().toLowerCase())}
          />
        ) : null}
        {p.regions.length > 0 ? (
          <SelectField
            label={t`Region`}
            value={region}
            options={p.regions.map((r) => ({ value: r, label: r }))}
            onChange={(event) => setRegion(event.target.value)}
          />
        ) : null}
        {store === "other" ? (
          <TextField
            label={t`Endpoint`}
            mono
            autoComplete="off"
            spellCheck={false}
            value={typedEndpoint}
            placeholder="https://s3.example.com"
            onChange={(event) => setTypedEndpoint(event.target.value)}
          />
        ) : null}
        <TextField
          label={t`Bucket`}
          mono
          autoComplete="off"
          spellCheck={false}
          value={bucket}
          placeholder="outline-uploads"
          hint={bucket !== "" && !bucketOk ? t`3 to 63 lowercase letters, digits, dots and dashes` : undefined}
          onChange={(event) => setBucket(event.target.value.trim())}
        />
        <div className="connection-sheet-row">
          <TextField
            label={t`Access key ID`}
            mono
            autoComplete="off"
            spellCheck={false}
            value={keyId}
            placeholder={keysKept ? t`Kept unless you type one` : undefined}
            onChange={(event) => setKeyId(event.target.value.trim())}
          />
          <TextField
            label={t`Secret access key`}
            mono
            type="password"
            autoComplete="off"
            value={secret}
            placeholder={keysKept ? t`Kept unless you type one` : undefined}
            onChange={(event) => setSecret(event.target.value.trim())}
          />
        </div>
        <div className="connection-sheet-check" aria-live="polite">
          {editing && moved && !typedKeys ? (
            <StatusBadge status="unknown" label={t`Moved: type its keys again`} labelIsState />
          ) : editing && keysNeeded && !typedKeys ? (
            <StatusBadge status="unknown" label={t`Type both keys`} labelIsState />
          ) : checking ? (
            <StatusBadge status="deploying" label={t`Checking`} labelIsState />
          ) : check ? (
            <>
              <StatusBadge
                status={check.passed ? "healthy" : unreached ? "unknown" : "down"}
                label={check.state}
                labelIsState
              />
              <span className="connection-sheet-said">
                {check.line}
                {unreached ? ` ${t`An app's link checks it again from where the app runs.`}` : null}
              </span>
            </>
          ) : editing && complete && mayCheck ? (
            <Button size="sm" variant="secondary" icon="refresh" onClick={() => setAskedToCheck((n) => n + 1)}>
              <Trans>Check</Trans>
            </Button>
          ) : null}
        </div>
        {failure ? (
          <Callout tone="critical" title={t`Not checked`}>
            {failure}
          </Callout>
        ) : null}

        <Advanced summary={summary}>
          {editing ? null : (
            <TextField
              label={t`Name`}
              mono
              value={name}
              hint={t`What an app's link names it by, and the CLI.`}
              onChange={(event) => setTypedName(event.target.value.trim())}
            />
          )}
          <TextField
            label={t`Label`}
            value={label}
            placeholder={p.name ? `${p.name} · ${bucket || "bucket"}` : bucket || "bucket"}
            onChange={(event) => setLabel(event.target.value)}
          />
          <Switch
            checked={reach === ConnectionReach.PROJECTS}
            onChange={(on) => setReach(on ? ConnectionReach.PROJECTS : ConnectionReach.EVERY)}
            label={t`Only some projects`}
            description={t`Their apps alone may use it.`}
          />
          {reach === ConnectionReach.PROJECTS ? (
            <fieldset className="connection-projects">
              <legend className="dw-sr">{t`Projects`}</legend>
              {projects.map((x) => (
                <label key={x.id} className="connection-project">
                  <input
                    type="checkbox"
                    checked={picked.includes(x.id)}
                    onChange={(event) =>
                      setPicked((was) => (event.target.checked ? [...was, x.id] : was.filter((y) => y !== x.id)))
                    }
                  />
                  <span>{x.label}</span>
                </label>
              ))}
            </fieldset>
          ) : null}
          <Switch
            checked={pathStyle}
            onChange={(on) => {
              setPathStyle(on);
              setPathTouched(true);
            }}
            label={t`Bucket in the path`}
            description={t`For stores that don't take the bucket in the host.`}
          />
          {store === "other" ? (
            <TextField
              label={t`Region`}
              mono
              value={region}
              placeholder="auto"
              onChange={(event) => setRegion(event.target.value.trim())}
            />
          ) : null}
          <TextField
            label={t`Public address`}
            mono
            value={publicUrl}
            placeholder="https://files.example.com"
            hint={t`Where browsers fetch its objects, if it has one.`}
            onChange={(event) => setPublicUrl(event.target.value)}
          />
          {servers.length > 1 ? (
            <SelectField
              label={t`Checked from`}
              value={from}
              options={servers.map((s) => ({ value: s.id, label: s.name }))}
              onChange={(event) => setServer(event.target.value)}
            />
          ) : null}
        </Advanced>
      </div>
    </Sheet>
  );
}
