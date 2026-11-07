// SPDX-License-Identifier: AGPL-3.0-only

import { Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { PointerEvent, ReactNode } from "react";

import type { BarKey } from "./TerminalScreen";

export interface KeyBarProps {
  /** Presses a key that isn't a character. */
  onPress: (key: BarKey) => void;
  /** Types a character. */
  onType: (text: string) => void;
  /** Whether Ctrl is held for the next key. */
  ctrl: boolean;
  onCtrl: (on: boolean) => void;
  onTextSize: () => void;
  onPaste: () => void;
}

/**
 * What a phone's keyboard hasn't: the keys a shell wants, as M-Terminal has them,
 * in a row that scrolls, then text size and Paste. Pressing one keeps the terminal
 * focused, so the keyboard stays up.
 */
export function KeyBar({ onPress, onType, ctrl, onCtrl, onTextSize, onPaste }: KeyBarProps) {
  const { t } = useLingui();
  return (
    <div className="terminal-keybar">
      <div role="toolbar" aria-label={t`Terminal keys, scroll for more`} className="terminal-keys">
        <Key label={t`Escape`} onPress={() => onPress("escape")}>
          <Trans>Esc</Trans>
        </Key>
        <Key label={t`Tab`} onPress={() => onPress("tab")}>
          <Trans>Tab</Trans>
        </Key>
        <Key label={t`Control`} pressed={ctrl} onPress={() => onCtrl(!ctrl)}>
          <Trans>Ctrl</Trans>
        </Key>
        <Key label={t`Left arrow`} square onPress={() => onPress("left")}>
          <Icon name="arrow-left" size={18} />
        </Key>
        <Key label={t`Up arrow`} square onPress={() => onPress("up")}>
          <span className="terminal-key-turn" style={{ transform: "rotate(-90deg)" }}>
            <Icon name="arrow-right" size={18} />
          </span>
        </Key>
        <Key label={t`Down arrow`} square onPress={() => onPress("down")}>
          <span className="terminal-key-turn" style={{ transform: "rotate(90deg)" }}>
            <Icon name="arrow-right" size={18} />
          </span>
        </Key>
        <Key label={t`Right arrow`} square onPress={() => onPress("right")}>
          <Icon name="arrow-right" size={18} />
        </Key>
        <Key label={t`Pipe`} square mono onPress={() => onType("|")}>
          |
        </Key>
        <Key label={t`Tilde`} square mono onPress={() => onType("~")}>
          ~
        </Key>
      </div>
      <span className="terminal-keybar-rule" aria-hidden="true" />
      <button
        type="button"
        className="terminal-keybar-size"
        aria-label={t`Text size`}
        onPointerDown={keep}
        onClick={onTextSize}
      >
        <span className="terminal-keybar-size-small">A</span>
        <span className="terminal-keybar-size-large">a</span>
      </button>
      <button type="button" className="terminal-keybar-paste" onPointerDown={keep} onClick={onPaste}>
        <Trans>Paste</Trans>
      </button>
    </div>
  );
}

/** Keeps the terminal's focus as a key is pressed, so a phone's keyboard stays up. */
function keep(e: PointerEvent) {
  e.preventDefault();
}

interface KeyProps {
  label: string;
  onPress: () => void;
  /** A held key, such as Ctrl: whether it's held. */
  pressed?: boolean;
  square?: boolean;
  mono?: boolean;
  children: ReactNode;
}

function Key({ label, onPress, pressed, square = false, mono = false, children }: KeyProps) {
  return (
    <button
      type="button"
      className={["terminal-key", square ? "terminal-key-square" : "", mono ? "terminal-key-mono" : ""]
        .filter(Boolean)
        .join(" ")}
      aria-label={label}
      aria-pressed={pressed}
      onPointerDown={keep}
      onClick={onPress}
    >
      {children}
    </button>
  );
}
