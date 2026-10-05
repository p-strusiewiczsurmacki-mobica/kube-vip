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
	role         lease.ElectionRole
	stopped      bool
}

func newCampaign(parent context.Context, svcLease *lease.Lease, vips []string) *campaign {
	participation := svcLease.JoinElection()
	ctx, cancel := svcLease.NewElectionContext(parent)
	role := lease.ElectionObserver
	if participation.RunsCampaign() {
		role = lease.ElectionRunner
	}
	return &campaign{
		ctx:      ctx,
		cancel:   cancel,
		election: participation.Session,
		vips:     append([]string(nil), vips...),
		role:     role,
	}
}

func (c *campaign) runsElection() bool {
	return c != nil && c.role == lease.ElectionRunner
}
