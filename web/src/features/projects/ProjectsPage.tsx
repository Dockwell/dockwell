// SPDX-License-Identifier: AGPL-3.0-only

import {
  Button,
  Callout,
  Icon,
  type IconName,
  Panel,
  PHONE_QUERY,
  SegmentedControl,
  StatusBadge,
  Table,
  type TableColumn,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { Page } from "../shell/Page";
import { useProjectDelete } from "./deleteProject";
import { type ProjectView, useProjects } from "./useProjects";
import { useContentsWords } from "./words";
import "./Projects.css";

/** Which projects the list shows. */
export type ProjectsFilter = "all" | "idle";

export interface ProjectsSearch {
  show?: ProjectsFilter;
}

/** From how many projects the list has a filter box, as Apps has. */
const FILTER_FROM = 6;

/**
 * Every project, each with what's in it, so what a project is shows by structure rather than
 * by a sentence (ADR 0074). A project with nothing running is marked, with Delete beside it,
 * so what deleted apps left behind is two clicks from gone.
 */
export function ProjectsPage({ search, onSearch }: { search: ProjectsSearch; onSearch: (s: ProjectsSearch) => void }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const { projects, error, refetch, isFetching } = useProjects();
  const [query, setQuery] = useState("");
  const idle = (projects ?? []).filter((p) => p.idle && !p.isDefault);
  const show: ProjectsFilter = search.show === "idle" && idle.length > 0 ? "idle" : "all";
  const shown = useMemo(() => {
    const q = query.trim().toLowerCase();
    return (projects ?? []).filter(
      (p) =>
        (show === "all" || (p.idle && !p.isDefault)) &&
        (q === "" || p.label.toLowerCase().includes(q) || p.name.includes(q)),
    );
  }, [projects, show, query]);
  const count = projects?.length ?? 0;
  const lead = [
    plural(count, { one: "# project", other: "# projects" }),
    ...(idle.length > 0
      ? [plural(idle.length, { one: "# with nothing running", other: "# with nothing running" })]
      : []),
  ].join(" · ");

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
    body = (
      <p className="projects-loading" aria-busy="true">
        <Trans>Loading projects</Trans>
      </p>
    );
  } else if (phone) {
    body = <PhoneList rows={shown} count={count} />;
  } else {
    body = <ProjectsTable rows={shown} count={count} />;
  }

  return (
    <Page
      title={t`Projects`}
      back="/more"
      lead={projects ? lead : <span aria-hidden="true">&nbsp;</span>}
      className="projects-page"
    >
      <section className="projects-section" aria-label={t`Projects`}>
        {projects && (idle.length > 0 || count >= FILTER_FROM) ? (
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
                onChange={(value) => onSearch(value === "idle" ? { show: "idle" } : {})}
              />
            ) : null}
            {count >= FILTER_FROM ? (
              <TextField
                className="projects-filter"
                aria-label={t`Filter projects`}
                placeholder={t`Filter by name`}
                icon="search"
                type="search"
                size={phone ? "lg" : "sm"}
                value={query}
                onChange={(event) => setQuery(event.target.value)}
              />
            ) : null}
          </div>
        ) : null}
        {body}
      </section>
    </Page>
  );
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

function ProjectsTable({ rows, count }: { rows: readonly ProjectView[]; count: number }) {
  const { t } = useLingui();
  const navigate = useNavigate();
  const columns = useMemo<TableColumn<ProjectView>[]>(
    () => [
      {
        key: "project",
        header: t`Project`,
        width: "34%",
        render: (view) => (
          <span className="projects-name-cell">
            <span className="projects-mark" aria-hidden="true">
              <Icon name={iconOf(view)} size={16} />
            </span>
            <span className="dw-cell">
              <Link to="/projects/$projectId" params={{ projectId: view.id }} className="dw-cell-title" tabIndex={-1}>
                {view.label}
              </Link>
              <Sub view={view} />
            </span>
          </span>
        ),
      },
      { key: "contents", header: t`What's in it`, render: (view) => <Contents view={view} /> },
      {
        key: "actions",
        header: <span className="dw-sr">{t`Actions`}</span>,
        width: 112,
        align: "right",
        render: (view) => <DeleteLeftover view={view} count={count} />,
      },
    ],
    [t, count],
  );
  return (
    <Table<ProjectView>
      className="projects-table"
      columns={columns}
      rows={rows}
      rowKey="id"
      caption={t`Projects`}
      onRowClick={(view) => void navigate({ to: "/projects/$projectId", params: { projectId: view.id } })}
      empty={<Trans>No project matches.</Trans>}
    />
  );
}

/** The list on a phone: each project's name and what's in it, with Delete for a leftover. */
function PhoneList({ rows, count }: { rows: readonly ProjectView[]; count: number }) {
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
        </div>
      ))}
    </Panel>
  );
}
