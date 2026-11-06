// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Tag } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { isPrivateAddress } from "./server";

import "./ServerAddress.css";

/** Says an address is private, as the internet can't reach a server there, and nothing for one that isn't. */
export function PrivateTag({ address }: { address: string }) {
  const { t } = useLingui();
  return isPrivateAddress(address) ? <Tag words>{t`Private`}</Tag> : null;
}

/** A server's address, as it's shown beside its name. One that's private is never shown as if it were where to reach it. */
export function ServerAddress({ address, className }: { address: string; className?: string }) {
  return (
    <span className={cx("server-address-tagged", className)}>
      {address}
      <PrivateTag address={address} />
    </span>
  );
}
