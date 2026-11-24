// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
)

// keepEveryProject reads the provider this edit replaces and keeps its
// every-project flag, unless the command explicitly set the flag. The revision
// prevents this read from overriding a concurrent change to reach.
func (r *runner) keepEveryProject(ctx context.Context, edit *dockwellv1.Edit) error {
	if err := r.connect(ctx); err != nil {
		return err
	}
	client := dockwellv1connect.NewDomainServiceClient(r.client.HTTP, r.client.URL, r.client.options()...)
	res, err := client.ListDnsProviders(ctx, connect.NewRequest(&dockwellv1.ListDnsProvidersRequest{}))
	if err != nil {
		return r.explain(err)
	}
	for _, p := range res.Msg.GetProviders() {
		if p.GetName() == edit.GetResource().GetName() {
			edit.GetSpec().GetDnsProvider().EveryProject = p.GetEveryProject()
			edit.BaseRevision = p.GetRevision()
			break
		}
	}
	return nil
}
