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
	registry     *coordinatorManager
	dependencies Dependencies
	id           lease.ID

	mutex        sync.Mutex
	membership   coordinatorMembership
	campaigns    coordinatorCampaignState
	retired      bool
	retiredDone  chan struct{}
	retiredCtx   context.Context
	retireCancel context.CancelFunc
}

const (
	restartBaseDelay = 200 * time.Millisecond
	restartMaxDelay  = 30 * time.Second
)

func newCoordinator(cm *coordinatorManager, id lease.ID) *coordinator {
	retiredCtx, retire := context.WithCancel(context.Background())
	return &coordinator{
		registry:     cm,
		dependencies: cm.dependencies,
		id:           id,
		membership:   coordinatorMembership{members: make(map[types.UID]*member)},
		retiredDone:  make(chan struct{}),
		retiredCtx:   retiredCtx,
		retireCancel: retire,
	}
}

func (c *coordinator) contains(member *member) bool {
	if c == nil || member == nil || member.service == nil {
		return false
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return !c.retired && c.membership.members[member.service.UID] == member
}

func (c *coordinator) join(svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration servicecontext.ReadinessGeneration) (*member, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.retired {
		return nil, false
	}
	if member := c.membership.members[service.UID]; member != nil && member.serviceContext == svcCtx &&
		member.readinessGeneration == readinessGeneration {
		return member, true
	}

	if previous := c.membership.members[service.UID]; previous != nil {
		previous.registration.Release()
	}
	member := newMember(c, svcCtx, service, readinessGeneration)
	member.registrationSpec = lease.RegistrationSpec{
		Name: c.registry.nextToken(), VIPProvider: serviceVIPProvider(member.service),
	}
	c.membership.members[service.UID] = member

	// A member can become ready again while the old campaign is still stopping.
	// Keep its new generation until that runner finishes; finishCampaign will
	// rebuild the lease and launch the replacement campaign.
	if c.membership.lease != nil && c.membership.lease.Ctx.Err() != nil && c.campaigns.current != nil {
		return member, true
	}
	if c.membership.lease == nil || c.membership.lease.Ctx.Err() != nil {
		if c.ensureLeaseLocked() == nil {
			delete(c.membership.members, service.UID)
			return nil, false
		}
		return member, true
	}
	if registration, _ := c.dependencies.Leases.ClaimRegistration(c.id, member.registrationSpec); registration != nil {
		member.registration = registration
		return member, true
	}

	// An external cleanup retired the manager entry. Rebuild it from the live
	// coordinator snapshot rather than admitting a member to a dead lease.
	c.membership.lease = nil
	if c.ensureLeaseLocked() == nil {
		delete(c.membership.members, service.UID)
		return nil, false
	}
	return member, true
}

func (c *coordinator) ensureLeaseLocked() *lease.Lease {
	if c.membership.lease != nil && c.membership.lease.Ctx.Err() == nil {
		return c.membership.lease
	}
	if len(c.membership.members) == 0 {
		return nil
	}
	specs := make([]lease.RegistrationSpec, 0, len(c.membership.members))
	for _, member := range c.membership.members {
		specs = append(specs, member.registrationSpec)
	}
	svcLease, registrations, err := c.dependencies.Leases.AcquireRegistrations(context.Background(), c.id, specs)
	if err != nil {
		return nil
	}
	for _, member := range c.membership.members {
		member.registration = registrations[member.registrationSpec.Name]
	}
	c.membership.lease = svcLease
	return svcLease
}

func (c *coordinator) currentMember(uid types.UID) *member {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.membership.members[uid]
}

func (c *coordinator) deactivateMember(member *member) {
	if member == nil {
		return
	}
	member.operationMutex.Lock()
	defer member.operationMutex.Unlock()
	c.deactivateMemberWithOperationLockHeld(member)
}

func (c *coordinator) withdrawMember(member *member) {
	campaign, leaseRetired, retired := c.removeMember(member)
	if !retired {
		return
	}
	c.retire()
	if campaign != nil && (!campaign.runsElection() || leaseRetired) {
		campaign.cancel()
	}
	// A Service-owned runner remains responsible for the shared election when
	// a non-Service member still holds the lease. Its lease-scoped context ends
	// when that final member leaves or the election itself stops.
}

func (c *coordinator) closeMember(member *member) {
	if member == nil {
		return
	}
	member.operationMutex.Lock()
	defer member.operationMutex.Unlock()
	c.deactivateMemberWithOperationLockHeld(member)
	c.withdrawMember(member)
}

// removeMember deletes member if it is still current and, once no members
// remain, marks the election retired and reports whether deleting its claim
// also retired the shared lease.
func (c *coordinator) removeMember(member *member) (campaign *campaign, leaseRetired, retired bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.membership.members[member.service.UID] != member {
		return nil, false, false
	}
	delete(c.membership.members, member.service.UID)
	leaseRetired = member.registration.Release()
	if len(c.membership.members) != 0 {
		return nil, leaseRetired, false
	}
	c.retired = true
	campaign = c.campaigns.current
	c.membership.lease = nil
	return campaign, leaseRetired, true
}

func (c *coordinator) retirement() (<-chan struct{}, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.retiredDone, c.retired
}

func (c *coordinator) retire() {
	if c.retireCancel != nil {
		c.retireCancel()
	}
	c.registry.remove(c)
	close(c.retiredDone)
}

// campaignCandidate carries the decision taken under the election mutex so the
// caller can act on it without holding the lock.
type campaignCandidate struct {
	action    campaignAction
	lease     *lease.Lease
	campaign  *campaign
	leaderCtx context.Context
	members   []*member
}

type campaignAction uint8

const (
	campaignNoop campaignAction = iota
	campaignJoin
	campaignObserve
	campaignRun
)

func (c *coordinator) newCampaignCandidate() campaignCandidate {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.retired || len(c.membership.members) == 0 {
		return campaignCandidate{}
	}
	if c.campaigns.current != nil {
		return campaignCandidate{
			action:    campaignJoin,
			lease:     c.membership.lease,
			campaign:  c.campaigns.current,
			leaderCtx: c.campaigns.current.leaderCtx,
		}
	}
	svcLease := c.ensureLeaseLocked()
	if svcLease == nil {
		return campaignCandidate{}
	}
	members := c.membersLocked()
	campaign := newCampaign(context.Background(), svcLease, memberVIPs(members))
	c.campaigns.current = campaign
	action := campaignObserve
	if campaign.runsElection() {
		action = campaignRun
	}
	return campaignCandidate{action: action, lease: svcLease, campaign: campaign, members: members}
}

func (c *coordinator) startCampaign(wg *sync.WaitGroup) {
	candidate := c.newCampaignCandidate()
	switch candidate.action {
	case campaignNoop:
		return
	case campaignJoin:
		if candidate.leaderCtx != nil {
			c.activateMembers(candidate.leaderCtx, candidate.lease, candidate.campaign, wg)
		}
		return
	case campaignObserve:
		wg.Go(func() {
			c.followCampaign(candidate.lease, candidate.campaign, wg)
		})
		return
	case campaignRun:
		for _, member := range candidate.members {
			metrics.ServiceElectionAttemptsTotal.WithLabelValues(member.service.Namespace, member.service.Name).Inc()
		}

		wg.Go(func() {
			c.runCampaign(candidate.lease, candidate.campaign, wg)
		})
	}
}

// adoptLeaderContext publishes the leader context for a campaign that just won
// an externally driven election.
func (c *coordinator) adoptLeaderContext(svcLease *lease.Lease, campaign *campaign,
	leaderCtx context.Context, cancelLeader context.CancelFunc) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.retired || c.membership.lease != svcLease || c.campaigns.current != campaign || campaign.stopped {
		return false
	}
	campaign.leaderCtx = leaderCtx
	campaign.cancelLeader = cancelLeader
	return true
}

