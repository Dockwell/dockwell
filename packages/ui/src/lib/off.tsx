// SPDX-License-Identifier: AGPL-3.0-only

import {
  createContext,
  type MouseEvent,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useId,
  useMemo,
  useState,
} from "react";

import "./off.css";

/**
 * A control someone may not use, and why. It stays where it is and in the tab
 * order, so a keyboard reaches it and hears why; it's `aria-disabled`, not
 * `disabled`, with its reason as its description, shown beside it on hover and
 * on focus, and its click does nothing. "Disabled controls say why nearby"
 * (design/system/README.md).
 */
export interface Off {
  /** Spread onto the control. Empty when it isn't off. */
  props: {
    "aria-disabled"?: true;
    "aria-describedby"?: string;
    "data-off"?: "";
    onClickCapture?: (event: MouseEvent) => void;
  };
  /** The reason, to render inside the control. Nothing when it isn't off. */
  why: ReactNode;
}

type Report = (id: string, reason: string | undefined) => void;

const OffReasons = createContext<Report | undefined>(undefined);

/** Swallows a click on a control that's off, before any handler of its own or its form's. */
function swallow(event: MouseEvent) {
  event.preventDefault();
  event.stopPropagation();
}

/**
 * What a control needs to be off with a reason: pass the reason, or nothing
 * when it isn't off. It tells the screen it's in, so a screen with no hover
 * can say the first reason once (useOffReasons).
 */
export function useOff(reason: string | undefined): Off {
  const id = useId();
  const report = useContext(OffReasons);
  useEffect(() => {
    if (!report || !reason) return;
    report(id, reason);
    return () => report(id, undefined);
  }, [report, id, reason]);
  return useMemo(() => {
    if (!reason) return { props: {}, why: null };
    return {
      props: { "aria-disabled": true, "aria-describedby": id, "data-off": "", onClickCapture: swallow },
      // Hidden from the control's name, and still its description, which names it by ID.
      why: (
        <span id={id} className="dw-off-why" aria-hidden="true">
          {reason}
        </span>
      ),
    };
  }, [id, reason]);
}

/**
 * Collects why the controls inside `Provider` are off. `first` is the reason of
 * the one that said so first, for a screen to say once where nothing hovers.
 */
export function useOffReasons(): {
  Provider: (props: { children: ReactNode }) => ReactNode;
  first: string | undefined;
} {
  const [reasons, setReasons] = useState<ReadonlyMap<string, string>>(new Map());
  const report = useCallback<Report>((id, reason) => {
    setReasons((was) => {
      if (was.get(id) === reason) return was;
      const next = new Map(was);
      if (reason) next.set(id, reason);
      else next.delete(id);
      return next;
    });
  }, []);
  const Provider = useCallback(
    ({ children }: { children: ReactNode }) => <OffReasons.Provider value={report}>{children}</OffReasons.Provider>,
    [report],
  );
  return { Provider, first: reasons.values().next().value };
}
