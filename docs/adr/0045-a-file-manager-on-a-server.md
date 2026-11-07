# 0045. A file manager on a server

- Status: Accepted
- Date: 2026-11-07

## Context

[ADR 0032](0032-files-on-a-server.md) gave Files three calls: list, read and save. Reading and editing files is what people do most in a panel like this, more than half of what they do, and a file manager that can't make a folder, rename a file or delete one sends them back to SSH for the rest. #243 asks for the rest: make, upload, download, rename, move, delete, owner and mode, and several at once.

Each of these writes to a server as root, outside revisions, as a save does, so each needs the care ADR 0032 gave a save: what's refused, where a path really leads, a passkey where the write lets someone back in, and a line on Activity. This record says how, for the whole of #243. It lands in slices, and each slice adds what it builds here.

## Decision

### What ADR 0032 says still holds

- **Not a revision.** Files on a server aren't managed state, so each of these is one request with no plan, as a save is.
- **A person, never a token.** Every method needs a signed-in session.
- **The agent decides, on what it opens.** The refused list, Docker's data and how a walk follows links are the same as for a read.
- **On record, never contents.** Each write is on Activity, by person, server and path, and so is each that failed.

### Paths read where they lead, and the last part is what's acted on

A path is walked as a read walks it: from `/`, a part at a time, following every link on the way, each part checked as it opens. The last part is different. It's what you act on, so it's taken as itself: a link is renamed, moved or deleted as a link, and what it points to stays as it is. That's what a listing shows, where a link is an entry of its own. Every answer names where it acted, with each link before the last part followed, and so does Activity.

Each act goes through the descriptor of the folder the walk opened, with the `*at` calls that don't follow the last part (`mkdirat`, `openat` with `O_CREAT|O_EXCL|O_NOFOLLOW`, `renameat2` with `RENAME_NOREPLACE`, `unlinkat`). So a link swapped in behind the walk changes nothing: the folder that was checked is the folder that's written.

### Where these act, and what they never touch

- **Never `/`, and nothing that's refused.** A path the refused list stops, or a name `credentials` refuses, can't be made, moved or deleted, as it can't be read.
- **Only where a person's files are.** In Docker's data root only a volume's `_data` and what's in it opens, so nothing is made, moved or deleted above that, including `_data` itself, which is the volume.
- **Nothing that holds what's refused.** A folder that holds Dockwell's data, Docker's or containerd's, or `/proc` and the like, can't be moved or deleted, by where each of those resolves.
- **Nothing that is, or holds, a mount point.** Deleting a folder would reach into what's mounted on it, and moving one would move the mount. The kernel refuses a mount point itself, and the agent says so first, by what `/proc/self/mountinfo` lists.

### A passkey where it lets someone back in

The rule is a save's ([ADR 0032](0032-files-on-a-server.md#a-step-up-for-a-way-back-in-and-for-nothing-else)), on each place a write touches:

- **Making** asks where the new path is a way back in, such as a file in `/etc/cron.d`.
- **Moving and renaming** ask where either end is one. Moving out of `/etc/sudoers.d` is as much a write there as moving in, and renaming a file to `authorized_keys` makes one.
- **Deleting** asks where the path is one.
- **A folder counts for what it holds.** Deleting or moving `/home/mira` or `/etc` is a write to every way in inside, so a folder that holds one asks, by the rule's own list and by every account's home, as each holds its owner's way in. So does making or moving a folder to where an account's home is.
- **Putting back** asks where the path it goes back to is one.

Each has a step-up action of its own: `STEP_UP_ACTION_MAKE_WAY_BACK_IN` (90), `MOVE_WAY_BACK_IN` (91), `DELETE_WAY_BACK_IN` (92) and `RESTORE_WAY_BACK_IN` (93). A proof is for paths as the agent resolved them, as a save's is: `srv_…:/path`, and for a move `srv_…:/from` and `/to` on two lines, as no path has a line break in it. The agent acts only if its own walk ends at each, so a link swapped in after the passkey doesn't pass. The panel always has these paths, as a listing gives its folder with every link followed. Unlike a save's, a proof lasts for its one act, as making, moving or deleting isn't done over and over to one path.

An agent that does these says so in its hello (`manages_files`), and the control plane sends none to one that doesn't, with words that say to update it. Its Recently deleted lists as empty.

### In the panel

