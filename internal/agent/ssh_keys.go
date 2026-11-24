// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

const maxSSHKeysFile = 1 << 20

type sshAccount struct {
	name, home string
	uid, gid   int
	admin      bool
}

func (r *sshReader) accounts() ([]sshAccount, error) {
	if r.tools.Off {
		return nil, nil
	}
	passwd, err := readSSHFile(r.rootPath("/etc/passwd"), maxSSHKeysFile)
	if err != nil {
		return nil, fmt.Errorf("can't read the server's SSH accounts: %w", err)
	}
	groups, err := readSSHFile(r.rootPath("/etc/group"), maxSSHKeysFile)
	if err != nil {
		return nil, fmt.Errorf("can't read the server's SSH administrator groups: %w", err)
	}
	var accounts []sshAccount
	for line := range strings.SplitSeq(string(passwd), "\n") {
		f := strings.Split(line, ":")
		if len(f) != 7 || !safeSSHAccount(f[0]) || !filepath.IsAbs(f[5]) || filepath.Clean(f[5]) == "/" {
			continue
		}
		uid, uerr := strconv.ParseUint(f[2], 10, 32)
		gid, gerr := strconv.ParseUint(f[3], 10, 32)
		shell := filepath.Base(f[6])
		if uerr != nil || gerr != nil || shell == "nologin" || shell == "false" || (uid == 0 && f[0] != "root") {
			continue
		}
		a := sshAccount{name: f[0], home: filepath.Clean(f[5]), uid: int(uid), gid: int(gid), admin: uid == 0}
		for line := range strings.SplitSeq(string(groups), "\n") {
			g := strings.Split(line, ":")
			if len(g) == 4 && (g[0] == "sudo" || g[0] == "wheel") {
				a.admin = a.admin || g[2] == f[3] || slices.Contains(strings.Split(g[3], ","), a.name)
			}
		}
		accounts = append(accounts, a)
	}
	return accounts, nil
}

func safeSSHAccount(name string) bool {
	return name != "" && len(name) <= 64 && !strings.HasPrefix(name, "-") && strings.IndexFunc(name, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		default:
			return !strings.ContainsRune("._-$", r)
		}
	}) < 0
}

func accountNamed(accounts []sshAccount, name string) (sshAccount, bool) {
	for _, account := range accounts {
		if account.name == name {
			return account, true
		}
	}
	return sshAccount{}, false
}

func (r *sshReader) rootPath(name string) string {
	if r.tools.Root == "" {
		return name
	}
	return filepath.Join(r.tools.Root, name)
}

func readSSHFile(name string, limit int64) ([]byte, error) {
	f, err := os.Open(name) //nolint:gosec // fixed configuration files under the agent's root
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readSSHBounded(f, limit)
}

func readSSHBounded(f io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("SSH file exceeds the size limit")
	}
	return data, err
}

// sshKeyDir walks each component without following links. Every directory
// from the home down must satisfy StrictModes. A descriptor pins the directory
// for all reads, metadata changes and replacement.
func (r *sshReader) sshKeyDir(a sshAccount, create bool) (*os.File, error) {
	fd, err := unix.Open(r.rootPath("/"), unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	names := strings.Split(strings.Trim(a.home, "/"), "/")
	for i, part := range append(names, ".ssh") {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && i == len(names) && create {
			if err := unix.Mkdirat(fd, part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, err
			}
			next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err == nil {
				err = unix.Fchown(next, a.uid, a.gid)
				if err != nil {
					_ = unix.Close(next)
				}
			}
		}
		if err != nil {
			return nil, err
		}
		_ = unix.Close(fd)
		fd = next
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return nil, err
		}
		owner := int(st.Uid)
		if st.Mode&0o022 != 0 || owner != 0 && (i < len(names)-1 || owner != a.uid) {
			return nil, errors.New("the SSH home or key directory has unsafe ownership or write permissions. Limit writes to its owner")
		}
	}
	out := os.NewFile(uintptr(fd), ".ssh")
	fd = -1
	return out, nil
}

