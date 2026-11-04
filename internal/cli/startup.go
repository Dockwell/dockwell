// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"log/slog"

	"github.com/dockwell/dockwell/internal/footprint"
)

// releaseStartup is what dockwell run does once its agent first connects:
// it lets go of the pages of the binary that starting up mapped and an
// idle server doesn't touch again, most of them the initialization of code
// for the other role (docs/adr/0017-release-the-pages-startup-maps.md).
func releaseStartup(logger *slog.Logger) func() {
	return func() {
		before, after, err := footprint.ReleaseStartupPages()
		switch {
		case errors.Is(err, footprint.ErrUnsupported):
		case err != nil:
			logger.Debug("kept the pages startup mapped", slog.String("error", err.Error()))
		default:
			logger.Debug("released the pages startup mapped",
				slog.Int64("rss_before_bytes", before), slog.Int64("rss_after_bytes", after))
		}
	}
}
