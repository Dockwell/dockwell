// SPDX-License-Identifier: AGPL-3.0-only

package docker

import "strconv"

// LabelRevision is the revision a container was made for, on what Dockwell
// makes: the step's, or the record's for one the agent made again.
const LabelRevision = OwnPrefix + "revision"

// Stand is what of a container says whether it, or another of the same service,
// stands for the service: which one a look at "does this service run" reads.
// More than one can answer to a service: what a rollout left behind when it
// failed or was cut short, or a twin someone made by hand. The agent's two looks,
// the health monitor's list and the driver's own read, both choose with Over, so
// they can't disagree (ADR 0054).
type Stand struct {
	// State is the container's state as a list or its own status has it:
	// "running", "exited", "created", "removing", "dead" and so on.
	State string
	// Number is its number among its service's, and ID its ID. A container
	// with no number label, or one that isn't a number, is numbered 0, as one
	// Dockwell didn't make is: it stands only where nothing numbered does.
	Number int
	ID     string
	// Revision is the revision it was made for, or 0 for one that doesn't say,
	// as one made by hand doesn't.
	Revision int64
}

// Leaving says Docker is removing it, or gave up on removing it. One that's
// leaving never stands for a service another container answers to: it's no news
// of the service, and beside a live one it would hide that one.
func (s Stand) Leaving() bool { return s.State == "removing" || s.State == "dead" }

// Runs says it runs: Docker restarting it, or someone having paused it, isn't a
// stop.
func (s Stand) Runs() bool {
	return s.State == "running" || s.State == "restarting" || s.State == "paused"
}

// Over reports whether a stands for the service rather than b, of two that
// aren't leaving. One that runs does before one that doesn't: the service runs if
// any of its containers does. Then the higher number, as each rollout that
// starts the new container first numbers it one on. Then the greater ID, which
// says nothing of the containers, but the same thing to both looks: when one was
// made isn't used, as a list and a container's own state don't say it alike.
func (s Stand) Over(b Stand) bool {
	switch {
	case s.Runs() != b.Runs():
		return s.Runs()
	case s.Number != b.Number:
		return s.Number > b.Number
	}
	return s.ID > b.ID
}

// Stand is what the list says of c, for choosing among a service's containers.
func (c *ContainerSummary) Stand() Stand {
	return Stand{State: c.State, Number: c.Number(), ID: c.ID, Revision: revisionOf(c.Labels)}
}

// Stand is what c's own state says of it, with its number among its service's.
func (c *Container) Stand(number int) Stand {
	return Stand{State: c.State.Status, Number: number, ID: c.ID, Revision: revisionOf(c.Config.Labels)}
}

func revisionOf(labels map[string]string) int64 {
	n, err := strconv.ParseInt(labels[LabelRevision], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Pick chooses, of a service's containers, the one that stands for it, as they're
// added in any order. It is the whole rule, for both of the agent's looks: one
// that's leaving never stands while another answers to the service, and of the
// rest the one Over the others does. The zero Pick has seen none.
type Pick struct {
	best, leaving   Stand
	has, hasLeaving bool
	others          int
}

// Add takes one more of the service's containers, and reports whether it's now
// the one that stands. Of two the rule can't tell apart, which only the same
// container listed twice is, the first added stands.
func (p *Pick) Add(s Stand) bool {
	if s.Leaving() {
		if !p.hasLeaving {
			p.leaving, p.hasLeaving = s, true
		}
		return false
	}
	switch {
	case !p.has:
		p.best, p.has = s, true
		return true
	case s.Over(p.best):
		p.best = s
		p.others++
		return true
	}
	p.others++
	return false
}

// Stands is the container that stands for the service, if any that isn't
// leaving answers to it.
func (p *Pick) Stands() (Stand, bool) { return p.best, p.has }

// Leaving is a container on its way out, which is all there is of the service
// when none stands: no news of it.
func (p *Pick) Leaving() (Stand, bool) { return p.leaving, p.hasLeaving }

// Sole says the one that stands is the service's only container that isn't
// leaving.
func (p *Pick) Sole() bool { return p.has && p.others == 0 }

// CouldntStart reports whether an error Docker left in the state of the
// container that stands, as it does of one it tried to start and couldn't, is
// news of the service that the agent may act on by starting it, for a record at
// revision recorded (ADR 0054). It is only when both hold:
//
//   - The container stands alone. Beside another that isn't leaving, what a
//     rollout that failed left behind, or a twin made by hand, is the one with
//     the error as likely as not, and a stop of the other is what happened to
//     the service. Then the error is no news: nothing is started, and the
//     service is noted as it would be with no error.
//   - It is of a revision the record covers: one it says, from 1 up to the
//     record's. A record is only ever of a revision that was applied and
//     passed, so a container of a later one is of a deploy that never did, and
//     one that says none wasn't made by Dockwell. Neither is what the record
//     describes, and the agent never starts what it has no record of. An
//     earlier one is the record's own: a fill, or a step that left the
//     container as it was, moves the record on without making it again.
func CouldntStart(sole bool, revision, recorded int64) bool {
	return sole && revision >= 1 && revision <= recorded
}
