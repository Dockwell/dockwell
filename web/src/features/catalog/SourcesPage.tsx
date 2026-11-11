// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { CatalogService, type CatalogSourceInfo, ChangeState } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  Panel,
  PHONE_QUERY,
  SelectField,
  Sheet,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useMay } from "../auth/may";
import { type Opening, PlanView, useChangeSheet } from "../changes/ChangeSheet";
import { Page } from "../shell/Page";
import { CopyButton } from "../system/CopyCommand";
import { useCatalog, useSources } from "./useCatalog";
import "./Sources.css";

/** What the source form starts from: nothing for a new source, or the one it moves. */
interface Editing {
  name?: string;
  repository?: string;
  path?: string;
}

/**
 * Catalog sources: the catalogs Dockwell reads apps from beside its own, each a
 * folder of entries in a public Git repository at one commit (ADR 0055). Each says
 * which apps the catalog shows from it, and which files it leaves out and why.
 * Adding one, moving it to another commit and removing it are each a plan to
 * review, as anything that changes what can be installed is.
 */
export function SourcesPage() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const sources = useSources();
  const catalog = useCatalog();
  const [editing, setEditing] = useState<Editing>();
  const names = useMemo(() => new Map((catalog.data?.entries ?? []).map((e) => [e.id, e.name])), [catalog.data]);
  const list = sources.data?.sources;
  // A source decides what anyone can install, so adding, moving and removing one are an admin's.
  const may = useMay();
  const set = may(CatalogService.method.setCatalogSource, { install: true });
  const remove = may(CatalogService.method.removeCatalogSource, { install: true });
  return (
    <Page
      title={t`Catalog sources`}
      back="/catalog"
      lead={t`Catalogs Dockwell reads apps from beside its own`}
      actions={
        // With none yet, the empty state's own button is the way in.
        !list?.length ? undefined : (
          <Button variant="primary" icon="plus" {...set.off} onClick={() => setEditing({})}>
            <Trans>Add a source</Trans>
          </Button>
        )
      }
      className="sources-page"
    >
      {/* A phone's header has no room for the page's actions, so this one leads its body. */}
      {phone && list?.length ? (
        <div className="sources-phone-head">
          <Button variant="primary" icon="plus" size="lg" {...set.off} onClick={() => setEditing({})}>
            <Trans>Add a source</Trans>
          </Button>
        </div>
      ) : null}
      {sources.error ? (
        <Callout
          tone="critical"
          title={t`Couldn't read the sources`}
          actions={
            <Button size="sm" icon="refresh" onClick={() => void sources.refetch()}>
              <Trans>Try again</Trans>
            </Button>
          }
        >
          {sentence(sources.error)}
        </Callout>
      ) : !list ? (
        <p className="sources-note" aria-busy="true">
          <Trans>Reading the sources</Trans>
        </p>
      ) : list.length === 0 ? (
        <section className="dw-panel sources-empty" aria-labelledby="sources-empty-heading">
          <Icon name="branch" size={24} />
          <h2 id="sources-empty-heading">
            <Trans>Dockwell's own catalog is all it reads</Trans>
          </h2>
          <p>
            <Trans>A source adds the apps in a public Git repository.</Trans>
          </p>
          <Button variant="primary" icon="plus" {...set.off} onClick={() => setEditing({})}>
            <Trans>Add a source</Trans>
          </Button>
        </section>
      ) : (
        list.map((source) => (
          <SourcePanel
            key={source.name}
            source={source}
            names={names}
            moveOff={set.off}
            removeOff={remove.off}
            onMove={() =>
              setEditing({
                name: source.name,
                repository: source.source?.repository ?? "",
                path: source.source?.path ?? "",
              })
            }
          />
        ))
      )}
      {editing ? (
        <SourceSheet
          editing={editing}
          taken={(list ?? []).map((source) => source.name)}
          onClose={() => setEditing(undefined)}
        />
      ) : null}
    </Page>
  );
}

