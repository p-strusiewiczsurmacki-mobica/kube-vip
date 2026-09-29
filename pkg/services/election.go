package services

import (
	"context"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/services/serviceelection"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

var (
	_ serviceelection.ServiceState     = (*Processor)(nil)
	_ serviceelection.Datapath         = (*Processor)(nil)
	_ serviceelection.CampaignRunner   = (*Processor)(nil)
	_ serviceelection.RestartScheduler = (*Processor)(nil)
)

// electionCoordinatorManager wires the generic election coordinator to the
// Service processor. The coordinator itself does not depend on pkg/services.
func (p *Processor) electionCoordinatorManager() *serviceelection.Manager {
	p.electionCoordinatorsOnce.Do(func() {
		var leaseStore serviceelection.LeaseStore
		if p.leaseMgr != nil {
			leaseStore = p.leaseMgr
		}
		p.electionCoordinators = serviceelection.NewManager(p.config, leaseStore, p.electionMgr,
			p, p, p, p)
	})
	return p.electionCoordinators
}

func (p *Processor) joinElectionCoordinator(svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration uint64) (*serviceelection.Member, bool) {
	return p.electionCoordinatorManager().Join(svcCtx, service, readinessGeneration)
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
func (p *Processor) IsCurrent(service *v1.Service, svcCtx *servicecontext.Context, readinessGeneration uint64) bool {
	currentCtx, err := p.currentServiceContext(service.UID)
	if err != nil || currentCtx != svcCtx || svcCtx.Ctx.Err() != nil ||
		!svcCtx.ReadinessGenerationCurrent(readinessGeneration) {
		return false
	}
	return true
}

// ActivateMember implements serviceelection.Datapath.
func (p *Processor) ActivateMember(ctx context.Context, member *serviceelection.Member, _ *lease.Lease,
	wg *sync.WaitGroup) error {
	return p.syncServices(ctx, member.ServiceContext(), member.Service(), wg, true)
}

// CleanupMember implements serviceelection.Datapath.
func (p *Processor) CleanupMember(member *serviceelection.Member, svcLease *lease.Lease) error {
	service := member.Service()
	unlockService := p.lockService(service.UID)
	defer unlockService()

	currentSvcCtx, err := p.getServiceContext(service.UID)
	if err != nil {
		return err
	}
	if currentSvcCtx != member.ServiceContext() || !member.Coordinator().Contains(member) {
		return nil
	}
	return p.deleteCurrentServiceByUID(context.WithoutCancel(svcLease.Ctx), service.UID)
}

// RunCampaign implements serviceelection.CampaignRunner.
func (p *Processor) RunCampaign(ctx context.Context, run *election.RunConfig) error {
	if p.electionRun != nil {
		return p.electionRun(ctx, run, p.config)
	}
	return election.RunOrDie(ctx, run, p.config)
}

// ScheduleRestart implements serviceelection.RestartScheduler.
func (p *Processor) ScheduleRestart(ctx context.Context, delay time.Duration, wg *sync.WaitGroup, restart func()) {
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