- **The toolbar** has New folder and New file beside the search, as the mock has New folder, and Recently deleted. A new file opens to edit.
- **Each row** has Rename, Move and Delete, shown as the row is hovered or focused. On a phone they're behind one button, as three on every row is too many there.
- **Move** takes a folder's path, which it lists first, so what a passkey confirms is where the server resolves it.
- **A delete** says where it goes and for how long, offers to delete for good, and leaves a notice with Put back. Where the server says it can't be put back, the sheet says so plainly and only then offers to delete for good.
- **Activity** names each write, and opens the folder it's in.

### Making

- **A file starts empty, and a folder starts with nothing in it.** A file has mode 0644 and a folder 0755, whatever the agent's umask.
- **It takes its folder's owner and group,** so a file made in a site's folder is the site's, not root's. A folder with the set-group-ID bit gives its group as the kernel does.
- **It never replaces.** Something already there refuses it, with the reason `file_exists`.
- **A name is one part,** 255 bytes at most, and not `.` or `..`.

### Moving and renaming

- **Renaming is a move within a folder.** Both take the whole new path.
- **It never replaces** what's at the new path (`file_exists`).
- **Within one filesystem.** A move to another disk refuses, with the reason `other_disk`, and says to copy it over SSH. A copy across disks needs the streaming that uploads and downloads bring, so it can come with them.
- **A folder can't go inside itself,** which the kernel refuses, and the agent says so plainly.

### Deleting, and putting back

- **Delete puts it in Recently deleted,** a folder in the data directory, for 7 days, where only root can reach it. It's a rename, so it's instant whatever the size, and a container that holds the file open keeps it.
- **That needs one filesystem.** Something on another disk, or on another mount of the same one, can't be renamed there. Deleting it refuses, with the reason `delete_for_good`, and says it can't be put back, so the panel asks again and says that plainly. A person who wants the space back at once can delete for good from the start, or from Recently deleted.
- **For good is gone at once, and removed after.** A file or a link is unlinked. A folder is renamed out of the way, into Recently deleted where it's on the same filesystem, or beside itself as `.dockwell-deleting-…` where it isn't, then removed in the background. So the path is free at once, however large the folder, and nothing reads it half removed. A note in Recently deleted keeps track of it, so a restart picks up where it left off, and what's removed is checked by device and inode first, so something else in its place stays.
- **Removal stays on one filesystem,** goes 256 folders deep at most, and works through descriptors, never paths. What it can't remove stays, and is in the agent's log.
- **Putting back** goes to the path it was deleted from, walked again as it is now. Something there now refuses it (`file_exists`), and so does a folder that's gone (`not_found`) or moved to another disk.
- **Recently deleted lists** each entry's path, what it was, its size for a file, and when it goes. It's at most 1,000 entries; past that the oldest go first.

### On record

| Event | When |
| --- | --- |
| `file.make`, `file.make_failed` | A file or folder made, or not |
| `file.move`, `file.move_failed` | Moved or renamed, with where to |
| `file.delete`, `file.delete_failed` | Deleted, to Recently deleted or for good |
| `file.restore`, `file.restore_failed` | Put back |
| `file.make_unconfirmed` and the like | Asked for a passkey, and none came within its minute |
| `file.change_unknown` | Its call ended as it acted, so it may yet happen |

A write that took a passkey carries `way_back_in`, and reads as a warning, as `file.save_way_back_in` does. Where it moved to is `to_path` on the event's subject, and a delete for good says `for_good`. Deleting for good from Recently deleted is `file.delete` too. Each failure is on Activity, as a save's is. An unconfirmed one reads as a warning, as that's what a stolen session looks like.

### Upload, download, owner and mode, and several at once

These come in the next slices, and each amends this record when it lands. What's settled now:

- **Uploads and downloads stream in bounded chunks,** on a connection of their own, as logs do ([ADR 0032](0032-files-on-a-server.md#bounds)), never whole on the agent or the control plane.
- **Owner and mode** ask for a passkey by the same rule, and name the common choices in words beside the octal.
- **Several at once** is each one done in turn, each on record, and each asked about as above.

## Consequences

- Files does what a person does most without SSH, on every server, through the agent, with no new listener.
- A delete can be undone for 7 days where it's on the same disk as Dockwell's data, which is most of a small server. Elsewhere the panel says it can't be, before it happens.
- Recently deleted holds disk space for 7 days. Deleting for good is there for when the space is the point.
- A folder deleted for good on another disk shows as `.dockwell-deleting-…` for as long as its removal takes.
- A move to another disk isn't possible here yet.
- The refused list and the rule for ways in now guard writes as well as reads, so what each misses, it misses for both.
