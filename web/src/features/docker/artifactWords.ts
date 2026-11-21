// SPDX-License-Identifier: AGPL-3.0-only

import { DockerCleanupKind, DockerCleanupReason, DockerReceiptReason } from "@dockwell/api";
import type { IconName } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

export function useDockerArtifactWords() {
  const { t } = useLingui();
  return {
    kind(kind: DockerCleanupKind): string {
      switch (kind) {
        case DockerCleanupKind.IMAGE:
          return t`Images`;
        case DockerCleanupKind.CONTAINER:
          return t`Writable layers`;
        case DockerCleanupKind.VOLUME:
          return t`Volumes`;
        case DockerCleanupKind.BUILD_CACHE:
          return t`Build cache`;
        case DockerCleanupKind.LOG:
          return t`Logs`;
        case DockerCleanupKind.NETWORK:
          return t`Networks`;
        default:
          return t`Docker artifacts`;
      }
    },
    icon(kind: DockerCleanupKind): IconName {
      switch (kind) {
        case DockerCleanupKind.IMAGE:
          return "archive";
        case DockerCleanupKind.CONTAINER:
          return "service";
        case DockerCleanupKind.VOLUME:
          return "database";
        case DockerCleanupKind.BUILD_CACHE:
          return "layers";
        case DockerCleanupKind.LOG:
          return "file";
        case DockerCleanupKind.NETWORK:
          return "network";
        default:
          return "archive";
      }
    },
    receiptReason(reason: DockerReceiptReason | undefined): string {
      switch (reason) {
        case DockerReceiptReason.IMAGE_ID_REFUSED:
          return t`Docker refused to remove this image by ID`;
        case DockerReceiptReason.EXTRA_CACHE_REMOVED:
          return t`Docker removed other build cache records`;
        case DockerReceiptReason.CACHE_UNVERIFIED:
          return t`The build cache removal could not be verified`;
        default:
          return "";
      }
    },
    reason(reason: DockerCleanupReason, kind: DockerCleanupKind): string {
      switch (reason) {
        case DockerCleanupReason.DANGLING:
          return t`Dangling`;
        case DockerCleanupReason.UNUSED:
          return t`Unused`;
        case DockerCleanupReason.IDLE:
          return t`Idle build cache`;
        case DockerCleanupReason.IN_USE:
          return kind === DockerCleanupKind.BUILD_CACHE ? t`In use by builds` : t`In use`;
        case DockerCleanupReason.ROLLBACK:
          return t`Needed for rollback or deployment`;
        case DockerCleanupReason.DESIRED:
          return t`Managed or built-in`;
        case DockerCleanupReason.TAKE_ON:
          return t`Take on before deletion`;
        case DockerCleanupReason.RETAINED:
          return kind === DockerCleanupKind.LOG
            ? t`Logs are never removed here`
            : t`Volumes require take-on before deletion`;
        case DockerCleanupReason.EXACT_REMOVAL_UNAVAILABLE:
          if (kind === DockerCleanupKind.BUILD_CACHE)
            return t`This Docker version can't remove one cache record at a time`;
          if (kind === DockerCleanupKind.NETWORK) return t`Only local networks can be removed here`;
          return t`This image has tags that could not all be reviewed`;
        case DockerCleanupReason.MULTIPLE_REPOSITORIES:
          return t`Tagged in more than one repository`;
        case DockerCleanupReason.MULTIPLE_TAGS:
          return t`Has more than one tag`;
        case DockerCleanupReason.DISABLED:
          return t`Cleanup disabled after a build cache removal`;
        case DockerCleanupReason.SHARED:
          return t`Shared storage`;
        default:
          return t`Unknown eligibility`;
      }
    },
  };
}
