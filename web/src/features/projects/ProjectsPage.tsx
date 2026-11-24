// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService } from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  type IconName,
  Loading,
  Panel,
  PHONE_QUERY,
  SegmentedControl,
  SkeletonRows,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import {
  ListPager,
  type ListPlace,
  ListSearchField,
  ListSortField,
  SortHeader,
  useListPlace,
  useRowMenu,
} from "../shell/ListTools";
import { type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { Page } from "../shell/Page";
import { useProjectDelete } from "./deleteProject";
import { NewProjectSheet, RenameProjectSheet } from "./ProjectSheets";
import { type ProjectView, useProjects } from "./useProjects";
import { useContentsWords } from "./words";
import "./Projects.css";

/** Which projects the list shows. */
export type ProjectsFilter = "all" | "idle";

export interface ProjectsSearch extends ListSearch {
  show?: ProjectsFilter;
}

/** The columns the list sorts by, as its address names them. */
const SORT_KEYS = ["name", "contents"] as const;

/**
 * Every project, each with what's in it, so what a project is shows by structure rather than
 * by a sentence (ADR 0074). A project with nothing running is marked, with Delete beside it,
 * so what deleted apps left behind is two clicks from gone. New project makes an empty one
 * here, and a row's menu, on its button, a right-click or Shift+F10, opens, renames and
 * deletes it, each as its plan.
 */
export function ProjectsPage({ search, onSearch }: { search: ProjectsSearch; onSearch: (s: ProjectsSearch) => void }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const { projects, error, refetch, isFetching } = useProjects();
  const may = useMay();
  const place = useListPlace(search, onSearch, SORT_KEYS);
  const [creating, setCreating] = useState(false);
  const [renaming, setRenaming] = useState<ProjectView>();
  const idle = (projects ?? []).filter((p) => p.idle && !p.isDefault);
  const show: ProjectsFilter = search.show === "idle" && idle.length > 0 ? "idle" : "all";
  const shown = useMemo(() => {
    const q = place.query;
    const filtered = (projects ?? []).filter(
      (p) =>
        (show === "all" || (p.idle && !p.isDefault)) &&
        (q === "" || p.label.toLowerCase().includes(q) || p.name.includes(q)),
    );
    return sortedRows(filtered, place.sort, (p, key) => (key === "contents" ? p.inside.length : p.label));
  }, [projects, show, place.query, place.sort]);
  const page = pagedRows(shown, place.page);
  const count = projects?.length ?? 0;
  const actionsFor = useProjectActions(count, setRenaming);
  const lead = [
    plural(count, { one: "# project", other: "# projects" }),
    ...(idle.length > 0
      ? [plural(idle.length, { one: "# with nothing running", other: "# with nothing running" })]
      : []),
  ].join(" · ");
  // A project is an admin's to make (ADR 0049): for anyone else the button stays, off, with why.
  const makes = may(ChangeService.method.proposeChange, { install: true });
  const newProject = (size?: "lg") => (
    <Button variant="primary" icon="plus" {...(size ? { size } : {})} onClick={() => setCreating(true)} {...makes.off}>
      <Trans>New project</Trans>
    </Button>
  );
  const clearFilters = () => {
    place.clearQuery();
    if (show !== "all") onSearch({ ...search, q: undefined, page: undefined, show: undefined });
  };

  let body: ReactNode;
  if (error) {
    body = (
      <Callout
        tone="critical"
        title={t`Can't load the projects`}
        actions={
          <Button onClick={refetch} loading={isFetching}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        {sentence(error)} <Trans>What runs in them isn't affected.</Trans>
      </Callout>
    );
  } else if (!projects) {
    body = phone ? (
      <Loading label={t`Loading projects`}>
        <Panel flush>
          <SkeletonRows rows={3} lead="mark" />
        </Panel>
      </Loading>
    ) : (
      <Table
        className="projects-table"
        columns={[
          { key: "project", header: t`Project`, render: () => null, skeleton: "titled" },
          { key: "contents", header: t`What's in it`, render: () => null },
          { key: "actions", header: <span className="dw-sr">{t`Actions`}</span>, width: 160, render: () => null },
        ]}
        rows={[]}
        loading
        loadingLabel={t`Loading projects`}
      />
    );
  } else if (count === 0) {
    body = (
      <EmptyState icon="folder" title={t`No projects yet`} action={newProject("lg")}>
        <p>
          <Trans>Apps, and what only they use, live in a project.</Trans>
        </p>
      </EmptyState>
    );
  } else if (shown.length === 0) {
    body = (
      <EmptyState
        icon="search"
        title={t`Nothing matches`}
        action={
          <Button size="lg" onClick={clearFilters}>
            <Trans>Clear the filter</Trans>
          </Button>
        }
      >
        <p>
          {place.query ? (
            <Trans>
              No project here has <code>{place.typed.trim()}</code> in its name.
            </Trans>
          ) : (
            <Trans>Every project has something running.</Trans>
          )}
        </p>
      </EmptyState>
    );
  } else if (phone) {
    body = (
      <>
        <PhoneList rows={page.rows} count={count} actionsFor={actionsFor} />
        <ListPager page={page} onPage={place.setPage} />
      </>
    );
  } else {
    body = (
      <>
        <ProjectsTable rows={page.rows} count={count} place={place} actionsFor={actionsFor} />
        <ListPager page={page} onPage={place.setPage} />
      </>
    );
  }

  return (
    <Page
      title={t`Projects`}
      back="/more"
      lead={projects && count > 0 ? lead : <span aria-hidden="true">&nbsp;</span>}
      className="projects-page"
      actions={projects && count > 0 ? newProject() : null}
    >
      <section className="projects-section" aria-label={t`Projects`}>
        {phone && projects && count > 0 ? (
          <div className="projects-phone-head">
            <h2 className="dw-text-heading">{lead}</h2>
            {newProject()}
          </div>
        ) : null}
        {projects && count > 0 ? (
          <div className="projects-bar">
            {idle.length > 0 ? (
              <SegmentedControl
                label={t`Show`}
                size="sm"
                value={show}
                options={[
                  { value: "all", label: t`All ${count}` },
                  { value: "idle", label: t`Nothing running ${idle.length}` },
                ]}
                onChange={(value) =>
                  onSearch({ ...search, page: undefined, show: value === "idle" ? "idle" : undefined })
                }
              />
            ) : null}
            <ListSearchField
              className="projects-filter"
              label={t`Filter projects`}
              placeholder={t`Filter by name`}
              size={phone ? "lg" : "sm"}
              value={place.typed}
              onChange={place.setTyped}
            />
            {phone ? <ListSortField place={place} columns={{ name: t`Project`, contents: t`What's in it` }} /> : null}
          </div>
        ) : null}
        {body}
      </section>
      {creating ? (
        <NewProjectSheet taken={new Set((projects ?? []).map((p) => p.name))} onClose={() => setCreating(false)} />
      ) : null}
      {renaming ? <RenameProjectSheet view={renaming} onClose={() => setRenaming(undefined)} /> : null}
    </Page>
  );
}

/** A project's menu: open it, rename it as a plan, or delete it as a plan, each only as the reader may. */
function useProjectActions(count: number, onRename: (view: ProjectView) => void): (view: ProjectView) => MenuAction[] {
  const { t } = useLingui();
  const navigate = useNavigate();
  const may = useMay();
  const remove = useProjectDelete(count);
  // A project is an admin's to change (ADR 0049), as its page's Settings says.
  const renames = may(ChangeService.method.proposeChange, { install: true });
  return (view) => {
    const offer = remove.offer(view);
    return [
      {
        id: "open",
        icon: "folder",
        label: t`Open`,
        onSelect: () => void navigate({ to: "/projects/$projectId", params: { projectId: view.id } }),
      },
      {
        id: "rename",
        icon: "edit",
        label: t`Rename`,
        ...(renames.allowed ? {} : { off: renames.reason || undefined, disabled: !renames.reason }),
        onSelect: () => onRename(view),
      },
      {
        id: "delete",
        icon: "trash",
        label: t`Delete`,
        danger: true,
        ...(offer.allowed ? {} : { off: offer.reason || undefined, disabled: !offer.reason }),
        onSelect: () => remove.open(view, () => {}),
      },
    ];
  };
}

/** A project's mark: what it came from, as an icon. */
function iconOf(view: ProjectView): IconName {
  if (view.website) return "browser";
  if (view.install) return "grid";
  if (view.isDefault) return "home";
  return "folder";
}

/** What a project came from, in a few words, where its mark and name don't say it already. */
function useSourceWords(): (view: ProjectView) => string {
  const { t } = useLingui();
  return (view) => {
    if (view.isDefault) return t`Apps with no project`;
    if (view.website) return t`Website`;
    if (view.install) return t`From the catalog · ${view.install.name || view.install.pin?.entry || ""}`;
    return "";
  };
}

/** Under a project's name: its name as URLs have it, when it shows another, and where it came from. */
function Sub({ view }: { view: ProjectView }) {
  const source = useSourceWords()(view);
  const named = view.label !== view.name;
  if (!named && !source) return null;
  return (
    <span className="projects-sub">
      {named ? <code>{view.name}</code> : null}
      {named && source ? <span aria-hidden="true">·</span> : null}
      {source ? <span>{source}</span> : null}
    </span>
  );
}

/** What's in a project, on one line, with "Nothing running" first for one that runs nothing. */
function Contents({ view }: { view: ProjectView }) {
  const words = useContentsWords();
  const parts = words(view);
  return (
    <span className="projects-contents">
      {view.idle && !view.isDefault ? (
        <StatusBadge status="stopped" label={<Trans>Nothing running</Trans>} labelIsState />
      ) : null}
      <span className="projects-contents-words">
        {parts.length > 0 ? parts.join(" · ") : <Trans>Nothing in it</Trans>}
      </span>
    </span>
  );
}

/** Delete, beside a project with nothing running, so a leftover goes in two clicks. */
function DeleteLeftover({ view, count }: { view: ProjectView; count: number }) {
  const { t } = useLingui();
  const remove = useProjectDelete(count);
  if (!view.idle || view.isDefault) return null;
  const offer = remove.offer(view);
  return (
    <Button
      size="sm"
      variant="ghost"
      icon="trash"
      className="projects-delete"
      onClick={(event) => {
        event.stopPropagation();
        remove.open(view, () => {});
      }}
      aria-label={t`Delete the project ${view.label}`}
      off={offer.reason || undefined}
    >
      <Trans>Delete</Trans>
    </Button>
  );
}

function ProjectsTable({
  rows,
  count,
  place,
  actionsFor,
}: {
  rows: readonly ProjectView[];
  count: number;
  place: ListPlace;
  actionsFor: (view: ProjectView) => MenuAction[];
}) {
  const { t } = useLingui();
  const navigate = useNavigate();
  const menu = useRowMenu<ProjectView>();
  const columns = useMemo<TableColumn<ProjectView>[]>(
    () => [
      {
        key: "project",
        header: <SortHeader label={t`Project`} sortKey="name" place={place} />,
        sort: place.sortOf("name"),
        width: "34%",
        render: (view) => (
          <span className="projects-name-cell">
            <span className="projects-mark" aria-hidden="true">
              <Icon name={iconOf(view)} size={16} />
            </span>
            <span className="dw-cell">
              <Link
                to="/projects/$projectId"
                params={{ projectId: view.id }}
                className="dw-cell-title projects-name"
                tabIndex={-1}
              >
                {view.label}
              </Link>
              <Sub view={view} />
            </span>
          </span>
        ),
      },
      {
        key: "contents",
        header: <SortHeader label={t`What's in it`} sortKey="contents" place={place} />,
        sort: place.sortOf("contents"),
        render: (view) => <Contents view={view} />,
      },
      {
        key: "actions",
        header: <span className="dw-sr">{t`Actions`}</span>,
        width: 160,
        align: "right",
        render: (view) => (
          <span className="projects-row-actions">
            <DeleteLeftover view={view} count={count} />
            <ActionMenu label={t`Actions for ${view.label}`} actions={actionsFor(view)} />
          </span>
        ),
      },
    ],
    [t, count, place, actionsFor],
  );
  return (
    <>
      <Table<ProjectView>
        className="projects-table"
        columns={columns}
        rows={rows}
        rowKey="id"
        caption={t`Projects`}
        onRowClick={(view) => void navigate({ to: "/projects/$projectId", params: { projectId: view.id } })}
        onRowContextMenu={(view, at, el) =>
          menu.open(view, at, el, { label: t`Actions for ${view.label}`, actions: actionsFor(view) })
        }
        empty={<Trans>No project matches.</Trans>}
      />
      {menu.element}
    </>
  );
}

/** The list on a phone: each project's name and what's in it, with Delete for a leftover and its menu. */
function PhoneList({
  rows,
  count,
  actionsFor,
}: {
  rows: readonly ProjectView[];
  count: number;
  actionsFor: (view: ProjectView) => MenuAction[];
}) {
  const { t } = useLingui();
  return (
    <Panel flush className="projects-phone">
      {rows.map((view) => (
        <div key={view.id} className="projects-phone-row">
          <Link to="/projects/$projectId" params={{ projectId: view.id }} className="projects-phone-link">
            <span className="projects-mark" aria-hidden="true">
              <Icon name={iconOf(view)} size={16} />
            </span>
            <span className="projects-phone-text">
              <span className="projects-phone-name">{view.label}</span>
              <Sub view={view} />
              <Contents view={view} />
            </span>
          </Link>
          <DeleteLeftover view={view} count={count} />
          <ActionMenu label={t`Actions for ${view.label}`} actions={actionsFor(view)} />
        </div>
      ))}
    </Panel>
  );
}
