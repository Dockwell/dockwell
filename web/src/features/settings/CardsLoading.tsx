// SPDX-License-Identifier: AGPL-3.0-only

import { Loading, Skeleton } from "@dockwell/ui";

/** Cards on their way, in a card's own parts: its mark and state, its name and line, its action. */
export function CardsLoading({ label, cards = 2 }: { label: string; cards?: number }) {
  const keys = Array.from({ length: cards }, (_, i) => `card-${i + 1}`);
  return (
    <Loading label={label} className="integrations-grid">
      {keys.map((key, index) => (
        <div key={key} className="dw-panel integrations-card" aria-hidden="true">
          <div className="integrations-card-head">
            <Skeleton shape="block" width={32} height={32} />
            <Skeleton shape="pill" width={72} />
          </div>
          <div className="integrations-card-text">
            <Skeleton text="label" width={[96, 120, 80][index % 3]} />
            <Skeleton width="88%" />
          </div>
          <Skeleton shape="block" width={96} height={28} />
        </div>
      ))}
    </Loading>
  );
}
