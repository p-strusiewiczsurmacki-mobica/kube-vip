package serviceelection

import (
	"context"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

// ServiceState validates the current Service context and readiness generation.
type ServiceState interface {
	IsCurrent(*v1.Service, *servicecontext.Context, uint64) bool
}

// Datapath activates and cleans up the network state of a Service.
type Datapath interface {
	Activate(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error
	Cleanup(context.Context, *v1.Service, *servicecontext.Context, func() bool) error
}

// CampaignRunner executes one Kubernetes leader-election campaign.
type CampaignRunner interface {
	RunCampaign(context.Context, *election.RunConfig) error
}

// RestartScheduler schedules a delayed campaign restart.
type RestartScheduler interface {
	ScheduleRestart(context.Context, time.Duration, *sync.WaitGroup, func())
}

// LeaseStore is the narrow lease ownership contract used by coordinators.
type LeaseStore interface {
	AcquireWithVIPProvider(context.Context, lease.ID, string, lease.VIPProvider) (*lease.Lease, bool)
	ClaimWithVIPProvider(lease.ID, string, lease.VIPProvider) (*lease.Lease, bool)
	Delete(lease.ID, string, *lease.Lease) bool
}

// Dependencies defines the collaborators required by Manager.
type Dependencies struct {
	Config          *kubevip.Config
	Leases          LeaseStore
	ElectionManager *election.Manager
	State           ServiceState
	Datapath        Datapath
	Runner          CampaignRunner
	Scheduler       RestartScheduler
}
