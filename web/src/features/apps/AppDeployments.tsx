// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ActorKind,
  ChangeState,
  DeploymentState,
  FieldOp,
  type ServiceRevision,
  ServiceService,
  Surface,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  type IconName,
  Panel,
  PHONE_QUERY,
  Sheet,
  type Status,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useCallback, useMemo, useRef } from "react";

import { sentence } from "../../lib/errors";
import { formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useChangeWords } from "../changes/words";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { useApp, useRevisions } from "./AppContext";
import { ListSkeleton } from "./AppsPage";
import {
  bareImage,
  canRollBackTo,
  fieldName,
  fieldOfChange,
  imageChange,
  nameOf,
  revisionName,
  shortDigest,
  shortImages,
  tidyMessage,
} from "./app";
import { ImageName, WholeImage } from "./ImageName";
import { type Look, useRevisionLook, useRevisionTitle, useWhoWords } from "./revision";
import { useAppWords } from "./words";

const DEPLOYMENT_LOOKS: Partial<Record<DeploymentState, Status>> = {
  [DeploymentState.LIVE]: "healthy",
  [DeploymentState.FAILED]: "down",
  [DeploymentState.PENDING]: "deploying",
};

/** From this wide, the list has room for how long each revision took. */
const ROOMY_QUERY = "(min-width: 900px)";

/** Where a change came from, by its mark: a person, a token, or Dockwell. */
function triggerIcon(rev: ServiceRevision): IconName {
  if (rev.change?.reverts || rev.rolledBackTo > 0n) return "undo";
  const kind = (rev.change?.appliedBy ?? rev.change?.proposedBy)?.kind;
  if (kind === ActorKind.TOKEN) return "key";
  if (kind === ActorKind.SHELL) return "terminal";
  if (kind === ActorKind.DOCKWELL) return "activity";
  return "user";
}

/** How long a change's steps took, from the first start to the last finish. */
function took(rev: ServiceRevision): number | undefined {
  let start: number | undefined;
  let finish: number | undefined;
  for (const step of rev.change?.steps ?? []) {
    if (step.startTime) start = Math.min(start ?? Number.POSITIVE_INFINITY, timestampMs(step.startTime));
    if (step.finishTime) finish = Math.max(finish ?? 0, timestampMs(step.finishTime));
  }
  return start !== undefined && finish !== undefined ? finish - start : undefined;
}

/**
 * Each revision, newest first: what changed, who did it, from where, and how
 * it went (App-Deployments). The chosen one opens beside the list, on phones
 * in a sheet, with its diff and the way back to it, which is a plan too.
 */