func readAuthorized(dir *os.File, account sshAccount) ([]byte, error) {
	if dir == nil {
		return nil, nil
	}
	fd, err := unix.Openat(int(dir.Fd()), "authorized_keys", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "authorized_keys")
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0o022 != 0 || (st.Uid != 0 && int(st.Uid) != account.uid) {
		return nil, errors.New("authorized_keys must be a regular file with a single name, writable only by its owner")
	}
	return readSSHBounded(f, maxSSHKeysFile)
}

type authorizedKey struct {
	raw     []byte
	options []string
	key     *dockwellv1.SshKey
}

func parseSSHKeys(data []byte) []authorizedKey {
	lines := bytes.SplitAfter(data, []byte{'\n'})
	keys := make([]authorizedKey, 0, len(lines))
	var added *timestamppb.Timestamp
	var fingerprint string
	for _, line := range lines {
		key, comment, options, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			added, fingerprint = nil, ""
			if tail, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "# dockwell-added "); ok {
				when, fp, _ := strings.Cut(tail, " ")
				if at, err := time.Parse(time.RFC3339, when); err == nil {
					added, fingerprint = timestamppb.New(at), fp
				}
			}
			keys = append(keys, authorizedKey{raw: line})
			continue
		}
		entry := &dockwellv1.SshKey{Type: key.Type(), Comment: strings.ToValidUTF8(comment, "\uFFFD"), Fingerprint: ssh.FingerprintSHA256(key)}
		if entry.Fingerprint == fingerprint {
			entry.Added = added
		}
		keys = append(keys, authorizedKey{raw: line, key: entry, options: options})
		added, fingerprint = nil, ""
	}
	return keys
}

func parsePastedSSHKey(text string) ([]byte, *dockwellv1.SshKey, error) {
	if strings.Contains(text, "PRIVATE KEY") || strings.HasPrefix(text, "PuTTY-User-Key-File") {
		return nil, nil, errors.New("this is a private key. Paste the public key instead")
	}
	text = strings.TrimSpace(text)
	if len(text) > 16<<10 || !utf8.ValidString(text) || strings.ContainsAny(text, "\r\n\x00") {
		return nil, nil, errors.New("paste one public key in OpenSSH format")
	}
	key, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil || len(options) != 0 {
		return nil, nil, errors.New("paste one public key in OpenSSH format without authorized_keys options")
	}
	if _, certificate := key.(*ssh.Certificate); certificate {
		return nil, nil, errors.New("paste a public key instead of an SSH certificate")
	}
	line := bytes.TrimSpace(ssh.MarshalAuthorizedKey(key))
	if comment != "" {
		line = fmt.Appendf(line, " %s", comment)
	}
	return line, &dockwellv1.SshKey{Type: key.Type(), Comment: comment, Fingerprint: ssh.FingerprintSHA256(key)}, nil
}

func (r *sshReader) listSSHKeys(ctx context.Context, req *dockwellv1.ListServerSshKeysRequest) (*dockwellv1.ListServerSshKeysResponse, error) {
	accounts, err := r.accounts()
	if err != nil {
		return nil, err
	}
	out := &dockwellv1.ListServerSshKeysResponse{}
	for _, a := range accounts {
		out.Accounts = append(out.Accounts, a.name)
	}
	slices.Sort(out.Accounts)
	if req.GetAccount() == "" {
		return out, nil
	}
	account, ok := accountNamed(accounts, req.GetAccount())
	if !ok {
		return nil, errors.New("choose an account from the server's account list")
	}
	keys, err := r.authorized(ctx, account)
	if err != nil {
		return nil, err
	}
	out.Account = account.name
	for _, k := range keys {
		if k.key != nil {
			out.Keys = append(out.Keys, k.key)
		}
	}
	return out, nil
}

