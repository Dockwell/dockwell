// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/ssh"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/access"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/jails"
)

func TestSSHKeyKeepsMetadataAndRejectsPrivateMaterial(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	public, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))) + " workstation"
	content, key, err := parsePastedSSHKey(line)
	if err != nil {
		t.Fatal(err)
	}
	stamped := fmt.Appendf(nil, "# dockwell-added 2026-01-02T03:04:05Z %s\n%s\n", key.Fingerprint, content)
	rows := parseSSHKeys(stamped)
	if got := rows[1].key; got.GetComment() != "workstation" || got.GetFingerprint() != ssh.FingerprintSHA256(public) || got.GetAdded() == nil {
		t.Fatalf("bad key metadata: %v", got)
	}
	if _, _, err := parsePastedSSHKey("-----BEGIN OPENSSH PRIVATE KEY-----"); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("private material: %v", err)
	}
	restricted := parseSSHKeys([]byte(`cert-authority,command="false",from="192.0.2.0/24" ` + line))
	if len(restricted) != 1 || len(restricted[0].key.GetOptions()) != 3 || restricted[0].unrestricted() {
		t.Fatalf("authorized_keys options were hidden or counted as unrestricted: %+v", restricted)
	}
	if _, _, err := parsePastedSSHKey("command=\"false\" " + line); err == nil {
		t.Fatal("accepted key with an option")
	}
}

func TestSSHKeysCannotFollowHomeOrAuthorizedKeyLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	r := newSSHReader(Config{Tools: &hosttool.Runner{Root: root}})
	a := sshAccount{name: "fixture", home: "/link", uid: os.Getuid(), gid: os.Getgid()}
	if dir, err := r.sshKeyDir(a, false); err == nil {
		_ = dir.Close()
		t.Fatal("linked home was read")
	}
	a.home = "/real"
	dir, err := r.sshKeyDir(a, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "real", ".ssh", "authorized_keys")); err != nil {
		t.Fatal(err)
	}
	if _, err := readAuthorized(dir, a); err == nil {
		t.Fatal("linked authorized_keys was read")
	}
}

func TestSSHChangeSettingsRenderBothPasswordMethodsAndTheTemporaryPort(t *testing.T) {
	off, on := false, true
	got := sshFile(&resourcev1.SshConfig{
		PasswordLogin: &off, KeyLogin: &on, RootLogin: "prohibit-password", Port: 2222,
		ListenAddresses: []string{"192.0.2.12", "2001:db8::12"},
	}, 22)
	for _, directive := range []string{"PasswordAuthentication no", "ChallengeResponseAuthentication no", "PubkeyAuthentication yes", "PermitRootLogin prohibit-password", "Port 2222", "Port 22", "ListenAddress 192.0.2.12", "ListenAddress 2001:db8::12"} {
		if !strings.Contains(got, directive+"\n") {
			t.Errorf("missing %q", directive)
		}
	}
}

func TestSSHLoginParserKeepsOnlyMetadata(t *testing.T) {
	now := time.Now()
	ok := parseSSHLogin(now, "Accepted publickey for deploy from 2001:db8::2 port 49152 ssh2: ED25519 SHA256:example")
	bad := parseSSHLogin(now, "Failed password for invalid user guest from 192.0.2.12 port 60234 ssh2")
	if !ok.GetAccepted() || ok.GetMethod() != "publickey" || ok.GetAddress() != "2001:db8::2" || bad.GetAccepted() || bad.GetUser() != "guest" {
		t.Fatalf("parsed %v and %v", ok, bad)
	}
	if parseSSHLogin(now, "Failed password for deploy from hostname port 49152") != nil {
		t.Fatal("accepted an unverified address")
	}
}

