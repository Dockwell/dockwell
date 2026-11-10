// SPDX-License-Identifier: AGPL-3.0-only

package alert

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource"
)

// What a channel takes.
const (
	// DefaultNtfyServer is where an ntfy channel publishes unless it says.
	DefaultNtfyServer = "https://ntfy.sh"
	// MaxRecipients bounds an email channel's addresses.
	MaxRecipients = 10
)

// The ports an email channel uses unless it says, by how it's secured.
const (
	PortSTARTTLS = 587
	PortTLS      = 465
	PortPlain    = 25
)

var (
	ntfyTopic     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	telegramToken = regexp.MustCompile(`^\d{3,20}:[A-Za-z0-9_-]{20,100}$`)
	telegramChat  = regexp.MustCompile(`^(-?\d{1,20}|@[A-Za-z][A-Za-z0-9_]{3,31})$`)
	hostName      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

// Channel is the alert channel kind.
type Channel struct{}

var (
	_ resource.Type       = Channel{}
	_ resource.Unplaced   = Channel{}
	_ resource.Rotates    = Channel{}
	_ resource.Keeper     = Channel{}
	_ resource.NewChecker = Channel{}
)

// Kind returns "alert.channel".
func (Channel) Kind() resource.Kind { return ChannelKind }

// Level is the organization: any rule can send to it.
func (Channel) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "ach".
func (Channel) IDPrefix() string { return id.AlertChannel }

// New returns an empty AlertChannel.
func (Channel) New() proto.Message { return &resourcev1.AlertChannel{} }

// Unplaced says the control plane sends to it.
func (Channel) Unplaced() {}

// RotatesSensitive says setting what signs in always plans as a change, never
// compared with what's there, so a plan can't confirm a guess at it (ADR 0022).
func (Channel) RotatesSensitive() {}

// CheckName takes what fits in a URL and on a command line.
func (Channel) CheckName(name string) error {
	return resource.CheckLabel("alert channel", name, "ops-webhook")
}

// Keep holds on to what signs in to a channel when an update leaves it out, so a
// form can change the rest without asking for a token again. It keeps only from a
// channel of the same type, and only while what it signs in to stays: a password
// is never sent to a server someone has just named (ADR 0044).
func (Channel) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.AlertChannel)
	is, same := desired.(*resourcev1.AlertChannel)
	if !ok || !same || Type(was) == "" || Type(was) != Type(is) {
		return resource.Kept{}, nil
	}
	from, to := body(was), body(is)
	missing := sealedEmpty(from, to)
	if len(missing) == 0 {
		return resource.Kept{}, nil
	}
	if moved := movedFrom(was, is); moved != "" {
		return resource.Kept{}, fmt.Errorf("%s, so it needs %s again: Dockwell won't send what it kept to where it hasn't sent before", moved, list(missing))
	}
	for _, fd := range missing {
		to.Set(fd, from.Get(fd))
	}
	return resource.Kept{Reason: "kept_credentials", Words: "keeps what signs in to it, as the update left that out"}, nil
}

// movedFrom says how a channel's update changes where what it kept signs in, in
// a clause, or "" when it doesn't.
func movedFrom(was, is *resourcev1.AlertChannel) string {
	switch {
	case is.GetWebhook() != nil && is.GetWebhook().GetUrl() != "" && host(is.GetWebhook().GetUrl()) != host(was.GetWebhook().GetUrl()):
		return "the webhook moves to another host"
	case is.GetEmail() != nil:
		a, b := was.GetEmail(), is.GetEmail()
		if !strings.EqualFold(a.GetHost(), b.GetHost()) || EmailPort(a) != EmailPort(b) || EmailSecurity(a) != EmailSecurity(b) || a.GetUsername() != b.GetUsername() {
			return "the email's server, port, security or user changes"
		}
	case is.GetNtfy() != nil && NtfyServer(was.GetNtfy()) != NtfyServer(is.GetNtfy()):
		return "ntfy's server changes"
	}
	return ""
}

