// SPDX-License-Identifier: AGPL-3.0-only

import {
  type GitDeployKeyInfo,
  GitLabConnectionState,
  GitLabListingProblem,
  type GitLabProject,
  type GitLabProjectConnection,
  type GitRepository,
} from "@dockwell/api";
import { cx, TextField } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { type KeyboardEvent, type ReactNode, type Ref, useEffect, useId, useMemo, useRef, useState } from "react";

import { GITLAB_LOGIN_PREFIX } from "./gitHost";

import "./ImageField.css";

/**
 * A repository's URL as two can be compared: no .git, no trailing slash, any
 * case. When both URLs are SSH, absolute paths (/srv/git/app.git) stay distinct
 * from relative paths (srv/git/app.git), matching service.SameSSHRepository.
 */
export function sameRepository(a: string, b: string): boolean {
  const isSSH = (url: string) => {
    const trimmed = url.trim().toLowerCase();
    return trimmed.startsWith("ssh://") || (!trimmed.includes("://") && /^[\w.-]+@[^:/]+:.+$/.test(trimmed));
  };
  const parseSSH = (url: string) => {
    const trimmed = url.trim().toLowerCase();
    const scp = /^[\w.-]+@([^:/]+):(.+)$/.exec(trimmed);
    if (scp?.[1] && scp[2]) {
      const path = scp[2].replace(/\/+$/, "").replace(/\.git$/, "");
      return `${scp[1]}:${path}`;
    }
    const noScheme = trimmed.replace(/^ssh:\/\//, "").replace(/^[^/@]+@/, "");
    const slash = noScheme.indexOf("/");
    if (slash < 0) return noScheme;
    const hostPort = noScheme.slice(0, slash).replace(/:22$/, "");
    let path = noScheme
      .slice(slash)
      .replace(/\/+$/, "")
      .replace(/\.git$/, "");
    if (path.startsWith("/~/")) path = path.slice(3);
    return `${hostPort}:${path}`;
  };
  const plain = (url: string) => {
    const trimmed = url.trim().toLowerCase();
    const scp = /^[\w.-]+@([^:/]+):\/?(.+)$/.exec(trimmed);
    const body = scp
      ? `${scp[1]}/${scp[2]}`
      : trimmed
          .replace(/^(https?|ssh):\/\//, "")
          .replace(/^[^/@]+@/, "")
          .replace(/^([^/]+)\/~\//, "$1/");
    return body.replace(/\/+$/, "").replace(/\.git$/, "");
  };
  if (a.trim() === "" || b.trim() === "") return false;
  if (isSSH(a) && isSSH(b)) return parseSSH(a) === parseSSH(b);
  return plain(a) === plain(b);
}

/** Optional metadata supplied when a repository is picked from a connected provider. */
export interface PickedRepository {
  gitLogin?: string;
  defaultBranch?: string;
}

interface PickerOption {
  key: string;
  url: string;
  label: string;
  privateRepo: boolean;
  replacedKeyName?: string;
  gitLogin?: string;
  defaultBranch?: string;
}

interface PickerGroup {
  key: string;
  heading: string;
  host?: string;
  note?: string;
  options: PickerOption[];
}

/**
 * A repository, picked rather than typed (ADR 0098, ADR 0113): focusing the
 * field lists repositories from the project's GitHub App installations and
 * GitLab connections grouped by connection, and typing narrows them (with
 * server-side search for large GitLab connections). Any repository's URL can
 * still be typed or pasted.
 */
export function RepositoryField({
  value,
  onChange,
  repositories,
  gitlabProjects = [],
  gitlabConnections = [],
  deployKeys = [],
  onSearch,
  error,
  hint,
  fieldRef,
}: {
  value: string;
  onChange: (url: string, picked?: PickedRepository) => void;
  /** What the project's GitHub App installations reach, as GitHub last listed them. */
  repositories: readonly GitRepository[];
  /** What the project's GitLab connections reach, from the cached searchable list. */
  gitlabProjects?: readonly GitLabProject[];
  /** Reachable GitLab connections, for grouping and showing connection status or server search. */
  gitlabConnections?: readonly GitLabProjectConnection[];
  /** Project SSH deploy keys, used to show when a GitLab connection is preferred over a key. */
  deployKeys?: readonly GitDeployKeyInfo[];
  /** Called (debounced) when typing and a GitLab connection has more projects on the server. */
  onSearch?: (query: string) => void;
  error?: ReactNode;
  hint?: ReactNode;
  fieldRef?: Ref<HTMLInputElement & HTMLTextAreaElement>;
}) {
  const { t } = useLingui();
  const id = useId();
  const listId = `${id}-list`;
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const wrap = useRef<HTMLDivElement>(null);
  const typed = value.trim().toLowerCase();

  const hasMoreGitLab = gitlabConnections.some((c) => c.hasMore);
  const lastSearch = useRef("");
  useEffect(() => {
    if (!onSearch) return;
    // Trigger server-side GitLab search only when a connection has >100 projects,
    // or to clear a previously sent query.
    const next = !typed || typed.includes("://") || typed.includes("@") ? "" : typed;
    if (!hasMoreGitLab && lastSearch.current === "") return;
    if (next === lastSearch.current) return;
    const timer = setTimeout(() => {
      lastSearch.current = next;
      onSearch(next);
    }, 250);
    return () => clearTimeout(timer);
  }, [hasMoreGitLab, onSearch, typed]);

  const hasAnySource = repositories.length > 0 || gitlabProjects.length > 0 || gitlabConnections.length > 0;

  const { groups, flatOptions } = useMemo(() => {
    const exactMatch =
      repositories.some((r) => sameRepository(r.url, value)) ||
      gitlabProjects.some((p) => sameRepository(p.url, value));
    if (exactMatch) return { groups: [] as PickerGroup[], flatOptions: [] as PickerOption[] };

    const words = typed.replace(/^https?:\/\//, "").replace(/^[^/]*github[^/]*\//, "");
    const builtGroups: PickerGroup[] = [];
    const showGroupHeaders = gitlabConnections.length > 0 || gitlabProjects.length > 0;

    // 1. GitHub App repositories (grouped by account when GitLab is also present).
    const ghFiltered = repositories
      .filter((r) => words === "" || r.fullName.toLowerCase().includes(words) || r.url.toLowerCase().includes(words))
      .slice(0, 50);
    if (ghFiltered.length > 0) {
      if (showGroupHeaders) {
        const byAccount = new Map<string, GitRepository[]>();
        for (const r of ghFiltered) {
          const acct = r.account || "GitHub";
          const list = byAccount.get(acct) ?? [];
          list.push(r);
          byAccount.set(acct, list);
        }
        for (const [acct, items] of byAccount) {
          builtGroups.push({
            key: `gh:${acct}`,
            heading: acct === "GitHub" ? "GitHub" : t`GitHub · ${acct}`,
            options: items.map((r) => ({
              key: `gh:${r.url}`,
              url: r.url,
              label: r.fullName,
              privateRepo: r.private,
              defaultBranch: r.defaultBranch,
            })),
          });
        }
      } else {
        builtGroups.push({
          key: "gh:all",
          heading: "",
          options: ghFiltered.map((r) => ({
            key: `gh:${r.url}`,
            url: r.url,
            label: r.fullName,
            privateRepo: r.private,
            defaultBranch: r.defaultBranch,
          })),
        });
      }
    }

    // 2. GitLab connection projects, grouped by connection.
    const glByConn = new Map<string, GitLabProject[]>();
    for (const p of gitlabProjects) {
      const connName = p.connectionName;
      if (
        words !== "" &&
        !p.pathWithNamespace.toLowerCase().includes(words) &&
        !p.url.toLowerCase().includes(words) &&
        !connName.toLowerCase().includes(words)
      ) {
        continue;
      }
      const list = glByConn.get(connName) ?? [];
      list.push(p);
      glByConn.set(connName, list);
    }

    const listingNoteFor = (conn: GitLabProjectConnection): string | undefined => {
      switch (conn.listingProblem) {
        case GitLabListingProblem.MISSING_READ_API:
          return t`Add read_api to pick projects from ${conn.name}. You can still paste a URL.`;
        case GitLabListingProblem.AUTH_FAILED:
          return t`GitLab refused this token. Check it in Settings, Integrations.`;
        case GitLabListingProblem.UNREACHABLE:
          return t`Could not reach ${conn.name} right now.`;
        default:
          break;
      }
      switch (conn.state) {
        case GitLabConnectionState.AUTH_FAILED:
          return t`GitLab refused this token. Check it in Settings, Integrations.`;
        case GitLabConnectionState.MISSING_SCOPE:
          return t`Add read_repository to build from ${conn.name}.`;
        case GitLabConnectionState.UNREACHABLE:
          return t`Could not reach ${conn.name} right now.`;
        default:
          return undefined;
      }
    };

    const seenConn = new Set<string>();
    for (const conn of gitlabConnections) {
      const connName = conn.name;
      seenConn.add(connName);
      const items = (glByConn.get(connName) ?? []).slice(0, 50);
      const note = listingNoteFor(conn);
      if (items.length === 0 && !note) continue;
      const host = conn.url.replace(/^https?:\/\//, "");
      builtGroups.push({
        key: `gl:${connName}`,
        heading: t`GitLab · ${connName}`,
        host,
        note,
        options: items.map((p) => {
          const matchingKey = deployKeys.find((k) => sameRepository(k.repository, p.url));
          return {
            key: `gl:${connName}:${p.url}`,
            url: p.url,
            label: p.pathWithNamespace,
            privateRepo: false,
            replacedKeyName: matchingKey?.name,
            gitLogin: `${GITLAB_LOGIN_PREFIX}${p.connectionName}`,
            defaultBranch: p.defaultBranch,
          };
        }),
      });
    }
    // Include any GitLab projects whose connection metadata wasn't listed separately.
    for (const [connName, items] of glByConn) {
      if (seenConn.has(connName) || items.length === 0) continue;
      builtGroups.push({
        key: `gl:${connName}`,
        heading: t`GitLab · ${connName}`,
        options: items.slice(0, 50).map((p) => {
          const matchingKey = deployKeys.find((k) => sameRepository(k.repository, p.url));
          return {
            key: `gl:${connName}:${p.url}`,
            url: p.url,
            label: p.pathWithNamespace,
            privateRepo: false,
            replacedKeyName: matchingKey?.name,
            gitLogin: `${GITLAB_LOGIN_PREFIX}${p.connectionName}`,
            defaultBranch: p.defaultBranch,
          };
        }),
      });
    }

    const flat = builtGroups.flatMap((g) => g.options);
    return { groups: builtGroups, flatOptions: flat };
  }, [deployKeys, gitlabConnections, gitlabProjects, repositories, t, typed, value]);

  // The list starts from its top again as what it lists changes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs when the list changes, which is all it reads
  useEffect(() => setActive(0), [flatOptions.length]);

  const pick = (opt: PickerOption) => {
    onChange(opt.url, { gitLogin: opt.gitLogin, defaultBranch: opt.defaultBranch });
    setOpen(false);
  };

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement & HTMLTextAreaElement>) => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setOpen(true);
      setActive((a) => Math.min(a + 1, Math.max(flatOptions.length - 1, 0)));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (event.key === "Enter" && open && flatOptions[active] && !event.metaKey && !event.ctrlKey) {
      event.preventDefault();
      pick(flatOptions[active]);
    } else if (event.key === "Escape" && open && groups.length > 0) {
      // Escape closes the list first, and the sheet only once the list is closed.
      event.stopPropagation();
      setOpen(false);
    }
  };

  // Closes when what's focused leaves the field and its list.
  useEffect(() => {
    if (!open) return;
    const away = (e: FocusEvent | MouseEvent) => {
      if (e.target instanceof Node && !wrap.current?.contains(e.target)) setOpen(false);
    };
    document.addEventListener("focusin", away);
    document.addEventListener("mousedown", away);
    return () => {
      document.removeEventListener("focusin", away);
      document.removeEventListener("mousedown", away);
    };
  }, [open]);

  const shows = open && groups.length > 0;
  const activeId = shows && flatOptions[active] ? `${listId}-${active}` : undefined;
  const placeholder = repositories[0]?.url ?? gitlabProjects[0]?.url ?? "https://github.com/acme/web";

  let optionIndex = 0;
  return (
    <div ref={wrap} className="image-field">
      <TextField
        ref={fieldRef}
        label={t`Repository`}
        mono
        placeholder={placeholder}
        value={value}
        error={error}
        hint={hint}
        role={hasAnySource ? "combobox" : undefined}
        aria-expanded={hasAnySource ? shows : undefined}
        aria-controls={hasAnySource ? listId : undefined}
        aria-autocomplete={hasAnySource ? "list" : undefined}
        aria-activedescendant={activeId}
        autoComplete="off"
        spellCheck={false}
        onFocus={() => setOpen(true)}
        onKeyDown={onKeyDown}
        onChange={(event) => {
          onChange(event.target.value);
          setOpen(true);
        }}
      />
      {hasAnySource ? (
        <div id={listId} role="listbox" aria-label={t`Repositories`} className="image-field-list" hidden={!shows}>
          {groups.map((group) => {
            const content = (
              <>
                {group.heading ? (
                  <div className="image-field-group-label" aria-hidden="true">
                    <span>{group.heading}</span>
                    {group.host ? <code>{group.host}</code> : null}
                  </div>
                ) : null}
                {group.note ? <div className="image-field-group-note">{group.note}</div> : null}
                {group.options.map((opt) => {
                  const idx = optionIndex++;
                  return (
                    // Focus stays in the field; aria-activedescendant points at the active option.
                    // biome-ignore lint/a11y/useKeyWithClickEvents: the field moves through the list with the arrow keys, and Enter picks
                    // biome-ignore lint/a11y/useFocusableInteractive: see above
                    <div
                      key={opt.key}
                      id={`${listId}-${idx}`}
                      role="option"
                      aria-selected={idx === active}
                      className={cx("image-field-option", idx === active && "is-active")}
                      onMouseDown={(event) => event.preventDefault()}
                      onClick={() => pick(opt)}
                    >
                      <span className="image-field-name">
                        <code>{opt.label}</code>
                        {opt.privateRepo ? (
                          <span className="image-field-tag">{t({ message: "Private", context: "a repository" })}</span>
                        ) : null}
                        {opt.replacedKeyName ? (
                          <span className="image-field-tag">{t`Uses GitLab, not ${opt.replacedKeyName}`}</span>
                        ) : null}
                      </span>
                    </div>
                  );
                })}
              </>
            );
            return group.heading ? (
              // biome-ignore lint/a11y/useSemanticElements: ARIA 1.2 listbox option groups use role="group", not <fieldset>
              <div key={group.key} role="group" aria-label={group.heading} className="image-field-group">
                {content}
              </div>
            ) : (
              <div key={group.key}>{content}</div>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}