func (c *coordinator) followCampaign(svcLease *lease.Lease, campaign *campaign, wg *sync.WaitGroup) {
	defer campaign.cancel()
	if !campaign.election.WaitForLeader(campaign.ctx) {
		c.stopCampaign(svcLease, campaign)
		c.finishCampaign(svcLease, campaign, wg)
		return
	}
	leaderCtx, cancelLeader := context.WithCancel(campaign.ctx)
	if !c.adoptLeaderContext(svcLease, campaign, leaderCtx, cancelLeader) {
		cancelLeader()
		return
	}
	c.activateMembers(leaderCtx, svcLease, campaign, wg)
	campaign.election.WaitForEnd(campaign.ctx)
	cancelLeader()
	c.stopCampaign(svcLease, campaign)
	c.finishCampaign(svcLease, campaign, wg)
}

func (c *coordinator) runCampaign(svcLease *lease.Lease, campaign *campaign, wg *sync.WaitGroup) {
	defer campaign.cancel()
	run := election.RunConfig{
		Config:           c.dependencies.Config,
		LeaseID:          c.id,
		Mgr:              c.dependencies.ElectionManager,
		LeaseAnnotations: map[string]string{},
		VIPs:             campaign.vips,
		VIPsProvider:     svcLease.OwnedVIPs,
		OnStartedLeading: func(ctx context.Context) {
			c.startedLeading(ctx, svcLease, campaign, wg)
		},
		OnStoppedLeading: func() {
			c.stopCampaign(svcLease, campaign)
			metrics.IsLeader.WithLabelValues(c.dependencies.Config.NodeName, c.id.Name()).Set(0)
		},
		OnNewLeader: func(identity string) {
			if identity != c.dependencies.Config.NodeName {
				log.Info("new leader", "leader", identity, "lease", c.id.NamespacedName())
			}
		},
	}
	if err := c.dependencies.Runner.RunCampaign(campaign.ctx, &run); err != nil {
		log.Error("services election failed", "lease", c.id.NamespacedName(), "error", err)
	}
	c.stopCampaign(svcLease, campaign)
	campaign.election.Stopped()
	c.finishCampaign(svcLease, campaign, wg)
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

func (c *coordinator) membersLocked() []*member {
	members := make([]*member, 0, len(c.membership.members))
	for _, member := range c.membership.members {
		members = append(members, member)
	}
	return members
}

// publishLeadership announces the result of a current, live campaign to every
// local participant sharing its Lease. A retired Service coordinator may still
// own that campaign on behalf of a non-Service participant.
func (c *coordinator) publishLeadership(campaign *campaign) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if campaign == nil || c.campaigns.current != campaign || campaign.stopped || campaign.ctx.Err() != nil {
		return false
	}
	return campaign.election.Started()
}

