// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useRef } from "react";

/**
 * How old something heard at wallAt, and at monoAt by a clock that only moves
 * forward, is now. A wall clock stepped back would make it younger for as long as
 * the step, so the forward clock counts; one that doesn't count sleep, as most
 * don't, would make it younger for as long as the sleep, so the wall clock counts
 * too. It is the older of the two.
 */
export function ageOf(wallAt: number, monoAt: number | undefined): number {
  const mono = monoAt === undefined ? Number.NEGATIVE_INFINITY : performance.now() - monoAt;
  return Math.max(mono, Date.now() - wallAt);
}

/** How far the two clocks may drift apart between two looks before it reads as a sleep or a step. */
const JUMP_MS = 2_000;
const LOOK_EVERY_MS = 1_000;

type Listener = () => void;
const listeners = new Set<Listener>();
let looking: ReturnType<typeof setInterval> | undefined;
let last = { wall: 0, mono: 0 };

function tell() {
  for (const each of listeners) each();
}

function onVisible() {
  if (document.visibilityState === "visible") tell();
}

function start() {
  last = { wall: Date.now(), mono: performance.now() };
  // Timers don't run in a sleep, so the first look after one finds the wall
  // clock far on, or its own wait far longer than asked.
  looking = setInterval(() => {
    const now = { wall: Date.now(), mono: performance.now() };
    const wall = now.wall - last.wall;
    const mono = now.mono - last.mono;
    last = now;
    if (Math.abs(wall - mono) > JUMP_MS || mono > LOOK_EVERY_MS + JUMP_MS) tell();
  }, LOOK_EVERY_MS);
  document.addEventListener("visibilitychange", onVisible);
}

function stop() {
  clearInterval(looking);
  looking = undefined;
  document.removeEventListener("visibilitychange", onVisible);
}

/**
 * Calls onWake as the page comes back into view, and as the machine wakes from a
 * sleep or its clock steps, so what holds old news looks at it again: none of
 * these run a timer that was waiting. One look, shared by every screen.
 */
export function useWake(onWake: () => void): void {
  const call = useRef(onWake);
  call.current = onWake;
  useEffect(() => {
    const listener = () => call.current();
    listeners.add(listener);
    if (listeners.size === 1) start();
    return () => {
      listeners.delete(listener);
      if (listeners.size === 0) stop();
    };
  }, []);
}
