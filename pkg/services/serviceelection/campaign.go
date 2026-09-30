package serviceelection

import (
	"context"

	"github.com/kube-vip/kube-vip/pkg/lease"
)

type campaign struct {
	ctx          context.Context
	cancel       context.CancelFunc
	election     *lease.ElectionSession
	leaderCtx    context.Context
	cancelLeader context.CancelFunc
	vips         []string
	external     bool
	stopped      bool
}

func newCampaign(parent context.Context, svcLease *lease.Lease, vips []string) *campaign {
	electionSession, owner := svcLease.AcquireElection()
	ctx, cancel := svcLease.NewElectionContext(parent)
	return &campaign{
		ctx:      ctx,
		cancel:   cancel,
		election: electionSession,
		vips:     append([]string(nil), vips...),
		external: !owner,
	}
}
