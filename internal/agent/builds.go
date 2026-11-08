// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// buildUpdates sends how a step's build goes on the stream, as BuildUpdate
// messages, which the builder already batches and bounds (ADR 0046).
func (c *calls) buildUpdates(change string, ref resource.Ref) func(resource.BuildUpdate) {
	return func(u resource.BuildUpdate) {
		msg := &agentv1.BuildUpdate{
			ChangeId: change, Resource: resourcepb.Ref(ref), State: buildState(u.State), Dropped: u.Dropped,
			CommitSubject: u.Subject, CommitAuthor: u.Author, Steps: int32(u.Steps), CachedSteps: int32(u.Cached), //nolint:gosec // a build's steps are few
			Step: int32(u.Step), StepName: u.StepName, Error: u.Error, FailedLine: u.FailedLine, Image: u.Image, //nolint:gosec // as above
		}
		if !u.Start.IsZero() {
			msg.StartTime = timestamppb.New(u.Start)
		}
		if !u.End.IsZero() {
			msg.EndTime = timestamppb.New(u.End)
		}
		for _, l := range u.Lines {
			msg.Lines = append(msg.Lines, &dockwellv1.BuildLine{Number: l.Number, Time: timestamppb.New(l.Time), Step: int32(l.Step), Text: l.Text}) //nolint:gosec // as above
		}
		_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Build{Build: msg}})
	}
}

// buildState is a build's state as the API has it.
func buildState(s resource.BuildState) dockwellv1.BuildState {
	switch s {
	case resource.BuildQueued:
		return dockwellv1.BuildState_BUILD_STATE_QUEUED
	case resource.BuildBuilding:
		return dockwellv1.BuildState_BUILD_STATE_BUILDING
	case resource.BuildDone:
		return dockwellv1.BuildState_BUILD_STATE_DONE
	case resource.BuildFailed:
		return dockwellv1.BuildState_BUILD_STATE_FAILED
	case resource.BuildCancelled:
		return dockwellv1.BuildState_BUILD_STATE_CANCELLED
	}
	return dockwellv1.BuildState_BUILD_STATE_UNSPECIFIED
}