// sealedEmpty lists the fields that sign in which an update left empty and the
// channel has.
func sealedEmpty(from, to protoreflect.Message) []protoreflect.FieldDescriptor {
	var out []protoreflect.FieldDescriptor
	fields := to.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if resource.IsSensitive(fd) && !to.Has(fd) && from.Has(fd) {
			out = append(out, fd)
		}
	}
	return out
}

func list(fds []protoreflect.FieldDescriptor) string {
	names := make([]string, len(fds))
	for i, fd := range fds {
		names[i] = strings.ReplaceAll(string(fd.Name()), "_", " ")
	}
	switch len(names) {
	case 1:
		return "its " + names[0]
	default:
		return "its " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// body is the message of a channel's type, or nil.
func body(c *resourcev1.AlertChannel) protoreflect.Message {
	r := c.ProtoReflect()
	fd := r.WhichOneof(r.Descriptor().Oneofs().ByName("to"))
	if fd == nil {
		return nil
	}
	return r.Get(fd).Message()
}

// Sealed lists the fields that sign in which a channel has, by their path, such
// as "email.password".
func Sealed(c *resourcev1.AlertChannel) []string {
	r := c.ProtoReflect()
	fd := r.WhichOneof(r.Descriptor().Oneofs().ByName("to"))
	if fd == nil {
		return nil
	}
	var out []string
	m := r.Get(fd).Message()
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		if f := fields.Get(i); resource.IsSensitive(f) && m.Has(f) {
			out = append(out, string(fd.Name())+"."+string(f.Name()))
		}
	}
	return out
}

// Redacted is a channel with every field that signs in left empty, for a form.
func Redacted(c *resourcev1.AlertChannel) *resourcev1.AlertChannel {
	out := proto.CloneOf(c)
	if m := body(out); m != nil {
		fields := m.Descriptor().Fields()
		for i := range fields.Len() {
			if f := fields.Get(i); resource.IsSensitive(f) {
				m.Clear(f)
			}
		}
	}
	return out
}

// CheckNew asks a new channel for everything it sends with. An update can leave
// what signs in out, which Keep fills in, so Validate can't.
func (Channel) CheckNew(spec proto.Message) error {
	c := spec.(*resourcev1.AlertChannel) //nolint:forcetypeassert // Validate saw it
	switch {
	case c.GetWebhook() != nil && c.GetWebhook().GetUrl() == "":
		return errors.New("a webhook channel needs its URL, such as https://hooks.example.com/dockwell")
	case c.GetSlack() != nil && c.GetSlack().GetUrl() == "":
		return errors.New("a Slack channel needs its incoming webhook's URL, which the Slack app's Incoming Webhooks page gives")
	case c.GetDiscord() != nil && c.GetDiscord().GetUrl() == "":
		return errors.New("a Discord channel needs its webhook's URL, which the channel's settings give under Integrations")
	case c.GetTelegram() != nil && c.GetTelegram().GetBotToken() == "":
		return errors.New("a Telegram channel needs its bot's token, which @BotFather gives")
	case c.GetNtfy() != nil && c.GetNtfy().GetTopic() == "":
		return errors.New("an ntfy channel needs its topic, such as dockwell-alerts-x7f2")
	case c.GetEmail() != nil && c.GetEmail().GetUsername() != "" && c.GetEmail().GetPassword() == "":
		return fmt.Errorf("an email channel that signs in as %s needs its password", c.GetEmail().GetUsername())
	}
	return nil
}

// Validate refuses what can't be sent to, in words that hold nothing that signs
// in, and fills in what a channel leaves to Dockwell.
func (Channel) Validate(spec proto.Message) error {
	c := spec.(*resourcev1.AlertChannel) //nolint:forcetypeassert // the registry gives this kind's message
	switch to := c.GetTo().(type) {
	case *resourcev1.AlertChannel_Webhook:
		if to.Webhook == nil {
			to.Webhook = &resourcev1.AlertWebhook{}
		}
		return checkSecretURL(&to.Webhook.Url, webhookURL)
	case *resourcev1.AlertChannel_Slack:
		if to.Slack == nil {
			to.Slack = &resourcev1.AlertSlack{}
		}
		return checkSecretURL(&to.Slack.Url, slackURL)
	case *resourcev1.AlertChannel_Discord:
		if to.Discord == nil {
			to.Discord = &resourcev1.AlertDiscord{}
		}
		return checkSecretURL(&to.Discord.Url, discordURL)
	case *resourcev1.AlertChannel_Ntfy:
		if to.Ntfy == nil {
			to.Ntfy = &resourcev1.AlertNtfy{}
		}
		return checkNtfy(to.Ntfy)
	case *resourcev1.AlertChannel_Telegram:
		if to.Telegram == nil {
			to.Telegram = &resourcev1.AlertTelegram{}
		}
		return checkTelegram(to.Telegram)
	case *resourcev1.AlertChannel_Email:
		if to.Email == nil {
			to.Email = &resourcev1.AlertEmail{}
		}
		return checkEmail(to.Email)
	default:
		return errors.New("a channel needs somewhere to send: a webhook, email, ntfy, Slack, Discord or Telegram")
	}
}

// urlWords are how a channel's URL is named in what Dockwell says of it.
type urlWords struct{ what, example string }

var (
	webhookURL = urlWords{"a webhook's URL", "https://hooks.example.com/dockwell"}
	slackURL   = urlWords{"a Slack webhook's URL", "https://hooks.slack.com/services/T000/B000/XXXX"}
	discordURL = urlWords{"a Discord webhook's URL", "https://discord.com/api/webhooks/0000/XXXX"}
	ntfyURL    = urlWords{"ntfy's server", DefaultNtfyServer}
)

// checkSecretURL checks a URL that signs in by itself, leaving an empty one to
// CheckNew and Keep.
func checkSecretURL(raw *string, words urlWords) error {
	*raw = strings.TrimSpace(*raw)
	if *raw == "" {
		return nil
	}
	return checkURL(*raw, words)
}

// CheckURL refuses a webhook's URL a channel can't send to, saying nothing of what
// it holds beyond its scheme and host.
func CheckURL(raw string) error { return checkURL(raw, webhookURL) }

// checkURL holds every URL a person gives a channel to the same rules: HTTPS,
// unless it's on this server or its private network; no user and password in it;
// and no link-local address, where a cloud keeps its metadata service. The sender
// checks each address a name resolves to again when it connects.
func checkURL(raw string, w urlWords) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%s is an address such as %s", w.what, w.example)
	}
	if u.User != nil {
		return fmt.Errorf("%s can't hold a user and password. Use a token in its path or query", w.what)
	}
	if why := netguard.Forbidden(u.Hostname(), Guard); why != "" {
		return fmt.Errorf("%s can't be %s, as that's %s", w.what, u.Hostname(), why)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !Nearby(u.Hostname()) {
			return fmt.Errorf("%s to %s has to use HTTPS, as an alert says what's wrong where anyone between could read it. Only an address on this server or its private network may use plain HTTP", w.what, u.Hostname())
		}
	default:
		return fmt.Errorf("%s starts with https://", w.what)
	}
	return nil
}

