// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useState } from "react";

/**
 * State that the parent may own. When value is defined the component is
 * controlled and only reports changes; otherwise it keeps its own state,
 * starting from defaultValue. onChange runs in both modes. Next narrows what
 * may be set, for state that starts empty but is only ever set to a value.
 */
export function useControlled<T, Next extends T = T>(
  value: T | undefined,
  defaultValue: T,
  onChange?: (next: Next) => void,
): [T, (next: Next) => void] {
  const [inner, setInner] = useState(defaultValue);
  const controlled = value !== undefined;
  const set = useCallback(
    (next: Next) => {
      if (!controlled) setInner(next);
      onChange?.(next);
    },
    [controlled, onChange],
  );
  return [controlled ? value : inner, set];
}
