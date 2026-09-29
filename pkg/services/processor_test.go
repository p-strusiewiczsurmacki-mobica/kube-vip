package services

import (
	"context"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/services/serviceelection"
	v1 "k8s.io/api/core/v1"
)

// newTestServiceLocks supplies the invariant normally established by
// NewServicesProcessor to tests that need a partially configured Processor.
func newTestServiceLocks() *ServiceLock {
	return NewServiceLock()
}

type testElectionDatapath struct {
	production *electionAdapter
	activate   func(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error
}

func (d *testElectionDatapath) Activate(ctx context.Context, service *v1.Service,
	svcCtx *servicecontext.Context, wg *sync.WaitGroup) error {
	if d.activate == nil {
		return nil
	}
	return d.activate(ctx, service, svcCtx, wg)
}

func (d *testElectionDatapath) Cleanup(ctx context.Context, service *v1.Service,
	svcCtx *servicecontext.Context, current func() bool) error {
	return d.production.Cleanup(ctx, service, svcCtx, current)
}

// initializeTestElectionCoordinators supplies the invariant normally
// established by NewServicesProcessor after a partial test Processor is built.
// Tests replace only datapath activation; cleanup retains production behavior.
func initializeTestElectionCoordinators(processor *Processor,
	activate ...func(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error) {
	adapter := &electionAdapter{processor: processor}
	datapath := &testElectionDatapath{production: adapter}
	if len(activate) != 0 {
		datapath.activate = activate[0]
	}
	var leaseStore serviceelection.LeaseStore
	if processor.leaseMgr != nil {
		leaseStore = processor.leaseMgr
	}
	processor.electionCoordinators = serviceelection.NewManager(serviceelection.Dependencies{
		Config: processor.config, Leases: leaseStore, ElectionManager: processor.electionMgr,
		State: adapter, Datapath: datapath, Runner: adapter, Scheduler: adapter,
	})
}

func TestNewServicesProcessorInitializesElectionCoordinators(t *testing.T) {
	processor := NewServicesProcessor(&kubevip.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if processor.electionCoordinators == nil {
		t.Fatal("NewServicesProcessor() did not initialize election coordinators")
	}
}
