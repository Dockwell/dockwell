// SPDX-License-Identifier: AGPL-3.0-only

export function shortImageId(id: string): string {
  return id.replace(/^sha256:/, "").slice(0, 12);
}
