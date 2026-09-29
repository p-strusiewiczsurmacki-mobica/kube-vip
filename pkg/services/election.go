package services

import (
	"context"
	log "log/slog"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/services/serviceelection"
	v1 "k8s.io/api/core/v1"
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

// newElectionCoordinatorManager wires the generic election coordinator to the
// Service processor. The coordinator itself does not depend on pkg/services.
func newElectionCoordinatorManager(p *Processor) *serviceelection.Manager {
	var leaseStore serviceelection.LeaseStore
	if p.leaseMgr != nil {
		leaseStore = p.leaseMgr
	}
	adapter := &electionAdapter{processor: p}
	return serviceelection.NewManager(serviceelection.Dependencies{
		Config: p.config, Leases: leaseStore, ElectionManager: p.electionMgr,
		State: adapter, Datapath: adapter, Runner: adapter, Scheduler: adapter,
	})
}

// IsCurrent implements serviceelection.ServiceState.

func (a *electionAdapter) IsCurrent(service *v1.Service, svcCtx *servicecontext.Context, readinessGeneration uint64) bool {
	p := a.processor
	p.serviceLock.Lock(service.UID)
	defer func() {
		if err := p.serviceLock.Unlock(service.UID); err != nil {
			log.Error("failed to release service lock", "uid", service.UID, "err", err)
		}
	}()
	currentCtx, err := p.getServiceContext(service.UID)
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
	return p.syncServicesWithContext(ctx, svcCtx, service, wg, true)
}

// Cleanup implements serviceelection.Datapath.
func (a *electionAdapter) Cleanup(ctx context.Context, service *v1.Service, svcCtx *servicecontext.Context,
	memberCurrent func() bool) error {
	p := a.processor
	p.serviceLock.Lock(service.UID)
	defer func() {
		if err := p.serviceLock.Unlock(service.UID); err != nil {
			log.Error("failed to release service lock", "uid", service.UID, "err", err)
		}
	}()

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
