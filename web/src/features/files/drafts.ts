// SPDX-License-Identifier: AGPL-3.0-only

/** What the server has of a file, as the field shows it, and what the next save goes over. */
export interface Base {
  text: string;
  version: string;
  /** The file's text as it was read, whose line endings a save keeps. */
  original: string;
  /** Where the path led when this was read: a save goes only there. */
  where: string;
  /** Saving it takes a passkey, as it's a way back onto the server. */
  wayBackIn: boolean;
}

/** What someone typed and hasn't saved, and what it was typed over. */
export interface Draft {
  /** Who typed it. */
  who: string;
  text: string;
  base: Base;
}

const drafts = new Map<string, Draft>();

const key = (serverId: string, path: string) => `${serverId}\0${path}`;

/**
 * Keeps what's being typed while the page around it goes, as it does when a session
 * ends and the sign-in gate takes its place. It lives in memory only, so file text
 * never reaches the browser's storage, and a tab that's closed takes it along, which
 * leaving the page asks about first.
 */
export function holdDraft(serverId: string, path: string, draft: Draft): void {
  drafts.set(key(serverId, path), draft);
}

/** What was kept of this file for who, if anything. Someone else's goes, unread. */
export function heldDraft(serverId: string, path: string, who: string): Draft | undefined {
  const draft = drafts.get(key(serverId, path));
  if (draft?.who === who) return draft;
  dropDraft(serverId, path);
  return undefined;
}

export function dropDraft(serverId: string, path: string): void {
  drafts.delete(key(serverId, path));
}

/** Lets go of them all, as a test starts from none. */
export function dropDrafts(): void {
  drafts.clear();
}
