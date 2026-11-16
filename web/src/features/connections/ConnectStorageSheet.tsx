// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeState,
  type ConnectionInfo,
  ConnectionReach,
  ConnectionService,
  LinkPart,
  type ServiceInfo,
} from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  cx,
  Icon,
  PHONE_QUERY,
  SelectField,
  Sheet,
  Switch,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { environmentOf, nameOf } from "../apps/app";
import { appNames } from "../apps/connectDatabase";
import { useApps, usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { useAllVariables } from "../variables/VariablesPage";
import { ConnectionSheet } from "./ConnectionSheet";
import { freeLinkName, freePrefix, keyPart, linksIn, STORAGE_PARTS, suggestedStorage } from "./storage";
import { useConnections } from "./useConnections";
import "../apps/Apps.css";
import "./Connections.css";

/** A name the link doesn't set. */
const UNSET = String(LinkPart.UNSPECIFIED);
/** A link's prefix in a bucket: folders, each ending with a slash, as the server takes it. */
const PREFIX = /^([A-Za-z0-9._-]+\/)*$/;

/**
 * The apps a person may connect to storage: in an environment they may plan in, and,
 * for a connection, in a project it reaches. Below an admin, its projects are those
 * of the caller's own that may use it.
 */
export function useConnectableApps(connection?: ConnectionInfo | undefined): readonly ServiceInfo[] {
  const may = useMay();
  const places = usePlaces();
  const apps = useApps();
  return useMemo(
    () =>
      (apps.data ?? []).filter((s) => {
        const env = environmentOf(s);
        const project = places.environments.find((e) => e.id === env)?.scope ?? "";
        return (
          may(ConnectionService.method.connectStorage, { environment: env }).allowed &&
          (connection === undefined ||
            connection.reach === ConnectionReach.EVERY ||
            connection.projects.includes(project))
        );
      }),
    [connection, apps.data, places.environments, may],
  );
}

/**
 * Connects an app to a connection's bucket, as one plan (ADR 0099, Connect
 * storage). One choice a line, defaults first: the connection, the only one or
 * the first its project may use; which of the app's names the link sets, each
 * suggested from its name, a key only into a secret; and under Advanced, the
 * link's name, where its objects go in the bucket, and whether it hands browsers
 * signed links. The bucket is checked from the app's network on its server before
 * the plan is made, and a check that fails plans nothing and says why.
 *
 * Opened from an app, it picks the connection; opened from a connection, the app.
 */
export function ConnectStorageSheet({
  service: fixedService,
  connection: fixedConnection,
  onClose,
}: {
  service?: ServiceInfo | undefined;
  connection?: ConnectionInfo | undefined;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ConnectionService, transport), [transport]);
  const may = useMay();
  const places = usePlaces();

  // The app: the one it was opened for, or one a person picks in a project the
  // connection reaches, as the server narrowed what it lists.
  const [pickedApp, setPickedApp] = useState<string>();
  const apps = useApps();
  const connectable = useConnectableApps(fixedConnection);
  const choosable = fixedService ? [fixedService] : connectable;
  const service = fixedService ?? choosable.find((s) => s.id === pickedApp) ?? choosable[0];
  const environment = service ? environmentOf(service) : "";
  const project = places.environments.find((e) => e.id === environment)?.scope ?? "";
  const app = service ? nameOf(service) : "";

  // The connections its project may use, the one it was opened for first.
  const listed = useConnections(environment);
  const all = useConnections();
  const usable = listed.data?.connections ?? [];
  const [pickedConnection, setPickedConnection] = useState<string>();
  const connection = fixedConnection ?? usable.find((c) => c.name === pickedConnection) ?? usable[0];
  const [adding, setAdding] = useState(false);
  const mayAdd = may(ConnectionService.method.checkConnection).allowed;

  const store = useAllVariables(environment);
  const names = useMemo(() => (service ? appNames(service, store.data?.variables) : []), [service, store.data]);
  const [chosen, setParts] = useState<Map<string, LinkPart>>();
  const parts = chosen ?? suggestedStorage(names);
  const setsAny = [...parts.values()].some((p) => p !== LinkPart.UNSPECIFIED);
  const linkName = freeLinkName(linksIn(all.data?.connections ?? [], environment));
  const [typedLink, setTypedLink] = useState<string>();
  const [typedPrefix, setTypedPrefix] = useState<string>();
  const [presigned, setPresigned] = useState(false);
  const link = typedLink ?? linkName;
  // Each app on a connection keeps to a prefix no other link's meets, as the server
  // refuses one that does.
  const prefix =
    typedPrefix ??
    (app
      ? freePrefix(
          `${app}/`,
          (connection?.uses ?? []).map((u) => u.prefix),
        )
      : "");
  const prefixOk = PREFIX.test(prefix);

  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLSelectElement>(null);
  const partWord: Record<number, string> = {
    [LinkPart.ENDPOINT]: t`Endpoint`,
    [LinkPart.REGION]: t`Region`,
    [LinkPart.BUCKET]: t`Bucket`,
    [LinkPart.ACCESS_KEY_ID]: t`Access key ID`,
    [LinkPart.SECRET_KEY]: t`Secret access key`,
    [LinkPart.PUBLIC_URL]: t`Public address`,
    [LinkPart.PATH_STYLE]: t`Bucket in the path: true or false`,
  };

  const ask = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || !service || !connection || !setsAny || !prefixOk) return;
    setBusy(true);
    setFailure(undefined);
    const sets: Record<string, LinkPart> = {};
    for (const [n, p] of parts) if (p !== LinkPart.UNSPECIFIED) sets[n] = p;
    try {
      const res = await client.connectStorage({
        project,
        environment,
        app,
        connection: connection.name,
        link,
        prefix,
        sets,
        presignedUrls: presigned,
      });
      if (res.change) {
        const change = res.change;
        setPlan({
          kind: "plan",
          make: () => Promise.resolve({ change }),
          retryLabel: t`Choose again`,
          // What reads the connection changed: its pages read the list again.
          onDone: () => void all.refetch().then(onClose),
          onClose: (after) => {
            if (!after || after.state === ChangeState.PLANNED || after.state === ChangeState.FAILED) setPlan(undefined);
            else void all.refetch().then(onClose);
          },
        });
      }
    } catch (err) {
      // A check that failed says what it found from the app's server, and the fix.
      setFailure(sentence(err));
    }
    setBusy(false);
  };

  if (adding) {
    return (
      <ConnectionSheet
        onClose={() => setAdding(false)}
        onSaved={(name) => {
          setPickedConnection(name);
          setAdding(false);
          void listed.refetch();
          void all.refetch();
        }}
        existing={all.data?.connections ?? []}
        project={project}
      />
    );
  }

  const loading = listed.isPending || store.isPending || (!fixedService && apps.isPending);
  const title = fixedService ? t`Connect ${app} to storage` : t`Use ${fixedConnection?.name ?? ""} in an app`;
  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={title}
      subtitle={t`Its values come from the connection, checked from where the app runs`}
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing changes until you apply the plan</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy}
            disabled={!service || !connection || !setsAny || !prefixOk}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void ask()}
          >
            <Trans>Check and review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="app-form"
        aria-label={t`Connect storage`}
        onSubmit={(event) => void ask(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void ask(event);
        }}
      >
        {failure ? (
          <Callout tone="critical" title={t`Nothing was planned`}>
            {failure}
          </Callout>
        ) : null}
        {fixedService ? null : (
          <SelectField
            ref={field}
            label={t`App`}
            value={service?.id ?? ""}
            options={choosable.map((s) => {
              const env = environmentOf(s);
              const where = places.environments.find((e) => e.id === env)?.label ?? "";
              return { value: s.id, label: [nameOf(s), where].filter(Boolean).join(" · ") };
            })}
            onChange={(event) => {
              setPickedApp(event.target.value);
              setParts(undefined);
              setTypedLink(undefined);
              setTypedPrefix(undefined);
            }}
          />
        )}
        {fixedConnection ? null : !loading && usable.length === 0 ? (
          <Callout
            tone="info"
            title={t`No connection reaches this project`}
            actions={
              mayAdd ? (
                <Button size="sm" variant="secondary" icon="plus" onClick={() => setAdding(true)}>
                  <Trans>Add a connection</Trans>
                </Button>
              ) : undefined
            }
          >
            {mayAdd ? <Trans>A bucket's keys, pasted once.</Trans> : <Trans>An admin adds one.</Trans>}
          </Callout>
        ) : connection ? (
          <div className="connection-pick">
            <SelectField
              ref={fixedService ? field : undefined}
              label={t`Connection`}
              value={connection.name}
              mono
              options={usable.map((c) => ({
                value: c.name,
                label: c.displayName && c.displayName !== c.name ? `${c.name} · ${c.displayName}` : c.name,
              }))}
              onChange={(event) => setPickedConnection(event.target.value)}
            />
            {mayAdd ? (
              <Button size="sm" variant="ghost" icon="plus" onClick={() => setAdding(true)}>
                <Trans>Add another</Trans>
              </Button>
            ) : null}
          </div>
        ) : null}
        {service && connection ? (
          <fieldset className="app-connect-sets">
            <legend className="dw-text-label">
              <Trans>What {app} reads</Trans>
            </legend>
            {names.map((n) => (
              <SelectField
                key={n.name}
                label={n.name}
                mono
                size="sm"
                value={String(parts.get(n.name) ?? LinkPart.UNSPECIFIED)}
                options={[
                  { value: UNSET, label: t`Not set` },
                  ...STORAGE_PARTS.filter((p) => n.secret || !keyPart(p)).map((p) => ({
                    value: String(p),
                    label: partWord[p] ?? "",
                  })),
                ]}
                onChange={(event) => {
                  const next = new Map(parts);
                  next.set(n.name, Number(event.target.value) as LinkPart);
                  setParts(next);
                }}
              />
            ))}
            {names.length === 0 && !loading ? (
              <p className="settings-meta">
                <Trans>{app} reads no variable or secret of its environment yet.</Trans>
              </p>
            ) : null}
            {names.some((n) => !n.secret && /(SECRET|ACCESS_KEY)/i.test(n.name)) ? (
              <p className="settings-meta">
                <Trans>A key goes only into a secret, as plans show a variable.</Trans>
              </p>
            ) : null}
          </fieldset>
        ) : null}
        {service && connection ? (
          <Advanced summary={[t`Link ${link}`, t`objects under ${prefix || "/"}`].join(" · ")}>
            <TextField
              label={t`Link`}
              mono
              value={link}
              onChange={(event) => setTypedLink(event.target.value.trim())}
            />
            <TextField
              label={t`Where its objects go`}
              mono
              value={prefix}
              error={prefixOk ? undefined : t`Folders, each ending with a slash, such as ${app}/`}
              onChange={(event) => setTypedPrefix(event.target.value.trim())}
            />
            <Switch
              checked={presigned}
              onChange={setPresigned}
              label={t`It hands browsers signed links`}
              description={t`The check fetches one too.`}
            />
          </Advanced>
        ) : null}
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}
