// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

// Reasons the CLI acts on, as the API names them under Dockwell-Error.
const (
	reasonChoice = "choice"
)

// Reason is the Dockwell-Error reason of an error from the API, if it has
// one.
func Reason(err error) string {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return ""
	}
	return cerr.Meta().Get(authapi.ReasonKey)
}

// ChoiceOf is the choice an error with reason "choice" offers.
func ChoiceOf(err error) *dockwellv1.Choice {
	var cerr *connect.Error
	if !errors.As(err, &cerr) || Reason(err) != reasonChoice {
		return nil
	}
	for _, d := range cerr.Details() {
		if v, err := d.Value(); err == nil {
			if c, ok := v.(*dockwellv1.Choice); ok {
				return c
			}
		}
	}
	return nil
}

// unreachable reports an error from not reaching the control plane at
// all, rather than one it sent.
func unreachable(err error) bool {
	var cerr *connect.Error
	return errors.As(err, &cerr) && !connect.IsWireError(cerr) && cerr.Code() == connect.CodeUnavailable
}

// explain turns an error from the API into one that says what to do from
// the CLI. The API's own messages are whole sentences with their fix
// (ADR 0017), so most go out as they are; reasons the CLI can fix better
// get its own words.
func explain(err error, c *Client, command string, names *strings.Replacer) error {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return err
	}
	msg := cerr.Message()
	if names != nil {
		// The API names its methods; the CLI names their commands.
		msg = names.Replace(msg)
	}
	switch Reason(err) {
	case authapi.ReasonTokenRejected:
		if c.Local {
			return errors.New("the control plane rejected the token for local access, even a new one. Check its logs, or sign in with dockwell login")
		}
		return fmt.Errorf("%s rejected your API token: it's unknown, revoked or expired. Make a new one in the panel, in Settings under API tokens, then run dockwell login %s",
			c.URL, c.URL)
	case authapi.ReasonTokenScope:
		if c.Local {
			return fmt.Errorf("local access on this server is read-only, so %s can't make changes. "+
				"Run dockwell login --local to let it make changes for %s, or sign in with a read-write API token", command, Span(auth.LocalTokenTTL))
		}
		return fmt.Errorf("your API token is read-only, so %s can't make changes. Make a read-write token in the panel, in Settings under API tokens, then run dockwell login %s",
			command, c.URL)
	case authapi.ReasonTokenLogs:
		return fmt.Errorf("your API token can't read logs, which often hold secrets that apps print. Make a token that can in the panel, in Settings under API tokens, then run dockwell login %s", c.URL)
	case authapi.ReasonNeedsSession:
		return ExitError{ExitNeedsPerson, fmt.Errorf("%s Open the panel at %s", sentence(msg), c.PanelURL("/"))}
	case authapi.ReasonMisdirected:
		return fmt.Errorf("%s isn't an address the panel answers on. %s Use the panel's domain, or 127.0.0.1 on the control server", c.URL, sentence(msg))
	case authapi.ReasonUnauthenticated:
		return fmt.Errorf("%s didn't take the call as signed in. Sign in with dockwell login %s", c.URL, c.URL)
	}
	switch {
	case unreachable(err):
		return fmt.Errorf("can't reach the control plane at %s: %s. Check that dockwell run is running there, and that this is its address",
			c.URL, strings.TrimPrefix(msg, "unavailable: "))
	case cerr.Code() == connect.CodeUnimplemented:
		return fmt.Errorf("the control plane at %s doesn't have this command's method, so it may be older than this CLI. Update it, or use a CLI of its version", c.URL)
	case cerr.Code() == connect.CodeCanceled && strings.HasPrefix(command, "dockwell logs"):
		// Ctrl-C ends a reader of logs, which nobody needs to be told of.
		return nil
	case cerr.Code() == connect.CodeCanceled && errors.Is(err, context.Canceled):
		return fmt.Errorf("%s stopped before the control plane answered, as it was interrupted. "+
			"Whatever it asked for may have gone through anyway: see dockwell changes list", command)
	}
	return errors.New(msg)
}

// Explain turns an error from the API into one that says what to do from
// the CLI, for command, which called c.
func Explain(err error, c *Client, command string) error { return explain(err, c, command, nil) }

func (r *runner) explain(err error) error { return explain(err, r.client, r.command, r.env.commands) }
