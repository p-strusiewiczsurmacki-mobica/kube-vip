package serviceelection

import (
	"context"

	"github.com/kube-vip/kube-vip/pkg/lease"
)

type campaignRole uint8

const (
	campaignRunner campaignRole = iota
	campaignObserver
)

type campaign struct {
	ctx          context.Context
	cancel       context.CancelFunc
	election     *lease.ElectionSession
	leaderCtx    context.Context
	cancelLeader context.CancelFunc
	vips         []string
	role         campaignRole
	stopped      bool
}

func newCampaign(parent context.Context, svcLease *lease.Lease, vips []string) *campaign {
	electionSession, runsCampaign := svcLease.AcquireElection()
	ctx, cancel := svcLease.NewElectionContext(parent)
	role := campaignObserver
	if runsCampaign {
		role = campaignRunner
	}
	return &campaign{
		ctx:      ctx,
		cancel:   cancel,
		election: electionSession,
		vips:     append([]string(nil), vips...),
		role:     role,
	}
}

func (c *campaign) runsElection() bool {
	return c != nil && c.role == campaignRunner
}
