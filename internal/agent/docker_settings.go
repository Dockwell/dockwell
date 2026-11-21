// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/resource/dockerdaemon"
)

func (c *calls) dockerSettings(ctx context.Context, ask *agentv1.DockerSettingsCall, fail func(error) *agentv1.CallResult) *agentv1.CallResult {
	d := c.cfg.DockerSettings
	if d == nil {
		d = &dockerdaemon.Driver{Client: c.cfg.Docker}
	}
	answer := &agentv1.DockerSettingsResult{}
	switch {
	case ask.GetRead():
		settings, err := d.Read(ctx)
		if err != nil {
			return fail(errors.New("the Docker settings could not be read"))
		}
		if ask.GetLoggingSummary() {
			d.ReadLoggingSummary(ctx, settings)
		}
		answer.Output = &agentv1.DockerSettingsResult_Settings{Settings: settings}
	case ask.GetCheck() != nil:
		answer.Output = &agentv1.DockerSettingsResult_Checked{Checked: d.Check(ctx, ask.GetCheck())}
	default:
		return fail(errDockerInventory)
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_DockerSettings{DockerSettings: answer}}
}