/** One source: where it reads from, the apps it gives, and what it leaves out. */
function SourcePanel({
  source,
  names,
  moveOff,
  removeOff,
  onMove,
}: {
  source: CatalogSourceInfo;
  names: ReadonlyMap<string, string>;
  /** Spread onto Move and Remove: off, with why, for whoever may not. */
  moveOff: { off?: string; disabled?: true };
  removeOff: { off?: string; disabled?: true };
  onMove: () => void;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  const name = source.name;
  const commit = source.source?.commit ?? "";
  const path = source.source?.path ?? "";
  const count = source.entries.length;
  const left = source.refused.length;
  return (
    <Panel
      title={name}
      className="sources-panel"
      actions={
        <>
          <Button size="sm" variant="ghost" icon="commit" {...moveOff} onClick={onMove}>
            <Trans context="catalog source">Move</Trans>
          </Button>
          <Button
            size="sm"
            variant="ghost"
            icon="trash"
            aria-label={t`Remove ${name}`}
            {...removeOff}
            onClick={() => sheet.open({ kind: "plan", make: () => client.removeCatalogSource({ name }) })}
          />
        </>
      }
    >
      <dl className="sources-facts">
        <div>
          <dt>
            <Trans>Repository</Trans>
          </dt>
          <dd>
            <code>{source.source?.repository}</code>
          </dd>
        </div>
        <div>
          <dt>
            <Trans>Folder</Trans>
          </dt>
          <dd>{path === "" ? <Trans>Its top</Trans> : <code>{path}</code>}</dd>
        </div>
        <div>
          <dt>
            <Trans>Commit</Trans>
          </dt>
          <dd className="sources-commit">
            <code title={commit}>{commit.slice(0, 12)}</code>
            <CopyButton
              text={commit}
              iconOnly
              variant="ghost"
              label={t`Copy the commit`}
              copiedLabel={t`Commit copied`}
            />
          </dd>
        </div>
      </dl>
      {source.unread !== "" ? (
        <Callout
          tone="warning"
          title={t`Can't be read now, so its apps are out of the catalog`}
          actions={
            <Button size="sm" icon="commit" {...moveOff} onClick={onMove}>
              <Trans>Move to another commit</Trans>
            </Button>
          }
        >
          {source.unread}
        </Callout>
      ) : (
        <>
          <h3 className="sources-heading">
            {plural(count, { 0: "No apps from here", one: "# app from here", other: "# apps from here" })}
          </h3>
          {count > 0 ? (
            <ul className="sources-apps">
              {source.entries.map((id) => (
                <li key={id}>
                  <Link to="/catalog/$entryId" params={{ entryId: id }} className="sources-app">
                    {names.get(id) ?? id}
                  </Link>
                </li>
              ))}
            </ul>
          ) : null}
          {left > 0 ? (
            <details className="sources-left">
              <summary>{plural(left, { one: "# file left out, and why", other: "# files left out, and why" })}</summary>
              <ul>
                {source.refused.map((r) => (
                  <li key={r.file}>
                    <code>{r.file}</code>
                    <span>{r.reason}</span>
                  </li>
                ))}
              </ul>
            </details>
          ) : null}
        </>
      )}
    </Panel>
  );
}

/** A commit in full, as a source is pinned by one. */
const COMMIT = /^[0-9a-f]{40}$/;

/** A source's name, as a resource's is. */
const NAME = /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** Whether a repository's URL is HTTPS and holds no credentials, query or fragment. */
function readableURL(text: string): boolean {
  try {
    const u = new URL(text);
    return u.protocol === "https:" && u.username === "" && u.password === "" && u.search === "" && u.hash === "";
  } catch {
    return false;
  }
}

/**
 * A name for a source from its repository: its owner's, as github.com/example/catalog
 * gives "example", or its host's, made a name a resource can have and one no source has.
 */
export function nameFor(repository: string, taken: readonly string[]): string {
  let base = "catalog";
  try {
    const u = new URL(repository);
    const [owner] = u.pathname.split("/").filter(Boolean);
    const from = (u.pathname.split("/").filter(Boolean).length > 1 ? owner : u.hostname.split(".").at(-2)) ?? "";
    const clean = from
      .toLowerCase()
      .replace(/[^a-z0-9-]+/g, "-")
      .replace(/^[^a-z]+|-+$/g, "")
      .slice(0, 40);
    if (NAME.test(clean)) base = clean;
  } catch {
    // Not a URL yet: the stand-in will do until it is.
  }
  if (!taken.includes(base)) return base;
  for (let n = 2; ; n++) if (!taken.includes(`${base}-${n}`)) return `${base}-${n}`;
}

/** A value once it has stood still for a moment, so a check isn't made on every key. */
function useSettled<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms]);
  return settled;
}