// beginServiceLeadership records the leader context only while this coordinator
// still has Services eligible for activation.
func (c *coordinator) beginServiceLeadership(ctx context.Context, svcLease *lease.Lease,
	campaign *campaign) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.retired || c.membership.lease != svcLease || c.campaigns.current != campaign || campaign == nil ||
		campaign.stopped || len(c.membership.members) == 0 || !campaign.election.IsLeading() {
		return false
	}
	campaign.leaderCtx = ctx
	return true
}

func (c *coordinator) startedLeading(ctx context.Context, svcLease *lease.Lease,
	campaign *campaign, wg *sync.WaitGroup) {
	if !c.publishLeadership(campaign) || !c.beginServiceLeadership(ctx, svcLease, campaign) {
		return
	}
	metrics.LeaderTransitionsTotal.WithLabelValues(c.id.Name()).Inc()
	metrics.IsLeader.WithLabelValues(c.dependencies.Config.NodeName, c.id.Name()).Set(1)
	c.activateMembers(ctx, svcLease, campaign, wg)
}

// activatableMembers snapshots the members eligible for activation, or nil when
// the campaign is no longer current.
func (c *coordinator) activatableMembers(svcLease *lease.Lease,
	campaign *campaign) []*member {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.retired || c.membership.lease != svcLease || c.campaigns.current != campaign || campaign == nil || campaign.stopped ||
		!campaign.election.IsLeading() {
		return nil
	}
	return c.membersLocked()
}

func (c *coordinator) activateMembers(ctx context.Context, svcLease *lease.Lease,
	campaign *campaign, wg *sync.WaitGroup) {
	if svcLease == nil || campaign == nil || !campaign.election.IsLeading() {
		return
	}
	for _, member := range c.activatableMembers(svcLease, campaign) {
		c.activateMember(ctx, member, svcLease, campaign, wg)
	}
}

func (c *coordinator) activateMember(ctx context.Context, member *member, svcLease *lease.Lease,
	campaign *campaign, wg *sync.WaitGroup) {
	readinessReservation, ready := member.serviceContext.AcquireReadinessGeneration(member.readinessGeneration)
	if !ready {
		return
	}
	defer readinessReservation.Release()

	member.operationMutex.Lock()
	defer member.operationMutex.Unlock()

	if !c.dependencies.State.IsCurrent(member.service, member.serviceContext, member.readinessGeneration) ||
		!c.markMemberActive(member, svcLease, campaign) {
		return
	}
	if err := c.dependencies.Datapath.Activate(ctx, member.service, member.serviceContext, wg); err != nil {
		metrics.ServiceElectionErrorsTotal.WithLabelValues(member.service.Namespace, member.service.Name, "service_sync").Inc()
		log.Error("start service after election", "service", member.service.Name, "namespace", member.service.Namespace, "error", err)
		c.deactivateMemberWithOperationLockHeld(member)
		if !c.hasOtherReadyMember(member) {
			c.cancelCampaign(svcLease, campaign)
		}
		return
	}
	c.resetRestartFailures()
	if !c.dependencies.State.IsCurrent(member.service, member.serviceContext, member.readinessGeneration) ||
		!c.activationStillValid(member, svcLease, campaign) {
		c.deactivateMemberWithOperationLockHeld(member)
	}
}

func (c *coordinator) resetRestartFailures() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.campaigns.restartFailures = 0
}

func (c *coordinator) activationStillValid(member *member, svcLease *lease.Lease,
	campaign *campaign) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.memberValidForActivationLocked(member, svcLease, campaign) && member.active
}