func checkNtfy(n *resourcev1.AlertNtfy) error {
	n.Server = strings.TrimSuffix(strings.TrimSpace(n.GetServer()), "/")
	if n.GetServer() == "" {
		n.Server = DefaultNtfyServer
	}
	if err := checkURL(n.GetServer(), ntfyURL); err != nil {
		return err
	}
	if u, _ := url.Parse(n.GetServer()); u.RawQuery != "" || u.Fragment != "" {
		return errors.New("ntfy's server is its address alone, such as https://ntfy.sh, with the topic apart")
	}
	n.Topic = strings.TrimSpace(n.GetTopic())
	if n.GetTopic() != "" && !ntfyTopic.MatchString(n.GetTopic()) {
		return errors.New("an ntfy topic is 1 to 64 letters, digits, dashes and underscores")
	}
	n.Token = strings.TrimSpace(n.GetToken())
	if strings.ContainsAny(n.GetToken(), " \r\n") {
		return errors.New("an ntfy access token is one word, such as tk_…")
	}
	return nil
}

func checkTelegram(tg *resourcev1.AlertTelegram) error {
	tg.BotToken = strings.TrimSpace(tg.GetBotToken())
	if tg.GetBotToken() != "" && !telegramToken.MatchString(tg.GetBotToken()) {
		return errors.New("a Telegram bot's token is its number, a colon and its secret, as @BotFather gives it")
	}
	tg.ChatId = strings.TrimSpace(tg.GetChatId())
	if !telegramChat.MatchString(tg.GetChatId()) {
		return errors.New("a Telegram chat is its ID, such as -1001234567890, or a channel's name, such as @example_alerts")
	}
	return nil
}

