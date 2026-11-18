// SPDX-License-Identifier: AGPL-3.0-only

import { cx } from "@dockwell/ui";

/** How much of a long name's end stays in sight, as servers differ there most. */
const TAIL = 8;

/**
 * A name that's too long for its line, shortened in the middle, so both its start and
 * its end stay in sight, with the whole of it on hover (#821). Its text is the whole
 * name, for copying and for anyone reading the page out.
 */
export function MiddleName({ name, className }: { name: string; className?: string }) {
  const long = name.length > TAIL * 2;
  return (
    <span className={cx("middle-name", className)} title={name}>
      <span className="middle-name-head">{long ? name.slice(0, -TAIL) : name}</span>
      {long ? <span className="middle-name-tail">{name.slice(-TAIL)}</span> : null}
    </span>
  );
}

/** A line that names a server, with the name shortened in the middle where it's long. */
export function NamedLine({ text, name }: { text: string; name?: string }) {
  const at = name ? text.indexOf(name) : -1;
  if (!name || at < 0) return <>{text}</>;
  return (
    <>
      {text.slice(0, at)}
      <MiddleName name={name} />
      {text.slice(at + name.length)}
    </>
  );
}