func (r *sshReader) authorized(ctx context.Context, account sshAccount) ([]authorizedKey, error) {
	if err := r.checkAuthorizedPath(ctx, account); err != nil {
		return nil, err
	}
	dir, err := r.sshKeyDir(account, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	data, err := readAuthorized(dir, account)
	return parseSSHKeys(data), err
}

func (r *sshReader) checkAuthorizedPath(ctx context.Context, account sshAccount) error {
	values, err := r.sshd(ctx, "-T", "-C", "user="+account.name+",host=localhost,addr=127.0.0.1,laddr=127.0.0.1,lport=22")
	if err != nil {
		return err
	}
	paths := strings.Fields(sshValue(string(values), "authorizedkeysfile"))
	if len(paths) == 0 || (paths[0] != ".ssh/authorized_keys" && paths[0] != "%h/.ssh/authorized_keys" && paths[0] != filepath.Join(account.home, ".ssh", "authorized_keys")) {
		return errors.New("sshd uses a custom authorized_keys path for this account. Use the key file configured on the server")
	}
	return nil
}

func (r *sshReader) editSSHKey(ctx context.Context, req *dockwellv1.EditServerSshKeyRequest) (*dockwellv1.EditServerSshKeyResponse, error) {
	if r.access != nil {
		release, err := r.access.HoldSSHKeys()
		if err != nil {
			return nil, err
		}
		defer release()
	}
	r.edits.Lock()
	defer r.edits.Unlock()
	accounts, err := r.accounts()
	if err != nil {
		return nil, err
	}
	a, ok := accountNamed(accounts, req.GetAccount())
	if !ok {
		return nil, errors.New("choose an account from the server's account list")
	}
	if err := r.checkAuthorizedPath(ctx, a); err != nil {
		return nil, err
	}
	add := req.GetPublicKey() != ""
	var line []byte
	var result *dockwellv1.SshKey
	if add {
		line, result, err = parsePastedSSHKey(req.GetPublicKey())
		if err != nil {
			return nil, err
		}
	}
	dir, err := r.sshKeyDir(a, add)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	data, err := readAuthorized(dir, a)
	if err != nil {
		return nil, err
	}
	original := slices.Clone(data)
	keys := parseSSHKeys(data)
	if add {
		for _, k := range keys {
			if k.key.GetFingerprint() == result.GetFingerprint() {
				return nil, errors.New("this key is already authorized for the account")
			}
		}
		at := time.Now().UTC().Truncate(time.Second)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		data = fmt.Appendf(data, "# dockwell-added %s %s\n%s\n", at.Format(time.RFC3339), result.Fingerprint, line)
		result.Added = timestamppb.New(at)
	} else {
		data = data[:0:0]
		var remaining bool
		for i, k := range keys {
			if k.key != nil && k.key.GetFingerprint() == req.GetRemoveFingerprint() {
				result = k.key
				continue
			}
			if k.key == nil && i+1 < len(keys) && keys[i+1].key.GetFingerprint() == req.GetRemoveFingerprint() && bytes.HasPrefix(k.raw, []byte("# dockwell-added ")) {
				continue
			}
			remaining = remaining || (k.key != nil && len(k.options) == 0)
			data = append(data, k.raw...)
		}
		if result == nil {
			return nil, errors.New("the key is no longer in authorized_keys. Read the keys again")
		}
		if !remaining && a.admin {
			return nil, errors.New("removing this key could leave the administrator without key access. Add a replacement key first")
		}
	}
	if len(data) > maxSSHKeysFile {
		return nil, errors.New("authorized_keys would exceed the size limit")
	}
	if err := replaceAuthorized(dir, a, original, data); err != nil {
		return nil, err
	}
	return &dockwellv1.EditServerSshKeyResponse{Key: result}, nil
}

func replaceAuthorized(dir *os.File, a sshAccount, original, data []byte) error {
	var st unix.Stat_t
	owner, group := a.uid, a.gid
	err := unix.Fstatat(int(dir.Fd()), "authorized_keys", &st, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0o022 != 0 || (st.Uid != 0 && int(st.Uid) != a.uid) {
			return errors.New("authorized_keys changed to an unsafe file. Check the server and refresh the list")
		}
		owner, group = int(st.Uid), int(st.Gid)
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}

	// CreateTemp uses the open directory, even if the account renames it.
	f, err := os.CreateTemp("/proc/self/fd/"+strconv.FormatUint(uint64(dir.Fd()), 10), ".dockwell-key-")
	if err != nil {
		return err
	}
	name := filepath.Base(f.Name())
	defer func() {
		_ = f.Close()
		_ = unix.Unlinkat(int(dir.Fd()), name, 0)
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chown(owner, group); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	latest, err := readAuthorized(dir, a)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, original) {
		return errors.New("authorized_keys changed while you were editing it. Refresh the list and try again")
	}
	if err := unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), "authorized_keys"); err != nil {
		return err
	}
	return dir.Sync()
}
