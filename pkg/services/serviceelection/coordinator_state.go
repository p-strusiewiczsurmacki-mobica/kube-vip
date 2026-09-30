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
	current         *campaign
	restartFailures int
}