func (c *coordinator) memberValidForActivationLocked(member *member, svcLease *lease.Lease,
	campaign *campaign) bool {
	return !c.retired && c.membership.lease == svcLease && c.campaigns.current == campaign &&
		campaign != nil && !campaign.stopped && campaign.election.IsLeading() &&
		c.membership.members[member.service.UID] == member
}

func (c *coordinator) markMemberActive(member *member, svcLease *lease.Lease, campaign *campaign) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if !c.memberValidForActivationLocked(member, svcLease, campaign) || member.active {
		return false
	}
	member.active = true
	return true
}

func (c *coordinator) hasOtherReadyMember(member *member) bool {
	for _, candidate := range c.otherMembers(member) {
		if candidate.serviceContext.Ctx.Err() == nil && candidate.serviceContext.ReadinessGenerationCurrent(candidate.readinessGeneration) {
			return true
		}
	}
	return false
}

// otherMembers returns every member except the supplied one.
func (c *coordinator) otherMembers(excluded *member) []*member {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	others := make([]*member, 0, len(c.membership.members))
	for _, candidate := range c.membership.members {
		if candidate != excluded {
			others = append(others, candidate)
		}
	}
	return others
}

// markMemberInactive clears the active flag and reports the lease that the
// caller must run cleanup against.
func (c *coordinator) markMemberInactive(member *member) (*lease.Lease, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.membership.members[member.service.UID] != member || !member.active {
		return nil, false
	}
	member.active = false
	return c.membership.lease, true
}

func (c *coordinator) deactivateMemberWithOperationLockHeld(member *member) {
	svcLease, deactivated := c.markMemberInactive(member)
	if !deactivated {
		return
	}
	c.cleanupMember(member, svcLease)
}

func (c *coordinator) cleanupMember(member *member, svcLease *lease.Lease) {
	if svcLease == nil {
		return
	}
	cleanupCtx := context.WithoutCancel(svcLease.Ctx)
	if err := c.dependencies.Datapath.Cleanup(cleanupCtx, member.service, member.serviceContext, func() bool {
		return c.contains(member)
	}); err != nil {
		log.Error("stop service after election", "service", member.service.Name, "namespace", member.service.Namespace, "error", err)
	}
}

// markCampaignStopped retires the campaign and returns the members whose
// datapath the caller must tear down outside the lock.
func (c *coordinator) markCampaignStopped(svcLease *lease.Lease,
	campaign *campaign) []*member {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.retired || c.membership.lease != svcLease || c.campaigns.current != campaign || campaign.stopped {
		return nil
	}
	campaign.stopped = true
	if campaign.cancelLeader != nil {
		campaign.cancelLeader()
	}
	return c.membersLocked()
}

func (c *coordinator) stopCampaign(svcLease *lease.Lease, campaign *campaign) {
	for _, member := range c.markCampaignStopped(svcLease, campaign) {
		c.deactivateMember(member)
	}
}

// recordCampaignFailure counts an activation failure for the restart backoff.
func (c *coordinator) recordCampaignFailure(svcLease *lease.Lease, campaign *campaign) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.retired || c.membership.lease != svcLease || c.campaigns.current != campaign {
		return false
	}
	c.campaigns.restartFailures++
	return true
}

func (c *coordinator) cancelCampaign(svcLease *lease.Lease, campaign *campaign) {
	if !c.recordCampaignFailure(svcLease, campaign) {
		return
	}
	campaign.cancel()
}

// completeCampaign clears the finished campaign and reports whether a restart
// is still needed, along with its backoff delay.
func (c *coordinator) completeCampaign(svcLease *lease.Lease,
	campaign *campaign) (bool, time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.retired || c.membership.lease != svcLease || c.campaigns.current != campaign {
		return false, 0
	}
	c.campaigns.current = nil
	if svcLease.Ctx.Err() != nil {
		c.membership.lease = nil
	}
	return len(c.membership.members) != 0, c.restartDelayLocked()
}

func (c *coordinator) finishCampaign(svcLease *lease.Lease, campaign *campaign, wg *sync.WaitGroup) {
	restart, delay := c.completeCampaign(svcLease, campaign)
	if !restart {
		return
	}

	c.dependencies.Scheduler.ScheduleRestart(c.retiredCtx, delay, wg, func() {
		c.startCampaign(wg)
	})
}

// restartDelayLocked doubles the restart delay for each consecutive
// activation failure, capped at restartMaxDelay, so a
// persistently broken Service does not spin the Lease and VIP in a tight
// add/delete loop. The caller must hold c.mutex.
func (c *coordinator) restartDelayLocked() time.Duration {
	delay := restartBaseDelay
	for i := 0; i < c.campaigns.restartFailures && delay < restartMaxDelay; i++ {
		delay *= 2
	}
	if delay > restartMaxDelay {
		delay = restartMaxDelay
	}
	return delay
}
