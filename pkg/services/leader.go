package services

import (
	"context"
	"fmt"
	"sync"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

// The StartServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesWatchForLeaderElection(ctx context.Context) error {
	err := p.ServicesWatcher(ctx, NewCallback(p.StartServicesLeaderElection, true))
	if err != nil {
		return err
	}

	if p.config.EnableRoutingTable {
		for _, instance := range p.ServiceInstances {
			for _, cluster := range instance.Clusters {
				for i := range cluster.Network {
					_ = cluster.Network[i].DeleteRoute()
				}
			}
		}
	}

	log.Info("Shutting down kube-Vip")

	return nil
}

// The startServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesLeaderElection(svcCtx *servicecontext.Context, service *v1.Service, _ *sync.WaitGroup, _ bool) error {
	if svcCtx == nil {
		return fmt.Errorf("no context context for service %q with UID %q: nil context", service.Name, service.UID)
	}

	leaseNamespace, serviceLease := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease)
	objectName := lease.ServiceNamespacedName(service)

	svcLease := p.leaseMgr.Get(id)
	if svcLease == nil {
		return fmt.Errorf("no existing lease found for service %q with UID %q", service.Name, service.UID)
	}

	isNew := svcLease.Add(objectName)

	// this service was already processed so we do not need to do anything
	if !isNew {
		log.Debug("this service was already handled, waiting for it to finish", "service", service.Name, "uid", service.UID)
		// Wait for either the service context or lease context to be done
		select {
		case <-svcCtx.Ctx.Done():
			// Service was deleted
			p.leaseMgr.Delete(id, objectName)
		case <-svcLease.Ctx.Done():
			// Leader election ended (leadership lost or context cancelled)
		}
		return nil
	}

	svcLease.Lock()

	defer func() {
		svcLease.Unlock()
	}()

	wg := sync.WaitGroup{}
	defer wg.Wait()

	// Start a goroutine that will delete the lease when the service context is cancelled.
	// This is important for proper cleanup when a service is deleted - it ensures that
	// the lease context (svcLease.Ctx) gets cancelled, which causes RunOrDie to return.
	// Without this, RunOrDie would continue running until leadership is naturally lost.
	wg.Go(func() {
		<-svcCtx.Ctx.Done()
		p.leaseMgr.Delete(id, objectName)
	})

	select {
	case <-svcCtx.Ctx.Done():
		return fmt.Errorf("context cancelled before election start: %w", svcCtx.Ctx.Err())
	case <-svcCtx.EndpointsReady:
	}

	// this service is sharing lease with another service
	if svcLease.Elected.Load() {
		svcLease.Unlock()
		// wait for leader election to start or context to be done
		select {
		case <-svcLease.Started:
		case <-svcLease.Ctx.Done():
			// Lease was cancelled (e.g., leader election ended), return immediately
			// This allows the restart loop to create a fresh lease
			log.Debug("lease context cancelled before leader election started", "service", service.Name, "uid", service.UID)
			return nil
		}

		p.onStartedLeading(svcCtx, svcLease, service, &wg)

		// Block until service context is cancelled
		<-svcCtx.Ctx.Done()

		p.onStoppedLeading(svcLease, service)

		// wait for leaderelection to be finished
		<-svcLease.Ctx.Done()

		return nil
	}

	// For new leases (not shared), ensure cleanup when the leader election ends
	// This is critical for the restartable service watcher to be able to restart
	// the leader election after leadership loss
	defer func() {
		// Delete the lease from the manager so subsequent calls can create a fresh lease
		// This handles the case where leader election ends due to:
		// 1. Leadership loss (e.g., network timeout)
		// 2. Context cancellation
		// 3. Any other reason RunOrDie returns
		p.leaseMgr.Delete(id, objectName)
	}()

	log.Info("new leader election", "service", service.Name, "namespace", service.Namespace, "lock_name", serviceLease, "host_id", p.config.NodeName)

	run := election.RunConfig{
		Config:           p.config,
		LeaseID:          id,
		Mgr:              p.electionMgr,
		LeaseAnnotations: map[string]string{},

		OnStartedLeading: func(_ context.Context) {
			svcLease.Elected.Store(true)
			svcLease.Unlock()
			close(svcLease.Started)
			// Mark this service as active (as we've started leading)
			// we run this in background as it's blocking
			p.onStartedLeading(svcCtx, svcLease, service, &wg)
		},
		OnStoppedLeading: func() {
			// we can do cleanup here
			svcLease.Elected.Store(false)
			log.Info("leadership lost", "service", service.Name, "uid", service.UID, "leader", p.config.NodeName)
			p.onStoppedLeading(svcLease, service)
			svcLease.Cancel()
		},
		OnNewLeader: func(identity string) {
			// we're notified when new leader elected
			if identity == p.config.NodeName {
				// I just got the lock
				return
			}
			log.Info("new leader", "leader", identity, "service", service.Name, "uid", service.UID)
		},
	}

	if err := election.RunOrDie(svcLease.Ctx, &run, p.config); err != nil {
		return fmt.Errorf("services election failed: %w", err)
	}

	log.Info("stopping leader election", "service", service.Name, "uid", service.UID)
	return nil
}

func (p *Processor) onStartedLeading(svcCtx *servicecontext.Context, svcLease *lease.Lease, service *v1.Service, wg *sync.WaitGroup) {
	// Mark this service as active (as we've started leading)
	// we run this in background as it's blocking
	if err := p.SyncServices(svcCtx, service, wg, true); err != nil {
		log.Error("service sync", "uid", service.UID, "err", err)
		svcLease.Cancel()
	}
}

func (p *Processor) onStoppedLeading(svcLease *lease.Lease, service *v1.Service) {
	log.Debug("deleting service due to lost leadership", "uid", service.UID)
	if err := p.deleteService(svcLease.Ctx, service.UID); err != nil {
		log.Error("service deletion", "err", err)
	}
}
