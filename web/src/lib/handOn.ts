// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from "react";

/** When a sheet last handed on to another in its place. */
let handedAt = 0;

/** How soon after the hand-on the next sheet still takes the place of the last. */
const WITHIN_MS = 1000;

/**
 * A sheet that hands on to another in its place, as New app's catalog does to an
 * app's install form and that form back to the catalog (#617): the one leaving says
 * so, and leaves no copy sliding out, and the one coming, mounted at once after,
 * comes without its slide, so the two read as one sheet that changed.
 */
export function useHandOn(): { handing: boolean; inPlace: boolean; handOn: () => void } {
  const [handing, setHanding] = useState(false);
  // Read once, on mount: whether this sheet takes another's place.
  const [inPlace] = useState(() => Date.now() - handedAt < WITHIN_MS);
  return {
    handing,
    inPlace,
    handOn: () => {
      handedAt = Date.now();
      setHanding(true);
    },
  };
}
