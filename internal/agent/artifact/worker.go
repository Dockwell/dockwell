// SPDX-License-Identifier: AGPL-3.0-only

package artifact

import (
	"context"
	"errors"
	"os"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// TakeValidation serializes validation and Engine archive operations.
func (s *Spool) TakeValidation(ctx context.Context) (func(), error) {
	select {
	case s.validation <- struct{}{}:
		return func() { <-s.validation }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// QueueSeal journals validation before acknowledging Finish. The worker is
// attached to the agent, so replacing a connection cannot cancel this job.
func (s *Spool) QueueSeal(id, owner string) (*v1.DockerArtifactInfo, error) {
	status, err := s.Status(id, owner)
	if err != nil || status.GetKind() != Import {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if status.GetState() != Receiving {
		return status, nil
	}
	l, err := s.locked(id, owner, false)
	if err != nil {
		return nil, err
	}
	defer l.mu.Unlock()
	if l.Info.GetState() != Receiving {
		return proto.CloneOf(l.Info), nil
	}
	if l.Info.ReceivedBytes != l.Info.SizeBytes || l.Expected != "" && l.Expected != l.Info.CommittedPrefixSha256 {
		return nil, ErrOffset
	}
	l.Info.State = Checking
	if err := l.save(); err != nil {
		l.Info.State = Receiving
		return nil, err
	}
	s.publish(l)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return proto.CloneOf(l.Info), nil
}

func (s *Spool) nextSeal() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, l := range s.leases {
		if l.snapshot.GetKind() == Import && l.snapshot.GetState() == Checking && !l.expired(now) {
			return id
		}
	}
	return ""
}

// RunSeals processes the durable queue with one worker.
func (s *Spool) RunSeals(ctx context.Context, takeTransfer func(context.Context) (func(), error)) {
	for ctx.Err() == nil {
		id := s.nextSeal()
		if id == "" {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		if !s.runSeal(ctx, id, takeTransfer) {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

func (s *Spool) runSeal(ctx context.Context, id string, takeTransfer func(context.Context) (func(), error)) bool {
	validation, err := s.TakeValidation(ctx)
	if err != nil {
		return false
	}
	defer validation()
	transfer, err := takeTransfer(ctx)
	if err != nil {
		return false
	}
	defer transfer()
	sealCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	done := make(chan struct{})
	s.sealingID, s.sealCancel, s.sealDone = id, cancel, done
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.sealingID, s.sealCancel, s.sealDone = "", nil, nil
		close(done)
		s.mu.Unlock()
	}()
	_, err = s.Seal(sealCtx, id, "")
	if err == nil || errors.Is(err, ErrUnavailable) {
		return true
	}
	if sealCtx.Err() != nil || errors.Is(err, ErrBusy) {
		return false
	}
	return s.failSeal(id, err) == nil
}

func (s *Spool) failSeal(id string, err error) error {
	l, lockErr := s.locked(id, "", false)
	if lockErr != nil {
		return lockErr
	}
	defer l.mu.Unlock()
	previous := proto.CloneOf(l.Info)
	l.Info.State = Failed
	if end := s.now().Add(idleLifetime); end.After(l.expires) {
		l.Info.ExpiresAt = timestamppb.New(end)
	}
	l.Info.Failure = v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED
	if errors.Is(err, ErrArchive) {
		l.Info.Failure = v1.DockerImageReason_DOCKER_IMAGE_REASON_ARCHIVE_REFUSED
	}
	var space *SpaceError
	if errors.As(err, &space) {
		l.Info.Failure = v1.DockerImageReason_DOCKER_IMAGE_REASON_QUOTA
		l.Info.FailureSpace = &v1.DockerArtifactSpace{RequiredBytes: space.Needed, AvailableBytes: space.Available}
	}
	if saveErr := l.save(); saveErr != nil {
		l.Info = previous
		return saveErr
	}
	_ = os.Remove(l.source())
	s.release(l, l.reserve)
	s.publish(l)
	return nil
}

// Sweep removes idle and expired leases even while a validation is running.
func (s *Spool) Sweep(ctx context.Context, checkPins func(context.Context)) {
	tick := time.NewTicker(2 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			checkPins(ctx)
			s.Prune(s.now())
		}
	}
}
