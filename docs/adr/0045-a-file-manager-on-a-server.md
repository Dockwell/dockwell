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
- **A folder counts for what it holds.** Deleting or moving `/home/mira` or `/etc` is a write to every way in inside, so a folder that holds one asks, by the rule's own list and by every account's home, as each holds its owner's way in. So does making or moving a folder to where an account's home is, or where it would be or hold one of an admin's way-in folders, `.ssh` and their systemd user units, wherever their home is.
- **Homes go by where they lead, and by what they are.** A walk follows every link, so each home is compared as the accounts write it and with every link followed, and by device and inode, as is a start file's folder in an admin's home. Where `/home` itself leads elsewhere, as it does to `/var/home` on an ostree system or where it moved to a data disk, what's there counts by the rule for `/home`. A bind mount isn't a link, so `/home` and each account's home are known by device and inode too: a walk that passes through one by another name, such as `/mnt/data/home` bound onto `/home`, is spelled from there as the rule knows it, and checked again that way, for the rule's paths and for an admin's way-in folders. A home on a network mount is compared as written, as following it there would hold every call while it's down.
- **Putting back** asks where the path it goes back to is one.

Each has a step-up action of its own: `STEP_UP_ACTION_MAKE_WAY_BACK_IN` (90), `MOVE_WAY_BACK_IN` (91), `DELETE_WAY_BACK_IN` (92) and `RESTORE_WAY_BACK_IN` (93). For someone who chooses to be asked less often, the session stands in for each, and the agent is asked once more with the paths confirmed ([ADR 0075](0075-how-often-a-passkey-is-asked-for-again.md)). A proof is for paths as the agent resolved them, as a save's is: `srv_…:/path`, and for a move `srv_…:/from` and `/to` on two lines, as no path has a line break in it. The agent acts only if its own walk ends at each, so a link swapped in after the passkey doesn't pass. The panel always has these paths, as a listing gives its folder with every link followed. Unlike a save's, a proof lasts for its one act, as making, moving or deleting isn't done over and over to one path.

An upload asks as making does, with `MAKE_WAY_BACK_IN` for `srv_…:/path`, when it begins. A download writes nothing, and asks for none.

An agent that does these says so in its hello (`manages_files`), and the control plane sends none to one that doesn't, with words that say to update it. Its Recently deleted lists as empty. One that uploads and downloads says `transfers_files`, the same way.

### In the panel

