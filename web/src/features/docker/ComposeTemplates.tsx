// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  ChangeState,
  ComposeService,
  type ComposeTemplate,
  type ComposeTemplateInput,
  ComposeTemplateInputKind,
  type ComposeTemplateRevision,
  type ComposeTemplateSummary,
  type ComposeTemplateValue,
  Connection,
  DockerService,
  type GetComposeTemplateResponse,
  type PrepareComposeTemplateRequestSchema,
  type PrepareComposeTemplateResponse,
  ResourceService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  CodeField,
  Icon,
  Loading,
  PaginationBar,
  Panel,
  SelectField,
  Sheet,
  SkeletonCard,
  SkeletonFacts,
  SkeletonField,
  Table,
  type TableColumn,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { keepPreviousData, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";

import "./Compose.css";
import "./ComposeTemplates.css";

const route = "/apps/templates";

function useTemplateProjects() {
  const projects = useQuery(ResourceService.method.listResources, { kind: "project" });
  return {
    pending: projects.isPending,
    options: (projects.data?.resources ?? []).map((p) => ({
      value: p.id,
      label: p.displayName || p.resource?.name || p.id,
    })),
  };
}

function TemplateRefusal({ message, line }: { message: string; line?: number | undefined }) {
  return (
    <Callout tone="critical" title={line ? <Trans>Line {line}</Trans> : <Trans>Check the Compose file</Trans>}>
      {message}
    </Callout>
  );
}

function Inputs({ inputs }: { inputs: readonly ComposeTemplateInput[] }) {
  return (
    <Panel title={<Trans>Variables</Trans>}>
      {inputs.length === 0 ? (
        <p className="template-muted">
          <Trans>No variables</Trans>
        </p>
      ) : (
        <ul className="template-inputs">
          {inputs.map((item) => (
            <li key={item.name}>
              <code>{item.name}</code>
              <Tag words>
                {item.kind === ComposeTemplateInputKind.SECRET ? (
                  <Trans>Secret</Trans>
                ) : item.required ? (
                  <Trans>Required</Trans>
                ) : (
                  <Trans>Default</Trans>
                )}
              </Tag>
              {item.defaultValue !== undefined ? <span>{item.defaultValue}</span> : null}
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

export function ComposeTemplates({
  projectId,
  templateId,
}: {
  projectId?: string | undefined;
  templateId?: string | undefined;
}) {
  const { t, i18n } = useLingui();
  const navigate = useNavigate();
  const may = useMay();
  const deleteTemplate = useDeleteTemplate();
  const projects = useTemplateProjects();
  const project =
    projectId || projects.options.find((p) => p.label === "default")?.value || projects.options[0]?.value || "";
  const [query, setQuery] = useState("");
  const [pages, setPages] = useState([""]);
  const [edit, setEdit] = useState<"new" | "edit" | "duplicate">();
  const [using, setUsing] = useState<ComposeTemplateSummary>();
  const [revision, setRevision] = useState(0n);
  const [historyPage, setHistoryPage] = useState("");
  const canRead = project !== "" && may(ComposeService.method.listComposeTemplates, { project }).allowed;
  const list = useQuery(
    ComposeService.method.listComposeTemplates,
    { projectId: project, query, pageSize: 20, pageToken: pages.at(-1) },
    { enabled: canRead && !templateId, placeholderData: keepPreviousData },
  );
  const detail = useQuery(
    ComposeService.method.getComposeTemplate,
    { id: templateId ?? "", revision, historyPageSize: 10, historyPageToken: historyPage },
    { enabled: canRead && !!templateId, placeholderData: keepPreviousData },
  );
  const rows = list.data?.templates ?? [];
  const selected = detail.data?.template?.id === templateId ? detail.data?.template : undefined;
  const selectedSpec = selected ? detail.data?.spec : undefined;
  const activeProject = selected?.projectId ?? project;
  const canEdit =
    activeProject !== "" && may(ComposeService.method.prepareComposeTemplate, { project: activeProject }).allowed;
  const phone = useMediaQuery("(max-width: 640px)");
  const projectName = projects.options.find((p) => p.value === activeProject)?.label;
  const linkSearch = (id?: string) => ({ project: activeProject || undefined, template: id });
  const columns: TableColumn<ComposeTemplateSummary>[] = [
    { key: "name", header: t`Name`, render: (row) => <TemplateLink row={row} /> },
    { key: "revision", header: t`Revision`, render: (row) => <Tag words>r{row.revision.toString()}</Tag> },
    { key: "inputs", header: t`Inputs`, render: (row) => row.inputCount },
    {
      key: "updated",
      header: t`Updated`,
      render: (row) => (row.updatedAt ? timestampDate(row.updatedAt).toLocaleDateString(i18n.locale) : ""),
    },
  ];
  const empty = (
    <EmptyState
      icon="layers"
      title={query ? t`No matching templates` : t`No templates yet`}
      action={
        query ? (
          <Button
            onClick={() => {
              setPages([""]);
              setQuery("");
            }}
          >
            <Trans>Clear search</Trans>
          </Button>
        ) : canEdit ? (
          <Button onClick={() => setEdit("new")}>
            <Trans>New template</Trans>
          </Button>
        ) : undefined
      }
    />
  );
  const download = () => {
    if (!selected || !selectedSpec) return;
    const url = URL.createObjectURL(new Blob([selectedSpec.composeYaml], { type: "application/yaml" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `${selected.name}.yaml`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 0);
  };
  const current = selected && !selected.archived && revision === 0n ? selected : undefined;
  const menu: MenuAction[] = [{ id: "download", label: t`Download YAML`, icon: "download", onSelect: download }];
  if (canEdit) {
    menu.unshift({ id: "duplicate", label: t`Duplicate`, icon: "copy", onSelect: () => setEdit("duplicate") });
    if (current) {
      menu.push({
        id: "delete",
        label: t`Delete template`,
        icon: "trash",
        danger: true,
        onSelect: () => deleteTemplate(current, () => void navigate({ to: route, search: linkSearch() })),
      });
    }
  }
  const detailActions = selected ? (
    <>
      {canEdit && !selected.archived ? (
        <Button variant="primary" onClick={() => setUsing(selected)}>
          <Trans>Use template</Trans>
        </Button>
      ) : null}
      {canEdit && current ? (
        <Button onClick={() => setEdit("edit")}>
          <Trans>Edit</Trans>
        </Button>
      ) : null}
      <ActionMenu label={t`Template actions`} actions={menu} />
    </>
  ) : undefined;
  const badge = selected ? <Tag words>r{selected.revision.toString()}</Tag> : undefined;
  const historyColumns: TableColumn<ComposeTemplateRevision>[] = [
    {
      key: "revision",
      header: t`Revision`,
      render: (row) => (
        <Button
          variant="ghost"
          onClick={() => setRevision(row.revision)}
          aria-label={t`Open revision ${row.revision.toString()}`}
        >
          r{row.revision.toString()}
        </Button>
      ),
    },
    {
      key: "date",
      header: t`Date`,
      render: (row) => (row.changedAt ? timestampDate(row.changedAt).toLocaleDateString(i18n.locale) : ""),
    },
  ];
  if (canEdit && selected && !selected.archived) {
    historyColumns.push({
      key: "use",
      header: "",
      render: (row) => (
        <Button variant="ghost" onClick={() => setUsing({ ...selected, revision: row.revision })}>
          <Trans>Use this revision</Trans>
        </Button>
      ),
    });
  }
  return (
    <Page
      key={templateId ?? project}
      title={selected?.name ?? t`Compose templates`}
      width="wide"
      pending={!!templateId && detail.isPending && !selected}
      badge={badge}
      phoneSub={badge}
      lead={selected?.description}
      actions={detailActions}
      breadcrumb={
        templateId ? (
          <nav className="page-breadcrumbs" aria-label={t`Breadcrumb`}>
            {projectName ? (
              <>
                <Link to="/projects/$projectId" params={{ projectId: activeProject }}>
                  {projectName}
                </Link>
                <span aria-hidden="true">/</span>
              </>
            ) : null}
            <Link to={route} search={linkSearch()}>
              <Trans>Compose templates</Trans>
            </Link>
          </nav>
        ) : undefined
      }
      backLink={
        templateId ? (
          <Link
            className="mobile-header-button"
            aria-label={t`Back to Compose templates`}
            to={route}
            search={linkSearch()}
          >
            <Icon name="arrow-left" size={18} />
          </Link>
        ) : undefined
      }
    >
      <div className="compose-templates">
        {templateId ? (
          <>
            {!detail.data && detail.isPending ? (
              <Loading label={t`Reading template`} slow={t`Reading this template is taking longer than usual`}>
                <SkeletonCard />
              </Loading>
            ) : null}
            {detail.error ? (
              <Callout tone="critical" title={t`Can't read the template`}>
                {sentence(detail.error)}
              </Callout>
            ) : null}
            {selected && selectedSpec ? (
              <>
                {phone && detailActions ? <div className="template-toolbar">{detailActions}</div> : null}
                {selectedSpec.git ? (
                  <div className="template-source">
                    <Trans>Git source</Trans>{" "}
                    <code>
                      {selectedSpec.git.path} · {selectedSpec.git.commit.slice(0, 12)}
                    </code>
                  </div>
                ) : null}
                <div className="template-grid">
                  <CodeField label={t`Compose file`} value={selectedSpec.composeYaml} readOnly />
                  <Inputs inputs={selectedSpec.inputs} />
                </div>
                <Panel
                  title={<Trans>History</Trans>}
                  actions={
                    revision > 0n ? (
                      <Button variant="ghost" onClick={() => setRevision(0n)}>
                        <Trans>Open latest revision</Trans>
                      </Button>
                    ) : undefined
                  }
                >
                  <Table columns={historyColumns} rows={detail.data?.history ?? []} />
                  {detail.data?.nextHistoryPageToken ? (
                    <Button onClick={() => setHistoryPage(detail.data?.nextHistoryPageToken ?? "")}>
                      <Trans>Older revisions</Trans>
                    </Button>
                  ) : null}
                  {historyPage ? (
                    <Button onClick={() => setHistoryPage("")}>
                      <Trans>Recent revisions</Trans>
                    </Button>
                  ) : null}
                </Panel>
              </>
            ) : null}
          </>
        ) : (
          <>
            <div className="template-toolbar template-list-toolbar">
              <div className="template-search">
                <TextField
                  aria-label={t`Find a template`}
                  placeholder={t`Find a template`}
                  icon="search"
                  value={query}
                  onChange={(event) => {
                    setPages([""]);
                    setQuery(event.target.value);
                  }}
                />
              </div>
              {projects.pending ? (
                <SkeletonField />
              ) : projects.options.length > 1 ? (
                <SelectField
                  aria-label={t`Project`}
                  value={project}
                  options={projects.options}
                  onChange={(e) => {
                    setPages([""]);
                    void navigate({ to: route, search: { project: e.target.value } });
                  }}
                />
              ) : null}
              {canEdit ? (
                <Button variant="primary" icon="plus" onClick={() => setEdit("new")}>
                  <Trans>New template</Trans>
                </Button>
              ) : null}
            </div>
            {list.error ? (
              <Callout tone="critical" title={t`Can't read templates`}>
                {sentence(list.error)}
              </Callout>
            ) : null}
            {phone ? (
              projects.pending || (canRead && list.isPending) ? (
                <Loading label={t`Reading templates`} slow={t`Reading templates is taking longer than usual`}>
                  <SkeletonCard />
                </Loading>
              ) : (
                <>
                  <div className="template-cards">
                    {rows.map((row) => (
                      <div key={row.id} className="template-panel">
                        <TemplateLink row={row} />
                        <Tag words>r{row.revision.toString()}</Tag>{" "}
                        <span>
                          <Trans>{row.inputCount} inputs</Trans>
                        </span>
                      </div>
                    ))}
                  </div>
                  {!rows.length && !list.error ? empty : null}
                </>
              )
            ) : (
              <Table
                columns={columns}
                rows={rows}
                loading={projects.pending || (canRead && list.isPending)}
                slow={t`Reading templates is taking longer than usual`}
                empty={list.error ? undefined : empty}
              />
            )}
            {pages.length > 1 || list.data?.nextPageToken ? (
              <PaginationBar
                summary={<Trans>Page {pages.length}</Trans>}
                hasPrevious={pages.length > 1}
                hasNext={Boolean(!list.isPlaceholderData && list.data?.nextPageToken)}
                onPrevious={() => setPages(pages.slice(0, -1))}
                onNext={() => setPages([...pages, list.data?.nextPageToken ?? ""])}
                previousLabel={<Trans>Previous</Trans>}
                nextLabel={<Trans>Next</Trans>}
              />
            ) : null}
          </>
        )}
      </div>
      {edit ? (
        <TemplateDraftSheet
          projectId={activeProject}
          onClose={() => setEdit(undefined)}
          initial={edit === "new" || !detail.data ? undefined : detail.data}
          duplicate={edit === "duplicate"}
        />
      ) : null}
      {using ? <TemplateUseSheet onClose={() => setUsing(undefined)} initial={using} /> : null}
    </Page>
  );
}

function TemplateLink({ row }: { row: ComposeTemplateSummary }) {
  return (
    <div className="template-name">
      <Link className="dw-link compose-row-title" to={route} search={{ project: row.projectId, template: row.id }}>
        <Icon name="layers" size={17} />
        {row.name}
      </Link>
      {row.description ? <span className="template-muted">{row.description}</span> : null}
    </div>
  );
}

function useRefreshTemplates() {
  const queries = useQueryClient();
  return () => {
    void queries.invalidateQueries({
      queryKey: createConnectQueryKey({ schema: ComposeService.method.listComposeTemplates, cardinality: undefined }),
    });
    void queries.invalidateQueries({
      queryKey: createConnectQueryKey({ schema: ComposeService.method.getComposeTemplate, cardinality: undefined }),
    });
  };
}

function useDeleteTemplate() {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const refresh = useRefreshTemplates();
  return (item: ComposeTemplateSummary, onDone: () => void) =>
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(ChangeService, transport).proposeChange({
          summary: t`Delete template ${item.name}`,
          edits: [
            {
              resource: { scope: item.projectId, kind: "compose.template", name: item.name },
              baseRevision: item.revision,
              delete: true,
            },
          ],
        }),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) {
          refresh();
          onDone();
        }
      },
    });
}

export function TemplateDraftSheet({
  projectId,
  environmentId,
  suggestedName,
  initial,
  duplicate,
  onClose,
}: {
  projectId: string;
  environmentId?: string | undefined;
  suggestedName?: string | undefined;
  initial?: GetComposeTemplateResponse | undefined;
  duplicate?: boolean | undefined;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(ComposeService, transport), [transport]);
  const sheet = useChangeSheet();
  const refresh = useRefreshTemplates();
  const editing = initial?.template && !duplicate;
  const [mode, setMode] = useState(environmentId ? "stack" : "yaml");
  const [name, setName] = useState(
    duplicate ? `${initial?.template?.name ?? ""}-copy` : (initial?.template?.name ?? suggestedName ?? ""),
  );
  const [description, setDescription] = useState(initial?.spec?.description ?? "");
  const [yaml, setYaml] = useState(initial?.spec?.composeYaml ?? "");
  const [repo, setRepo] = useState(initial?.spec?.git?.repository ?? "");
  const [ref, setRef] = useState(initial?.spec?.git?.ref ?? "");
  const [file, setFile] = useState(initial?.spec?.git?.path ?? "compose.yaml");
  const [result, setResult] = useState<PrepareComposeTemplateResponse>();
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const [secretKinds, setSecretKinds] = useState<ReadonlySet<string>>(
    new Set(
      initial?.spec?.inputs
        .filter((input) => input.kind === ComposeTemplateInputKind.SECRET)
        .map((input) => input.name),
    ),
  );
  const prepare = useCallback(
    async (source: MessageInitShape<typeof PrepareComposeTemplateRequestSchema>["source"], signal?: AbortSignal) => {
      setPending(true);
      setResult(undefined);
      setError("");
      try {
        const next = await client.prepareComposeTemplate({ projectId, source }, { signal });
        if (!signal?.aborted) setResult(next);
      } catch (err) {
        if (!signal?.aborted) setError(sentence(err));
      } finally {
        if (!signal?.aborted) setPending(false);
      }
    },
    [client, projectId],
  );
  useEffect(() => {
    if (mode === "git" || (mode === "yaml" && !yaml.trim())) {
      setResult(undefined);
      setPending(false);
      return;
    }
    const controller = new AbortController();
    setResult(undefined);
    setPending(true);
    const timer = setTimeout(
      () =>
        void prepare(
          mode === "stack"
            ? { case: "stack", value: { environmentId: environmentId ?? "" } }
            : { case: "composeYaml", value: yaml },
          controller.signal,
        ),
      mode === "stack" ? 0 : 280,
    );
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [environmentId, mode, yaml, prepare]);
  const draft = result?.draft;
  const review = () => {
    if (!draft) return;
    const template: ComposeTemplate = {
      ...draft,
      description,
      git: draft.git ?? initial?.spec?.git,
      inputs: draft.inputs.map((input) =>
        secretKinds.has(input.name) ? { ...input, kind: ComposeTemplateInputKind.SECRET } : input,
      ),
    };
    onClose();
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(ChangeService, transport).proposeChange({
          summary: editing ? t`Edit Compose template ${name}` : t`Save Compose template ${name}`,
          edits: [
            {
              resource: { kind: "compose.template", scope: projectId, name: name.trim() },
              baseRevision: editing ? initial.template?.revision : 0n,
              spec: { kind: { case: "composeTemplate", value: template } },
            },
          ],
        }),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) refresh();
      },
    });
  };
  return (
    <Sheet
      open
      title={environmentId ? t`Save as template` : editing ? t`Edit template` : t`New template`}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!draft || pending || !name.trim()} onClick={review}>
            <Trans>Review template</Trans>
          </Button>
        </>
      }
    >
      <div className="template-form">
        <TextField label={t`Name`} value={name} readOnly={!!editing} onChange={(e) => setName(e.target.value)} />
        <TextField
          label={t`Description`}
          value={description}
          maxLength={160}
          onChange={(e) => setDescription(e.target.value)}
        />
        {!environmentId ? (
          <SelectField
            label={t`Source`}
            value={mode}
            options={[
              { label: t`Compose file`, value: "yaml" },
              { label: t`Git repository`, value: "git" },
            ]}
            disabled={mode === "git" && pending}
            onChange={(e) => {
              setResult(undefined);
              setMode(e.target.value);
            }}
          />
        ) : null}
        {mode === "yaml" ? (
          <>
            <CodeField
              label={t`Compose file`}
              value={yaml}
              markedLine={result?.refusal?.line}
              onChange={(e) => {
                setResult(undefined);
                setYaml(e.target.value);
              }}
            />
            <p className="template-muted template-note">
              <Trans>Use secret inputs. The file is visible to the project.</Trans>
            </p>
            <TextField
              label={t`Upload YAML`}
              type="file"
              accept=".yaml,.yml"
              onChange={(e) => {
                const file = e.target.files?.[0];
                if (file && file.size <= 256 * 1024) void file.text().then(setYaml);
                else if (file) setError(t`Choose a Compose file up to 256 KiB.`);
              }}
            />
          </>
        ) : mode === "git" ? (
          <>
            <TextField
              label={t`Repository URL`}
              value={repo}
              disabled={pending}
              onChange={(e) => {
                setRepo(e.target.value);
                setResult(undefined);
              }}
            />
            <TextField
              label={t`Branch, tag or commit`}
              placeholder={t`Default branch`}
              value={ref}
              disabled={pending}
              onChange={(e) => {
                setRef(e.target.value);
                setResult(undefined);
              }}
            />
            <TextField
              label={t`File path`}
              value={file}
              disabled={pending}
              onChange={(e) => {
                setFile(e.target.value);
                setResult(undefined);
              }}
            />
            <Button
              disabled={!repo || pending}
              onClick={() => void prepare({ case: "git", value: { repository: repo, ref, path: file } })}
            >
              <Trans>Read file</Trans>
            </Button>
          </>
        ) : null}
        {error ? (
          <Callout tone="critical" title={t`Can't prepare the template`}>
            {error}
          </Callout>
        ) : null}
        {result?.refusal ? <TemplateRefusal {...result.refusal} /> : null}
        {pending ? (
          <Loading
            label={t`Checking Compose file`}
            slow={
              mode === "git"
                ? t`Git is still reading the selected file`
                : t`The Compose file check is taking longer than usual`
            }
          >
            <SkeletonField />
            <SkeletonFacts count={3} />
          </Loading>
        ) : draft ? (
          <>
            {draft.git ? (
              <Tag words>
                {draft.git.commit.slice(0, 12)} · {draft.git.path}
              </Tag>
            ) : null}
            {mode === "stack" || mode === "git" ? (
              <CodeField label={t`Prepared Compose file`} readOnly value={draft.composeYaml} />
            ) : null}
            <Inputs inputs={draft.inputs} />
            {draft.inputs
              .filter((input) => input.kind === ComposeTemplateInputKind.TEXT && !input.defaultValue)
              .map((input) => (
                <label key={input.name} className="template-checkbox">
                  <input
                    type="checkbox"
                    checked={secretKinds.has(input.name)}
                    onChange={(e) =>
                      setSecretKinds((was) => {
                        const next = new Set(was);
                        if (e.target.checked) next.add(input.name);
                        else next.delete(input.name);
                        return next;
                      })
                    }
                  />
                  <Trans>Keep {input.name} as a secret</Trans>
                </label>
              ))}
          </>
        ) : null}
        {result?.omissions.some((item) => !item.path.startsWith("environment.")) ? (
          <Panel title={<Trans>Workspace settings left out</Trans>}>
            <ul className="template-inputs">
              {result.omissions
                .filter((item) => !item.path.startsWith("environment."))
                .map((item) => (
                  <li key={`${item.kind}/${item.name}/${item.path}`}>
                    <code>{item.name}</code>
                    <span>{item.path}</span>
                  </li>
                ))}
            </ul>
          </Panel>
        ) : null}
      </div>
    </Sheet>
  );
}

export function TemplateUseSheet({
  initial,
  initialServer,
  onClose,
}: {
  initial?: ComposeTemplateSummary | undefined;
  initialServer?: string | undefined;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const may = useMay();
  const transport = useTransport();
  const navigate = useNavigate();
  const sheet = useChangeSheet();
  const projects = useTemplateProjects();
  const { servers } = useFleetView();
  const [projectChoice, setProject] = useState(initial?.projectId ?? "");
  const project =
    projectChoice || projects.options.find((p) => p.label === "default")?.value || projects.options[0]?.value || "";
  const canPlan = !!project && may(ComposeService.method.planFromComposeTemplate, { project }).allowed;
  const canReadWorkspace = may(DockerService.method.getDockerObject).allowed;
  const [item, setItem] = useState(initial);
  const [revision, setRevision] = useState(initial?.revision ?? 0n);
  const [environment, setEnvironment] = useState(initial?.name ?? "");
  const [serverChoice, setServer] = useState(initialServer ?? "");
  const eligible = (servers ?? []).filter(
    (row) => row.connection === Connection.CONNECTED && row.dockerUnreadableSince === undefined,
  );
  const picked = serverChoice
    ? eligible.find((row) => row.id === serverChoice)
    : eligible.length === 1
      ? eligible[0]
      : undefined;
  const server = picked?.id ?? "";
  const [values, setValues] = useState<Record<string, string>>({});
  const [manual, setManual] = useState<ReadonlySet<string>>(new Set());
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const list = useQuery(
    ComposeService.method.listComposeTemplates,
    { projectId: project, pageSize: 100 },
    { enabled: canPlan && !initial },
  );
  const detail = useQuery(
    ComposeService.method.getComposeTemplate,
    { id: item?.id ?? "", revision, historyPageSize: 20 },
    { enabled: canPlan && !!item && revision > 0n },
  );
  const inputs = detail.data?.spec?.inputs ?? [];
  const bad = (input: ComposeTemplateInput) => {
    const value = values[input.name] ?? input.defaultValue ?? "";
    if (input.kind === ComposeTemplateInputKind.SECRET && !manual.has(input.name)) return false;
    if (input.required && !value.trim()) return true;
    return (
      input.kind === ComposeTemplateInputKind.PORT &&
      (!/^\d+$/.test(value) || Number(value) < 1 || Number(value) > 65535)
    );
  };
  const plan = async () => {
    if (!item || !server || !canPlan) return;
    setPending(true);
    setError("");
    const supplied = inputs.flatMap((input): Array<Pick<ComposeTemplateValue, "name" | "choice">> => {
      if (input.kind === ComposeTemplateInputKind.SECRET && !manual.has(input.name))
        return [{ name: input.name, choice: { case: "generate", value: true } }];
      return values[input.name] === undefined
        ? []
        : [{ name: input.name, choice: { case: "value", value: values[input.name] ?? "" } }];
    });
    try {
      const made = await createClient(ComposeService, transport).planFromComposeTemplate({
        id: item.id,
        revision,
        destination: { environmentName: environment.trim(), serverId: server },
        inputs: supplied,
      });
      if (made.refusal) {
        setError(made.refusal.message);
        return;
      }
      if (!made.change) {
        setError(t`The template did not produce a plan.`);
        return;
      }
      const env = made.change.plan?.resources.find((r) => r.resource?.kind === "environment")?.id;
      setValues({});
      onClose();
      sheet.open({
        kind: "plan",
        make: async () => made,
        onClose: (change) => {
          if (change?.state !== ChangeState.APPLIED || !env) return;
          if (canReadWorkspace) {
            void navigate({
              to: "/servers/$serverId/docker/composes/$composeId",
              params: { serverId: server, composeId: env },
            });
          } else {
            void navigate({ to: "/apps", search: { environment: env } });
          }
        },
      });
    } catch (err) {
      setError(sentence(err));
    } finally {
      setPending(false);
    }
  };
  const showInput = (input: ComposeTemplateInput) => {
    const isSecret = input.kind === ComposeTemplateInputKind.SECRET;
    return (
      <div key={input.name} className="template-variable">
        {isSecret ? (
          <SelectField
            label={input.name}
            value={manual.has(input.name) ? "manual" : "generate"}
            options={[
              { label: t`Generate secret`, value: "generate" },
              { label: t`Enter a secret`, value: "manual" },
            ]}
            onChange={(e) =>
              setManual((was) => {
                const next = new Set(was);
                if (e.target.value === "manual") next.add(input.name);
                else next.delete(input.name);
                return next;
              })
            }
          />
        ) : null}
        {!isSecret || manual.has(input.name) ? (
          <TextField
            label={isSecret ? t`Secret value` : input.name}
            type={isSecret ? "password" : input.kind === ComposeTemplateInputKind.PORT ? "number" : "text"}
            autoComplete={isSecret ? "new-password" : undefined}
            value={values[input.name] ?? input.defaultValue ?? ""}
            onChange={(e) => setValues((was) => ({ ...was, [input.name]: e.target.value }))}
            hint={bad(input) && values[input.name] !== undefined ? t`Check this value` : undefined}
          />
        ) : null}
      </div>
    );
  };
  return (
    <Sheet
      open
      title={t`Use Compose template`}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            disabled={
              !canPlan ||
              pending ||
              !detail.data?.spec ||
              !server ||
              !/^[a-z0-9][a-z0-9-]*$/.test(environment) ||
              inputs.some(bad)
            }
            onClick={() => void plan()}
          >
            <Trans>Review deployment</Trans>
          </Button>
        </>
      }
    >
      <div className="template-form">
        {!initial ? (
          <>
            {projects.options.length > 1 ? (
              <SelectField
                label={t`Project`}
                value={project}
                options={projects.options}
                onChange={(e) => {
                  setProject(e.target.value);
                  setItem(undefined);
                  setRevision(0n);
                  setValues({});
                }}
              />
            ) : null}
            {projects.pending || (canPlan && list.isPending) ? (
              <Loading label={t`Reading templates`} slow={t`Reading templates is taking longer than usual`}>
                <SkeletonField />
              </Loading>
            ) : list.data?.templates.length ? (
              <SelectField
                label={t`Template`}
                value={item?.id ?? ""}
                options={[
                  { label: t`Choose a template`, value: "" },
                  ...list.data.templates.map((row) => ({ label: row.name, value: row.id })),
                ]}
                onChange={(e) => {
                  const next = list.data?.templates.find((row) => row.id === e.target.value);
                  setItem(next);
                  setRevision(next?.revision ?? 0n);
                  setEnvironment(next?.name ?? "");
                  setValues({});
                  setManual(new Set());
                }}
              />
            ) : (
              <Link
                className="compose-backup-link"
                to={route}
                search={{ project: project || undefined }}
                onClick={onClose}
              >
                <Trans>Open template library</Trans>
              </Link>
            )}
          </>
        ) : (
          <Tag words>{initial.name}</Tag>
        )}
        {item && detail.data?.history.length ? (
          <SelectField
            label={t`Revision`}
            value={revision.toString()}
            options={detail.data.history.map((row) => ({
              value: row.revision.toString(),
              label: `r${row.revision.toString()}`,
            }))}
            onChange={(e) => {
              setRevision(BigInt(e.target.value));
              setValues({});
              setManual(new Set());
            }}
          />
        ) : null}
        {item ? (
          <>
            <TextField
              value={environment}
              onChange={(e) => setEnvironment(e.target.value)}
              label={t`New environment`}
            />
            {servers === undefined ? (
              <SkeletonField />
            ) : eligible.length > 1 || (eligible.length > 0 && !picked) ? (
              <SelectField
                label={t`Runs on`}
                value={server}
                options={[
                  { value: "", label: t`Choose a server` },
                  ...eligible.map((row) => ({ value: row.id, label: row.name })),
                ]}
                onChange={(e) => setServer(e.target.value)}
              />
            ) : picked ? (
              <span className="template-server">
                <Icon name="server" size={16} />
                <span>{picked.name}</span>
              </span>
            ) : (
              <Callout
                tone="warning"
                title={servers.length ? t`No connected server can read Docker` : t`Add a server to run this template`}
              />
            )}
            {detail.isPending || pending ? (
              <Loading label={t`Checking deployment`} slow={t`Checking this deployment is taking longer than usual`}>
                <SkeletonField />
              </Loading>
            ) : (
              <>
                {inputs.filter((input) => input.required).map(showInput)}
                {inputs.some((input) => !input.required) ? (
                  <details>
                    <summary>
                      <Trans>Defaults</Trans>
                    </summary>
                    {inputs.filter((input) => !input.required).map(showInput)}
                  </details>
                ) : null}
              </>
            )}
          </>
        ) : null}
        {error || detail.error ? (
          <Callout tone="critical" title={t`Can't plan from this template`}>
            {error || sentence(detail.error)}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}
