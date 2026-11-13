// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ActorKind,
  BuildState,
  ChangeState,
  DeploymentState,
  FieldOp,
  IntentKind,
  type ServiceRevision,
  ServiceService,
  Surface,
} from "@dockwell/api";
import {
  Button,
  cx,
  Icon,
  type IconName,
  Panel,
  PHONE_QUERY,
  type PlanValue,
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
import { isValidElement, type ReactNode, useCallback, useEffect, useMemo, useRef } from "react";

import { formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { FailureNotice } from "../changes/FailureNotice";
import { useFieldRows } from "../changes/fields";
import { failedStep, intentOf, wentOffline } from "../changes/plan";
import { RollbackLine, rollbackSubject } from "../changes/RollbackLine";
import { useChangeWords } from "../changes/words";
import { DeployLogLink } from "../logs/DeployLogLinks";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { ReadFailed } from "../shell/ReadFailed";
import { RefLink } from "../shell/RefLink";
import { useApp, useRevisions } from "./AppContext";
import { ListSkeleton } from "./AppsPage";
import {
  bareImage,
  builtCommit,
  canRollBackTo,
  environmentOf,
  fieldOfChange,
  imageChange,
  nameOf,
  revisionName,
  shortDigest,
  shortImage,
  shortImages,
} from "./app";
import { BuildPart, buildUnderWay, shortCommit } from "./Build";
import { ImageName, WholeImage } from "./ImageName";
import { type Look, plansBuild, useRevisionLook, useRevisionTitle, useWhoWords } from "./revision";
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

/** Where a revision stands: with a state where its record has one, and otherwise only that it deployed. */
function RevisionLook({ look, className }: { look: Look; className?: string }) {
  if (!look.status) return <span className="app-muted">{look.word}</span>;
  return (
    <StatusBadge
      status={look.status}
      label={look.word}
      labelIsState
      title={look.word}
      {...(className ? { className } : {})}
    />
  );
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
  const builds = service.spec?.build !== undefined;
  // A change that builds tells of its build once its server begins it, and the build tells of its
  // commit, its steps and how it ends as it goes: until it ends, the list reads again, so each row
  // says it, the open one's live log aside.
  const waitingForBuild = (revisions ?? []).some(
    (r) => (!r.build && r.change?.state === ChangeState.APPLYING && plansBuild(r.change)) || buildUnderWay(r.build),
  );
  useEffect(() => {
    if (!waitingForBuild) return;
    const again = setInterval(() => void history.refetch(), 2000);
    return () => clearInterval(again);
  }, [waitingForBuild, history.refetch]);
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
          return <RevisionLook look={look(rev, latest, state)} className="apps-status" />;
        },
      },
      {
        key: "what",
        header: builds ? t`Commit` : t`What changed`,
        render: (rev) => {
          const title = revisionTitle(rev, revisions);
          const built = rev.build;
          if (built?.commitSubject) {
            // A build reads as its commit, as a list of pushes does (App-Deployments).
            return (
              <span className="dw-cell">
                <span className="dw-cell-title deploy-clip" title={built.commitSubject}>
                  {built.commitSubject}
                </span>
                <span className="deploy-what">
                  <code>{shortCommit(built.commit)}</code>
                  {built.commitAuthor ? <span className="app-muted"> · {built.commitAuthor}</span> : null}
                </span>
              </span>
            );
          }
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
          // "the CLI on hz-1" says where already, so it isn't said again.
          const via =
            rev.change && !who.saysWhere(rev.change) ? words.via(rev.change.appliedVia || rev.change.proposedVia) : "";
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
  }, [t, i18n, latest, look, state, revisionTitle, revisions, words, who, time, now, roomy, builds]);

  const newest = revisions?.[0] ? revisionName(revisions[0].number) : undefined;
  const lastShown = revisions?.[revisions.length - 1];
  const oldest = lastShown ? revisionName(lastShown.number) : undefined;
  const rows = useMemo(() => (revisions ?? []).map((rev) => ({ ...rev, id: rev.number.toString() })), [revisions]);

  if (history.error && !revisions) {
    return (
      <ReadFailed
        error={history.error}
        title={t`Can't load the deployments`}
        actions={
          <Button onClick={() => void history.refetch()} loading={history.isFetching}>
            <Trans>Try again</Trans>
          </Button>
        }
      />
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
              const when = rev.createTime ? timestampDate(rev.createTime) : new Date(0);
              return (
                <li key={rev.number.toString()}>
                  <button type="button" className="deploy-phone-row" onClick={() => select(rev)}>
                    <span className="deploy-phone-top">
                      <code className="deploy-number">{revisionName(rev.number)}</code>
                      <RevisionLook look={look(rev, latest, state)} />
                      <span className="deploy-phone-time">{time.ago(when, now)}</span>
                    </span>
                    <span className="deploy-phone-summary">{revisionTitle(rev, revisions).node}</span>
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
          onBuildEnded={() => void history.refetch()}
        />
      ) : null}
      {selected && phone ? (
        <Sheet
          open
          title={revisionName(selected.number)}
          subtitle={revisionTitle(selected, revisions).node}
          onClose={() => select(undefined)}
          className="app-sheet is-phone"
          footer={
            <RollBackFoot rev={selected} latest={latest} revisions={revisions ?? []} onRollBack={rollBack} wide />
          }
        >
          <RevisionBody rev={selected} onBuildEnded={() => void history.refetch()} />
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
              : fieldOfChange(first.path) === "image"
                ? sameBuild(first.from, first.to)
                  ? t`${shortImage(first.to)}, built again`
                  : shortImages(first.from, first.to)
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
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const { service } = useApp();
  const mayRollBack = useMay()(ServiceService.method.rollBackService, { environment: environmentOf(service) });
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
            {...mayRollBack.off}
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
        {...mayRollBack.off}
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
  onBuildEnded,
}: {
  rev: ServiceRevision;
  latest: bigint;
  look: Look;
  revisions: readonly ServiceRevision[];
  onRollBack: (rev: ServiceRevision) => void;
  onBuildEnded: () => void;
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
            <RevisionLook look={look} />
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
          <RevisionBody rev={rev} onBuildEnded={onBuildEnded} />
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

/** A secret's value, which is never shown. */
function isHidden(value: PlanValue | undefined): boolean {
  return typeof value === "object" && value !== null && "secret" in value;
}

/** A value as a plan reads it: a machine value on its own, or words with machine values among them. */
function DiffValue({ value }: { value: PlanValue | undefined }) {
  return isValidElement(value) ? value : <code>{value as ReactNode}</code>;
}

/** Two images Dockwell built of one commit, as a deploy that builds the same commit again makes. */
function sameBuild(from: string, to: string): boolean {
  const commit = builtCommit(from);
  return commit !== undefined && commit === builtCommit(to);
}

/**
 * What a revision changed, in the plan's words: each field's old value, then
 * the new, read across as App-Deployments has them. Each value stays whole,
 * and the new one wraps under the old rather than break in the middle of a
 * word. An image reads by what moved, and its full name is the Image fact below.
 */
function RevisionDiff({ rev }: { rev: ServiceRevision }) {
  const { t } = useLingui();
  const rows = useFieldRows();
  return (
    <dl className="deploy-diff">
      {rows({ kind: "service", changes: rev.changes }).map((row) => {
        const image =
          row.path === "image" && typeof row.from === "string" && typeof row.to === "string"
            ? imageChange(row.from, row.to)
            : undefined;
        // The same commit built again moves the image's name, and nothing else. The
        // rows say images as people read them, so the change itself says which.
        const raw = row.path === "image" ? rev.changes.find((c) => fieldOfChange(c.path) === "image") : undefined;
        const again = image !== undefined && raw !== undefined && sameBuild(raw.from, raw.to);
        const from = again ? undefined : (image?.from ?? row.from);
        const to = again ? t`${image?.to ?? ""}, built again` : (image?.to ?? row.to);
        const secret = isHidden(from) || isHidden(to);
        return (
          <div key={row.path} className="deploy-diff-row">
            <dt className="deploy-diff-path">{row.label ?? row.path}</dt>
            <dd className="deploy-diff-values">
              {from !== undefined && from !== "" && !secret ? (
                <del className={cx("deploy-diff-from", isValidElement(from) && "is-words")}>
                  <DiffValue value={from} />
                </del>
              ) : null}
              {from !== undefined && from !== "" && to !== undefined && to !== "" && !secret ? (
                <Icon name="arrow-right" size={12} className="deploy-diff-arrow" />
              ) : null}
              {to !== undefined && to !== "" && !secret ? (
                row.tone === "change" ? (
                  <code>{to as ReactNode}</code>
                ) : (
                  <ins className={cx("deploy-diff-to", isValidElement(to) && "is-words")}>
                    <DiffValue value={to} />
                  </ins>
                )
              ) : null}
              {secret || row.hidden ? <span className="app-muted">{t`Changed, and hidden`}</span> : null}
            </dd>
          </div>
        );
      })}
    </dl>
  );
}

/** What a revision did and how it went. */
function RevisionBody({ rev, onBuildEnded }: { rev: ServiceRevision; onBuildEnded: () => void }) {
  const { t, i18n } = useLingui();
  const { service } = useApp();
  const appName = nameOf(service);
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
  // A deploy no one named has the summary the server wrote, in English: said here
  // in the reader's words. One someone wrote stays as they wrote it.
  const planWords = (c: NonNullable<typeof change>): string => {
    const intent = intentOf(c.plan);
    const name = appName;
    if (intent?.kind === IntentKind.DEPLOY) {
      const what = intent.commit ? intent.commit.slice(0, 7) : intent.image;
      if (c.summary === `Deploy ${what} to ${name}`) {
        return words.intent({ ...intent, image: shortImage(bareImage(intent.image)) }, "plan");
      }
    }
    return c.summary;
  };
  const when = rev.createTime ? timestampDate(rev.createTime) : new Date(0);
  return (
    <div className="deploy-body">
      {change?.state === ChangeState.FAILED ? (
        <RollbackLine
          change={change}
          subject={rollbackSubject(change.plan, { id: service.id, name: appName, changes: rev.changes })}
        />
      ) : null}
      {(change?.state === ChangeState.FAILED || failed) &&
      !wentOffline(change) &&
      rev.build?.state !== BuildState.CANCELLED ? (
        <FailureNotice message={failed?.error || change?.error || ""} detail={failedStep(change)?.detail} />
      ) : null}
      {rev.deleted || wentOffline(change) ? null : (
        <DeployLogLink serviceId={service.id} change={change?.id} revision={rev.number} />
      )}
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
      {rev.build ? (
        <BuildPart build={rev.build} onEnded={onBuildEnded} />
      ) : change?.state === ChangeState.APPLYING && plansBuild(change) ? (
        <div className="deploy-part build-part">
          <div className="build-head">
            <h3 className="dw-text-label">
              <Trans>Build</Trans>
            </h3>
            <StatusBadge status="unknown" label={t`Queued`} labelIsState />
          </div>
          <p className="app-note">
            <Trans>Waiting for the server to begin the build.</Trans>
          </p>
        </div>
      ) : null}
      <dl className="app-facts">
        <dt>{t`Who`}</dt>
        <dd>{who.name(change) || t`Someone`}</dd>
        {who.saysWhere(change) ? null : (
          <>
            <dt>{t`From`}</dt>
            <dd>{via}</dd>
          </>
        )}
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
            <dd>{planWords(change)}</dd>
          </>
        ) : null}
        {rev.image ? (
          <>
            <dt>{t`Image`}</dt>
            <dd className="deploy-image">
              {/* An image Dockwell built says which commit it is; its name is in the title. */}
              {builtCommit(rev.image) ? <ImageName image={rev.image} /> : <WholeImage image={bareImage(rev.image)} />}
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
                  <code>
                    <RefLink kind="server" id={d.serverId} look="plain">
                      {d.serverName}
                    </RefLink>
                  </code>
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