export function AppDeployments() {
  const { t, i18n } = useLingui();
  const { service, state } = useApp();
  const phone = useMediaQuery(PHONE_QUERY);
  const roomy = useMediaQuery(ROOMY_QUERY);
  const time = useTimeWords();
  const look = useRevisionLook();
  const revisionTitle = useRevisionTitle();
  const who = useWhoWords();
  const navigate = useNavigate();
  const words = useChangeWords();
  const search: { revision?: string } = useSearch({ strict: false });
  const history = useRevisions(service.id);
  const appName = nameOf(service);
  const { revisions } = history;
  const now = useAgoNow(revisions?.[0]?.createTime ? timestampDate(revisions[0].createTime) : undefined);
  const rollBack = useRollBack();
  const latest = service.revision;
  const selectedNumber = search.revision;
  // Without a choice, wide screens show the one that is live; on a phone the list comes first.
  const selected =
    revisions?.find((r) => r.number.toString() === selectedNumber) ??
    (phone || selectedNumber ? undefined : revisions?.[0]);

  // Below the list, where a narrow screen puts it, the one chosen comes into view.
  const layout = useRef<HTMLDivElement>(null);
  const select = (rev: ServiceRevision | undefined) => {
    void navigate({
      to: "/apps/$serviceId/deployments",
      params: { serviceId: service.id },
      search: rev ? { revision: rev.number.toString() } : {},
      replace: true,
    });
    requestAnimationFrame(() => {
      const panel = layout.current?.querySelector<HTMLElement>(".deploy-panel");
      const list = layout.current?.querySelector<HTMLElement>(".deploy-list");
      if (panel && list && panel.offsetTop > list.offsetTop + 8)
        panel.scrollIntoView?.({ block: "start", behavior: "smooth" });
    });
  };

  const columns = useMemo<TableColumn<ServiceRevision & { id: string }>[]>(() => {
    const out: TableColumn<ServiceRevision & { id: string }>[] = [
      {
        key: "number",
        header: t`Revision`,
        width: 72,
        render: (rev) => <code className="deploy-number">{revisionName(rev.number)}</code>,
      },
      {
        key: "status",
        header: t`Status`,
        width: 112,
        render: (rev) => {
          const { status, word } = look(rev, latest, state);
          return <StatusBadge status={status} label={word} labelIsState title={word} className="apps-status" />;
        },
      },
      {
        key: "what",
        header: t`What changed`,
        render: (rev) => {
          const title = revisionTitle(rev);
          return (
            <span className="dw-cell">
              <span className="dw-cell-title deploy-clip" title={title.text}>
                {title.node}
              </span>
              <span className="deploy-what">
                <ChangedLine rev={rev} />
              </span>
            </span>
          );
        },
      },
      {
        key: "who",
        header: t`By`,
        width: 136,
        render: (rev) => {
          const name = who.name(rev.change);
          const via = rev.change ? words.via(rev.change.appliedVia || rev.change.proposedVia) : "";
          return (
            <span className="dw-cell deploy-who">
              <span className="deploy-who-name">
                <Icon name={triggerIcon(rev)} size={14} />
                <span className="deploy-clip" title={name}>
                  {name || t`Someone`}
                </span>
              </span>
              <span className="deploy-who-via">{via}</span>
            </span>
          );
        },
      },
    ];
    // How long it took waits in the revision's panel on a screen with no room for it.
    if (roomy) {
      out.push({
        key: "took",
        header: t`Took`,
        width: 76,
        align: "right",
        muted: true,
        render: (rev) => {
          const ms = took(rev);
          return ms === undefined ? "" : time.bound(ms);
        },
      });
    }
    out.push({
      key: "at",
      header: t`Deployed`,
      width: 100,
      align: "right",
      muted: true,
      render: (rev) => {
        const when = rev.createTime ? timestampDate(rev.createTime) : new Date(0);
        return (
          <time dateTime={when.toISOString()} title={formatDateTime(when, i18n.locale)}>
            {time.ago(when, now)}
          </time>
        );
      },
    });
    return out;
  }, [t, i18n, latest, look, state, revisionTitle, words, who, time, now, roomy]);

  const newest = revisions?.[0] ? revisionName(revisions[0].number) : undefined;
  const lastShown = revisions?.[revisions.length - 1];
  const oldest = lastShown ? revisionName(lastShown.number) : undefined;
  const rows = useMemo(() => (revisions ?? []).map((rev) => ({ ...rev, id: rev.number.toString() })), [revisions]);

  if (history.error && !revisions) {
    return (
      <Callout
        tone="critical"
        title={t`Can't load the deployments`}
        actions={
          <Button onClick={() => void history.refetch()} loading={history.isFetching}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        {sentence(history.error)}
      </Callout>
    );
  }

  const list = (
    <section className="deploy-list" aria-label={t`Revisions`}>
      <div className="deploy-list-head">
        <h2 className="dw-text-heading">
          <Trans>Revisions</Trans>
        </h2>
        <span className="app-muted">
          {plural(Number(latest), { one: "# revision · newest first", other: "# revisions · newest first" })}
        </span>
      </div>
      {!revisions ? (
        <ListSkeleton label={t`Loading revisions`} />
      ) : phone ? (
        <Panel flush>
          <ul className="deploy-phone">
            {revisions.map((rev) => {
              const { status, word } = look(rev, latest, state);
              const when = rev.createTime ? timestampDate(rev.createTime) : new Date(0);
              return (
                <li key={rev.number.toString()}>
                  <button type="button" className="deploy-phone-row" onClick={() => select(rev)}>
                    <span className="deploy-phone-top">
                      <code className="deploy-number">{revisionName(rev.number)}</code>
                      <StatusBadge status={status} label={word} labelIsState />
                      <span className="deploy-phone-time">{time.ago(when, now)}</span>
                    </span>
                    <span className="deploy-phone-summary">{revisionTitle(rev).node}</span>
                    <span className="deploy-phone-by">{who.name(rev.change)}</span>
                  </button>
                </li>
              );
            })}
          </ul>
        </Panel>
      ) : (
        <Table
          className="deploy-table"
          columns={columns}
          rows={rows}
          selectedKey={selected?.number.toString() ?? null}
          caption={t`Revisions of ${appName}`}
          onRowClick={(rev) => select(rev)}
        />
      )}
      {history.hasNextPage && oldest !== undefined ? (
        <div className="deploy-more">
          <span className="app-muted">
            <Trans>
              Showing {oldest} to {newest}
            </Trans>
          </span>
          <Button
            variant="ghost"
            size="sm"
            icon="chevron-down"
            loading={history.isFetchingNextPage}
            onClick={() => void history.fetchNextPage()}
          >
            <Trans>Show older revisions</Trans>
          </Button>
        </div>
      ) : null}
    </section>
  );

  return (
    <div ref={layout} className="deploy-layout">
      {list}
      {selected && !phone ? (
        <RevisionPanel
          rev={selected}
          latest={latest}
          look={look(selected, latest, state)}
          onRollBack={rollBack}
          revisions={revisions ?? []}
        />
      ) : null}
      {selected && phone ? (
        <Sheet
          open
          title={revisionName(selected.number)}
          subtitle={revisionTitle(selected).node}
          onClose={() => select(undefined)}
          className="app-sheet is-phone"
          footer={
            <RollBackFoot rev={selected} latest={latest} revisions={revisions ?? []} onRollBack={rollBack} wide />
          }
        >
          <RevisionBody rev={selected} />
        </Sheet>
      ) : null}
    </div>
  );
}

/** Plans going back to a revision, and opens the plan. */
function useRollBack() {
  const { t } = useLingui();
  const { service } = useApp();
  const name = nameOf(service);
  const transport = useTransport();
  const sheet = useChangeSheet();
  return useCallback(
    (rev: ServiceRevision) => {
      const target = revisionName(rev.number);
      sheet.open({
        kind: "plan",
        make: () =>
          createClient(ServiceService, transport).rollBackService({
            id: service.id,
            revision: rev.number,
            summary: t`Roll back ${name} to ${target}`,
          }),
      });
    },
    [t, name, service.id, transport, sheet],
  );
}

/**
 * Where the way back from a revision goes: from the one that is live, to the
 * newest before it that ran and differs from it; from any other, to it, unless
 * it matches what runs. Nowhere while it is moving.
 */
function rollBackTarget(
  rev: ServiceRevision,
  latest: bigint,
  revisions: readonly ServiceRevision[],
): ServiceRevision | undefined {
  if (revisions[0]?.deployments.some((d) => d.state === DeploymentState.PENDING)) return undefined;
  const recovering = revisions[0]?.deployments.some((d) => d.state === DeploymentState.FAILED) ?? false;
  if (rev.number === latest) return revisions.find((r) => r.number < latest && canRollBackTo(r, recovering));
  return canRollBackTo(rev, recovering) ? rev : undefined;
}

/** A revision's changes in a line: the first field and how many more. */
function ChangedLine({ rev }: { rev: ServiceRevision }) {
  const { t } = useLingui();
  const words = useAppWords();
  const changes = rev.changes;
  const [first] = changes;
  if (rev.deleted) return <span>{t`Deleted`}</span>;
  if (!first || rev.number === 1n) return <ImageName image={bareImage(rev.image)} className="deploy-image-name" />;
  // A variable goes by its own name, and a port's three fields are one change.
  const groupOf = (path: string) => (fieldOfChange(path) === "env" ? path : fieldOfChange(path));
  const more = new Set(changes.map((c) => groupOf(c.path))).size - 1;
  const name =
    fieldOfChange(first.path) === "env"
      ? (/\[(.*)\]/.exec(first.path)?.[1] ?? words.field("env"))
      : words.field(fieldOfChange(first.path));
  const secret = first.sensitive;
  return (
    <>
      <code className="deploy-field">{name}</code>
      {secret ? (
        <span>{t`changed`}</span>
      ) : first.from || first.to ? (
        <code
          className="deploy-clip"
          title={first.from && first.to ? `${first.from} → ${first.to}` : first.to || first.from}
        >
          {first.op === FieldOp.ADDED
            ? first.to
            : first.op === FieldOp.REMOVED
              ? first.from
              : name === "image"
                ? shortImages(first.from, first.to)
                : `${first.from} → ${first.to}`}
        </code>
      ) : null}
      {more > 0 ? <span className="deploy-more-count">{t`and ${more} more`}</span> : null}
    </>
  );
}

/** The way back from a revision, or why there isn't one that would change anything. */
function RollBackFoot({
  rev,
  latest,
  revisions,
  onRollBack,
  wide = false,
}: {
  rev: ServiceRevision;
  latest: bigint;
  revisions: readonly ServiceRevision[];
  onRollBack: (rev: ServiceRevision) => void;
  wide?: boolean;
}) {
  const target = rollBackTarget(rev, latest, revisions);
  const shown = revisionName(rev.number);
  if (!target) {
    if (rev.number === latest || !rev.matchesLatest) return null;
    // The record matches, which says nothing of a container removed by hand. The
    // plan asks the server, and refuses if there's nothing to do.
    const moving = revisions[0]?.deployments.some((d) => d.state === DeploymentState.PENDING);
    return (
      <>
        <span className="deploy-matches app-muted">
          <Trans>
            {shown} matches the latest revision. If what runs has drifted from it, rolling back puts it right.
          </Trans>
        </span>
        {moving ? null : (
          <Button
            icon="undo"
            size={wide ? "lg" : "md"}
            onClick={() => onRollBack(rev)}
            className={cx(wide && "deploy-wide")}
          >
            <Trans>Roll back to {shown}</Trans>
          </Button>
        )}
      </>
    );
  }
  const name = revisionName(target.number);
  return (
    <>
      {wide ? null : (
        <span className="app-muted">
          <Trans>Opens a plan to review first</Trans>
        </span>
      )}
      <Button
        icon="undo"
        size={wide ? "lg" : "md"}
        onClick={() => onRollBack(target)}
        className={cx(wide && "deploy-wide")}
      >
        <Trans>Roll back to {name}</Trans>
      </Button>
    </>
  );
}

function RevisionPanel({
  rev,
  latest,
  look,
  revisions,
  onRollBack,
}: {
  rev: ServiceRevision;
  latest: bigint;
  look: Look;
  revisions: readonly ServiceRevision[];
  onRollBack: (rev: ServiceRevision) => void;
}) {
  const { i18n } = useLingui();
  const time = useTimeWords();
  const when = rev.createTime ? timestampDate(rev.createTime) : new Date(0);
  const now = useAgoNow(when);
  const name = revisionName(rev.number);
  const foot = rollBackTarget(rev, latest, revisions) !== undefined || (rev.number !== latest && rev.matchesLatest);
  return (
    <div className="deploy-panel">
      <Panel
        title={
          <span className="deploy-panel-title">
            <span className="deploy-number">{name}</span>
            <StatusBadge status={look.status} label={look.word} labelIsState />
          </span>
        }
        actions={
          <span className="app-muted deploy-panel-time" title={formatDateTime(when, i18n.locale)}>
            {time.ago(when, now)}
          </span>
        }
        flush
      >
        <div className="deploy-panel-body">
          <RevisionBody rev={rev} />
        </div>
        {foot ? (
          <div className="deploy-foot">
            <RollBackFoot rev={rev} latest={latest} revisions={revisions} onRollBack={onRollBack} />
          </div>
        ) : null}
      </Panel>
    </div>
  );
}

/**
 * What a revision changed, each field's old value above its new one, so a long
 * value never breaks in the middle of a word. An image reads by what moved, and
 * its full name is the Image fact below.
 */
function RevisionDiff({ rev }: { rev: ServiceRevision }) {
  const { t } = useLingui();
  return (
    <ul className="deploy-diff">
      {rev.changes.map((change) => {
        const path = fieldName(change.path);
        const image =
          path === "image" && change.op === FieldOp.MODIFIED ? imageChange(change.from, change.to) : undefined;
        const from = image?.from ?? change.from;
        const to = image?.to ?? change.to;
        return (
          <li key={change.path} className="deploy-diff-row">
            <code className="deploy-diff-path">{path}</code>
            {change.sensitive ? (
              <span className="app-muted">{t`Changed, and hidden`}</span>
            ) : (
              <span className="deploy-diff-values">
                {change.op !== FieldOp.ADDED && from !== "" ? (
                  <del className="deploy-diff-from">
                    <code>{from}</code>
                  </del>
                ) : null}
                {change.op !== FieldOp.REMOVED && to !== "" ? (
                  <ins className="deploy-diff-to">
                    <code>{to}</code>
                  </ins>
                ) : null}
              </span>
            )}
          </li>
        );
      })}
    </ul>
  );
}

/** What a revision did and how it went. */
function RevisionBody({ rev }: { rev: ServiceRevision }) {
  const { t, i18n } = useLingui();
  const appWords = useAppWords();
  // What a moving tag really was, so it can be told apart from the same tag later.
  const digest = rev.deployments.find((d) => d.digest !== "")?.digest ?? "";
  const { fleet } = useFleetView();
  const change = rev.change;
  const words = useChangeWords();
  const who = useWhoWords();
  const time = useTimeWords();
  const failed = rev.deployments.find((d) => d.state === DeploymentState.FAILED);
  const via = change ? words.surface(change.appliedVia || change.proposedVia) : "";
  const proposedApart =
    change?.appliedBy && change.proposedBy && change.proposedBy.id !== change.appliedBy.id
      ? change.proposedBy
      : undefined;
  const duration = took(rev);
  const when = rev.createTime ? timestampDate(rev.createTime) : new Date(0);
  return (
    <div className="deploy-body">
      {change?.state === ChangeState.FAILED || failed ? (
        <Callout tone="critical" title={t`Why it failed`}>
          {tidyMessage(failed?.error || change?.error || "")}
          {change?.rolledBackBy ? (
            <>
              {" "}
              <Trans>Dockwell rolled back the steps already done.</Trans>
            </>
          ) : null}
        </Callout>
      ) : null}
      {rev.deleted ? null : (
        <div className="deploy-part">
          <h3 className="dw-text-label">
            <Trans>What changed</Trans>
          </h3>
          {rev.changes.length > 0 ? (
            <RevisionDiff rev={rev} />
          ) : (
            <p className="app-note">
              <Trans>Nothing in its spec changed. It moved, or came back as it was.</Trans>
            </p>
          )}
        </div>
      )}
      <dl className="app-facts">
        <dt>{t`Who`}</dt>
        <dd>{who.name(change) || t`Someone`}</dd>
        <dt>{t`From`}</dt>
        <dd>{via}</dd>
        {proposedApart ? (
          <>
            <dt>{t`Proposed by`}</dt>
            <dd>
              {proposedApart.name}
              <span className="app-muted"> · {words.surface(change?.proposedVia ?? Surface.UNSPECIFIED)}</span>
            </dd>
          </>
        ) : null}
        <dt>{t`When`}</dt>
        <dd>{formatDateTime(when, i18n.locale)}</dd>
        {duration !== undefined ? (
          <>
            <dt>{t`Took`}</dt>
            <dd>{time.bound(duration)}</dd>
          </>
        ) : null}
        {/* Dockwell's own rollback is said in the title; its plan's words are only for itself. */}
        {change?.summary && !change.rollback ? (
          <>
            <dt>{t`Plan`}</dt>
            <dd>{change.summary}</dd>
          </>
        ) : null}
        {rev.image ? (
          <>
            <dt>{t`Image`}</dt>
            <dd className="deploy-image">
              <WholeImage image={bareImage(rev.image)} />
              {digest ? (
                <span className="deploy-digest app-muted" title={digest}>
                  {" "}
                  {shortDigest(digest)}
                </span>
              ) : null}
            </dd>
          </>
        ) : null}
      </dl>
      {fleet && rev.deployments.length > 0 ? (
        <div className="deploy-part">
          <h3 className="dw-text-label">
            <Trans>Rollout</Trans>
          </h3>
          <dl className="deploy-rollout">
            {rev.deployments.map((d) => (
              <div key={d.serverId} className="deploy-rollout-row">
                <dt>
                  <code>{d.serverName}</code>
                </dt>
                <dd>
                  <StatusBadge
                    status={DEPLOYMENT_LOOKS[d.state] ?? "stopped"}
                    label={appWords.deployment(d.state)}
                    labelIsState
                  />
                </dd>
              </div>
            ))}
          </dl>
        </div>
      ) : null}
    </div>
  );
}