func checkEmail(e *resourcev1.AlertEmail) error {
	e.Host = strings.TrimSuffix(strings.TrimSpace(e.GetHost()), ".")
	switch {
	case e.GetHost() == "":
		return errors.New("an email channel needs its SMTP server, such as smtp.example.com")
	case !hostName.MatchString(e.GetHost()) && net.ParseIP(e.GetHost()) == nil:
		return errors.New("an SMTP server is its name or address alone, such as smtp.example.com, with its port apart")
	}
	if why := netguard.Forbidden(e.GetHost(), Guard); why != "" {
		return fmt.Errorf("the SMTP server can't be %s, as that's %s", e.GetHost(), why)
	}
	switch e.GetSecurity() {
	case resourcev1.AlertEmail_SECURITY_UNSPECIFIED:
		e.Security = resourcev1.AlertEmail_SECURITY_STARTTLS
	case resourcev1.AlertEmail_SECURITY_STARTTLS, resourcev1.AlertEmail_SECURITY_TLS:
	case resourcev1.AlertEmail_SECURITY_NONE:
		if !Nearby(e.GetHost()) {
			return fmt.Errorf("email to %s has to use TLS, as an alert, and the password, would cross the internet where anyone between could read them. Only a server on this server or its private network may go without", e.GetHost())
		}
	default:
		return errors.New("an email channel's security is SECURITY_STARTTLS, SECURITY_TLS or SECURITY_NONE")
	}
	if e.GetPort() > 65535 {
		return errors.New("an SMTP port is from 1 to 65535, such as 587")
	}
	if e.GetPort() == 0 {
		e.Port = uint32(EmailPort(e)) //nolint:gosec // one of three ports, each under 65536
	}
	e.Username = strings.TrimSpace(e.GetUsername())
	if strings.ContainsAny(e.GetUsername(), "\r\n") {
		return errors.New("an SMTP user is one line")
	}
	if e.GetUsername() != "" && e.GetSecurity() == resourcev1.AlertEmail_SECURITY_NONE {
		return errors.New("an email channel signs in only over TLS, as Dockwell sends a password no other way. Use STARTTLS or TLS, or a server that takes mail from this one without signing in")
	}
	e.From = strings.TrimSpace(e.GetFrom())
	if _, err := mail.ParseAddress(e.GetFrom()); err != nil {
		return errors.New("an email channel needs who its emails come from, such as Dockwell <alerts@example.com>")
	}
	if len(e.GetTo()) == 0 {
		return errors.New("an email channel needs who its emails go to, such as ops@example.com")
	}
	if len(e.GetTo()) > MaxRecipients {
		return fmt.Errorf("an email channel goes to at most %d addresses. Use a list address for more", MaxRecipients)
	}
	for i, to := range e.GetTo() {
		a, err := mail.ParseAddress(strings.TrimSpace(to))
		if err != nil {
			return fmt.Errorf("%q isn't an email address, such as ops@example.com", strings.TrimSpace(to))
		}
		e.To[i] = a.Address
	}
	return nil
}

// EmailSecurity is how an email channel secures its connection, STARTTLS unless
// it says.
func EmailSecurity(e *resourcev1.AlertEmail) resourcev1.AlertEmail_Security {
	if e.GetSecurity() == resourcev1.AlertEmail_SECURITY_UNSPECIFIED {
		return resourcev1.AlertEmail_SECURITY_STARTTLS
	}
	return e.GetSecurity()
}

