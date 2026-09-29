package services

import (
	"context"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/services/serviceelection"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

var (
	_ serviceelection.ServiceState     = (*electionAdapter)(nil)
	_ serviceelection.Datapath         = (*electionAdapter)(nil)
	_ serviceelection.CampaignRunner   = (*electionAdapter)(nil)
	_ serviceelection.RestartScheduler = (*electionAdapter)(nil)
)

type electionAdapter struct {
	processor *Processor
}

// electionCoordinatorManager wires the generic election coordinator to the
// Service processor. The coordinator itself does not depend on pkg/services.
func (p *Processor) electionCoordinatorManager() *serviceelection.Manager {
	p.electionCoordinatorsOnce.Do(func() {
		var leaseStore serviceelection.LeaseStore
		if p.leaseMgr != nil {
			leaseStore = p.leaseMgr
		}
		adapter := &electionAdapter{processor: p}
		p.electionCoordinators = serviceelection.NewManager(serviceelection.Dependencies{
			Config: p.config, Leases: leaseStore, ElectionManager: p.electionMgr,
			State: adapter, Datapath: adapter, Runner: adapter, Scheduler: adapter,
		})
	})
	return p.electionCoordinators
}

func (p *Processor) leaveElectionCoordinatorForContext(svcCtx *servicecontext.Context, service *v1.Service) {
	p.electionCoordinatorManager().LeaveForContext(svcCtx, service)
}

func (p *Processor) watchElectionCoordinator(svcCtx *servicecontext.Context, service *v1.Service, wg *sync.WaitGroup) {
	p.electionCoordinatorManager().Watch(svcCtx, service, wg)
}

func (p *Processor) currentServiceContext(uid types.UID) (*servicecontext.Context, error) {
	unlockService := p.lockService(uid)
	defer unlockService()
	return p.getServiceContext(uid)
}

// IsCurrent implements serviceelection.ServiceState.

func (a *electionAdapter) IsCurrent(service *v1.Service, svcCtx *servicecontext.Context, readinessGeneration uint64) bool {
	p := a.processor
	currentCtx, err := p.currentServiceContext(service.UID)
	if err != nil || currentCtx != svcCtx || svcCtx.Ctx.Err() != nil ||
		!svcCtx.ReadinessGenerationCurrent(readinessGeneration) {
		return false
	}
	return true
}

// Activate implements serviceelection.Datapath.
func (a *electionAdapter) Activate(ctx context.Context, service *v1.Service, svcCtx *servicecontext.Context,
	wg *sync.WaitGroup) error {
	p := a.processor
	return p.syncServices(ctx, svcCtx, service, wg, true)
}

// Cleanup implements serviceelection.Datapath.
func (a *electionAdapter) Cleanup(ctx context.Context, service *v1.Service, svcCtx *servicecontext.Context,
	memberCurrent func() bool) error {
	p := a.processor
	unlockService := p.lockService(service.UID)
	defer unlockService()

	currentSvcCtx, err := p.getServiceContext(service.UID)
	if err != nil {
		return err
	}
	if currentSvcCtx != svcCtx || !memberCurrent() {
		return nil
	}
	return p.deleteCurrentServiceByUID(ctx, service.UID)
}

// RunCampaign implements serviceelection.CampaignRunner.
func (a *electionAdapter) RunCampaign(ctx context.Context, run *election.RunConfig) error {
	p := a.processor
	if p.electionRun != nil {
		return p.electionRun(ctx, run, p.config)
	}
	return election.RunOrDie(ctx, run, p.config)
}

// ScheduleRestart implements serviceelection.RestartScheduler.
func (a *electionAdapter) ScheduleRestart(ctx context.Context, delay time.Duration, wg *sync.WaitGroup, restart func()) {
	p := a.processor
	if p.scheduleElectionRestart != nil {
		p.scheduleElectionRestart(restart)
		return
	}
	wg.Go(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			restart()
		}
	})
}

func (p *Processor) syncServices(operationCtx context.Context, svcCtx *servicecontext.Context,
	service *v1.Service, wg *sync.WaitGroup, usesLeaderElection bool) error {
	if p.serviceSync != nil {
		return p.serviceSync(operationCtx, svcCtx, service, wg, usesLeaderElection)
	}
	return p.syncServicesWithContext(operationCtx, svcCtx, service, wg, usesLeaderElection)
}
