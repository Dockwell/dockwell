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
	// Revision is the revision it was made for, or 0 for one that doesn't say:
	// one a release from before the label made, one that was taken on, or one
	// made by hand.
	Revision int64
	// Adopted says it was taken on, as the 1Panel importer and Adopt do: it
	// wasn't made by Dockwell, and its record describes it all the same.
	Adopted bool
	// Image is the ID of the image it was made from, where its own state was
	// read; a list doesn't fill it.
	Image string
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
	return Stand{State: c.State, Number: c.Number(), ID: c.ID, Revision: revisionOf(c.Labels), Adopted: c.Labels[LabelAdopted] != ""}
}

// Stand is what c's own state says of it, with its number among its service's.
func (c *Container) Stand(number int) Stand {
	return Stand{
		State: c.State.Status, Number: number, ID: c.ID, Revision: revisionOf(c.Config.Labels),
		Adopted: c.Config.Labels[LabelAdopted] != "", Image: c.Image,
	}
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

// ImageTie is how the image a container was made from stands to the one its
// record says ran.
type ImageTie int

const (
	// ImageUnknown is when nothing says: the record names no image, or Docker
	// couldn't be asked.
	ImageUnknown ImageTie = iota
	// ImageSame is the record's own image.
	ImageSame
	// ImageOther is another image than the record's.
	ImageOther
)

// Covered reports whether a record at revision recorded, whose image stands to
// the container's as tie says, describes the container: whether it's what the
// record says the server should run, and so the agent's to start when Docker
// couldn't. It says why not, as the end of a sentence, for Activity.
//
//   - Of another image than the record's, it isn't, whatever its labels say: a
//     restart, or a pull that found a newer image, makes its container for the
//     revision the record already has, and that image never passed here.
//   - One that was taken on is, as its record was made of it as it stood.
//   - One made for a revision is when that's no later than the record's. A
//     record is only ever of a revision that was applied and passed, so a later
//     one is of a deploy that never did. An earlier one is the record's own: a
//     fill, or a step that left the container as it was, moves the record on
//     without making it again.
//   - One that says no revision, as a release from before the label made it, or
//     a hand did, is only when its image is known to be the record's: nothing
//     else on it ties it to what last applied.
func (s Stand) Covered(recorded int64, tie ImageTie) (covered bool, why string) {
	switch {
	case tie == ImageOther:
		return false, "it was made from another image than the one this server last ran, which never passed here"
	case s.Adopted:
		return true, ""
	case s.Revision > recorded:
		return false, "it was made for a later revision than the last that applied here, by a deploy that never passed"
	case s.Revision >= 1:
		return true, ""
	case tie == ImageSame:
		return true, ""
	}
	return false, "nothing on it says which revision it was made for, and its image can't be told to be the one this server last ran"
}

// CouldntStart reports whether an error Docker left in the state of the
// container that stands, as it does of one it tried to start and couldn't, is
// news of the service that the agent may act on by starting it (ADR 0054). It is
// only when both hold:
//
//   - The container stands alone. Beside another that isn't leaving, what a
//     rollout that failed left behind, or a twin made by hand, is the one with
//     the error as likely as not, and a stop of the other is what happened to
//     the service. Then the error is no news: nothing is started, and the
//     service is noted as it would be with no error.
//   - Its record covers it (Covered). One that stands alone and isn't covered is
//     left down, and isn't noted as stopped either: nobody stopped it. The agent
//     says on Activity that it left it down, and why.
func CouldntStart(sole, covered bool) bool { return sole && covered }
