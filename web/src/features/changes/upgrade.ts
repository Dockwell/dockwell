// SPDX-License-Identifier: AGPL-3.0-only

import {
  type Change,
  type Intent,
  IntentKind,
  type Plan,
  type PlanStep,
  StepPhase,
  type StepProgress,
  StepState,
} from "@dockwell/api";

import { intentOf } from "./plan";

/** What a person follows an update by, in the order it goes (ADR 0080). */
export type UpgradePhaseKey = "backup" | "pull" | "start" | "check";

/** How far a phase is. */
export type UpgradePhaseState = "queued" | "current" | "done" | "failed";

export interface UpgradePhase {
  key: UpgradePhaseKey;
  state: UpgradePhaseState;
  /** What its step says it is doing, while it's the one under way. */
  activity?: string;
  /** How much of a pull is done, and how much it is in all, while it pulls and Docker has said. */
  done?: bigint;
  total?: bigint;
}

/** The plan's intent, where it's an update. */
export function upgradeOf(plan: Plan | undefined): Intent | undefined {
  const intent = intentOf(plan);
  return intent?.kind === IntentKind.UPGRADE ? intent : undefined;
}

/** The tag of an image, or the image where it names none: "16.4" for "postgres:16.4". */
export function tagOf(image: string): string {
  const bare = image.split("@")[0] ?? image;
  const slash = bare.lastIndexOf("/");
  const colon = bare.lastIndexOf(":");
  return colon > slash ? bare.slice(colon + 1) : "latest";
}

/** What an update goes to, as a person names it: the release, or the image's tag. */
export function upgradeTo(intent: Intent): string {
  return intent.release || tagOf(intent.image);
}

/** What an update leaves, as a person names it. */
export function upgradeFrom(intent: Intent): string {
  return intent.fromRelease || tagOf(intent.fromImage);
}

/** A digest as a person reads it: its first 12 hex digits. */
export function shortDigest(digest: string): string {
  return digest.replace(/^sha256:/, "").slice(0, 12);
}

/** Where a failure of a service's step comes down to, by its reason, as a phase. */
function failedIn(reason: string): UpgradePhaseKey {
  switch (reason) {
    case "image":
    case "registry":
    case "disk":
      return "pull";
    case "unhealthy":
    case "exits":
      return "check";
  }
  return "start";
}

const ORDER: readonly UpgradePhaseKey[] = ["backup", "pull", "start", "check"];

function phaseKey(phase: StepPhase): UpgradePhaseKey | undefined {
  switch (phase) {
    case StepPhase.BACKING_UP:
      return "backup";
    case StepPhase.PULLING:
      return "pull";
    case StepPhase.STARTING:
      return "start";
    case StepPhase.CHECKING:
      return "check";
  }
  return undefined;
}

interface Stepped {
  plan: PlanStep;
  progress: StepProgress | undefined;
}

/**
 * The phases of an update, from its plan and how its steps are going: a backup
 * first where it takes one, then the pull, the start and the health check of what
 * it replaces. A plan not yet applied has them all to come.
 */
export function upgradePhases(plan: Plan | undefined, change: Change | undefined): UpgradePhase[] {
  const progress = change?.steps ?? [];
  const steps: Stepped[] = (plan?.servers ?? []).flatMap((server) =>
    server.steps.map((step) => ({
      plan: step,
      progress: progress.find((p) => p.serverId === server.serverId && p.resourceId === step.resourceId),
    })),
  );
  const backups = steps.filter((s) => s.plan.backupFirst !== undefined);
  const services = steps.filter((s) => s.plan.resource?.kind === "service");
  const out: UpgradePhase[] = [];
  if (backups.length > 0) {
    const states = backups.map((s) => s.progress?.state);
    const running = backups.find((s) => s.progress?.state === StepState.RUNNING);
    out.push({
      key: "backup",
      state: states.includes(StepState.FAILED)
        ? "failed"
        : states.every((s) => s === StepState.DONE)
          ? "done"
          : running
            ? "current"
            : "queued",
      ...(running?.progress?.activity ? { activity: running.progress.activity } : {}),
    });
  }
  const failed = services.find((s) => s.progress?.state === StepState.FAILED);
  const running = services.find((s) => s.progress?.state === StepState.RUNNING);
  const allDone = services.length > 0 && services.every((s) => s.progress?.state === StepState.DONE);
  const at = failed
    ? failedIn(failed.progress?.reason ?? "")
    : running
      ? (phaseKey(running.progress?.phase ?? StepPhase.UNSPECIFIED) ?? "pull")
      : undefined;
  for (const key of ORDER.slice(1)) {
    let state: UpgradePhaseState = "queued";
    if (allDone) state = "done";
    else if (at) {
      const here = ORDER.indexOf(key);
      const there = ORDER.indexOf(at);
      state = here < there ? "done" : here === there ? (failed ? "failed" : "current") : "queued";
    }
    const phase: UpgradePhase = { key, state };
    const p = running?.progress;
    if (state === "current" && p) {
      if (p.activity) phase.activity = p.activity;
      if (key === "pull" && p.totalBytes > 0n) {
        phase.done = p.doneBytes;
        phase.total = p.totalBytes;
      }
    }
    out.push(phase);
  }
  return out;
}
