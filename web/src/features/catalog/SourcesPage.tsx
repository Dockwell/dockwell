// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { CatalogFormat, CatalogService, type CatalogSourceInfo, type CatalogStore, ChangeState } from "@dockwell/api";
import {
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
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { onApple } from "../../lib/platform";
import { useTickingNow, useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { type Opening, PlanView, useChangeSheet } from "../changes/ChangeSheet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { CopyButton } from "../system/CopyCommand";
import { AppIcon } from "./media";
import { converted, FormatLine, LeftOut, repositoryKey, useFormatWords } from "./SourceFormats";
import {
  StoreGallery,
  StoreMark,
  StoreTile,
  storeSourceName,
  useLicenceWords,
  useStoreName,
  useStores,
} from "./Stores";
import { monogram, useCatalog, useSourceNames, useSources } from "./useCatalog";
import "./Sources.css";

/**
 * What the source form starts from: nothing for a new source, the one it changes, or
 * a repository filled in for a person, as a preset of a known catalog would.
 */
export interface Editing {
  name?: string;
  repository?: string;
  path?: string;
  /** How its folder is laid out, as a preset knows it; left out, Dockwell tells (ADR 0078). */
  format?: CatalogFormat;
  /** The name a new source starts from, as a preset gives it. */
  suggested?: string;
  /** The branch it follows, or follows to start with: empty pins it. */
  follow?: string;
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
  // Each app's name and icon, by its ID, for a source's few icons, whichever catalog lists it.
  const icons = useMemo(() => {
    const out = new Map<string, { name: string; icon: string }>();
    for (const e of catalog.data?.entries ?? []) out.set(e.id, { name: e.name, icon: e.icon });
    return out;
  }, [catalog.data]);
  const list = sources.data?.sources;
  // To a source's card, from the store that it reads, with its name read out as it lands.
  const go = (name: string) => {
    const card = document.getElementById(sourceAnchor(name));
    card?.scrollIntoView?.({ block: "start", behavior: "smooth" });
    card?.querySelector<HTMLElement>("h3")?.focus({ preventScroll: true });
  };
  // A source decides what anyone can install, so adding, moving and removing one are an admin's.
  const may = useMay();
  const set = may(CatalogService.method.setCatalogSource, { install: true });
  const remove = may(CatalogService.method.removeCatalogSource, { install: true });
  const check = may(CatalogService.method.checkCatalogSource, { install: true });
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  // Dockwell's own catalog, back in one click, following its main branch as a new install's does.
  const addOwn = () =>
    sheet.open({
      kind: "plan",
      make: () =>
        client.setCatalogSource({
          name: nameFor(
            OWN_REPOSITORY,
            (list ?? []).map((source) => source.name),
          ),
          repository: OWN_REPOSITORY,
          path: "entries",
          follow: "main",
        }),
    });
  return (
    <Page
      title={t`Catalog sources`}
      back="/catalog"
      lead={t`The catalogs Dockwell installs apps from`}
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
        <>
          <EmptyState
            icon="branch"
            title={<Trans>No catalogs to install from</Trans>}
            action={
              <>
                <Button variant="primary" size="lg" icon="plus" {...set.off} onClick={addOwn}>
                  <Trans>Add Dockwell's catalog</Trans>
                </Button>
                <Button size="lg" icon="plus" {...set.off} onClick={() => setEditing({})}>
                  <Trans>Add another source</Trans>
                </Button>
              </>
            }
          >
            <p>
              <Trans>Dockwell's own apps, kept up to date, or any public Git repository.</Trans>
            </p>
          </EmptyState>
          <StoreGallery sources={list} off={set.off} onGo={go} />
        </>
      ) : (
        <>
          <StoreGallery sources={list} off={set.off} onGo={go} />
          <section className="sources-list" aria-labelledby="sources-list-heading">
            <h2 id="sources-list-heading" className="sources-list-heading">
              {plural(list.length, { one: "# catalog you read", other: "# catalogs you read" })}
            </h2>
            {list.map((source) => (
              <SourceCard
                key={source.name}
                source={source}
                icons={icons}
                moveOff={set.off}
                removeOff={remove.off}
                checkOff={check.off}
                onRead={() => void sources.refetch()}
                onMove={() =>
                  setEditing({
                    name: source.name,
                    repository: source.source?.repository ?? "",
                    path: source.source?.path ?? "",
                    follow: source.source?.follow ?? "",
                  })
                }
              />
            ))}
          </section>
        </>
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

/** Where a source's card is on the page, by the source's name. */
function sourceAnchor(name: string): string {
  return `source-${name}`;
}

/** How many of a source's apps its card shows the icons of, before the count. */
const FEW = 6;

/**
 * One source, in a card that stays short however many apps it brings: what it is and
 * where it reads, following and Check now, how many apps it brings with a few of their
 * icons and the way into the catalog filtered to it, and what it leaves out, grouped
 * by why, one step away.
 */
function SourceCard({
  source,
  icons,
  moveOff,
  removeOff,
  checkOff,
  onRead,
  onMove,
}: {
  source: CatalogSourceInfo;
  icons: ReadonlyMap<string, { name: string; icon: string }>;
  /** Spread onto Change, Pin, Check now and Remove: off, with why, for whoever may not. */
  moveOff: { off?: string; disabled?: true };
  removeOff: { off?: string; disabled?: true };
  checkOff: { off?: string; disabled?: true };
  /** Reads the sources again, once a look may have moved this one. */
  onRead: () => void;
  onMove: () => void;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  const name = source.name;
  const said = useSourceNames()(name);
  const commit = source.source?.commit ?? "";
  const path = source.source?.path ?? "";
  const follow = source.source?.follow ?? "";
  const repository = source.source?.repository ?? "";
  const formatWords = useFormatWords();
  const licence = useLicenceWords();
  // Every app it brings, those listed from a catalog before it included, which it offers as another choice.
  const apps = [...source.entries, ...source.also];
  const count = apps.length;
  const few = apps.filter((id) => icons.get(id)?.icon).slice(0, FEW);
  // Where it reads, as its link writes it, without the scheme or a trailing .git.
  const where =
    repository
      .trim()
      .replace(/^https:\/\//, "")
      .replace(/\/+$/, "")
      .replace(/\.git$/, "") + (path ? `/${path}` : "");
  const heading = `${sourceAnchor(name)}-heading`;
  return (
    <section className="dw-panel source-card" id={sourceAnchor(name)} aria-labelledby={heading}>
      <div className="source-card-head">
        {source.own ? (
          <span className="store-mark has-art" aria-hidden="true">
            <img src="/favicon.svg" alt="" width={40} height={40} />
          </span>
        ) : source.store ? (
          <StoreMark id={source.store} name={said} />
        ) : (
          <span className="store-mark" aria-hidden="true">
            {monogram(said)}
          </span>
        )}
        <div className="source-card-title">
          <h3 id={heading} tabIndex={-1} title={t`From the catalog ${name}`}>
            {said}
          </h3>
          <p className="source-card-where">
            <span title={repository}>{where}</span>
            {converted(source) && !source.store ? (
              <span>{formatWords(source.source?.format ?? CatalogFormat.UNSPECIFIED)}</span>
            ) : null}
            {/* Another store's licence, as its repository states it, or that it states none. */}
            {source.license ? (
              <span>
                <span className="store-licence" title={t`Licence`}>
                  {licence(source.license)}
                </span>
              </span>
            ) : null}
            <span className="source-card-commit">
              <code title={commit}>{commit.slice(0, 12)}</code>
              <CopyButton
                text={commit}
                iconOnly
                variant="ghost"
                label={t`Copy the commit`}
                copiedLabel={t`Commit copied`}
              />
            </span>
          </p>
        </div>
        <div className="source-card-actions">
          <Button size="sm" variant="ghost" icon="branch" aria-label={t`Change ${said}`} {...moveOff} onClick={onMove}>
            <Trans context="catalog source">Change</Trans>
          </Button>
          <Button
            size="sm"
            variant="ghost"
            icon="trash"
            aria-label={t`Remove ${said}`}
            {...removeOff}
            onClick={() => sheet.open({ kind: "plan", make: () => client.removeCatalogSource({ name }) })}
          />
        </div>
      </div>
      {follow !== "" ? (
        <FollowLine
          source={source}
          checkOff={checkOff}
          pinOff={moveOff}
          onChecked={(changeId) => {
            onRead();
            // What Dockwell moved it to, applied, with its undo one step away.
            if (changeId) sheet.open({ kind: "change", id: changeId, witnessed: true });
          }}
          onPin={() =>
            sheet.open({
              kind: "plan",
              make: () => client.setCatalogSource({ name, repository, path, commit, follow: "" }),
            })
          }
        />
      ) : null}
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
          <div className="source-card-apps">
            {few.length > 0 ? (
              <span className="source-card-icons" aria-hidden="true">
                {few.map((id) => (
                  <AppIcon key={id} name={icons.get(id)?.name ?? id} path={icons.get(id)?.icon ?? ""} />
                ))}
              </span>
            ) : null}
            <span className="source-card-count">
              {plural(count, { 0: "No apps from here", one: "# app from here", other: "# apps from here" })}
            </span>
            {count > 0 ? (
              <RouterButtonLink
                to="/catalog"
                search={{ catalog: name }}
                size="sm"
                variant="secondary"
                icon="arrow-right"
                aria-label={t`Browse the apps from ${said}`}
              >
                <Trans>Browse these apps</Trans>
              </RouterButtonLink>
            ) : null}
          </div>
          <LeftOut refused={source.refused} />
        </>
      )}
    </section>
  );
}

/**
 * What following a branch comes to now: when Dockwell last looked and the way to
 * look now, the way to stop at this commit, and why the source isn't where its
 * branch is, when it isn't.
 */
function FollowLine({
  source,
  checkOff,
  pinOff,
  onChecked,
  onPin,
}: {
  source: CatalogSourceInfo;
  checkOff: { off?: string; disabled?: true };
  pinOff: { off?: string; disabled?: true };
  onChecked: (changeId?: string) => void;
  onPin: () => void;
}) {
  const { t } = useLingui();
  const time = useTimeWords();
  const transport = useTransport();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  const [checking, setChecking] = useState(false);
  const [failed, setFailed] = useState<string>();
  const branch = source.source?.follow ?? "";
  const checked = source.checked ? timestampDate(source.checked) : undefined;
  const now = useTickingNow(checked?.getTime());
  const look = async () => {
    setChecking(true);
    setFailed(undefined);
    try {
      const res = await client.checkCatalogSource({ name: source.name });
      onChecked(res.change?.id);
    } catch (err) {
      setFailed(sentence(err));
    } finally {
      setChecking(false);
    }
  };
  const words = checked ? (
    <Trans>
      Following {branch} · checked {time.ago(checked, now)}
    </Trans>
  ) : (
    <Trans>Following {branch}</Trans>
  );
  return (
    <>
      <div className="sources-follow">
        <span className="sources-follow-words">
          <Icon name="branch" size={14} />
          {words}
        </span>
        <span className="sources-follow-actions">
          <Button size="sm" variant="ghost" icon="refresh" loading={checking} {...checkOff} onClick={() => void look()}>
            <Trans>Check now</Trans>
          </Button>
          <Button size="sm" variant="ghost" icon="commit" {...pinOff} onClick={onPin}>
            <Trans context="catalog source">Pin here</Trans>
          </Button>
        </span>
      </div>
      {failed ? <Callout tone="critical">{failed}</Callout> : null}
      {source.problem !== "" ? (
        <Callout tone="warning" title={t`Didn't follow ${branch}`}>
          {source.problem}
        </Callout>
      ) : null}
    </>
  );
}

/** Dockwell's own catalog, which every install starts with (ADR 0077). */
const OWN_REPOSITORY = "https://github.com/Dockwell/catalog";

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
 * Adds a source, or changes one. It asks for the repository alone: Dockwell reads it
 * as it's typed, offers its branches and tags, and follows the chosen branch, or
 * pins the commit a tag or a branch left to stand is at. Its name and folder start at
 * defaults, one step away, and a commit can still be typed. Then the plan, which says
 * what the commit holds.
 */
export function SourceSheet({
  editing,
  taken,
  onClose,
}: {
  editing: Editing;
  taken: readonly string[];
  onClose: () => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  const moving = editing.name !== undefined;
  // The source it moves, as a person says it; its ID is in the form's Name.
  const movingWords = useSourceNames()(editing.name ?? "");
  const [named, setNamed] = useState(editing.name);
  const [repository, setRepository] = useState(editing.repository ?? "");
  const [path, setPath] = useState(editing.path ?? "");
  const [format, setFormat] = useState(editing.format);
  const [picked, setPicked] = useState<string | undefined>(editing.follow ? `refs/heads/${editing.follow}` : undefined);
  // A branch is followed unless a person leaves it to stand; a source that's pinned stays so.
  const [following, setFollowing] = useState(editing.name === undefined || editing.follow !== "");
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
  // How its default branch reads, as Dockwell tells from its folders: a fact, or a choice where it can't tell.
  const guesses = askable ? (refs.data?.formats ?? []) : [];
  const guess = guesses.find((g) => g.format === format) ?? (guesses.length === 1 ? guesses[0] : undefined);
  // A store's own link is named by the store's ID, before one made from the link.
  const stores = useStores().data?.stores ?? [];
  const storeName = useStoreName();
  const storeID = editing.suggested ?? stores.find((s) => repositoryKey(s.repository) === repositoryKey(url))?.id;
  const suggested = storeID && !taken.includes(storeID) ? storeID : undefined;
  const name = named ?? suggested ?? nameFor(url, taken);
  // The repository's default branch, unless another is chosen; main or master where it names none.
  const head = askable ? (refs.data?.head ?? "") : "";
  const lead =
    found.find((r) => head !== "" && r.name === head) ??
    found.find((r) => r.name === "refs/heads/main") ??
    found.find((r) => r.name === "refs/heads/master");
  const choice = picked ?? (lead ?? found[0])?.name ?? TYPED;
  const chosen = found.find((r) => r.name === choice);
  const branch = chosen?.name.startsWith("refs/heads/") ? refWords(chosen.name).short : "";
  const follows = following && branch !== "";
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
    // A folder Dockwell found the catalog in, such as entries, where nobody named one.
    const folder = path.trim() === "" && guess ? guess.path : path.trim();
    const request = {
      name,
      repository: url,
      path: folder,
      commit: pin,
      format: format ?? guess?.format,
      follow: follows ? branch : "",
    };
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

  // A store, picked in the sheet: its plan, at once, over the sheet, which Cancel comes back to.
  const addStore = (store: CatalogStore) => {
    if (current.current) return;
    const opening: Opening = {
      kind: "plan",
      make: () =>
        client.setCatalogSource({
          name: storeSourceName(store, taken),
          repository: store.repository,
          path: "",
          follow: store.branch,
          format: store.format,
        }),
      onDone: close,
      onClose: (after) => {
        current.current = undefined;
        if (!after || after.state === ChangeState.PLANNED || after.state === ChangeState.DISCARDED) setPlan(undefined);
        else close();
      },
    };
    current.current = opening;
    setPlan(opening);
  };
  // The stores come first in a new, empty form: one click each, and a link for anything else.
  const picking = !moving && editing.repository === undefined && url === "" && stores.length > 0;

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
      title={moving ? t`Change ${movingWords}` : t`Add a catalog source`}
      subtitle={picking ? t`An app store, or any public Git repository` : t`Apps from a public Git repository`}
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
        {picking ? (
          <fieldset className="sources-pick">
            <legend>
              <Trans>App stores</Trans>
            </legend>
            <ul className="sources-pick-list">
              {stores.map((store) => (
                <StoreTile
                  key={store.id}
                  store={store}
                  small
                  // An added store says so in its line; the page's card is the way to it.
                  action={
                    store.source ? undefined : (
                      <Button
                        size="sm"
                        icon="plus"
                        aria-label={t`Add ${storeName(store)}`}
                        onClick={() => addStore(store)}
                      >
                        <Trans context="catalog store">Add</Trans>
                      </Button>
                    )
                  }
                />
              ))}
            </ul>
          </fieldset>
        ) : null}
        {picking ? (
          <p className="sources-or">
            <Trans>Or any public Git repository</Trans>
          </p>
        ) : null}
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
            // What a preset knew of one repository says nothing of another.
            setFormat(undefined);
            // A press for another repository doesn't carry over to this one.
            setWaiting(false);
            setErrors({ ...errors, repository: undefined });
          }}
        />
        <FormatLine guesses={guesses} chosen={format} onChoose={(g) => setFormat(g.format)} />
        {count > 0 ? (
          <SelectField
            label={t`Branch`}
            value={choice}
            hint={
              chosen && !follows
                ? t`At ${chosen.commit.slice(0, 12)} now. It stays there until you move it.`
                : undefined
            }
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
        {branch !== "" ? (
          <Switch
            label={t`Follow ${branch}`}
            description={
              following
                ? t`Its new commits come in on their own. What's installed stays as it is.`
                : t`Pinned at ${pin.slice(0, 12)}.`
            }
            checked={following}
            onChange={setFollowing}
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
