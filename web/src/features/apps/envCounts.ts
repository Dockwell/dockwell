// SPDX-License-Identifier: AGPL-3.0-only

import { plural } from "@lingui/core/macro";

/**
 * How many values an app is given, as its Overview and its Environment both say it
 * (#797): each variable, shared or its own, and each secret, leaving out a none.
 */
export function envCountWords(plain: number, secrets: number): string {
  return [
    plain > 0 ? plural(plain, { one: "# variable", other: "# variables" }) : "",
    secrets > 0 ? plural(secrets, { one: "# secret", other: "# secrets" }) : "",
  ]
    .filter(Boolean)
    .join(" · ");
}
