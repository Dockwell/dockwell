// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { ChangeService, MemberService, type Resource, Role } from "@dockwell/api";
import { Button, Callout, Icon, type IconName, Panel, StatusBadge, Tag, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type FormEvent, type ReactNode, useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { imageOf, nameOf } from "../apps/app";
import { MayEdit } from "../auth/MayEdit";
import { useMay } from "../auth/may";
import { useRoleNames } from "../auth/user";
import { useChangeSheet } from "../changes/ChangeSheet";
import { Page } from "../shell/Page";
import { countKinds, useResourceWords } from "./contents";
import { useProjectDelete } from "./deleteProject";
import { type ProjectView, useProjects } from "./useProjects";
import { useContentsWords } from "./words";
import "../apps/Apps.css";
import "./Projects.css";

/**
 * One project: what's in it, its domains, its variables and secrets, its environments once it
 * has two, who may reach it, and its settings, with Delete (ADR 0074). What's in it says what a
 * project is, so the page explains nothing in a sentence.
 */
export function ProjectPage({ projectId }: { projectId: string }) {
  const { t } = useLingui();
  const { projects, error } = useProjects();
  const view = projects?.find((p) => p.id === projectId || p.name === projectId);
  const words = useContentsWords();
  const crumbs = <Crumbs label={view?.label} />;

  if (error && !projects) {
    return (
      <Page title={t`Project`} breadcrumb={crumbs}>
        <Callout tone="critical" title={t`Can't load this project`}>
          <Trans>What runs in it isn't affected.</Trans>
        </Callout>
      </Page>
    );
  }
  if (!projects) {
    return (
      <Page title={t`Project`} breadcrumb={crumbs}>
        <p className="projects-loading" aria-busy="true">
          <Trans>Loading the project</Trans>
        </p>
      </Page>
    );
  }
  if (!view) {
    return (
      <Page title={t`Project`} breadcrumb={crumbs}>
        <Callout
          tone="info"
          title={t`No project with this ID`}
          actions={
            <RouterButtonLink to="/projects">
              <Trans>Go to the projects</Trans>
            </RouterButtonLink>
          }
        >
          <Trans>It may have been deleted, or the link may be wrong.</Trans>
        </Callout>
      </Page>
    );
  }
  const parts = words(view);
  return (
    <Page
      title={view.label}
      breadcrumb={crumbs}
      back="/projects"
      badge={
        view.idle && !view.isDefault ? (
          <StatusBadge status="stopped" label={<Trans>Nothing running</Trans>} labelIsState />
        ) : undefined
      }
      lead={
        <span className="projects-lead">
          <code>{view.name}</code>
          {parts.length > 0 ? <span>{parts.join(" · ")}</span> : null}
        </span>
      }
      className="project-page"
    >
      {/* Two stacks, each as tall as its own panels, so neither waits on the other's rows: what's in it on the
          wide side, what it's set with on the narrow one. On a narrow screen they fold into one, Delete last. */}
      <div className="project-columns">
        <div className="project-column is-main">
          <Contents view={view} />
          <Domains view={view} />
          <Environments view={view} />
          <Danger view={view} count={projects.length} />
        </div>
        <div className="project-column is-side">
          <Values view={view} />
          <Access view={view} />
          <Settings view={view} />
        </div>
      </div>
    </Page>
  );
}

function Crumbs({ label }: { label?: string | undefined }) {
  const { t } = useLingui();
  return (
    <nav className="app-crumbs" aria-label={t`Breadcrumb`}>
      <Link to="/projects">
        <Trans>Projects</Trans>
      </Link>
      {label ? (
        <>
          <span aria-hidden="true">/</span>
          <span aria-current="page">{label}</span>
        </>
      ) : null}
    </nav>
  );
}

/** A row's mark, name and what it is. */
function RowBody({ icon, name, sub, link }: { icon: IconName; name: string; sub: ReactNode; link?: boolean }) {
  return (
    <>
      <span className="projects-mark" aria-hidden="true">
        <Icon name={icon} size={16} />
      </span>
      <span className="project-row-text">
        <span className="project-row-name">{name}</span>
        <span className="project-row-sub">{sub}</span>
      </span>
      {link ? <Icon name="chevron-right" size={16} className="project-row-chevron" /> : null}
    </>
  );
}

/** A row with nowhere to go: what's in the project with no page of its own, such as a volume. */
function Row({ icon, name, sub }: { icon: IconName; name: string; sub: ReactNode }) {
  return (
    <li className="project-row">
      <span className="project-row-link">
        <RowBody icon={icon} name={name} sub={sub} />
      </span>
    </li>
  );
}

function resourceName(r: Resource): string {
  return r.resource?.name ?? r.id;
}

/** What runs in the project, and what it keeps: its website, apps, databases, volumes and tasks. */
function Contents({ view }: { view: ProjectView }) {
  const { t } = useLingui();
  const volumes = view.inside.filter((r) => r.resource?.kind === "volume");
  const tasks = view.inside.filter((r) => r.resource?.kind === "task");
  const rows: ReactNode[] = [];
  if (view.website) {
    const site = view.website;
    rows.push(
      <li key={site.id} className="project-row">
        <Link to="/websites/$websiteId" params={{ websiteId: site.id }} className="project-row-link">
          <RowBody icon="browser" name={site.domain || site.name} sub={t`Website`} link />
        </Link>
      </li>,
    );
  }
  for (const app of view.apps) {
    rows.push(
      <li key={app.id} className="project-row">
        <Link to="/apps/$serviceId" params={{ serviceId: app.id }} className="project-row-link">
          <RowBody icon="service" name={nameOf(app)} sub={<code>{imageOf(app)}</code>} link />
        </Link>
      </li>,
    );
  }
  for (const db of view.databases) {
    rows.push(
      <li key={db.id} className="project-row">
        <Link to="/databases/$serviceId" params={{ serviceId: db.id }} className="project-row-link">
          <RowBody icon="database" name={nameOf(db)} sub={<code>{imageOf(db)}</code>} link />
        </Link>
      </li>,
    );
  }
  for (const volume of volumes) {
    rows.push(<Row key={volume.id} icon="archive" name={resourceName(volume)} sub={t`Volume`} />);
  }
  for (const task of tasks) {
    rows.push(
      <li key={task.id} className="project-row">
        <Link to="/scheduled-tasks" className="project-row-link">
          <RowBody icon="calendar" name={resourceName(task)} sub={t`Scheduled task`} link />
        </Link>
      </li>,
    );
  }
  return (
    <Panel title={t`What's in it`} className="project-contents" flush>
      {rows.length > 0 ? (
        <ul className="project-rows">{rows}</ul>
      ) : (
        <p className="project-empty">
          <Trans>Nothing runs in it, and it keeps no data.</Trans>
        </p>
      )}
    </Panel>
  );
}

/** The domains its routes answer, each with what it sends to. */
function Domains({ view }: { view: ProjectView }) {
  const { t } = useLingui();
  if (view.domains.length === 0) return null;
  return (
    <Panel title={t`Domains`} className="project-domains" flush>
      <ul className="project-rows">
        {view.domains.map((domain) => {
          const to = [...new Set(domain.routes.map((r) => r.service).filter(Boolean))].join(", ");
          return (
            <li key={domain.name} className="project-row">
              <Link to="/domains" className="project-row-link">
                <RowBody icon="link" name={domain.name} sub={to ? t`To ${to}` : t`To a port on the server`} link />
              </Link>
            </li>
          );
        })}
      </ul>
    </Panel>
  );
}

/** Its variables and secrets, by count, for each environment, a click from the Variables page. */
function Values({ view }: { view: ProjectView }) {
  const { t } = useLingui();
  const several = view.environments.length > 1;
  return (
    <Panel title={t`Variables and secrets`} className="project-values" flush>
      <ul className="project-rows">
        {view.environments.map((env) => {
          const kinds = countKinds(view.inside.filter((r) => r.resource?.scope === env.id));
          const counts = [
            plural(kinds.get("variable") ?? 0, { one: "# variable", other: "# variables" }),
            plural(kinds.get("secret") ?? 0, { one: "# secret", other: "# secrets" }),
          ].join(" · ");
          const envName = env.displayName || env.resource?.name || env.id;
          return (
            <li key={env.id} className="project-row">
              <Link to="/variables" search={{ environment: env.id }} className="project-row-link">
                <RowBody
                  icon="key"
                  name={several ? envName : counts}
                  sub={several ? counts : t`Open in Variables`}
                  link
                />
              </Link>
            </li>
          );
        })}
      </ul>
    </Panel>
  );
}

/** Its environments, once it has two: until then production is the only one, and says nothing. */
function Environments({ view }: { view: ProjectView }) {
  const { t } = useLingui();
  if (view.environments.length < 2) return null;
  return (
    <Panel title={t`Environments`} className="project-environments" flush>
      <ul className="project-rows">
        {view.environments.map((env) => (
          <Row
            key={env.id}
            icon="layers"
            name={env.displayName || env.resource?.name || env.id}
            sub={env.protected ? <Tag words>{t`Protected`}</Tag> : t`Not protected`}
          />
        ))}
      </ul>
    </Panel>
  );
}

const RANK: Partial<Record<Role, number>> = {
  [Role.VIEWER]: 1,
  [Role.DEVELOPER]: 2,
  [Role.ADMIN]: 3,
  [Role.OWNER]: 4,
};
const rank = (role: Role) => RANK[role] ?? 0;

/**
 * Who may reach the project, for an admin, once there's someone besides them (ADR 0049):
 * everyone with a role on the whole install, and anyone with one on this project.
 */
function Access({ view }: { view: ProjectView }) {
  const { t } = useLingui();
  const roleName = useRoleNames();
  const may = useMay();
  const allowed = may(MemberService.method.listMembers).allowed;
  const members = useQuery(MemberService.method.listMembers, {}, { enabled: allowed }).data?.members;
  if (!allowed || !members || members.length < 2) return null;
  const here = members.flatMap((m) => {
    const grants = m.grants.filter((g) => g.projectId === "" || g.projectId === view.id);
    const best = grants.reduce<(typeof grants)[number] | undefined>(
      (a, b) => (!a || rank(b.role) > rank(a.role) ? b : a),
      undefined,
    );
    return best ? [{ member: m, role: best.role, install: best.projectId === "" }] : [];
  });
  return (
    <Panel
      title={t`Who has access`}
      className="project-access"
      flush
      actions={
        <RouterButtonLink to="/settings/members" size="sm" variant="ghost">
          <Trans>Members</Trans>
        </RouterButtonLink>
      }
    >
      <ul className="project-rows">
        {here.map(({ member, role, install }) => (
          <Row
            key={member.user?.id ?? member.user?.email}
            icon="user"
            name={member.user?.name || member.user?.email || ""}
            sub={install ? t`${roleName(role)} of the whole install` : t`${roleName(role)} of this project`}
          />
        ))}
      </ul>
    </Panel>
  );
}

/** Its display name, as a plan. */
function Settings({ view }: { view: ProjectView }) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const may = useMay();
  const [name, setName] = useState(view.project.displayName);
  const shown = name.trim();
  const changed = shown !== view.project.displayName;
  const rename = (event: FormEvent) => {
    event.preventDefault();
    if (!changed) return;
    sheet.open({
      kind: "propose",
      summary: shown ? t`Show ${view.name} as ${shown}` : t`Show ${view.name} by its name`,
      // The engine keeps what it was installed from and what it serves (ADR 0074).
      edits: [
        {
          resource: { kind: "project", name: view.name },
          spec: { kind: { case: "project", value: { displayName: shown } } },
        },
      ],
    });
  };
  return (
    <Panel title={t`Settings`} className="project-settings">
      {/* A project is an admin's to change (ADR 0049); for anyone else the form is there to read, with why. */}
      <MayEdit offer={may(ChangeService.method.proposeChange, { install: true })}>
        <form className="project-rename" onSubmit={rename} aria-describedby="project-rename-hint">
          <TextField
            label={t`Display name`}
            placeholder={view.name}
            value={name}
            maxLength={63}
            onChange={(event) => setName(event.target.value)}
          />
          <Button type="submit" disabled={!changed}>
            <Trans>Review plan</Trans>
          </Button>
        </form>
        <p className="project-rename-hint" id="project-rename-hint">
          <Trans>URLs and the CLI keep its name, {view.name}.</Trans>
        </p>
      </MayEdit>
    </Panel>
  );
}

/** Delete, with everything in it, in one plan. */
function Danger({ view, count }: { view: ProjectView; count: number }) {
  const { t } = useLingui();
  const words = useResourceWords();
  const remove = useProjectDelete(count);
  const offer = remove.offer(view);
  // What the plan takes, in the words an app's danger zone uses: "1 app, 1 secret and 1 volume".
  const holds = view.inside.length > 0 ? words(view.inside) : "";
  return (
    <Panel title={t`Danger zone`} className="app-danger project-danger">
      <div className="app-danger-row">
        <div className="app-danger-text">
          <span className="app-danger-title">{t`Delete the project ${view.label}`}</span>
          <span className="app-danger-what">
            {holds ? (
              <Trans>Takes {holds} in one plan, to review first.</Trans>
            ) : (
              <Trans>It holds nothing. You review a plan first.</Trans>
            )}
          </span>
        </div>
        <Button variant="danger" icon="trash" onClick={() => remove.open(view)} off={offer.reason || undefined}>
          {t`Delete the project`}
        </Button>
      </div>
    </Panel>
  );
}
