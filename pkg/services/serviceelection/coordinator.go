// Package serviceelection coordinates Service membership, shared leases, and
// leader-election campaigns without owning Kubernetes Service state or network
// datapaths. Callers provide those capabilities through focused interfaces.
package serviceelection

import (
	"context"
	log "log/slog"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// coordinator owns membership and campaign lifetime for one lease.
// Service contexts remain responsible for endpoint readiness and datapath work.
type coordinator struct {
	registry     *registry
	dependencies Dependencies
	id           lease.ID

	mutex        sync.Mutex
	members      map[types.UID]*member
	lease        *lease.Lease
	campaign     *campaign
	retired      bool
	retiredDone  chan struct{}
	retiredCtx   context.Context
	retireCancel context.CancelFunc

	// restartFailures counts consecutive campaigns that ended via
	// cancelCampaign (an activation failure with no other ready member)
	// rather than a normal leadership change. It backs off campaign restarts
	// and resets on the next successful activation.
	restartFailures int
}

const (
	restartBaseDelay = 200 * time.Millisecond
	restartMaxDelay  = 30 * time.Second
)

func (e *coordinator) contains(member *member) bool {
	if e == nil || member == nil || member.service == nil {
		return false
	}
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return !e.retired && e.members[member.service.UID] == member
}

func (e *coordinator) join(svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration uint64) (*member, bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if e.retired {
		return nil, false
	}
	if member := e.members[service.UID]; member != nil && member.serviceContext == svcCtx &&
		member.readinessGeneration == readinessGeneration {
		return member, true
	}

	if previous := e.members[service.UID]; previous != nil {
		e.dependencies.Leases.Delete(e.id, previous.claimToken, e.lease)
	}
	member := newMember(e, svcCtx, service, readinessGeneration)
	member.claimToken = e.registry.nextToken()
	member.vipProvider = serviceVIPProvider(member.service)
	e.members[service.UID] = member

	// A member can become ready again while the old campaign is still stopping.
	// Keep its new generation until that runner finishes; finishCampaign will
	// rebuild the lease and launch the replacement campaign.
	if e.lease != nil && e.lease.Ctx.Err() != nil && e.campaign != nil {
		return member, true
	}
	if e.lease == nil || e.lease.Ctx.Err() != nil {
		if e.createLeaseLocked() == nil {
			delete(e.members, service.UID)
			return nil, false
		}
		return member, true
	}
	if claimed, _ := e.dependencies.Leases.ClaimWithVIPProvider(e.id, member.claimToken, member.vipProvider); claimed != nil {
		return member, true
	}

	// An external cleanup retired the manager entry. Rebuild it from the live
	// coordinator snapshot rather than admitting a member to a dead lease.
	e.lease = nil
	if e.createLeaseLocked() == nil {
		delete(e.members, service.UID)
		return nil, false
	}
	return member, true
}

func (e *coordinator) createLeaseLocked() *lease.Lease {
	if e.lease != nil && e.lease.Ctx.Err() == nil {
		return e.lease
	}
	var first *member
	for _, member := range e.members {
		first = member
		break
	}
	if first == nil {
		return nil
	}
	svcLease, _ := e.dependencies.Leases.AcquireWithVIPProvider(context.Background(), e.id, first.claimToken,
		first.vipProvider)
	for _, member := range e.members {
		if member == first {
			continue
		}
		if claimed, _ := e.dependencies.Leases.ClaimWithVIPProvider(e.id, member.claimToken, member.vipProvider); claimed == nil {
			svcLease.Cancel()
			return nil
		}
	}
	e.lease = svcLease
	return svcLease
}

func (e *coordinator) currentMember(uid types.UID) *member {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.members[uid]
}

func (e *coordinator) deactivateMember(member *member) {
	if member == nil {
		return
	}
	member.operationMutex.Lock()
	defer member.operationMutex.Unlock()
	e.deactivateMemberOperationHeld(member)
}

func (e *coordinator) withdrawMember(member *member) {
	campaign, leaseRetired, retired := e.removeMember(member)
	if !retired {
		return
	}
	e.retire()
	if campaign != nil && (campaign.external || leaseRetired) {
		campaign.cancelRunner()
	}
	// A Service-owned runner remains responsible for the shared election when
	// a non-Service member still holds the lease. Its lease-scoped context ends
	// when that final member leaves or the election itself stops.
}

func (e *coordinator) closeMember(member *member) {
	if member == nil {
		return
	}
	member.operationMutex.Lock()
	defer member.operationMutex.Unlock()
	e.deactivateMemberOperationHeld(member)
	e.withdrawMember(member)
}

// removeMember deletes member if it is still current and, once no members
// remain, marks the election retired and reports whether deleting its claim
// also retired the shared lease.
func (e *coordinator) removeMember(member *member) (campaign *campaign, leaseRetired, retired bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if e.members[member.service.UID] != member {
		return nil, false, false
	}
	delete(e.members, member.service.UID)
	leaseRetired = e.dependencies.Leases.Delete(e.id, member.claimToken, e.lease)
	if len(e.members) != 0 {
		return nil, leaseRetired, false
	}
	e.retired = true
	campaign = e.campaign
	e.lease = nil
	return campaign, leaseRetired, true
}

func (e *coordinator) retirement() (<-chan struct{}, bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.retiredDone, e.retired
}

func (e *coordinator) retire() {
	if e.retireCancel != nil {
		e.retireCancel()
	}
	e.registry.remove(e)
	close(e.retiredDone)
}

// campaignStart carries the decision taken under the election mutex so the
// caller can act on it without holding the lock.
type campaignStart struct {
	lease        *lease.Lease
	campaign     *campaign
	leaderCtx    context.Context
	members      []*member
	joinExisting bool
}

func (e *coordinator) prepareCampaign() campaignStart {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || len(e.members) == 0 {
		return campaignStart{}
	}
	if e.campaign != nil {
		return campaignStart{
			lease:        e.lease,
			campaign:     e.campaign,
			leaderCtx:    e.campaign.leaderCtx,
			joinExisting: true,
		}
	}
	svcLease := e.createLeaseLocked()
	if svcLease == nil {
		return campaignStart{}
	}
	external := !svcLease.BeginElection()
	campaignCtx, campaignCancel := svcLease.NewElectionContext(context.Background())
	members := e.membersLocked()
	campaign := &campaign{
		ctx:      campaignCtx,
		cancel:   campaignCancel,
		vips:     memberVIPs(members),
		external: external,
	}
	e.campaign = campaign
	return campaignStart{lease: svcLease, campaign: campaign, members: members}
}

func (e *coordinator) startCampaign(wg *sync.WaitGroup) {
	start := e.prepareCampaign()
	if start.campaign == nil {
		return
	}
	if start.joinExisting {
		if start.leaderCtx != nil {
			e.activateMembers(start.leaderCtx, start.lease, start.campaign, wg)
		}
		return
	}
	if start.campaign.external {
		wg.Go(func() {
			e.followCampaign(start.lease, start.campaign, wg)
		})
		return
	}
	for _, member := range start.members {
		metrics.ServiceElectionAttemptsTotal.WithLabelValues(member.service.Namespace, member.service.Name).Inc()
	}

	wg.Go(func() {
		e.runCampaign(start.lease, start.campaign, wg)
	})
}

// adoptLeaderContext publishes the leader context for a campaign that just won
// an externally driven election.
func (e *coordinator) adoptLeaderContext(svcLease *lease.Lease, campaign *campaign,
	leaderCtx context.Context, cancelLeader context.CancelFunc) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign || campaign.stopped {
		return false
	}
	campaign.leaderCtx = leaderCtx
	campaign.cancelLeader = cancelLeader
	return true
}

