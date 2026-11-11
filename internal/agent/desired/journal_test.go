// SPDX-License-Identifier: AGPL-3.0-only

package desired

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

func openJournal(t *testing.T, dir string) *Journal {
	t.Helper()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestAnUnfinishedActionKeepsItsIdentityAcrossAKill(t *testing.T) {
	dir := t.TempDir()
	j := openJournal(t, dir)
	e, err := j.Begin(web, Made, "Its image was retained", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if pending, _, _ := j.Pending(); len(pending) != 0 {
		t.Fatal("an unfinished action was handed back as complete")
	}
	j = openJournal(t, dir)
	again, err := j.Begin(web, Made, "", "", time.Now().Add(time.Hour))
	if err != nil || again.ID != e.ID || !again.At.Equal(e.At) {
		t.Fatalf("resumed %+v: %v; want %+v", again, err, e)
	}
	if err := j.Finish(e.ID, Made, e.Reason); err != nil {
		t.Fatal(err)
	}
	j = openJournal(t, dir)
	if err := j.Finish(e.ID, Made, e.Reason); err != nil {
		t.Fatal(err)
	}
	entries, _, _ := j.Pending()
	if len(entries) != 1 || entries[0].ID != e.ID || !entries[0].At.Equal(e.At) || len(j.Doing()) != 0 {
		t.Fatalf("receipts %+v, pending %+v", entries, j.Doing())
	}
}

func TestACompletionThatCantBeKeptResendsTheSameReceipt(t *testing.T) {
	dir := t.TempDir()
	j := openJournal(t, dir)
	e, err := j.Begin(web, Made, "", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	j.path = filepath.Join(dir, "missing", "journal")
	if err := j.Finish(e.ID, Made, ""); err == nil {
		t.Fatal("completion unexpectedly kept")
	}
	memory, _, _ := j.Pending()
	j = openJournal(t, dir)
	if err := j.Finish(e.ID, Made, ""); err != nil {
		t.Fatal(err)
	}
	disk, _, _ := j.Pending()
	if len(memory) != 1 || len(disk) != 1 || memory[0].ID != disk[0].ID || !memory[0].At.Equal(disk[0].At) {
		t.Fatalf("before kill %+v, after %+v", memory, disk)
	}
}

func add(t *testing.T, j *Journal, ref resource.Ref, what, reason string) {
	t.Helper()
	if err := j.Add(ref, what, reason, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// boot stands in a boot of the server, by its ID.
func boot(t *testing.T, id string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "boot_id")
	write(t, f, []byte(id+"\n"))
	was := bootIDFile
	bootIDFile = f
	t.Cleanup(func() { bootIDFile = was })
}

// What the agent did on its own is kept until the control plane has it, across a
// restart, each entry with an ID of its boot and a sequence that goes on.
func TestTheJournalKeepsWhatWasDone(t *testing.T) {
	boot(t, "aaaa-1111")
	dir := t.TempDir()
	j := openJournal(t, dir)
	add(t, j, web, Made, "")
	add(t, j, api, Waiting, "its volume is gone")
	j = openJournal(t, dir)
	add(t, j, web, Started, "")
	got, dropped, _ := j.Pending()
	if len(got) != 3 || dropped != 0 || got[0].ID != "aaaa1111-1" || got[2].ID != "aaaa1111-3" || got[1].Reason != "its volume is gone" || got[1].Ref != api {
		t.Fatalf("pending %+v, %d dropped", got, dropped)
	}
	b, _ := os.ReadFile(filepath.Join(dir, Folder, JournalFile))
	if info, err := os.Stat(filepath.Join(dir, Folder, JournalFile)); err != nil || info.Mode().Perm() != 0o600 || strings.Contains(string(b), "correct-horse") {
		t.Errorf("the journal is %v, %v", info.Mode(), err)
	}
	// The control plane has up to the second: those go.
	if err := j.Heard(got[1].ID); err != nil {
		t.Fatal(err)
	}
	if left, _, _ := openJournal(t, dir).Pending(); len(left) != 1 || left[0].ID != "aaaa1111-3" {
		t.Errorf("after the control plane had two, %+v is left", left)
	}
	// A new boot numbers afresh, under its own ID.
	boot(t, "bbbb-2222")
	j = openJournal(t, dir)
	add(t, j, web, Made, "")
	if got, _, _ := j.Pending(); got[len(got)-1].ID != "bbbb2222-1" {
		t.Errorf("after a reboot the entry is %s", got[len(got)-1].ID)
	}
}

// A wait goes on minute after minute, and is written down once until it changes.
func TestAWaitIsWrittenDownOnce(t *testing.T) {
	j := openJournal(t, t.TempDir())
	for range 3 {
		add(t, j, web, Waiting, "its image is gone")
	}
	add(t, j, api, Made, "")
	add(t, j, web, Waiting, "its image is gone")
	add(t, j, web, Waiting, "its volume is gone")
	if got, _, _ := j.Pending(); len(got) != 3 {
		t.Errorf("wrote down %+v", got)
	}
}

// The journal keeps 500 entries, the oldest going first, and counts what went,
// until the control plane has what's left.
func TestTheJournalKeepsFiveHundred(t *testing.T) {
	j := openJournal(t, t.TempDir())
	for i := range JournalCap + 7 {
		add(t, j, resource.Ref{Kind: "service", Scope: "env_A", Name: fmt.Sprintf("web-%d", i)}, Made, "")
	}
	got, dropped, _ := j.Pending()
	if len(got) != JournalCap || dropped != 7 || got[0].Ref.Name != "web-7" {
		t.Fatalf("keeps %d, first %s, %d dropped", len(got), got[0].Ref.Name, dropped)
	}
	if err := j.Heard(got[len(got)-1].ID); err != nil {
		t.Fatal(err)
	}
	if got, dropped, _ := j.Pending(); len(got) != 0 || dropped != 0 {
		t.Errorf("after the control plane had them all, %d left and %d dropped", len(got), dropped)
	}
}

// A journal that doesn't read counts as one dropped, and goes on.
func TestADamagedJournalSaysSo(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Folder), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, Folder, JournalFile), []byte("{not json"))
	j := openJournal(t, dir)
	if got, dropped, _ := j.Pending(); len(got) != 0 || dropped != 1 {
		t.Errorf("a damaged journal: %+v, %d dropped", got, dropped)
	}
	add(t, j, web, Made, "")
	if got, _, _ := openJournal(t, dir).Pending(); len(got) != 1 {
		t.Errorf("it goes on: %+v", got)
	}
}

// What the agent saw of a resource is kept with its record, rewritten only as it
// changes; Refs lists what it keeps; Wipe forgets all of it, the key included.
func TestSeenRefsAndWipe(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "srv_1")
	keep(t, s, record(1))
	other := record(1)
	other.Ref = api
	keep(t, s, other)
	keep(t, s, Record{Ref: resource.Ref{Kind: "service", Scope: "env_01TEST", Name: "gone"}, Revision: 2})
	if refs := s.Refs(); len(refs) != 2 || refs[0] != api || refs[1] != web {
		t.Fatalf("refs %v, want api and web, and not the deleted one", refs)
	}
	before, _ := os.Stat(path(dir, web))
	if err := s.Seen(web, Standing{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path(dir, web)); !after.ModTime().Equal(before.ModTime()) {
		t.Error("seen as it was, the record was written again")
	}
	if err := s.Seen(web, Standing{Stopped: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _, err := open(t, dir, "srv_1").Read(web)
	if err != nil || !got.Stopped || got.Values["DATABASE_PASSWORD"] != "correct-horse-battery-staple" || got.Revision != 1 {
		t.Errorf("after it was seen stopped, it reads %+v, %v", got, err)
	}
	if err := s.Wipe(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, Folder)); !os.IsNotExist(err) {
		t.Errorf("the records are still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFile)); !os.IsNotExist(err) {
		t.Errorf("the key is still there: %v", err)
	}
	if len(s.Refs()) != 0 {
		t.Errorf("it still lists %v", s.Refs())
	}
}

// How a resource was seen to stand outlives the steps and fills that keep it: a
// step or a fill says what should run, not how it stands. Only a step that started
// it says it runs again (#357's first review, H1).
func TestWhatWasSeenOutlivesAKeep(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "srv_1")
	keep(t, s, record(1))
	data := map[string]string{"/var/lib/postgresql/data": strings.Repeat("ab", 32)}
	if err := s.Seen(web, Standing{Stopped: true, Volumes: data}, time.Now()); err != nil {
		t.Fatal(err)
	}
	keep(t, s, record(2)) // a step that changed it in place
	if got, _, err := open(t, dir, "srv_1").Read(web); err != nil || !got.Stopped || !maps.Equal(got.Volumes, data) {
		t.Fatalf("after a step that didn't start it: %+v, %v; want it stopped, with its volumes", got, err)
	}
	if kept, err := s.Remember(record(3)); err != nil || !kept {
		t.Fatal(kept, err)
	}
	if got, _, _ := s.Read(web); !got.Stopped || !maps.Equal(got.Volumes, data) {
		t.Fatalf("after a fill: %+v; want it stopped, with its volumes", got)
	}
	started := record(4)
	started.Started = true
	keep(t, s, started)
	if got, _, _ := s.Read(web); got.Stopped || !maps.Equal(got.Volumes, data) {
		t.Fatalf("after a step that started it: %+v; want it running, with its volumes", got)
	}
	if st, ok := s.Stands(web); !ok || st.Stopped || !maps.Equal(st.Volumes, data) {
		t.Errorf("it says it stands %+v, %t", st, ok)
	}
}

// A look from before the last step or fill is old news: what it saw may be what the
// step changed (#357's first review, H1).
func TestALookFromBeforeAKeepIsOldNews(t *testing.T) {
	s := open(t, t.TempDir(), "srv_1")
	before := time.Now()
	keep(t, s, record(1))
	if err := s.Seen(web, Standing{Stopped: true}, before); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Read(web); got.Stopped {
		t.Error("a look from before the step marked it stopped")
	}
	if err := s.Seen(web, Standing{Stopped: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Read(web); !got.Stopped {
		t.Error("a look from after the step didn't mark it stopped")
	}
}

// A record that no longer says what runs, and that the folder won't let go, is cut
// short, so it doesn't open after a restart either (#357's first review, a low).
func TestARecordThatCantGoIsCutShort(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a folder it can't write")
	}
	dir := t.TempDir()
	s := open(t, dir, "srv_1")
	keep(t, s, record(1))
	folder := filepath.Join(dir, Folder)
	if err := os.Chmod(folder, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(folder, 0o700) })
	if err := s.Keep(record(2)); err == nil {
		t.Fatal("kept a record in a folder it can't write")
	}
	if err := os.Chmod(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	again := open(t, dir, "srv_1")
	if _, ok, err := again.Read(web); ok || err != nil {
		t.Errorf("after a restart the record of revision 1 reads: %t, %v", ok, err)
	}
	if again.Holds().GetDamaged() != 1 {
		t.Errorf("%d records put aside, want the one cut short", again.Holds().GetDamaged())
	}
}

// A wait that goes on is written down once, not again each time the control plane
// goes away; anything else, or another wait, is (#357's first review, a low).
func TestAWaitTheControlPlaneHeardIsntWrittenAgain(t *testing.T) {
	j := openJournal(t, t.TempDir())
	now := time.Now()
	add := func(what, why string) {
		t.Helper()
		if err := j.Add(web, what, why, now); err != nil {
			t.Fatal(err)
		}
	}
	heard := func() {
		t.Helper()
		got, _, _ := j.Pending()
		if err := j.Heard(got[len(got)-1].ID); err != nil {
			t.Fatal(err)
		}
	}
	add(Waiting, "its volume is gone")
	heard()
	add(Waiting, "its volume is gone") // the next time the control plane is away
	if got, _, _ := j.Pending(); len(got) != 0 {
		t.Fatalf("a wait the control plane heard was written down again: %+v", got)
	}
	add(Waiting, "Docker can't say")
	add(Made, "")
	heard()
	add(Waiting, "its volume is gone")
	if got, _, _ := j.Pending(); len(got) != 1 {
		t.Fatalf("after it was made, a wait wasn't written down: %+v", got)
	}
}

// The count of entries dropped is named by the newest of them, so the same count
// sent again is one event, and a hand-back with no entries clears it, leaving the
// entries written down since (#357's first review, M4).
func TestTheDroppedCountHasAName(t *testing.T) {
	j := openJournal(t, t.TempDir())
	for i := range JournalCap + 2 {
		if err := j.Add(resource.Ref{Kind: "service", Scope: "env_01TEST", Name: fmt.Sprintf("web-%d", i)}, Made, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	got, dropped, to := j.Pending()
	if dropped != 2 || to != j.j.Boot+"-2" || got[0].ID != j.j.Boot+"-3" {
		t.Fatalf("%d dropped, named %q, and %s first, want 2, named %s-2", dropped, to, got[0].ID, j.j.Boot)
	}
	if err := j.Heard(""); err != nil {
		t.Fatal(err)
	}
	if got, dropped, to := j.Pending(); len(got) != JournalCap || dropped != 0 || to != "" {
		t.Errorf("after the count was heard: %d entries, %d dropped, named %q", len(got), dropped, to)
	}
}
