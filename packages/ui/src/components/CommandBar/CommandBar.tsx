// SPDX-License-Identifier: AGPL-3.0-only

import {
  Fragment,
  type KeyboardEvent,
  type Ref,
  type RefObject,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from "react";

import { cx } from "../../lib/cx";
import { keyShortcut } from "../../lib/keyShortcut";
import { ModalLayer } from "../../lib/modal";
import { useUIStrings } from "../../lib/strings";
import { PHONE_QUERY, useMediaQuery } from "../../lib/useMediaQuery";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import { Kbd } from "../Kbd/Kbd";
import { fuzzyMatch, splitMatches } from "./fuzzy";
import "./CommandBar.css";

export interface CommandItem {
  id: string;
  /** A full sentence of intent ("Restart api-gateway in staging"), so typing any part of it finds the item. */
  label: string;
  /** Heading the item sits under while the query is empty. */
  group?: string;
  icon?: IconName;
  /** Muted context on the right, such as "main · 8be41d2". */
  hint?: string;
  /**
   * Shortcut that runs the item from anywhere, such as "G F". It shows as key
   * caps, and assistive technology gets it as aria-keyshortcuts where the
   * attribute can say it, never in the option's name.
   */
  kbd?: string;
}

export interface CommandBarProps {
  items: readonly CommandItem[];
  /** Overlay mode: render only while open. */
  open?: boolean;
  /** Called on Escape and on a click outside. */
  onClose?: () => void;
  /** Called with the chosen item. Destructive or production-changing items open a plan; they never run from here. */
  onSelect?: (item: CommandItem) => void;
  placeholder?: string;
  defaultQuery?: string;
  /** Called with the query as it is typed, for items that take what is typed, such as a name to create. */
  onQueryChange?: (query: string) => void;
  /** Footer text naming where commands apply, such as "Acme / storefront". */
  scope?: string;
  /** Render the panel in place, without scrim, portal or fixed positioning. */
  inline?: boolean;
  /** Focus the input when shown. On by default in overlay mode, off inline. */
  autoFocus?: boolean;
  className?: string;
}

/**
 * The keyboard front door: finds any server, app, page or action and runs
 * it. Matching is fuzzy on characters in order. Arrow keys move, Enter runs,
 * Escape closes. In overlay mode focus stays inside until it closes, then
 * returns to where it was. On phones the overlay fills the screen and closes
 * with a Cancel button.
 */
export function CommandBar(props: CommandBarProps) {
  if (props.inline) return <CommandPanel {...props} modal={false} />;
  if (!props.open) return null;
  return <CommandLayer {...props} />;
}

function CommandLayer(props: CommandBarProps) {
  const panelRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  return (
    <ModalLayer
      className="dw-cmd-layer"
      containerRef={panelRef}
      initialFocusRef={props.autoFocus === false ? undefined : inputRef}
      onClose={props.onClose}
    >
      <CommandPanel {...props} modal ref={panelRef} inputRef={inputRef} />
    </ModalLayer>
  );
}

interface CommandPanelProps extends CommandBarProps {
  modal: boolean;
  ref?: Ref<HTMLDivElement>;
  inputRef?: RefObject<HTMLInputElement | null>;
}

interface Result {
  item: CommandItem;
  indexes: readonly number[];
  score: number;
}

function CommandPanel({
  items,
  onClose,
  onSelect,
  placeholder,
  defaultQuery = "",
  onQueryChange,
  scope,
  inline = false,
  autoFocus,
  className,
  modal,
  ref,
  inputRef: givenInputRef,
}: CommandPanelProps) {
  const strings = useUIStrings();
  const phone = useMediaQuery(PHONE_QUERY);
  const ownInputRef = useRef<HTMLInputElement>(null);
  const inputRef = givenInputRef ?? ownInputRef;
  const listRef = useRef<HTMLDivElement>(null);
  const listId = useId();
  const [query, setQuery] = useState(defaultQuery);
  const [active, setActive] = useState(0);
  const searching = query.trim() !== "";

  const results = useMemo<Result[]>(() => {
    if (!searching) return items.map((item) => ({ item, indexes: [], score: 0 }));
    return items
      .flatMap((item) => {
        const match = fuzzyMatch(query, item.label);
        return match ? [{ item, ...match }] : [];
      })
      .sort((a, b) => b.score - a.score);
  }, [items, query, searching]);

  const activeIndex = results.length === 0 ? -1 : Math.min(Math.max(active, 0), results.length - 1);
  const optionId = (index: number) => `${listId}-option-${index}`;

  useEffect(() => {
    if (inline && autoFocus === true) inputRef.current?.focus();
  }, [inline, autoFocus, inputRef]);

  useEffect(() => {
    if (activeIndex < 0) return;
    const option = listRef.current?.querySelector<HTMLElement>("[data-active]");
    option?.scrollIntoView?.({ block: "nearest" });
  }, [activeIndex]);

  const run = (result: Result | undefined) => {
    if (result) onSelect?.(result.item);
  };

  const onInputKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    // Keys that confirm an input method's candidate belong to the input method.
    // Safari ends composition before keydown, so it only shows in keyCode 229.
    if (event.nativeEvent.isComposing || event.keyCode === 229) return;
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setActive(Math.max(0, Math.min(results.length - 1, activeIndex + 1)));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setActive(Math.max(0, activeIndex - 1));
    } else if (event.key === "Enter") {
      event.preventDefault();
      // Ctrl+Enter goes on through a flow elsewhere. Here it would run whatever is highlighted, unasked.
      if (!event.metaKey && !event.ctrlKey) run(results[activeIndex]);
    } else if (event.key === "Escape" && !modal && onClose) {
      // In overlay mode the layer handles Escape.
      event.preventDefault();
      onClose();
    }
  };

  // Consecutive results that share a group form a section. Groups show only while the query is empty.
  const sections: { group: string | undefined; entries: { result: Result; index: number }[] }[] = [];
  results.forEach((result, index) => {
    const group = searching ? undefined : result.item.group;
    const last = sections[sections.length - 1];
    if (last && last.group === group) last.entries.push({ result, index });
    else sections.push({ group, entries: [{ result, index }] });
  });

  const option = ({ result, index }: { result: Result; index: number }) => {
    const isActive = index === activeIndex;
    return (
      // Focus stays in the input; aria-activedescendant points at the active option.
      // biome-ignore lint/a11y/useKeyWithClickEvents: the combobox input handles the keys
      // biome-ignore lint/a11y/useFocusableInteractive: see above
      <div
        key={result.item.id}
        id={optionId(index)}
        role="option"
        aria-selected={isActive}
        aria-keyshortcuts={result.item.kbd ? keyShortcut(result.item.kbd) : undefined}
        data-active={isActive ? "" : undefined}
        className={cx("dw-cmd-item", isActive && "is-active")}
        onMouseMove={() => {
          if (!isActive) setActive(index);
        }}
        onMouseDown={(event) => event.preventDefault()}
        onClick={() => run(result)}
      >
        <Icon name={result.item.icon ?? "arrow-right"} size={16} />
        <span className="dw-cmd-label">
          {splitMatches(result.item.label, result.indexes).map((part, i) =>
            // The parts of one label never reorder, so the index is a stable key.
            // biome-ignore lint/suspicious/noArrayIndexKey: see above
            part.match ? <mark key={i}>{part.text}</mark> : <Fragment key={i}>{part.text}</Fragment>,
          )}
        </span>
        {/* The space keeps the hint a word apart in the option's name. */}
        {result.item.hint ? (
          <>
            {" "}
            <span className="dw-cmd-hint">{result.item.hint}</span>
          </>
        ) : null}
        {/* Kept out of the name, so the option reads "Fleet", not "FleetG F". */}
        {result.item.kbd ? <Kbd aria-hidden="true">{result.item.kbd}</Kbd> : null}
      </div>
    );
  };

  const shownPlaceholder = placeholder ?? strings.commandBar.placeholder;
  return (
    <div
      ref={ref}
      className={cx("dw-cmd", inline && "is-inline", className)}
      {...(modal
        ? { role: "dialog", "aria-modal": true, "aria-label": strings.commandBar.label }
        : { role: "search", "aria-label": strings.commandBar.label })}
      tabIndex={modal ? -1 : undefined}
    >
      <div className="dw-cmd-input-row">
        <Icon name="search" size={18} />
        <input
          ref={inputRef}
          className="dw-cmd-input"
          type="text"
          role="combobox"
          aria-label={shownPlaceholder}
          aria-autocomplete="list"
          aria-expanded={results.length > 0}
          aria-controls={listId}
          aria-activedescendant={activeIndex >= 0 ? optionId(activeIndex) : undefined}
          autoComplete="off"
          spellCheck={false}
          placeholder={shownPlaceholder}
          value={query}
          onChange={(event) => {
            setQuery(event.target.value);
            setActive(0);
            onQueryChange?.(event.target.value);
          }}
          onKeyDown={onInputKeyDown}
        />
        {onClose && modal && phone ? (
          <button type="button" className="dw-cmd-cancel" onClick={onClose}>
            {strings.commandBar.cancel}
          </button>
        ) : onClose ? (
          <span aria-hidden="true">
            <Kbd>esc</Kbd>
          </span>
        ) : null}
      </div>
      <div ref={listRef} className="dw-cmd-list">
        <div
          id={listId}
          role="listbox"
          aria-label={strings.commandBar.results}
          className="dw-cmd-options"
          hidden={results.length === 0}
        >
          {sections.flatMap((section, i) =>
            section.group === undefined
              ? section.entries.map(option)
              : [
                  // Inside a listbox, options are grouped with the ARIA group role.
                  // biome-ignore lint/a11y/useSemanticElements: a fieldset can't sit in a listbox
                  <div
                    key={`group-${section.entries[0]?.result.item.id}`}
                    role="group"
                    aria-labelledby={`${listId}-group-${i}`}
                    className="dw-cmd-section"
                  >
                    <div id={`${listId}-group-${i}`} className="dw-cmd-group" aria-hidden="true">
                      {section.group}
                    </div>
                    {section.entries.map(option)}
                  </div>,
                ],
          )}
        </div>
        <div role="status">
          {searching && results.length === 0 ? (
            <div className="dw-cmd-empty">{strings.commandBar.empty(query)}</div>
          ) : null}
        </div>
      </div>
      <div className={cx("dw-cmd-foot", !scope && "is-keys-only")}>
        <span aria-hidden="true">
          <Kbd>↑</Kbd>
          <Kbd>↓</Kbd> {strings.commandBar.move}
        </span>
        <span aria-hidden="true">
          <Kbd>↵</Kbd> {strings.commandBar.run}
        </span>
        {scope ? <span className="dw-cmd-scope">{strings.commandBar.scope(scope)}</span> : null}
      </div>
    </div>
  );
}
