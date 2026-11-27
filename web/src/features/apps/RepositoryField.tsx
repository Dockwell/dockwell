// SPDX-License-Identifier: AGPL-3.0-only

import type { GitRepository } from "@dockwell/api";
import { cx, TextField } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { type KeyboardEvent, type ReactNode, type Ref, useEffect, useId, useMemo, useRef, useState } from "react";

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

/**
 * A repository, picked rather than typed (ADR 0098): focusing the field lists the
 * repositories the project's GitHub App installations reach, private ones too, and
 * typing narrows them by name. Any repository's URL can still be typed or pasted.
 */
export function RepositoryField({
  value,
  onChange,
  repositories,
  error,
  hint,
  fieldRef,
}: {
  value: string;
  onChange: (url: string) => void;
  /** What the project's installations reach, as GitHub last listed them. */
  repositories: readonly GitRepository[];
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

  const options = useMemo(() => {
    // A repository picked or pasted whole lists nothing more.
    if (repositories.some((r) => sameRepository(r.url, value))) return [];
    const words = typed.replace(/^https?:\/\//, "").replace(/^[^/]*github[^/]*\//, "");
    return repositories.filter((r) => words === "" || r.fullName.toLowerCase().includes(words)).slice(0, 50);
  }, [repositories, typed, value]);

  // The list starts from its top again as what it lists changes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs when the list changes, which is all it reads
  useEffect(() => setActive(0), [options.length]);

  const pick = (r: GitRepository) => {
    onChange(r.url);
    setOpen(false);
  };

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement & HTMLTextAreaElement>) => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setOpen(true);
      setActive((a) => Math.min(a + 1, Math.max(options.length - 1, 0)));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (event.key === "Enter" && open && options[active] && !event.metaKey && !event.ctrlKey) {
      event.preventDefault();
      pick(options[active]);
    } else if (event.key === "Escape" && open && options.length > 0) {
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

  const shows = open && options.length > 0;
  const activeId = shows ? `${listId}-${active}` : undefined;
  return (
    <div ref={wrap} className="image-field">
      <TextField
        ref={fieldRef}
        label={t`Repository`}
        mono
        placeholder={repositories[0]?.url ?? "https://github.com/acme/web"}
        value={value}
        error={error}
        hint={hint}
        role={repositories.length > 0 ? "combobox" : undefined}
        aria-expanded={repositories.length > 0 ? shows : undefined}
        aria-controls={repositories.length > 0 ? listId : undefined}
        aria-autocomplete={repositories.length > 0 ? "list" : undefined}
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
      {repositories.length > 0 ? (
        <div id={listId} role="listbox" aria-label={t`Repositories`} className="image-field-list" hidden={!shows}>
          {options.map((r, i) => (
            // Focus stays in the field; aria-activedescendant points at the active option.
            // biome-ignore lint/a11y/useKeyWithClickEvents: the field moves through the list with the arrow keys, and Enter picks
            // biome-ignore lint/a11y/useFocusableInteractive: see above
            <div
              key={r.url}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              className={cx("image-field-option", i === active && "is-active")}
              onMouseDown={(event) => event.preventDefault()}
              onClick={() => pick(r)}
            >
              <span className="image-field-name">
                <code>{r.fullName}</code>
                {r.private ? (
                  <span className="image-field-tag">{t({ message: "Private", context: "a repository" })}</span>
                ) : null}
              </span>
            </div>
          ))}
        </div>
      ) : null}
    </div>
  );
}