/** A branch or tag as a person names it. */
function refWords(name: string): { short: string; tag: boolean } {
  const tag = name.startsWith("refs/tags/");
  return { short: name.replace(/^refs\/(heads|tags)\//, ""), tag };
}

/** The choice that stands for typing a commit, among the branches and tags. */
const TYPED = "";

/**
 * Adds a source, or moves one to another commit. It asks for the repository alone:
 * Dockwell reads it as it's typed, offers its branches and tags, and pins the commit
 * the chosen one is at. Its name and folder start at defaults, one step away, and a
 * commit can still be typed. Then the plan, which says what the commit holds.
 */
function SourceSheet({ editing, taken, onClose }: { editing: Editing; taken: readonly string[]; onClose: () => void }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  const moving = editing.name !== undefined;
  const [named, setNamed] = useState(editing.name);
  const [repository, setRepository] = useState(editing.repository ?? "");
  const [path, setPath] = useState(editing.path ?? "");
  const [picked, setPicked] = useState<string>();
  const [commit, setCommit] = useState("");
  const [more, setMore] = useState(false);
  const [errors, setErrors] = useState<{ name?: string; repository?: string; commit?: string }>({});
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  // Review plan pressed before the repository was read: it goes once the branches are in.
  const [waiting, setWaiting] = useState(false);
  const current = useRef<Opening | undefined>(undefined);
  const first = useRef<HTMLInputElement>(null);

  // The repository is read once it's a URL that has stood still, and what it holds is the form's next choice.
  const url = repository.trim();
  const settled = useSettled(url, 500);
  const askable = settled !== "" && settled === url && readableURL(settled);
  const refs = useQuery(
    CatalogService.method.listCatalogSourceRefs,
    { repository: settled },
    { enabled: askable, retry: false, staleTime: 60_000 },
  );
  const found = askable ? (refs.data?.refs ?? []) : [];
  const name = named ?? nameFor(url, taken);
  // The repository's default branch, unless another is chosen; main or master where it names none.
  const head = askable ? (refs.data?.head ?? "") : "";
  const lead =
    found.find((r) => head !== "" && r.name === head) ??
    found.find((r) => r.name === "refs/heads/main") ??
    found.find((r) => r.name === "refs/heads/master");
  const choice = picked ?? (lead ?? found[0])?.name ?? TYPED;
  const chosen = found.find((r) => r.name === choice);
  const typed = commit.trim().toLowerCase();
  const pin = chosen ? chosen.commit : typed;
  const at = found.filter((r) => r.commit === typed).map((r) => refWords(r.name).short);
  // A URL that can be read, whose branches aren't in yet: while it stands still, or while it's read.
  const reading = readableURL(url) && (!askable || refs.isPending);

  // A failed request may return after this form closed or another plan opened.
  useEffect(
    () => () => {
      current.current = undefined;
    },
    [],
  );
  const close = () => {
    current.current = undefined;
    onClose();
  };

  const submit = (event?: FormEvent) => {
    event?.preventDefault();
    if (current.current) return;
    const next = {
      name: !NAME.test(name)
        ? t`Use lowercase letters, digits and dashes, starting with a letter, such as community.`
        : !moving && taken.includes(name)
          ? t`A source is already named ${name}. Use another name.`
          : undefined,
      repository: readableURL(url) ? undefined : t`Use the repository's https:// URL.`,
      // Until the repository is read there's no branch to choose yet, so that's no error: the press waits.
      commit:
        COMMIT.test(pin) || reading ? undefined : t`Use the commit in full, as 40 hex digits, or choose a branch.`,
    };
    setErrors(next);
    setFailure(undefined);
    if (next.name) setMore(true);
    if (next.name || next.repository || next.commit) return;
    if (!COMMIT.test(pin)) {
      setWaiting(true);
      return;
    }
    const request = { name, repository: url, path: path.trim(), commit: pin };
    const opening: Opening = {
      kind: "plan",
      // PlanView owns the request from its start, so leaving mid-fetch discards a late plan too.
      make: async () => {
        try {
          return await client.setCatalogSource(request);
        } catch (err) {
          if (current.current === opening) {
            current.current = undefined;
            setFailure(
              reasonOf(err) === "unreadable" ? t`Dockwell can't read that commit: ${sentence(err)}` : sentence(err),
            );
            setPlan(undefined);
          }
          throw err;
        }
      },
      onDone: close,
      onClose: (after) => {
        current.current = undefined;
        // Cancelling comes back here, with what you typed. Anything else is under way or over.
        if (!after || after.state === ChangeState.PLANNED || after.state === ChangeState.DISCARDED) setPlan(undefined);
        else close();
      },
    };
    current.current = opening;
    setPlan(opening);
  };

  // Once the branches are in, a press that waited for them goes on: with the branch chosen, or to the commit field.
  // biome-ignore lint/correctness/useExhaustiveDependencies: submit reads the state this runs on, and is new each render
  useEffect(() => {
    if (!waiting || reading) return;
    setWaiting(false);
    if (COMMIT.test(pin)) submit();
  }, [waiting, reading, pin]);

  const count = found.length;
  const checking = askable && refs.isPending;
  const repositoryState = errors.repository
    ? undefined
    : checking
      ? t`Reading the repository`
      : refs.data?.more && count > 0
        ? t`Read. It has ${count} or more branches and tags.`
        : count > 0
          ? plural(count, { one: "Read. It has # branch or tag.", other: "Read. It has # branches and tags." })
          : undefined;
  const folderWords = path.trim() === "" ? t`its top` : path.trim();
  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={first}
      title={moving ? t`Move ${name} to another commit` : t`Add a catalog source`}
      subtitle={t`Apps from a public Git repository, pinned at one commit`}
      onClose={close}
      className={cx("sources-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing changes until you apply the plan</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={close}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            loading={waiting}
            onClick={() => void submit()}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="sources-form"
        onSubmit={(event) => void submit(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void submit(event);
        }}
      >
        <button type="submit" hidden />
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        <TextField
          ref={first}
          label={t`Repository`}
          mono
          placeholder="https://github.com/example/catalog"
          value={repository}
          error={errors.repository ?? (askable && refs.error ? sentence(refs.error) : undefined)}
          hint={repositoryState}
          onChange={(event) => {
            setRepository(event.target.value);
            setPicked(undefined);
            // A press for another repository doesn't carry over to this one.
            setWaiting(false);
            setErrors({ ...errors, repository: undefined });
          }}
        />
        {count > 0 ? (
          <SelectField
            label={t`Pin to`}
            value={choice}
            hint={chosen ? t`At ${chosen.commit.slice(0, 12)} now. It stays there until you move it.` : undefined}
            options={[
              ...found.map((r) => {
                const { short, tag } = refWords(r.name);
                return { value: r.name, label: tag ? t`${short} (tag)` : short };
              }),
              { value: TYPED, label: t`Another commit` },
            ]}
            onChange={(event) => {
              setPicked(event.target.value);
              setErrors({ ...errors, commit: undefined });
            }}
          />
        ) : null}
        {/* Typing a commit comes up once the repository is read: when it has no branch to offer, can't be read, or you ask. */}
        {chosen || !askable || checking ? null : (
          <TextField
            label={t`Commit`}
            mono
            value={commit}
            error={errors.commit}
            hint={
              !COMMIT.test(typed) || count === 0
                ? undefined
                : at.length > 0
                  ? t`${at.join(", ")} is at this commit.`
                  : t`No branch or tag is at this commit.`
            }
            onChange={(event) => {
              setCommit(event.target.value);
              setErrors({ ...errors, commit: undefined });
            }}
          />
        )}
        {more ? (
          <>
            {moving ? null : (
              <TextField
                label={t`Name`}
                mono
                value={name}
                error={errors.name}
                onChange={(event) => {
                  setNamed(event.target.value);
                  setErrors({ ...errors, name: undefined });
                }}
              />
            )}
            <TextField
              label={t`Folder`}
              mono
              value={path}
              hint={t`Where its entries are. Empty is its top.`}
              onChange={(event) => setPath(event.target.value)}
            />
          </>
        ) : (
          <p className="sources-defaults">
            <span className="sources-defaults-words">
              {moving ? (
                <Trans>Entries at {folderWords}</Trans>
              ) : (
                <Trans>
                  Named {name} · entries at {folderWords}
                </Trans>
              )}
            </span>
            <Button variant="ghost" size="sm" onClick={() => setMore(true)}>
              <Trans>Change</Trans>
            </Button>
          </p>
        )}
      </form>
    </Sheet>
  );
}
