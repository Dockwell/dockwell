// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"slices"
	"strings"
	"time"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

func (h *Handler) imageDeployments(ctx context.Context, server, id string, image *v1.DockerImage) error {
	return h.db.View(ctx, func(tx *store.Tx) error {
		rows, err := revision.RetainedDockerImages(ctx, tx, server, time.Now())
		if err != nil {
			return err
		}
		for i := range rows {
			row := &rows[i]
			if row.LocalImageID != id && !slices.Contains(image.GetTags(), row.Reference) && !slices.ContainsFunc(image.GetDigests(), func(ref string) bool {
				_, digest, _ := strings.Cut(ref, "@")
				return row.Digest == digest || row.Reference == ref
			}) {
				continue
			}
			if len(image.Deployments) == 64 {
				image.DetailsTruncated = true
				break
			}
			image.Deployments = append(image.Deployments, &v1.DockerImageDeployment{
				ServiceId: row.ResourceID, ServiceName: row.Name, Revision: int64(row.Revision),
			})
		}
		return nil
	})
}
