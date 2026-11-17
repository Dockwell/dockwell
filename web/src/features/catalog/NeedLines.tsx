// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type CatalogInstall,
  CatalogService,
  type ConnectionInfo,
  ConnectionReach,
  type Engine,
  type Entry,
  LinkKind,
  type Need,
  Part,
} from "@dockwell/api";
import { Button } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";

import { environmentOf, imageOf, nameOf } from "../apps/app";
import { usePlaces, useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { tagOf } from "../changes/upgrade";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { keptPrefix } from "../connections/storage";
import { useConnections } from "../connections/useConnections";
import { useAllVariables } from "../variables/VariablesPage";
import type { CardRow } from "./CardRows";
import { NeedBackups } from "./NeedBackups";
import { engineName } from "./needs";
import { isMail, isStorage, Reason, useMayLink } from "./StorageNeed";

/**
 * What meets each of an install's needs, on its app's card, as rows (#722): the need,
 * what meets it, and Use another, which lists the servers Dockwell runs that take it,
 * each one plan away (ADR 0084). A database it backs up alone has a row for its
 * backups. A server it shares is named where it is now, with the way to it, or as in
 * a project the reader can't see.
 */
export function useNeedRows(entry: Entry | undefined, install: CatalogInstall): CardRow[] {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const places = usePlaces();
  const services = useServices().data ?? [];
  const [open, setOpen] = useState<string>();
  const [backups, setBackups] = useState<string>();
  // Storage is met by a connection, linked from here by an install admin (ADR 0099).
  const connections = useConnections();
  const mayLink = useMayLink();
  const [addingFor, setAddingFor] = useState<string>();
  // The values a Disconnect left, where storage is on them: the bucket they name is
  // where the app's files are already.
  const onOwn = (entry?.needs ?? []).some((n) => isStorage(n) && install.pin?.needs[n.name]?.own);
  const variables = useAllVariables(onOwn ? install.environmentId : undefined);
  const id = entry?.id ?? "";
  const options = useQuery(CatalogService.method.getCatalogEntry, { id }, { enabled: (entry?.needs.length ?? 0) > 0 })
    .data?.needs;
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const may = useMay()(CatalogService.method.setCatalogNeed, { project: install.projectId });
  if (!entry) return [];
  const needs = entry.needs.filter((n) => install.pin?.needs[n.name]);
  const app = install.name || entry.name;
  const use = (need: string, choice: string, startEmpty = false) =>
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(CatalogService, transport).setCatalogNeed({ id: install.projectId, need, choice, startEmpty }),
    });
  /**
   * A storage need's row on an install's card (ADR 0099): what meets it now, the
   * connection with where its files go, the app's own disk, or its own values since a
   * Disconnect; and Use a connection, which lists each connection the install's project
   * may use, one that serves another app, or that the app can't take, off with why.
   * Leaving its disk or a bucket leaves the app's files there, which its button says,
   * and so does its plan (slice 2 review, M3).
   */
  const storageRow = ({
    need,
    open,
    onToggle,
    onUse,
    onAdd,
    adding,
  }: {
    need: Need;
    open: boolean;
    onToggle: () => void;
    onUse: (choice: string, startEmpty: boolean) => void;
    onAdd: () => void;
    adding: ReactNode;
  }): CardRow => {
    const connectionsList: readonly ConnectionInfo[] = connections.data?.connections ?? [];
    const may = mayLink;
    const pinned = install.pin?.needs[need.name];
    const on = pinned?.connection ?? "";
    const prefix = pinned?.prefix || "/";
    // Moving off a bucket, or off the values a Disconnect left, starts empty there.
    // Wherever its files are now, its own disk or a bucket, they stay there: but for
    // the bucket its own values name, as a Disconnect leaves them, where they are
    // already (slice 2 review, L3).
    const own = new Set(
      pinned?.own
        ? (variables.data?.variables ?? []).filter((v) => !v.secret && v.value !== "").map((v) => v.value)
        : [],
    );
    // Where the need sets a key prefix, its files are there only under the prefix its
    // own value keeps to, as the server compares it (slice 2 re-check, N4).
    const prefixName = need.sets.find((x) => x.part === Part.KEY_PREFIX)?.name;
    const keptUnder = (variables.data?.variables ?? []).find((v) => v.name === prefixName && !v.secret)?.value;
    const samePrefix = prefixName === undefined || keptUnder === undefined || keptUnder === (pinned?.prefix ?? "");
    const back = (c: ConnectionInfo) => samePrefix && c.endpoint !== "" && own.has(c.endpoint) && own.has(c.bucket);
    const asURL = need.sets.some((s) => s.part === Part.URL);
    const candidates = connectionsList.filter(
      (c) =>
        c.kind === LinkKind.S3 &&
        c.name !== on &&
        (c.reach === ConnectionReach.EVERY || c.projects.includes(install.projectId)),
    );
    let value: ReactNode;
    if (on) {
      value = (
        <>
          <Link to="/connections/$name" params={{ name: on }}>
            {on}
          </Link>{" "}
          <span className="card-row-quiet">· {t`objects under ${prefix}`}</span>
        </>
      );
    } else if (pinned?.own) {
      value = <Trans>Its own settings, since it was disconnected</Trans>;
    } else {
      value = <Trans>On its own disk</Trans>;
    }
    return {
      key: `need-${need.name}`,
      label: need.label,
      value,
      action: (
        <Button variant="ghost" size="sm" aria-expanded={open} onClick={onToggle} {...may.off}>
          {on ? <Trans>Use another connection</Trans> : <Trans>Use a connection</Trans>}
        </Button>
      ),
      under:
        adding ??
        (open ? (
          <ul className="catalog-need-line-ways" aria-label={t`Connections it can use`}>
            {candidates.map((c) => {
              // A connection serves one app unless an admin shared it (ADR 0099): one
              // another link reads is off. An app's own values hold it too, as a
              // Disconnect leaves them, but for this app's own, which it may go back onto
              // (slice 2 re-check, N2). Shared, an app that keeps to a prefix takes it
              // beside apps that keep to theirs, as the server offers it.
              const others = c.uses.filter((u) => !(u.environment === install.environmentId && u.link === need.name));
              const held = c.held && !back(c);
              const serves = others.length > 0 || held;
              const beside = c.shared && prefixName !== undefined && !held && others.every((u) => keptPrefix(u) !== "");
              const why = asURL
                ? t`${app} reads its storage as one URL, which a connection can't give yet`
                : serves && !beside
                  ? c.shared && prefixName === undefined
                    ? t`It serves another app already, and ${app} can't keep to a prefix of the bucket`
                    : t`It serves another app already`
                  : "";
              const label = c.displayName || c.name;
              const leaves = !back(c);
              return (
                <li key={c.name}>
                  <span className="catalog-need-line-way">
                    <span>{label}</span>
                    {why ? (
                      <span className="catalog-need-line-now is-reason">
                        <Reason why={why} />
                      </span>
                    ) : (
                      <span className="catalog-need-line-now">
                        {serves
                          ? t`Its files stay where they are now · shared`
                          : leaves
                            ? t`Its files stay where they are now`
                            : t`Its files are there already`}
                      </span>
                    )}
                  </span>
                  <code>{c.name}</code>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => onUse(`connection:${c.name}`, leaves)}
                    {...(why ? { disabled: true } : may.off)}
                  >
                    {leaves ? <Trans context="storage">Start empty on it</Trans> : <Trans>Use it</Trans>}
                  </Button>
                </li>
              );
            })}
            {candidates.length === 0 ? (
              <li className="catalog-need-line-none">
                <span className="catalog-need-line-now">
                  <Trans>No connection this project may use</Trans>
                </span>
              </li>
            ) : null}
            {may.allowed ? (
              <li className="catalog-need-line-add">
                <Button variant="ghost" size="sm" icon="plus" onClick={onAdd}>
                  <Trans>Add a connection</Trans>
                </Button>
              </li>
            ) : null}
          </ul>
        ) : undefined),
    };
  };

  /**
   * A mail need's row on an install's card (ADR 0099): the connection it sends
   * through and who its mail comes from, the values a person typed, or nothing; and
   * the way to a connection, which lists each one the install's project may use, one
   * that serves another app unshared, or whose security the app can't be told, off
   * with why. A mail server keeps no files, so a move takes one plan.
   */
  const mailRow = (need: Need): CardRow => {
    const pinned = install.pin?.needs[need.name];
    const on = pinned?.connection ?? "";
    const isOpen = open === need.name;
    const listed: readonly ConnectionInfo[] = connections.data?.connections ?? [];
    const current = listed.find((c) => c.name === on);
    const candidates = listed.filter(
      (c) =>
        c.kind === LinkKind.SMTP &&
        c.name !== on &&
        (c.reach === ConnectionReach.EVERY || c.projects.includes(install.projectId)),
    );
    let value: ReactNode;
    if (on) {
      value = (
        <>
          <Link to="/connections/$name" params={{ name: on }}>
            {on}
          </Link>
          {current?.sender ? <span className="card-row-quiet"> · {t`sends as ${current.sender}`}</span> : null}
        </>
      );
    } else if (pinned?.own) {
      value = <Trans>Set by hand</Trans>;
    } else {
      value = <Trans>Not set</Trans>;
    }
    return {
      key: `need-${need.name}`,
      label: need.label,
      value,
      action: (
        <Button
          variant="ghost"
          size="sm"
          aria-expanded={isOpen}
          onClick={() => setOpen(isOpen ? undefined : need.name)}
          {...mayLink.off}
        >
          {on ? (
            <Trans>Use another connection</Trans>
          ) : pinned?.own ? (
            <Trans>Use a connection</Trans>
          ) : (
            <Trans>Set up</Trans>
          )}
        </Button>
      ),
      under:
        addingFor === need.name ? (
          <ConnectionSheet
            onClose={() => setAddingFor(undefined)}
            onSaved={() => {
              setAddingFor(undefined);
              void connections.refetch();
            }}
            existing={listed}
            project={install.projectId}
            kind="mail"
          />
        ) : isOpen ? (
          <ul className="catalog-need-line-ways" aria-label={t`Connections it can use`}>
            {candidates.map((c) => {
              // The server says why one can't meet it, its security among it; one it
              // didn't list, as one only this project may use, is off where another
              // app uses it unshared, as the server refuses that too.
              const said = (options ?? []).find((o) => o.need === need.name && o.connection === c.name);
              const serves = c.uses.some((u) => !(u.environment === install.environmentId && u.link === need.name));
              const why = said?.why || (serves && !c.shared ? t`It serves another app already, and isn't shared` : "");
              return (
                <li key={c.name}>
                  <span className="catalog-need-line-way">
                    <span>{c.displayName || c.name}</span>
                    {why ? (
                      <span className="catalog-need-line-now is-reason">
                        <Reason why={why} />
                      </span>
                    ) : (
                      <span className="catalog-need-line-now">
                        {c.sender ? t`Sends as ${c.sender}` : t`Sends as what ${app} says`}
                      </span>
                    )}
                  </span>
                  <code>{c.name}</code>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => use(need.name, `connection:${c.name}`)}
                    {...(why ? { disabled: true } : mayLink.off)}
                  >
                    <Trans>Use it</Trans>
                  </Button>
                </li>
              );
            })}
            {candidates.length === 0 ? (
              <li className="catalog-need-line-none">
                <span className="catalog-need-line-now">
                  {on ? (
                    <Trans>No other mail server this project may use</Trans>
                  ) : (
                    <Trans>No mail server this project may use</Trans>
                  )}
                </span>
              </li>
            ) : null}
            {mayLink.allowed ? (
              <li className="catalog-need-line-add">
                <Button variant="ghost" size="sm" icon="plus" onClick={() => setAddingFor(need.name)}>
                  <Trans>Add a connection</Trans>
                </Button>
              </li>
            ) : null}
          </ul>
        ) : undefined,
    };
  };

  const rows: CardRow[] = [];
  for (const need of needs) {
    const pinned = install.pin?.needs[need.name];
    if (isMail(need)) {
      rows.push(mailRow(need));
      continue;
    }
    if (isStorage(need)) {
      rows.push(
        storageRow({
          need,
          open: open === need.name,
          onToggle: () => setOpen(open === need.name ? undefined : need.name),
          onUse: (choice, startEmpty) => use(need.name, choice, startEmpty),
          onAdd: () => setAddingFor(need.name),
          adding:
            addingFor === need.name ? (
              <ConnectionSheet
                onClose={() => setAddingFor(undefined)}
                onSaved={() => {
                  setAddingFor(undefined);
                  void connections.refetch();
                }}
                existing={connections.data?.connections ?? []}
                project={install.projectId}
                kind="storage"
              />
            ) : undefined,
        }),
      );
      continue;
    }
    const what = need.label.toLowerCase();
    const engine = engineName(Number(pinned?.engine ?? 0) as Engine);
    const shared = pinned?.environment ?? "";
    const place = shared ? places.place(shared) : undefined;
    const meets = pinned?.meets ?? "";
    const server = services.find((s) => environmentOf(s) === shared && nameOf(s) === meets);
    // The version the database runs now, as an upgrade of it moves it on; the pin keeps
    // the version it was installed at.
    const image = server ? imageOf(server) : "";
    const version = image ? tagOf(image) : (pinned?.version ?? "");
    const runs = `${engine} ${version === "latest" ? (pinned?.version ?? "") : version}`.trim();
    const where = place?.label ?? "";
    let value: ReactNode;
    if (!shared) {
      value = (
        <Trans>
          Its own {runs}, beside {app}
        </Trans>
      );
    } else if (!place) {
      // Where it is now isn't the reader's to see: its project is someone else's.
      value = <Trans>Shares {runs} in a project you can't see</Trans>;
    } else {
      const named = server ? (
        <Link to="/apps/$serviceId" params={{ serviceId: server.id }}>
          {meets}
        </Link>
      ) : (
        <code>{meets}</code>
      );
      value = (
        <>
          <Trans>
            Shares {named} in the project {where}
          </Trans>{" "}
          <span className="card-row-quiet">· {runs}</span>
        </>
      );
    }
    const elsewhere = (options ?? []).filter(
      (o) =>
        o.need === need.name &&
        o.service !== "" &&
        o.why === "" &&
        !(shared && o.project === place?.project && o.service === meets),
    );
    rows.push({
      key: `need-${need.name}`,
      label: need.label,
      value,
      action: (
        <Button
          variant="ghost"
          size="sm"
          aria-expanded={open === need.name}
          onClick={() => setOpen(open === need.name ? undefined : need.name)}
          {...(elsewhere.length === 0 ? { disabled: true, title: t`Nothing else Dockwell runs takes it` } : may.off)}
        >
          <Trans>Use another {what}</Trans>
        </Button>
      ),
      under:
        open === need.name ? (
          <ul className="catalog-need-line-ways" aria-label={t`Where else it can run`}>
            {elsewhere.map((o) => {
              const there = o.service;
              const project = o.project;
              // Its data comes along where both run the same engine; otherwise starting
              // empty is a choice of its own, never a default (ADR 0084).
              const copies = Number(o.engine) === Number(pinned?.engine ?? 0);
              return (
                <li key={o.choice}>
                  <span className="catalog-need-line-way">
                    <span>
                      <Trans>
                        {there} in {project}
                      </Trans>
                    </span>
                    <span className="catalog-need-line-now">
                      {copies ? <Trans>Copies its data</Trans> : <Trans>Its data can't be copied there</Trans>}
                    </span>
                  </span>
                  <code>{`${engineName(o.engine)} ${o.version}`.trim()}</code>
                  <Button variant="secondary" size="sm" onClick={() => use(need.name, o.choice, !copies)} {...may.off}>
                    {copies ? <Trans>Use it</Trans> : <Trans>Start empty on it</Trans>}
                  </Button>
                </li>
              );
            })}
          </ul>
        ) : undefined,
    });
    if (sqlEngines.includes(engine)) {
      rows.push({
        key: `backups-${need.name}`,
        label: <Trans>Backups</Trans>,
        value: <Trans>Of its {what} alone</Trans>,
        action: (
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={backups === need.name}
            onClick={() => setBackups(backups === need.name ? undefined : need.name)}
          >
            <Trans>Its backups</Trans>
          </Button>
        ),
        under: backups === need.name ? <NeedBackups install={install} need={need.name} /> : undefined,
      });
    }
  }
  return rows;
}

/** The engines whose one database an app backs up and restores alone (ADR 0084). */
const sqlEngines = ["PostgreSQL", "MySQL", "MariaDB"];
