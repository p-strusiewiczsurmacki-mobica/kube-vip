package serviceelection

import "context"

type campaign struct {
	ctx          context.Context
	cancel       context.CancelFunc
	leaderCtx    context.Context
	cancelLeader context.CancelFunc
	vips         []string
	external     bool
	stopped      bool
}

func (campaign *campaign) cancelRunner() {
	if campaign != nil && campaign.cancel != nil {
		campaign.cancel()
	}
}
