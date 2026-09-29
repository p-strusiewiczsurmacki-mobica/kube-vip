package serviceelection

import (
	"sync"

	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

type member struct {
	coordinator         *coordinator
	service             *v1.Service
	serviceContext      *servicecontext.Context
	readinessGeneration uint64
	claimToken          string
	vipProvider         lease.VIPProvider
	operationMutex      sync.Mutex
	active              bool
}

// close deactivates the member datapath and withdraws its lease claim. It is
// safe to call more than once and stale members cannot remove replacements.
func (m *member) close() {
	if m == nil || m.coordinator == nil {
		return
	}
	m.deactivate()
	m.withdraw()
}

func (m *member) deactivate() {
	m.operationMutex.Lock()
	defer m.operationMutex.Unlock()
	m.coordinator.deactivateMemberOperationHeld(m)
}

func (m *member) withdraw() {
	if m == nil || m.coordinator == nil {
		return
	}
	m.coordinator.leave(m)
}
