// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { ImageAnswer, type ImageConfig, ImageService } from "@dockwell/api";
import { cx, Glyph, STATUS, TextField, TONE } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { type KeyboardEvent, type Ref, useEffect, useId, useMemo, useRef, useState } from "react";

import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import "./ImageField.css";

/** How long typing pauses before an image is looked up. */
const LOOK_AFTER_MS = 400;
/** How long a read is good for in this browser, as the agent keeps it for 10 minutes. */
const READ_FOR_MS = 10 * 60_000;

/** A reference a registry could hold, as typed: lowercase name parts, and a tag or digest. */
const REFERENCE =
  /^[a-z0-9]+(?:[._/-][a-z0-9]+)*(?::[0-9]+)?(?:\/[a-z0-9]+(?:[._-][a-z0-9]+)*)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@sha256:[a-f0-9]{64})?$/;

/** A name's repository and what's typed after its tag's colon, if it has one. */
export function splitTag(value: string): { repository: string; tag: string | undefined } {
  const v = value.trim();
  const slash = v.lastIndexOf("/");
  const colon = v.lastIndexOf(":");
  if (colon > slash) return { repository: v.slice(0, colon), tag: v.slice(colon + 1) };
  return { repository: v, tag: undefined };
}

/** Whether a name names a registry other than Docker Hub, which a search can't find. */
function elsewhere(repository: string): boolean {
  const first = repository.split("/")[0] ?? "";
  return repository.includes("/") && (first.includes(".") || first.includes(":") || first === "localhost");
}

/** What a form learns of an image as it's typed. */
export interface ImageRead {
  reference: string;
  digest: string;
  config: ImageConfig | undefined;
}

/** A value that settles a moment after the last change. */
function useSettled<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms]);
  return settled;
}

interface Option {
  value: string;
  label: string;
  /** What it says after the name, such as "latest" or "Official". */
  tag?: string | undefined;
  detail?: string | undefined;
  /** Picking it opens the repository's tags. */
  repository?: boolean;
}

/**
 * An image, picked rather than typed: what's typed searches Docker Hub, a name
 * picked lists its tags newest first, with the one latest points at said, and
 * beside the field it says whether the image is there as the server's pull
 * would find it. Any reference can still be typed. What a found image declares
 * goes to onRead, once for each image, for the form to fill in.
 */