func (e *coordinator) followCampaign(svcLease *lease.Lease, campaign *campaign, wg *sync.WaitGroup) {
	defer campaign.cancelRunner()
	leaderGeneration, elected := svcLease.WaitForLeaderGeneration(campaign.ctx)
	if !elected {
		e.stopCampaign(svcLease, campaign)
		e.finishCampaign(svcLease, campaign, wg)
		return
	}
	leaderCtx, cancelLeader := context.WithCancel(campaign.ctx)
	if !e.adoptLeaderContext(svcLease, campaign, leaderCtx, cancelLeader) {
		cancelLeader()
		return
	}
	e.activateMembers(leaderCtx, svcLease, campaign, wg)
	svcLease.WaitForElectionEndAfter(campaign.ctx, leaderGeneration)
	cancelLeader()
	e.stopCampaign(svcLease, campaign)
	e.finishCampaign(svcLease, campaign, wg)
}

func (e *coordinator) runCampaign(svcLease *lease.Lease, campaign *campaign, wg *sync.WaitGroup) {
	defer campaign.cancelRunner()
	run := election.RunConfig{
		Config:           e.dependencies.Config,
		LeaseID:          e.id,
		Mgr:              e.dependencies.ElectionManager,
		LeaseAnnotations: map[string]string{},
		VIPs:             campaign.vips,
		VIPsProvider:     svcLease.OwnedVIPs,
		OnStartedLeading: func(ctx context.Context) {
			e.startedLeading(ctx, svcLease, campaign, wg)
		},
		OnStoppedLeading: func() {
			e.stopCampaign(svcLease, campaign)
			metrics.IsLeader.WithLabelValues(e.dependencies.Config.NodeName, e.id.Name()).Set(0)
		},
		OnNewLeader: func(identity string) {
			if identity != e.dependencies.Config.NodeName {
				log.Info("new leader", "leader", identity, "lease", e.id.NamespacedName())
			}
		},
	}
	if err := e.dependencies.Runner.RunCampaign(campaign.ctx, &run); err != nil {
		log.Error("services election failed", "lease", e.id.NamespacedName(), "error", err)
	}
	e.stopCampaign(svcLease, campaign)
	svcLease.ElectionStopped()
	e.finishCampaign(svcLease, campaign, wg)
}