func TestSSHChallengeRejectsAnOrdinaryReconnect(t *testing.T) {
	proc := fstest.MapFS{
		"100/task/100/children": {Data: []byte("101")},
		"101/stat":              {Data: []byte("101 (sshd-session) S 100")},
		"101/task/101/children": {Data: []byte("102")},
		"102/stat":              {Data: []byte("102 (sh) S 101")},
		"102/cmdline":           {Data: []byte("/bin/sh\x00-c\x00ci-runner\x00")},
	}
	if sshChallengeRunning(proc, "100", "challenge") {
		t.Fatal("an automation reconnect carried no challenge")
	}
	proc["102/cmdline"].Data = []byte("/bin/sh\x00-c\x00" + sshProofScript + "\x00challenge\x00")
	if !sshChallengeRunning(proc, "100", "challenge") {
		t.Fatal("one-time command attached to the session was missed")
	}
	if sshChallengeRunning(proc, "100", "another-challenge") {
		t.Fatal("another trial's command proved this trial")
	}
	proc["102/stat"].Data = []byte("102 (sh) S 999")
	if sshChallengeRunning(proc, "100", "challenge") {
		t.Fatal("a process in another session proved the trial")
	}
}

func TestSSHProofRequiresTheAcceptedKeyAndSocket(t *testing.T) {
	now := time.Now()
	trial := &access.SSHChange{Account: "deploy", StartedAt: now, ProofKeys: []string{"SHA256:owner"}}
	peer := netip.MustParseAddrPort("192.0.2.5:42000")
	for _, tc := range []struct {
		title, message string
		at             time.Time
		want           bool
	}{
		{"owner", "Accepted publickey for deploy from 192.0.2.5 port 42000 ssh2: ED25519 SHA256:owner", now, true},
		{"old", "Accepted publickey for deploy from 192.0.2.5 port 42000 ssh2: ED25519 SHA256:owner", now.Add(-time.Second), false},
		{"other socket", "Accepted publickey for deploy from 192.0.2.5 port 42001 ssh2: ED25519 SHA256:owner", now, false},
		{"other key", "Accepted publickey for deploy from 192.0.2.5 port 42000 ssh2: ED25519 SHA256:automation", now, false},
		{"password", "Accepted password for deploy from 192.0.2.5 port 42000 ssh2", now, false},
	} {
		t.Run(tc.title, func(t *testing.T) {
			proof := &access.SSHProof{}
			entry := sshJournalEntry{At: strconv.FormatInt(tc.at.UnixMicro(), 10), Message: tc.message, UID: "0"}
			if got := checkSSHAuthentication(trial, peer, entry, proof); got != tc.want {
				t.Fatalf("accepted = %t, want %t", got, tc.want)
			}
			if tc.want && (proof.Method != "publickey" || proof.Fingerprint != "SHA256:owner") {
				t.Fatalf("unidentified proof %+v", proof)
			}
		})
	}
}

