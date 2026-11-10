// SPDX-License-Identifier: AGPL-3.0-only

package docker

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
	// Number is its number among its service's, and ID its ID.
	Number int
	ID     string
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
	return Stand{State: c.State, Number: c.Number(), ID: c.ID}
}

// Stand is what c's own state says of it, with its number among its service's.
func (c *Container) Stand(number int) Stand {
	return Stand{State: c.State.Status, Number: number, ID: c.ID}
}