func memberVIPs(members []*member) []string {
	services := make([]*v1.Service, 0, len(members))
	for _, member := range members {
		if member != nil && member.service != nil {
			services = append(services, member.service)
		}
	}
	return instance.OrderedServiceAddresses(services)
}

func serviceVIPProvider(service *v1.Service) lease.VIPProvider {
	addresses, _ := instance.FetchServiceAddresses(service)
	return lease.StaticVIPProvider(addresses)
}

func (e *coordinator) membersLocked() []*member {
	members := make([]*member, 0, len(e.members))
	for _, member := range e.members {
		members = append(members, member)
	}
	return members
}

// beginLeading records the leader context for a campaign this process won.
func (e *coordinator) beginLeading(ctx context.Context, svcLease *lease.Lease,
	campaign *campaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign || campaign.stopped || len(e.members) == 0 {
		return false
	}
	campaign.leaderCtx = ctx
	svcLease.ElectionStarted()
	return true
}

func (e *coordinator) startedLeading(ctx context.Context, svcLease *lease.Lease,
	campaign *campaign, wg *sync.WaitGroup) {
	if !e.beginLeading(ctx, svcLease, campaign) {
		return
	}
	metrics.LeaderTransitionsTotal.WithLabelValues(e.id.Name()).Inc()
	metrics.IsLeader.WithLabelValues(e.dependencies.Config.NodeName, e.id.Name()).Set(1)
	e.activateMembers(ctx, svcLease, campaign, wg)
}

// activatableMembers snapshots the members eligible for activation, or nil when
// the campaign is no longer current.
func (e *coordinator) activatableMembers(svcLease *lease.Lease,
	campaign *campaign) []*member {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign || (campaign != nil && campaign.stopped) || !svcLease.Elected.Load() {
		return nil
	}
	return e.membersLocked()
}

func (e *coordinator) activateMembers(ctx context.Context, svcLease *lease.Lease,
	campaign *campaign, wg *sync.WaitGroup) {
	if svcLease == nil || !svcLease.Elected.Load() {
		return
	}
	for _, member := range e.activatableMembers(svcLease, campaign) {
		e.activateMember(ctx, member, svcLease, campaign, wg)
	}
}

func (e *coordinator) activateMember(ctx context.Context, member *member, svcLease *lease.Lease,
	campaign *campaign, wg *sync.WaitGroup) {
	releaseReadiness, ready := member.serviceContext.AcquireReadinessGeneration(member.readinessGeneration)
	if !ready {
		return
	}
	defer releaseReadiness()

	member.operationMutex.Lock()
	defer member.operationMutex.Unlock()

	if !e.dependencies.State.IsCurrent(member.service, member.serviceContext, member.readinessGeneration) ||
		!e.markMemberActive(member, svcLease, campaign) {
		return
	}
	if err := e.dependencies.Datapath.Activate(ctx, member.service, member.serviceContext, wg); err != nil {
		metrics.ServiceElectionErrorsTotal.WithLabelValues(member.service.Namespace, member.service.Name, "service_sync").Inc()
		log.Error("start service after election", "service", member.service.Name, "namespace", member.service.Namespace, "error", err)
		e.deactivateMemberOperationHeld(member)
		if !e.hasOtherReadyMember(member) {
			e.cancelCampaign(svcLease, campaign)
		}
		return
	}
	e.resetRestartFailures()
	if !e.dependencies.State.IsCurrent(member.service, member.serviceContext, member.readinessGeneration) ||
		!e.memberActivationCurrent(member, svcLease, campaign) {
		e.deactivateMemberOperationHeld(member)
	}
}

func (e *coordinator) resetRestartFailures() {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.restartFailures = 0
}

func (e *coordinator) memberActivationCurrent(member *member, svcLease *lease.Lease,
	campaign *campaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.memberActivationCurrentLocked(member, svcLease, campaign) && member.active
}