export function ImageField({
  value,
  onChange,
  environment,
  server,
  error,
  fieldRef,
  onRead,
}: {
  value: string;
  onChange: (value: string) => void;
  /** The environment the app is for, by ID, which says who may read. */
  environment: string;
  /** The server it will run on, by ID; undefined while there is only one. */
  server: string | undefined;
  error?: string | undefined;
  fieldRef?: Ref<HTMLInputElement>;
  onRead?: (read: ImageRead) => void;
}) {
  const { t } = useLingui();
  const time = useTimeWords();
  const id = useId();
  const listId = `${id}-list`;
  const stateId = `${id}-state`;
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const wrap = useRef<HTMLDivElement>(null);
  const typed = value.trim();
  const { repository, tag } = splitTag(typed);
  const where = { environment, server: server ?? "" };
  // Reading an image is the start of planning an app, which is a developer's (ADR 0059).
  const may = useMay();
  const canSearch = may(ImageService.method.searchImages).allowed;
  const canList = may(ImageService.method.listImageTags).allowed;
  const canRead = may(ImageService.method.getImage).allowed;

  // Names while there's no tag, of Docker Hub's alone; a repository's tags after its colon.
  const term = useSettled(tag === undefined && !elsewhere(repository) ? repository : "", LOOK_AFTER_MS);
  const search = useQuery(
    ImageService.method.searchImages,
    { term, ...where },
    { enabled: canSearch && open && term.length >= 2, staleTime: READ_FOR_MS, retry: false },
  );
  const tagsOf = tag === undefined ? "" : repository;
  const tags = useQuery(
    ImageService.method.listImageTags,
    { repository: tagsOf, ...where },
    { enabled: canList && open && tagsOf !== "" && REFERENCE.test(tagsOf), staleTime: READ_FOR_MS, retry: false },
  );

  // Whether it's there, once typing pauses on what could be a reference.
  const settled = useSettled(typed, LOOK_AFTER_MS);
  const checks = canRead && settled !== "" && REFERENCE.test(settled) && !settled.endsWith(":");
  const image = useQuery(
    ImageService.method.getImage,
    { reference: settled, ...where },
    { enabled: checks, staleTime: READ_FOR_MS, retry: false },
  );
  const read = settled === typed && checks ? image.data : undefined;
  const sent = useRef("");
  useEffect(() => {
    if (!read || read.answer !== ImageAnswer.FOUND || !onRead) return;
    const key = `${settled}@${read.digest}`;
    if (sent.current === key) return;
    sent.current = key;
    onRead({ reference: settled, digest: read.digest, config: read.config });
  }, [read, settled, onRead]);

  const options = useMemo<Option[]>(() => {
    if (tag !== undefined) {
      const wanted = tag.toLowerCase();
      return (tags.data?.tags ?? [])
        .filter((x) => x.name.toLowerCase().startsWith(wanted))
        .slice(0, 50)
        .map((x) => ({
          value: `${repository}:${x.name}`,
          label: x.name,
          tag: x.latest && x.name !== "latest" ? t`latest` : undefined,
          detail: x.pushTime ? time.ago(new Date(Number(x.pushTime.seconds) * 1000), Date.now()) : undefined,
        }));
    }
    return (search.data?.matches ?? []).map((m) => ({
      value: m.name,
      label: m.name,
      tag: m.official ? t({ message: "Official", context: "a Docker Hub image" }) : undefined,
      detail: m.description,
      repository: true,
    }));
  }, [tag, tags.data, search.data, repository, t, time]);

  // The list starts from its top again as what it lists changes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs when the list changes, which is all it reads
  useEffect(() => setActive(0), [options.length, tag]);

  const pick = (o: Option) => {
    if (o.repository) {
      // A name picked goes on to its tags.
      onChange(`${o.value}:`);
      setOpen(true);
      return;
    }
    onChange(o.value);
    setOpen(false);
  };

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
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

  const state: { kind: "checking" | "found" | "missing" | "unreachable"; word: string } | undefined = (() => {
    if (!checks && typed === "") return undefined;
    if (typed !== settled || (checks && image.isFetching))
      return checks ? { kind: "checking", word: t`Checking` } : undefined;
    if (!read) return undefined;
    switch (read.answer) {
      case ImageAnswer.FOUND:
        return { kind: "found", word: t({ message: "Found", context: "an image in its registry" }) };
      case ImageAnswer.NOT_FOUND:
        return { kind: "missing", word: t`Not found, or private` };
      case ImageAnswer.UNREACHABLE:
        return { kind: "unreachable", word: t`Can't reach the registry` };
    }
    return undefined;
  })();

  const shows = open && options.length > 0;
  const activeId = shows ? `${listId}-${active}` : undefined;
  return (
    <div ref={wrap} className="image-field">
      <TextField
        ref={fieldRef}
        label={t`Image`}
        mono
        value={value}
        placeholder="nginx:1.28"
        error={error}
        role="combobox"
        aria-expanded={shows}
        aria-controls={listId}
        aria-autocomplete="list"
        aria-activedescendant={activeId}
        aria-describedby={state ? stateId : undefined}
        autoComplete="off"
        onFocus={() => setOpen(true)}
        onKeyDown={onKeyDown}
        onChange={(event) => {
          onChange(event.target.value);
          setOpen(true);
        }}
        suffix={
          state ? (
            <span id={stateId} className={cx("image-field-state", `is-${state.kind}`)} aria-live="polite">
              <Glyph
                shape={
                  state.kind === "checking"
                    ? STATUS.deploying.shape
                    : state.kind === "found"
                      ? TONE.healthy.shape
                      : state.kind === "missing"
                        ? TONE.warning.shape
                        : STATUS.unknown.shape
                }
                size={9}
              />
              <span>{state.word}</span>
            </span>
          ) : undefined
        }
      />
      <div
        id={listId}
        role="listbox"
        aria-label={tag !== undefined ? t`Versions` : t`Images found`}
        className="image-field-list"
        hidden={!shows}
      >
        {options.map((o, i) => (
          // Focus stays in the field; aria-activedescendant points at the active option.
          // biome-ignore lint/a11y/useKeyWithClickEvents: the field moves through the list with the arrow keys, and Enter picks
          // biome-ignore lint/a11y/useFocusableInteractive: see above
          <div
            key={o.value}
            id={`${listId}-${i}`}
            role="option"
            aria-selected={i === active}
            className={cx("image-field-option", i === active && "is-active")}
            onMouseDown={(event) => event.preventDefault()}
            onClick={() => pick(o)}
          >
            <span className="image-field-name">
              <code>{o.label}</code>
              {o.tag ? <span className="image-field-tag">{o.tag}</span> : null}
            </span>
            {o.detail ? <span className="image-field-detail">{o.detail}</span> : null}
          </div>
        ))}
      </div>
    </div>
  );
}