func TestSSHKeyRemovalRefusesKeyOnlyAccounts(t *testing.T) {
	for _, tc := range []struct {
		values string
		root   bool
		want   bool
	}{
		{"passwordauthentication yes\nauthenticationmethods any\n", false, true},
		{"passwordauthentication no\nauthenticationmethods any\n", false, false},
		{"passwordauthentication yes\nauthenticationmethods publickey,password\n", false, false},
		{"passwordauthentication yes\nauthenticationmethods any\npermitrootlogin without-password\n", true, false},
	} {
		if err := checkKeyRemovalAuth(tc.values, tc.root, true); (err == nil) != tc.want {
			t.Errorf("check(%q, root %t) returned %v", tc.values, tc.root, err)
		}
	}
	root := t.TempDir()
	shadow := filepath.Join(root, "etc", "shadow")
	if err := os.Mkdir(filepath.Dir(shadow), 0o700); err != nil {
		t.Fatal(err)
	}
	r := newSSHReader(Config{Tools: &hosttool.Runner{Root: root}})
	for _, tc := range []struct {
		hash string
		want bool
	}{
		{"", false}, {"!", false}, {"!!", false}, {"!$6$locked", false}, {"*", false}, {"*LK*", false}, {"$6$test$fixture", true},
	} {
		if err := os.WriteFile(shadow, []byte("alice:"+tc.hash+":20000:0:99999:7:::\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := checkKeyRemovalAuth("passwordauthentication yes\nauthenticationmethods any\n", false, r.hasSSHPassword("alice"))
		if (err == nil) != tc.want || r.hasSSHPassword("missing") {
			t.Fatalf("password check for %q returned %v", tc.hash, err)
		}
	}
}

func TestSSHJailMovesStockAndDockwellPortsButRefusesAnOverride(t *testing.T) {
	root := t.TempDir()
	put := func(name, body string, mode os.FileMode) {
		t.Helper()
		name = filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	put("/usr/bin/fail2ban-client", "#!/bin/sh\nexit 0\n", 0o755)
	put("/etc/fail2ban/jail.conf", "[sshd]\nport = ssh\n", 0o600)
	put("/etc/fail2ban/jail.d/defaults-debian.conf", "[sshd]\nenabled = true\n", 0o600)
	s := &sshSettings{reader: newSSHReader(Config{Tools: &hosttool.Runner{Root: root}})}
	trial, err := s.planSSHJail(t.Context(), 22, 2222)
	if err != nil || trial == nil || trial.Old != "" || trial.Candidate != sshJailBody([]uint16{2222, 22}) || trial.Final != sshJailBody([]uint16{2222}) {
		t.Fatalf("stock jail %+v, error %v", trial, err)
	}
	put(jails.File, jails.Render(map[string]*resourcev1.Jail{"sshd": {Filter: "sshd", Ports: []uint32{22, 2200}}}, nil), 0o600)
	trial, err = s.planSSHJail(t.Context(), 22, 2222)
	if err != nil || trial == nil || trial.Final != sshJailBody([]uint16{2200, 2222}) {
		t.Fatalf("managed jail %+v, error %v", trial, err)
	}
	put("/etc/fail2ban/jail.local", "[sshd]\nport = 22\n", 0o600)
	if _, err := s.planSSHJail(t.Context(), 22, 2222); err == nil || !strings.Contains(err.Error(), "/etc/fail2ban/jail.local") {
		t.Fatalf("unmanaged jail returned %v", err)
	}
}

func TestSSHStagedConfigReplacesOnlyTheOwnedDropIn(t *testing.T) {
	root := t.TempDir()
	r := newSSHReader(Config{Tools: &hosttool.Runner{Root: root}})
	dir := r.rootPath("/etc/ssh/sshd_config.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "10-site.conf")
	if err := os.WriteFile(site, []byte("UsePAM yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	main := []byte("Include sshd_config.d/*.conf\nPermitRootLogin no\n")
	staged, err := r.stagedSSHMain(main, "/tmp/fixture-candidate", true)
	if err != nil || !strings.HasPrefix(string(staged), "Include \"/tmp/fixture-candidate\"\nInclude \""+site+"\"\n") {
		t.Fatalf("staged config %q, error %v", staged, err)
	}
	staged, err = r.stagedSSHMain(main, "/tmp/fixture-candidate", false)
	if err != nil || !strings.HasPrefix(string(staged), "Include \""+site+"\"\n") || strings.Contains(string(staged), "fixture-candidate") {
		t.Fatalf("staged removal %q, error %v", staged, err)
	}
}

func TestSSHBootRestoreWorksWithoutTheAgentAndHonorsKeep(t *testing.T) {
	for _, kept := range []bool{false, true} {
		t.Run(strconv.FormatBool(kept), func(t *testing.T) {
			dir := t.TempDir()
			sshFile, jailFile := filepath.Join(dir, "live-ssh"), filepath.Join(dir, "live-jail")
			for _, unit := range []string{"ssh.service", "sshd.service", "fail2ban.service"} {
				if err := os.Mkdir(filepath.Join(dir, unit+".d"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]string{
				"ssh-revert.sh": sshRevertScript, "ssh-boot-id": "previous-boot",
				"ssh-wait-id": "chg_fixture@10", "waiting.json": "{ \"wait\": \"chg_fixture@10\" }",
				"ssh-previous": "original", "ssh-jail-previous": "",
				"live-ssh": "candidate", "live-jail": "trial ports",
				"systemctl":                              "#!/bin/sh\n[ \"$1\" = daemon-reload ]\n",
				sshRestoreUnit:                           "fixture unit",
				"ssh.service.d/" + sshRestoreDropIn:      "fixture drop-in",
				"fail2ban.service.d/" + sshRestoreDropIn: "fixture drop-in",
			}
			if kept {
				files["confirmed.json"] = files["waiting.json"]
			}
			for name, body := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(filepath.Join(dir, "systemctl"), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(dir, "ssh-revert.sh"), dir, "boot", "", sshFile, "ssh.service", jailFile, "", dir)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("boot helper: %v, %s", err, out)
			}
			want := "original"
			if kept {
				want = "candidate"
			}
			if content, err := os.ReadFile(sshFile); err != nil || string(content) != want {
				t.Errorf("SSH after boot = %q, %v", content, err)
			}
			if _, err := os.Stat(jailFile); (err == nil) != kept {
				t.Errorf("jail after boot = %v", err)
			}
			for _, file := range []string{"ssh-revert.sh", "ssh-wait-id", sshRestoreUnit, "ssh.service.d/" + sshRestoreDropIn, "fail2ban.service.d/" + sshRestoreDropIn} {
				if _, err := os.Stat(filepath.Join(dir, file)); !os.IsNotExist(err) {
					t.Errorf("%s was left behind: %v", file, err)
				}
			}
		})
	}
}

func TestSSHForeignRulesNeedEquivalentAccessOnTheNewPort(t *testing.T) {
	tool := &dockwellv1.FirewallTool{Rules: []*dockwellv1.FirewallSeenRule{
		{Port: "ssh", Verdict: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW},
	}}
	if conflict := foreignSSHPortConflict(tool, 22, 2222); conflict != "ssh" {
		t.Fatalf("old external allow returned %q", conflict)
	}
	tool.Rules = append(tool.Rules, &dockwellv1.FirewallSeenRule{Port: "2222/tcp", Verdict: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW})
	if conflict := foreignSSHPortConflict(tool, 22, 2222); conflict != "" {
		t.Fatalf("equivalent external access returned %q", conflict)
	}
	tool.Rules[1].Source = "192.0.2.0/24"
	if conflict := foreignSSHPortConflict(tool, 22, 2222); conflict != "ssh" {
		t.Fatalf("narrower external allow returned %q", conflict)
	}
}

func TestSSHTextLogFallbackIsUnverified(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "var", "log")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := time.Now().UTC().Format(time.RFC3339) + " fixture sshd[42]: Failed password for deploy from 192.0.2.4 port 12345 ssh2\n"
	if err := os.WriteFile(filepath.Join(dir, "auth.log"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newSSHReader(Config{Tools: &hosttool.Runner{Root: root}})
	out, err := r.loginLog(t.Context(), &dockwellv1.ListServerSshLoginsRequest{})
	if err != nil || !out.GetUnverified() || out.GetBanAvailable() || len(out.GetFailures()) != 1 {
		t.Fatalf("text log output = %v, error %v", out, err)
	}
}

func TestSSHBanChecksActiveJailTCPPorts(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "usr", "bin", "fail2ban-client")
	if err := os.MkdirAll(filepath.Dir(tool), 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$*" in
  "get sshd actions") printf 'The jail sshd has the following actions:\niptables-multiport\n';;
  "get sshd action iptables-multiport protocol") printf 'tcp\n';;
  "get sshd action iptables-multiport port") printf 'ssh,2222\n';;
  *) exit 1;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fail2banReader{tools: &hosttool.Runner{Root: root}}
	if err := f.sshJailCovers(t.Context(), []uint16{22, 2222}); err != nil {
		t.Fatal(err)
	}
	if err := f.sshJailCovers(t.Context(), []uint16{22, 2223}); err == nil {
		t.Fatal("Ban was offered when the jail missed a listener")
	}
}

func TestSSHForeignInputNamesTheMatchingRule(t *testing.T) {
	header := "*filter\n:INPUT ACCEPT [0:0]\n:ts-input - [0:0]\n-A INPUT -j ts-input\n"
	common := "-A ts-input -i tailscale0 -j ACCEPT\n-A ts-input -m mark --mark 0x40000/0xff0000 -j ACCEPT\n"
	for _, tc := range []struct {
		add, want string
	}{
		{add: "-A ts-input -p udp -m udp --dport 41641 -j ACCEPT\n"},
		{add: "-A ts-input -p tcp -m multiport --dports 22,2222 -j ACCEPT\n"},
		{add: "-A ts-input -p tcp --dport 22 -j ACCEPT\n", want: "filter/ts-input -A ts-input -p tcp --dport 22 -j ACCEPT"},
		{add: "-A ts-input -m mystery --selector 1 -j DROP\n", want: "filter/ts-input -A ts-input -m mystery --selector 1 -j DROP"},
	} {
		rules := parseIptablesSave([]byte(header+common+tc.add+"COMMIT\n"), "IPv4", 4)
		if got := rules.sshPortConflict(22, 2222, false); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.add, got, tc.want)
		}
	}

	const nftPrefix = `{"nftables":[
		{"table":{"family":"inet","name":"external"}},
		{"chain":{"family":"inet","table":"external","name":"input","type":"filter","hook":"input","policy":"drop"}},
		{"chain":{"family":"inet","table":"external","name":"ssh_access"}},
		{"rule":{"family":"inet","table":"external","chain":"input","expr":[{"jump":{"target":"ssh_access"}}]}},
		{"rule":{"family":"inet","table":"external","chain":"ssh_access","expr":[
			{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":`
	const nftSuffix = `}},{"accept":null}]}}]}`
	for _, right := range []string{`{"set":[22,2222]}`, "22", `"@allowed"`} {
		rules, err := parseNftables([]byte(nftPrefix + right + nftSuffix))
		if err != nil {
			t.Fatal(err)
		}
		conflict := rules[0].sshPortConflict(22, 2222, false)
		if right == `{"set":[22,2222]}` {
			if conflict != "" {
				t.Fatalf("matching nftables rule blocked the move: %s", conflict)
			}
		} else if !strings.Contains(conflict, "ssh_access") || !strings.Contains(conflict, right) {
			t.Fatalf("nftables refusal did not identify the rule: %q", conflict)
		}
	}
}

func TestSSHKeepRemovesTheBootHelper(t *testing.T) {
	root := t.TempDir()
	system := filepath.Join(root, "etc", "systemd", "system")
	dir := filepath.Join(root, "agent", "access")
	systemctl := filepath.Join(root, "usr", "bin", "systemctl")
	for _, folder := range []string{dir, system, filepath.Dir(systemctl)} {
		if err := os.MkdirAll(folder, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\n[ \"$1\" = daemon-reload ]\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &sshSettings{reader: newSSHReader(Config{Tools: &hosttool.Runner{Root: root}})}
	if err := s.checkSSHRestoreDir(dir); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(dir, "ssh-wait-id"), filepath.Join(dir, "ssh-revert.sh")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("trial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.armSSHBootRestore(t.Context(), dir, &access.SSHChange{Unit: "ssh.service", Jail: &access.SSHJail{}}); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(system, sshRestoreUnit)
	unit, err := os.ReadFile(unitPath)
	if err != nil || !strings.Contains(string(unit), "\nConditionPathExists="+paths[0]+"\n") {
		t.Fatalf("boot helper does not require a pending trial: %v, %s", err, unit)
	}
	if err := s.Disarm(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	paths = append(paths, unitPath, filepath.Join(system, "ssh.service.d", sshRestoreDropIn), filepath.Join(system, "fail2ban.service.d", sshRestoreDropIn))
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("Keep left %s behind: %v", path, err)
		}
	}
	if err := os.Chmod(filepath.Dir(dir), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := s.checkSSHRestoreDir(dir); err == nil {
		t.Fatal("the root helper accepted a group-writable ancestor")
	}
}