// EmailPort is the port an email channel connects to: its own, or its
// security's.
func EmailPort(e *resourcev1.AlertEmail) int {
	if e.GetPort() != 0 {
		return int(e.GetPort())
	}
	switch EmailSecurity(e) {
	case resourcev1.AlertEmail_SECURITY_TLS:
		return PortTLS
	case resourcev1.AlertEmail_SECURITY_NONE:
		return PortPlain
	default:
		return PortSTARTTLS
	}
}

// NtfyServer is where an ntfy channel publishes.
func NtfyServer(n *resourcev1.AlertNtfy) string {
	if s := strings.TrimSuffix(n.GetServer(), "/"); s != "" {
		return s
	}
	return DefaultNtfyServer
}

// Nearby reports a host on this server or its private network, by its address or
// a name only such a network gives.
func Nearby(host string) bool {
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addr = addr.Unmap()
		// Carrier-grade NAT's, as a tailnet's are, is a private network too, as
		// netguard counts it.
		return addr.IsLoopback() || addr.IsPrivate() || cgnat.Contains(addr)
	}
	for _, suffix := range []string{".internal", ".lan", ".local", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// Guard is what a channel may reach (netguard, #341): this server and its private
// networks as well as the internet, as a webhook, a chat server or an SMTP server on
// the install's own network is what many want, and a channel is made and tested only
// by a person or a token that can make changes. Never a link-local or metadata
// address.
var Guard = netguard.Options{Private: true, Loopback: true}

// TestGuard is what a test of a channel may reach: what Guard does but this
// server's own addresses, so a test, which shows what answered, can't read what
// listens on the control server's loopback.
var TestGuard = netguard.Options{Private: true}

// cgnat is carrier-grade NAT's range.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Type is what sort of channel it is, such as "webhook".
func Type(c *resourcev1.AlertChannel) string {
	switch c.GetTo().(type) {
	case *resourcev1.AlertChannel_Webhook:
		return "webhook"
	case *resourcev1.AlertChannel_Email:
		return "email"
	case *resourcev1.AlertChannel_Ntfy:
		return "ntfy"
	case *resourcev1.AlertChannel_Slack:
		return "slack"
	case *resourcev1.AlertChannel_Discord:
		return "discord"
	case *resourcev1.AlertChannel_Telegram:
		return "telegram"
	default:
		return ""
	}
}

// Target is where a channel sends, with nothing that signs in: a URL's host,
// whom an email goes to, and "+N" for more, or a Telegram chat. It holds no
// words, so it reads the same in every language.
func Target(c *resourcev1.AlertChannel) string {
	switch to := c.GetTo().(type) {
	case *resourcev1.AlertChannel_Webhook:
		return host(to.Webhook.GetUrl())
	case *resourcev1.AlertChannel_Slack:
		return host(to.Slack.GetUrl())
	case *resourcev1.AlertChannel_Discord:
		return host(to.Discord.GetUrl())
	case *resourcev1.AlertChannel_Ntfy:
		return host(NtfyServer(to.Ntfy))
	case *resourcev1.AlertChannel_Telegram:
		return to.Telegram.GetChatId()
	case *resourcev1.AlertChannel_Email:
		switch n := len(to.Email.GetTo()); n {
		case 0:
			return ""
		case 1:
			return to.Email.GetTo()[0]
		default:
			return to.Email.GetTo()[0] + " +" + strconv.Itoa(n-1)
		}
	}
	return ""
}

// Host is the server a channel connects to, with nothing that signs in: a URL's
// host, Telegram's API, or the SMTP server.
func Host(c *resourcev1.AlertChannel) string {
	switch to := c.GetTo().(type) {
	case *resourcev1.AlertChannel_Telegram:
		return "api.telegram.org"
	case *resourcev1.AlertChannel_Email:
		return to.Email.GetHost()
	}
	return Target(c)
}

func host(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}