- **The toolbar** has New folder, New file and Upload beside the search, as the mock has New folder and Upload, and Recently deleted. A new file opens to edit.
- **Upload** opens a sheet to choose files or drop them, and files dropped on the listing open it with them. It says the limit before anything is chosen, shows how far each file got, and offers to unpack an archive. A folder can't be chosen, and one that's dropped says to make a .zip of it.
- **Download** is on each row for a file or a folder, in an open file's header, and where a file is too large to show or isn't text.
- **Each row** has Download, for a file or a folder, and Rename, Move and Delete, shown as the row is hovered or focused. On a phone, and beside an open file, they're behind one button, as four on every row is too many there, and a name needs the room.
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
- **Removal stays on one filesystem,** goes 256 folders deep at most, and works through descriptors, never paths. What it removes is checked by device and inode first, and a file deleted for good goes out of the way and is checked before it's unlinked, so what took its place stays. What it can't remove stays, and is in the agent's log.
- **An entry is its own while it's made.** A delete claims its entry's ID before the entry exists, and an entry that doesn't say what it is yet isn't listed, and isn't pruned until it's an hour old, so cleanup never takes what was just deleted.
- **Putting back** goes to the path it was deleted from, walked again as it is now. Something there now refuses it (`file_exists`), and so does a folder that's gone (`folder_gone`, which says to make it again first) or moved to another disk.
- **A delete that can't be put back says so before it asks for a passkey,** by whether it's on Recently deleted's filesystem, so one delete takes one passkey. Another mount of the same filesystem is found as the rename refuses, after the passkey, and the panel spends the same proof on the delete for good that follows.
- **Anyone signed in** can put back or delete for good what's in a server's Recently deleted, as they can delete it in the first place, until roles come (#263).
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
| `file.upload`, `file.upload_failed` | Uploaded and put in place, with its size and the folder it unpacked into, or not |
| `file.download` | Downloaded, with its size for a file, before any of it went |

A write that took a passkey carries `way_back_in`, and reads as a warning, as `file.save_way_back_in` does. Where it moved to is `to_path` on the event's subject, and a delete for good says `for_good`. Deleting for good from Recently deleted is `file.delete` too. Each failure is on Activity, as a save's is. An unconfirmed one reads as a warning, as that's what a stolen session looks like.

### Uploading

- **Three calls.** `BeginUpload` names the path, its size and whether to unpack it. It refuses as making does (`file_exists`, what's refused, a way in without a passkey), and a file larger than the disk has free (`too_large`), and makes `.dockwell-upload-<id>` beside where it goes, mode 0600, which only root reads. `UploadFile` writes from `offset`, which has to be how far it got (`upload_offset`). `FinishUpload` checks it's all there and that its folder still leads where it did, syncs it, gives it its folder's owner and group and mode 0644, and renames it into place without replacing (`file_exists`). A note in the data directory says what each upload is, for where it goes and what its passkey confirmed.
- **An upload is its session's.** The control plane names who asks, the user and the session, on every call about an upload, and the agent keeps who began it in its note. A call from anyone else, another person or another session of the same one, finds no such upload (`upload_gone`), as a step-up's proof is for its session alone. A passkey confirms one person at one moment, and an upload that anyone could fill once they had its ID would let a second session put its own bytes where the first one's passkey said. An upload's file is in no listing and no folder's download, so its ID isn't there to read.
- **`CancelUpload` stops one,** from the same session, and removes what it had, by device and inode, so what took its place stays. The panel calls it when a file is left out, and for each one not finished when the sheet closes.
- **16 GB at most** (`max_bytes`), counted in steps of 1024 as the panel counts. The panel says so before anything is chosen, and leaves out a larger file with words that say to copy it over SSH.
- **A frame at a time on each side.** The control plane passes an upload on in frames of 128 KiB, and the agent takes them over a connection of its own (`ReceiveFile`, keyed by a token as `SendLogs` is) and writes each where it goes in the file. Neither holds more than a frame or two of each upload under way, never the file. A server runs 3 uploads and downloads at once, so an agent stays under its 40 MB with every one going. Its connection for them takes in at most 256 KiB of each upload, and 1 MiB in all, before it writes it: Go's own window is 4 MiB a stream, and three uploads, each waiting on a disk, took an agent to as much as 61 MB with it. `make transfers-load`, the Transfer load job, holds an agent to its budget with four uploads and then four downloads at once. A fourth waits up to 20 seconds for one of them to end, then is refused with the reason `transfers_busy` (429), and the panel waits and tries again, saying the server is busy.
- **A transfer that stops moving lets go.** Each frame of a PUT's body has 60 seconds to arrive, and each 64 KiB of a download 60 seconds to go, or the control plane ends the request, which ends the agent's call and frees its place. A browser that pauses a download, or a sender that stops, would hold one of the three for hours otherwise. A slow link is fine as long as it moves. A download is a GET, and a HEAD on its path is refused (405), as a download goes on record as it starts.
- **Memory goes back after.** While transfers run, the collector runs twice as often (GOGC 50), as each frame is garbage once it's written and what's live is small, so a collector that falls behind on a busy server doesn't let the agent grow. Go keeps what it freed for a while, so two seconds after the last transfer ends the agent puts the collector back and hands that memory to the system, and its memory at rest is what it was before.
- **The panel sends 64 MiB a PUT,** each from where the last ended, as a proxy in front of the control plane may take 100 MB at most, as Cloudflare's does. A PUT answers with how far the upload got.
- **A dropped connection costs a frame.** What arrived stays, and `BeginUpload` with `upload_id` says how far it got. The panel sends again up to five times, waiting longer each time, from where the server says it got, and then says what happened and offers to go on.
- **An upload not finished within a day goes,** with its file, when the agent next tidies Recently deleted.
- **One connection for a stream's transfers.** The agent opens it the first time one asks, keeps it for that stream's calls so a file's next part doesn't wait on a handshake, and lets it go with the stream, so a renewed certificate reaches the next.

### Unpacking

- **Into a new folder beside the archive,** named as it is without `.zip`, `.tar.gz` or `.tgz`, so `theme.zip` unpacks into `theme`, and nothing that's there is replaced. Something already at that name stops it. The archive stays as it was uploaded.
- **Through descriptors.** Each entry is made through the descriptors of the folders above it, made or opened without following a link and never onto another filesystem, so none leads out. Links, devices and the like aren't made. A file is 0644, or 0755 where the archive marks it executable, and a folder 0755, with the folder's owner and group.
- **A zip is read a record at a time.** `archive/zip` reads an archive's whole directory before it gives any of it, a few hundred bytes an entry, so 99,000 small entries took an agent from 24 MB to 75 MB before the entry limit could say no, and Go kept it after. So the agent reads a zip's end first, refuses one that says it holds more than the limit before reading or making anything, and then reads its directory a record at a time, with one buffer and one decompressor, checking each entry's size and CRC-32. A .tar.gz is read as it comes already. Unpacking runs in a file call's place, not a transfer's, so it hands back what it used once it's done, as the last transfer does.
- **What stops it:** an entry with `..` or a control character in its name, more than 64 folders deep, more than 100,000 entries, more than 16 GB unpacked, an encrypted entry, one packed any way but stored or deflated, one that doesn't match its checksum, a way back in, such as `.ssh/authorized_keys` in a home, or a file the refused list holds as credentials. The passkey was for the archive, not for what's in it, so a way in stops it rather than asking.
- **All or nothing.** What it made by then goes, by device and inode, and the archive stays in place. `FinishUpload` then fails with the reason `unpack_refused`, and the upload is still on record, so the panel says it's uploaded, not unpacked, and why.

### Downloading

- **A file as it is, or a folder as a zip archive made as it goes,** over a connection of its own (`SendFile`), a frame at a time. The control plane passes each on as it comes, and a reader that stops reading stops the agent.
- **What a read refuses, a download does,** by where it is, by its name, and by what its first part holds. A folder leaves out what a read would refuse in it, doesn't follow a link or cross onto another filesystem, and stops past 200,000 entries.
- **It stops at a private key or a password hash.** Past what a read checks, each part is looked at by the read's own line rules before it goes: a PEM key's first line on a line of its own, encrypted in the old form or not, and a line of a password file, with the start of a line a part ends in kept for the next. A key or a hash line anywhere in what it sends ends the download before it goes, and the browser shows it cut short.
- **On record before any of it goes,** as `file.download`, with its size for a file. One that can't go on record doesn't go.
- **Over REST it's a GET** with `Content-Disposition: attachment` and the file's name, and a file's size, so the panel links to it and the browser saves it as it comes, and says how far it got. A refused download answers with an error before anything goes. One that stops after it started, at a key or a dropped agent, cuts the connection, so a browser shows it failed rather than save part of it as the whole.

### Streamed over REST

Vanguard holds a REST call's body whole, both ways, up to 8 MiB (`serve.MaxMessageBytes`), as REST puts the status before the body and Vanguard waits for the end to know it. A file can be any size, so the upload's PUT and the download's GET don't go through Vanguard. Each becomes the same Connect stream call to the same handler, in the same Guard context, so the interceptor, the rate limits and the audit log treat it as they treat every other call, and its bytes go through a frame at a time (`internal/control/reststream.go`). The PUT answers with the `Upload` as JSON, and an error from either is the REST error Vanguard would write, reason and all.

A browser can't stream a request's body over Connect, and a download is a link the browser saves, so these two go over REST for every client. They're the public API's own routes, and no token can call either.

- **The OpenAPI document says they stream.** `protoc-gen-openapi` writes an `HttpBody` route as JSON, so `make generate` rewrites each method whose REST body or answer is one (`scripts/openapistreams`, which finds them in the schema): the upload takes `application/octet-stream` and answers JSON, and the download answers `application/octet-stream` or `application/zip`, with `Content-Disposition` and, for a file, `Content-Length`. Each is marked `x-dockwell-stream`, `request` or `response`.
- **Clients reach them through `@dockwell/api`,** as they reach the rest. The same step writes their method, path, query and media types from the document to `packages/api/src/gen/streams.ts`, and `uploadFilePart` and `downloadFileURL`, beside the generated client, are built on that and nothing written by hand. The panel calls those two, and `client.test.ts` holds that they're the only network calls in the API package's own source, and the only way the panel reaches these routes.

### Owner and mode, and several at once

These come in the next slices, and each amends this record when it lands. What's settled now:

- **Owner and mode** ask for a passkey by the same rule, and name the common choices in words beside the octal.
- **Several at once** is each one done in turn, each on record, and each asked about as above.

## Consequences

- Files does what a person does most without SSH, on every server, through the agent, with no new listener.
- A delete can be undone for 7 days where it's on the same disk as Dockwell's data, which is most of a small server. Elsewhere the panel says it can't be, before it happens.
- Recently deleted holds disk space for 7 days. Deleting for good is there for when the space is the point.
- A folder deleted for good on another disk shows as `.dockwell-deleting-…` for as long as its removal takes.
- A move to another disk isn't possible here yet.
- A file of up to 16 GB goes either way through the panel, a frame at a time, and a dropped connection costs a frame.
- Two REST paths are served beside Vanguard rather than through it, so a change to how Vanguard answers REST has to be made there too. A Vanguard that streams REST would let them go back.
- An upload left unfinished holds its space on the server's disk for up to a day.
- The refused list and the rule for ways in now guard writes as well as reads, so what each misses, it misses for both.