func (e *coordinator) memberActivationCurrentLocked(member *member, svcLease *lease.Lease,
	campaign *campaign) bool {
	return !e.retired && e.lease == svcLease && e.campaign == campaign &&
		(campaign == nil || !campaign.stopped) && svcLease.Elected.Load() &&
		e.members[member.service.UID] == member
}

func (e *coordinator) markMemberActive(member *member, svcLease *lease.Lease, campaign *campaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if !e.memberActivationCurrentLocked(member, svcLease, campaign) || member.active {
		return false
	}
	member.active = true
	return true
}

func (e *coordinator) hasOtherReadyMember(member *member) bool {
	for _, candidate := range e.otherMembers(member) {
		if candidate.serviceContext.Ctx.Err() == nil && candidate.serviceContext.ReadinessGenerationCurrent(candidate.readinessGeneration) {
			return true
		}
	}
	return false
}

// otherMembers returns every member except the supplied one.
func (e *coordinator) otherMembers(excluded *member) []*member {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	others := make([]*member, 0, len(e.members))
	for _, candidate := range e.members {
		if candidate != excluded {
			others = append(others, candidate)
		}
	}
	return others
}

// markMemberInactive clears the active flag and reports the lease that the
// caller must run cleanup against.
func (e *coordinator) markMemberInactive(member *member) (*lease.Lease, bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.members[member.service.UID] != member || !member.active {
		return nil, false
	}
	member.active = false
	return e.lease, true
}

func (e *coordinator) deactivateMemberOperationHeld(member *member) {
	svcLease, deactivated := e.markMemberInactive(member)
	if !deactivated {
		return
	}
	e.cleanupMember(member, svcLease)
}

func (e *coordinator) cleanupMember(member *member, svcLease *lease.Lease) {
	if svcLease == nil {
		return
	}
	cleanupCtx := context.WithoutCancel(svcLease.Ctx)
	if err := e.dependencies.Datapath.Cleanup(cleanupCtx, member.service, member.serviceContext, func() bool {
		return e.contains(member)
	}); err != nil {
		log.Error("stop service after election", "service", member.service.Name, "namespace", member.service.Namespace, "error", err)
	}
}

// markCampaignStopped retires the campaign and returns the members whose
// datapath the caller must tear down outside the lock.
func (e *coordinator) markCampaignStopped(svcLease *lease.Lease,
	campaign *campaign) []*member {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign || campaign.stopped {
		return nil
	}
	campaign.stopped = true
	if campaign.cancelLeader != nil {
		campaign.cancelLeader()
	}
	if !campaign.external {
		svcLease.ElectionStopped()
	}
	return e.membersLocked()
}

func (e *coordinator) stopCampaign(svcLease *lease.Lease, campaign *campaign) {
	for _, member := range e.markCampaignStopped(svcLease, campaign) {
		e.deactivateMember(member)
	}
}

// recordCampaignFailure counts an activation failure for the restart backoff.
func (e *coordinator) recordCampaignFailure(svcLease *lease.Lease, campaign *campaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign {
		return false
	}
	e.restartFailures++
	return true
}

func (e *coordinator) cancelCampaign(svcLease *lease.Lease, campaign *campaign) {
	if !e.recordCampaignFailure(svcLease, campaign) {
		return
	}
	campaign.cancelRunner()
}

// completeCampaign clears the finished campaign and reports whether a restart
// is still needed, along with its backoff delay.
func (e *coordinator) completeCampaign(svcLease *lease.Lease,
	campaign *campaign) (bool, time.Duration) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign {
		return false, 0
	}
	e.campaign = nil
	if svcLease.Ctx.Err() != nil {
		e.lease = nil
	}
	return len(e.members) != 0, e.restartDelayLocked()
}

func (e *coordinator) finishCampaign(svcLease *lease.Lease, campaign *campaign, wg *sync.WaitGroup) {
	restart, delay := e.completeCampaign(svcLease, campaign)
	if !restart {
		return
	}

	e.dependencies.Scheduler.ScheduleRestart(e.retiredCtx, delay, wg, func() {
		e.startCampaign(wg)
	})
}

// restartDelayLocked doubles the restart delay for each consecutive
// activation failure, capped at restartMaxDelay, so a
// persistently broken Service does not spin the Lease and VIP in a tight
// add/delete loop. The caller must hold e.mutex.
func (e *coordinator) restartDelayLocked() time.Duration {
	delay := restartBaseDelay
	for i := 0; i < e.restartFailures && delay < restartMaxDelay; i++ {
		delay *= 2
	}
	if delay > restartMaxDelay {
		delay = restartMaxDelay
	}
	return delay
}
