package serviceelection

import (
	"github.com/kube-vip/kube-vip/pkg/lease"
	"k8s.io/apimachinery/pkg/types"
)

// coordinatorMembership is the membership and shared-Lease state protected by
// coordinator.mutex.
type coordinatorMembership struct {
	members map[types.UID]*member
	lease   *lease.Lease
}

// coordinatorCampaignState is the campaign and retry state protected by
// coordinator.mutex.
type coordinatorCampaignState struct {
	current *campaign

	// restartFailures counts consecutive campaigns that ended via
	// cancelCampaign (an activation failure with no other ready member)
	// rather than a normal leadership change. It backs off campaign restarts
	// and resets on the next successful activation.
	restartFailures int
}
