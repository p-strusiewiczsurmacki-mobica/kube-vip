// Package serviceelection coordinates Service membership, shared leases, and
// leader-election campaigns without owning Kubernetes Service state or network
// datapaths. Callers provide those capabilities through focused interfaces.
package serviceelection

import (
	"context"
	log "log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ServiceState answers whether a member still represents the current Service
// context and readiness generation. It keeps Kubernetes state ownership out of
// the election coordinator.
type ServiceState interface {
	IsCurrent(*v1.Service, *servicecontext.Context, uint64) bool
}

// Datapath owns activation and cleanup of a Service datapath. The coordinator
// only decides when these operations must happen.
type Datapath interface {
	Activate(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error
	Cleanup(context.Context, *v1.Service, *servicecontext.Context, func() bool) error
}

// CampaignRunner abstracts the Kubernetes leader-election implementation.
type CampaignRunner interface {
	RunCampaign(context.Context, *election.RunConfig) error
}

// RestartScheduler abstracts delayed campaign restarts.
type RestartScheduler interface {
	ScheduleRestart(context.Context, time.Duration, *sync.WaitGroup, func())
}

// LeaseStore is the narrow lease ownership contract needed by coordinators.
type LeaseStore interface {
	AcquireWithVIPProvider(context.Context, lease.ID, string, lease.VIPProvider) (*lease.Lease, bool)
	ClaimWithVIPProvider(lease.ID, string, lease.VIPProvider) (*lease.Lease, bool)
	Delete(lease.ID, string, *lease.Lease) bool
}

// Dependencies defines the collaborators required by Service election
// coordination. Named fields keep construction explicit as the collaborators
// evolve independently.
type Dependencies struct {
	Config          *kubevip.Config
	Leases          LeaseStore
	ElectionManager *election.Manager
	State           ServiceState
	Datapath        Datapath
	Runner          CampaignRunner
	Scheduler       RestartScheduler
}

// Manager owns the registry of lease coordinators. It deliberately delegates
// Service state, datapath work, campaign execution, and scheduling to focused
// interfaces supplied by pkg/services.
type Manager struct {
	config   *kubevip.Config
	state    ServiceState
	registry *registry
}

type registry struct {
	mutex           sync.Mutex
	coordinators    map[string]*Coordinator
	nextMemberToken atomic.Uint64
	dependencies    coordinatorDependencies
}

type coordinatorDependencies struct {
	config          *kubevip.Config
	leases          LeaseStore
	electionManager *election.Manager
	state           ServiceState
	datapath        Datapath
	runner          CampaignRunner
	scheduler       RestartScheduler
	nextToken       func() string
	onRetired       func(*Coordinator)
}

// Coordinator owns membership and campaign lifetime for one lease.
// Service contexts remain responsible for endpoint readiness and datapath work.
type Coordinator struct {
	dependencies coordinatorDependencies
	id           lease.ID

	mutex        sync.Mutex
	members      map[types.UID]*Member
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

type campaign struct {
	done         chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	leaderCtx    context.Context
	cancelLeader context.CancelFunc
	vips         []string
	external     bool
	stopped      bool
}

func (campaign *campaign) cancelRunner() {
	if campaign != nil && campaign.cancel != nil {
		campaign.cancel()
	}
}

type Member struct {
	coordinator         *Coordinator
	service             *v1.Service
	serviceContext      *servicecontext.Context
	readinessGeneration uint64
	claimToken          string
	vipProvider         lease.VIPProvider
	operationMutex      sync.Mutex
	active              bool
}

// Close deactivates the member datapath and withdraws its lease claim. It is
// safe to call more than once and stale members cannot remove replacements.
func (m *Member) Close() {
	if m == nil || m.coordinator == nil {
		return
	}
	m.deactivate()
	m.withdraw()
}

func (m *Member) deactivate() {
	m.operationMutex.Lock()
	defer m.operationMutex.Unlock()
	m.coordinator.deactivateMemberOperationHeld(m)
}

func (m *Member) withdraw() {
	if m == nil || m.coordinator == nil {
		return
	}
	m.coordinator.leave(m)
}

func (m *Member) Service() *v1.Service { return m.service }

func (m *Member) ServiceContext() *servicecontext.Context { return m.serviceContext }

func (m *Member) ReadinessGeneration() uint64 { return m.readinessGeneration }

func (m *Member) ClaimToken() string { return m.claimToken }

func (m *Member) Coordinator() *Coordinator { return m.coordinator }

func (m *Member) Active() bool {
	if m == nil || m.coordinator == nil {
		return false
	}
	m.coordinator.mutex.Lock()
	defer m.coordinator.mutex.Unlock()
	return m.active
}

func (e *Coordinator) Lease() *lease.Lease {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.lease
}

func (e *Coordinator) MemberCount() int {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return len(e.members)
}

func (e *Coordinator) Contains(member *Member) bool {
	if e == nil || member == nil || member.service == nil {
		return false
	}
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return !e.retired && e.members[member.service.UID] == member
}

func NewManager(dependencies Dependencies) *Manager {
	registry := &registry{
		coordinators: make(map[string]*Coordinator),
		dependencies: coordinatorDependencies{
			config: dependencies.Config, leases: dependencies.Leases,
			electionManager: dependencies.ElectionManager, state: dependencies.State,
			datapath: dependencies.Datapath, runner: dependencies.Runner, scheduler: dependencies.Scheduler,
		},
	}
	registry.dependencies.nextToken = registry.nextToken
	registry.dependencies.onRetired = registry.remove
	return &Manager{config: dependencies.Config, state: dependencies.State, registry: registry}
}

func (r *registry) coordinatorFor(id lease.ID) *Coordinator {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.coordinators == nil {
		r.coordinators = make(map[string]*Coordinator)
	}
	key := id.NamespacedName()
	if coordinator := r.coordinators[key]; coordinator != nil {
		return coordinator
	}
	retiredCtx, retire := context.WithCancel(context.Background())
	coordinator := &Coordinator{
		dependencies: r.dependencies,
		id:           id,
		members:      make(map[types.UID]*Member),
		retiredDone:  make(chan struct{}),
		retiredCtx:   retiredCtx,
		retireCancel: retire,
	}
	r.coordinators[key] = coordinator
	return coordinator
}

func (r *registry) nextToken() string {
	return strconv.FormatUint(r.nextMemberToken.Add(1), 10)
}

// joinServiceElection registers the current ready generation of a Service. A
// caller that races coordinator retirement retries against its replacement.
func (m *Manager) Join(svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration uint64) (*Member, bool) {
	if svcCtx == nil || service == nil || m.registry.dependencies.leases == nil {
		return nil, false
	}

	namespace, name := lease.ServiceName(service)
	id := lease.NewID(m.config.LeaderElectionType, namespace, name)
	for {
		if !m.state.IsCurrent(service, svcCtx, readinessGeneration) {
			return nil, false
		}
		coordinator := m.registry.coordinatorFor(id)
		member, joined := coordinator.join(svcCtx, service, readinessGeneration)
		if joined {
			if m.state.IsCurrent(member.service, member.serviceContext, member.readinessGeneration) {
				return member, true
			}
			member.withdraw()
			return nil, false
		}
		if retiredDone, retired := coordinator.retirement(); retired {
			select {
			case <-svcCtx.Ctx.Done():
				return nil, false
			case <-retiredDone:
				continue
			}
		}
		return nil, false
	}
}

func newMember(coordinator *Coordinator, svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration uint64) *Member {
	return &Member{
		coordinator: coordinator, service: service.DeepCopy(), serviceContext: svcCtx,
		readinessGeneration: readinessGeneration,
	}
}

func (e *Coordinator) join(svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration uint64) (*Member, bool) {
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
		e.dependencies.leases.Delete(e.id, previous.claimToken, e.lease)
	}
	member := newMember(e, svcCtx, service, readinessGeneration)
	member.claimToken = e.dependencies.nextToken()
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
	if claimed, _ := e.dependencies.leases.ClaimWithVIPProvider(e.id, member.claimToken, member.vipProvider); claimed != nil {
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

func (e *Coordinator) createLeaseLocked() *lease.Lease {
	if e.lease != nil && e.lease.Ctx.Err() == nil {
		return e.lease
	}
	var first *Member
	for _, member := range e.members {
		first = member
		break
	}
	if first == nil {
		return nil
	}
	svcLease, _ := e.dependencies.leases.AcquireWithVIPProvider(context.Background(), e.id, first.claimToken,
		first.vipProvider)
	for _, member := range e.members {
		if member == first {
			continue
		}
		if claimed, _ := e.dependencies.leases.ClaimWithVIPProvider(e.id, member.claimToken, member.vipProvider); claimed == nil {
			svcLease.Cancel()
			return nil
		}
	}
	e.lease = svcLease
	return svcLease
}

func (m *Manager) LeaveForContext(svcCtx *servicecontext.Context, service *v1.Service) {
	if svcCtx == nil || service == nil {
		return
	}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(m.config.LeaderElectionType, namespace, name)
	coordinator := m.Current(id)
	if coordinator == nil {
		return
	}
	member := coordinator.CurrentMember(service.UID)
	if member != nil && member.serviceContext == svcCtx {
		// The caller already owns Service cleanup. Withdrawing here must not
		// reacquire the Service lock through datapath cleanup.
		member.withdraw()
	}
}

func (m *Manager) Current(id lease.ID) *Coordinator {
	return m.registry.current(id)
}

func (r *registry) current(id lease.ID) *Coordinator {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.coordinators[id.NamespacedName()]
}

func (e *Coordinator) CurrentMember(uid types.UID) *Member {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.members[uid]
}

func (e *Coordinator) leave(member *Member) {
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

// removeMember deletes member if it is still current and, once no members
// remain, marks the election retired and reports whether deleting its claim
// also retired the shared lease.
func (e *Coordinator) removeMember(member *Member) (campaign *campaign, leaseRetired, retired bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if e.members[member.service.UID] != member {
		return nil, false, false
	}
	delete(e.members, member.service.UID)
	leaseRetired = e.dependencies.leases.Delete(e.id, member.claimToken, e.lease)
	if len(e.members) != 0 {
		return nil, leaseRetired, false
	}
	e.retired = true
	campaign = e.campaign
	e.lease = nil
	return campaign, leaseRetired, true
}

func (e *Coordinator) retirement() (<-chan struct{}, bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.retiredDone, e.retired
}

func (e *Coordinator) retire() {
	if e.retireCancel != nil {
		e.retireCancel()
	}
	e.dependencies.onRetired(e)
	close(e.retiredDone)
}

func (r *registry) remove(coordinator *Coordinator) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.coordinators[coordinator.id.NamespacedName()] == coordinator {
		delete(r.coordinators, coordinator.id.NamespacedName())
	}
}

func (m *Manager) Watch(svcCtx *servicecontext.Context, service *v1.Service,
	wg *sync.WaitGroup) {
	for {
		generation, ready, lost, isReady := svcCtx.ReadinessState()
		if !isReady {
			select {
			case <-svcCtx.Ctx.Done():
				return
			case <-ready:
				continue
			}
		}

		member, joined := m.Join(svcCtx, service, generation)
		if !joined {
			if !m.state.IsCurrent(service, svcCtx, generation) {
				return
			}
			select {
			case <-svcCtx.Ctx.Done():
				return
			case <-time.After(restartBaseDelay):
				continue
			}
		}
		member.coordinator.StartCampaign(wg)

		select {
		case <-svcCtx.Ctx.Done():
			member.Close()
			return
		case <-lost:
			member.Close()
		}
	}
}

// campaignStart carries the decision taken under the election mutex so the
// caller can act on it without holding the lock.
type campaignStart struct {
	lease        *lease.Lease
	campaign     *campaign
	leaderCtx    context.Context
	members      []*Member
	joinExisting bool
	external     bool
}

func (e *Coordinator) prepareCampaign() campaignStart {
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
		done:     make(chan struct{}),
		ctx:      campaignCtx,
		cancel:   campaignCancel,
		vips:     memberVIPs(members),
		external: external,
	}
	e.campaign = campaign
	return campaignStart{lease: svcLease, campaign: campaign, members: members, external: external}
}

func (e *Coordinator) StartCampaign(wg *sync.WaitGroup) {
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
	if start.external {
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
func (e *Coordinator) adoptLeaderContext(svcLease *lease.Lease, campaign *campaign,
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

func (e *Coordinator) followCampaign(svcLease *lease.Lease, campaign *campaign, wg *sync.WaitGroup) {
	defer close(campaign.done)
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

func (e *Coordinator) runCampaign(svcLease *lease.Lease, campaign *campaign, wg *sync.WaitGroup) {
	defer close(campaign.done)
	defer campaign.cancelRunner()
	run := election.RunConfig{
		Config:           e.dependencies.config,
		LeaseID:          e.id,
		Mgr:              e.dependencies.electionManager,
		LeaseAnnotations: map[string]string{},
		VIPs:             campaign.vips,
		VIPsProvider:     svcLease.OwnedVIPs,
		OnStartedLeading: func(ctx context.Context) {
			e.startedLeading(ctx, svcLease, campaign, wg)
		},
		OnStoppedLeading: func() {
			e.stopCampaign(svcLease, campaign)
			metrics.IsLeader.WithLabelValues(e.dependencies.config.NodeName, e.id.Name()).Set(0)
		},
		OnNewLeader: func(identity string) {
			if identity != e.dependencies.config.NodeName {
				log.Info("new leader", "leader", identity, "lease", e.id.NamespacedName())
			}
		},
	}
	if err := e.dependencies.runner.RunCampaign(campaign.ctx, &run); err != nil {
		log.Error("services election failed", "lease", e.id.NamespacedName(), "error", err)
	}
	e.stopCampaign(svcLease, campaign)
	svcLease.ElectionStopped()
	e.finishCampaign(svcLease, campaign, wg)
}

func memberVIPs(members []*Member) []string {
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

func (e *Coordinator) membersLocked() []*Member {
	members := make([]*Member, 0, len(e.members))
	for _, member := range e.members {
		members = append(members, member)
	}
	return members
}

// beginLeading records the leader context for a campaign this process won.
func (e *Coordinator) beginLeading(ctx context.Context, svcLease *lease.Lease,
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

func (e *Coordinator) startedLeading(ctx context.Context, svcLease *lease.Lease,
	campaign *campaign, wg *sync.WaitGroup) {
	if !e.beginLeading(ctx, svcLease, campaign) {
		return
	}
	metrics.LeaderTransitionsTotal.WithLabelValues(e.id.Name()).Inc()
	metrics.IsLeader.WithLabelValues(e.dependencies.config.NodeName, e.id.Name()).Set(1)
	e.activateMembers(ctx, svcLease, campaign, wg)
}

// activatableMembers snapshots the members eligible for activation, or nil when
// the campaign is no longer current.
func (e *Coordinator) activatableMembers(svcLease *lease.Lease,
	campaign *campaign) []*Member {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign || (campaign != nil && campaign.stopped) || !svcLease.Elected.Load() {
		return nil
	}
	return e.membersLocked()
}

func (e *Coordinator) activateMembers(ctx context.Context, svcLease *lease.Lease,
	campaign *campaign, wg *sync.WaitGroup) {
	if svcLease == nil || !svcLease.Elected.Load() {
		return
	}
	for _, member := range e.activatableMembers(svcLease, campaign) {
		e.activateMember(ctx, member, svcLease, campaign, wg)
	}
}

func (e *Coordinator) activateMember(ctx context.Context, member *Member, svcLease *lease.Lease,
	campaign *campaign, wg *sync.WaitGroup) {
	releaseReadiness, ready := member.serviceContext.AcquireReadinessGeneration(member.readinessGeneration)
	if !ready {
		return
	}
	defer releaseReadiness()

	member.operationMutex.Lock()
	defer member.operationMutex.Unlock()

	if !e.dependencies.state.IsCurrent(member.service, member.serviceContext, member.readinessGeneration) ||
		!e.markMemberActive(member, svcLease, campaign) {
		return
	}
	if err := e.dependencies.datapath.Activate(ctx, member.service, member.serviceContext, wg); err != nil {
		metrics.ServiceElectionErrorsTotal.WithLabelValues(member.service.Namespace, member.service.Name, "service_sync").Inc()
		log.Error("start service after election", "service", member.service.Name, "namespace", member.service.Namespace, "error", err)
		e.deactivateMemberOperationHeld(member)
		if !e.hasOtherReadyMember(member) {
			e.cancelCampaign(svcLease, campaign)
		}
		return
	}
	e.resetRestartFailures()
	if !e.dependencies.state.IsCurrent(member.service, member.serviceContext, member.readinessGeneration) ||
		!e.memberActivationCurrent(member, svcLease, campaign) {
		e.deactivateMemberOperationHeld(member)
	}
}

func (e *Coordinator) resetRestartFailures() {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.restartFailures = 0
}

func (e *Coordinator) memberActivationCurrent(member *Member, svcLease *lease.Lease,
	campaign *campaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.memberActivationCurrentLocked(member, svcLease, campaign) && member.active
}

func (e *Coordinator) memberActivationCurrentLocked(member *Member, svcLease *lease.Lease,
	campaign *campaign) bool {
	return !e.retired && e.lease == svcLease && e.campaign == campaign &&
		(campaign == nil || !campaign.stopped) && svcLease.Elected.Load() &&
		e.members[member.service.UID] == member
}

func (e *Coordinator) markMemberActive(member *Member, svcLease *lease.Lease, campaign *campaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if !e.memberActivationCurrentLocked(member, svcLease, campaign) || member.active {
		return false
	}
	member.active = true
	return true
}

func (e *Coordinator) hasOtherReadyMember(member *Member) bool {
	for _, candidate := range e.otherMembers(member) {
		if candidate.serviceContext.Ctx.Err() == nil && candidate.serviceContext.ReadinessGenerationCurrent(candidate.readinessGeneration) {
			return true
		}
	}
	return false
}

// otherMembers returns every member except the supplied one.
func (e *Coordinator) otherMembers(member *Member) []*Member {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	others := make([]*Member, 0, len(e.members))
	for _, candidate := range e.members {
		if candidate != member {
			others = append(others, candidate)
		}
	}
	return others
}

// markMemberInactive clears the active flag and reports the lease that the
// caller must run cleanup against.
func (e *Coordinator) markMemberInactive(member *Member) (*lease.Lease, bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.members[member.service.UID] != member || !member.active {
		return nil, false
	}
	member.active = false
	return e.lease, true
}

func (e *Coordinator) deactivateMemberOperationHeld(member *Member) {
	svcLease, deactivated := e.markMemberInactive(member)
	if !deactivated {
		return
	}
	e.cleanupMember(member, svcLease)
}

func (e *Coordinator) cleanupMember(member *Member, svcLease *lease.Lease) {
	if svcLease == nil {
		return
	}
	cleanupCtx := context.WithoutCancel(svcLease.Ctx)
	if err := e.dependencies.datapath.Cleanup(cleanupCtx, member.service, member.serviceContext, func() bool {
		return e.Contains(member)
	}); err != nil {
		log.Error("stop service after election", "service", member.service.Name, "namespace", member.service.Namespace, "error", err)
	}
}

// markCampaignStopped retires the campaign and returns the members whose
// datapath the caller must tear down outside the lock.
func (e *Coordinator) markCampaignStopped(svcLease *lease.Lease,
	campaign *campaign) []*Member {
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

func (e *Coordinator) stopCampaign(svcLease *lease.Lease, campaign *campaign) {
	for _, member := range e.markCampaignStopped(svcLease, campaign) {
		member.deactivate()
	}
}

// recordCampaignFailure counts an activation failure for the restart backoff.
func (e *Coordinator) recordCampaignFailure(svcLease *lease.Lease, campaign *campaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign {
		return false
	}
	e.restartFailures++
	return true
}

func (e *Coordinator) cancelCampaign(svcLease *lease.Lease, campaign *campaign) {
	if !e.recordCampaignFailure(svcLease, campaign) {
		return
	}
	campaign.cancelRunner()
}

// completeCampaign clears the finished campaign and reports whether a restart
// is still needed, along with its backoff delay.
func (e *Coordinator) completeCampaign(svcLease *lease.Lease,
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

func (e *Coordinator) finishCampaign(svcLease *lease.Lease, campaign *campaign, wg *sync.WaitGroup) {
	restart, delay := e.completeCampaign(svcLease, campaign)
	if !restart {
		return
	}

	e.dependencies.scheduler.ScheduleRestart(e.retiredCtx, delay, wg, func() {
		e.StartCampaign(wg)
	})
}

// restartDelayLocked doubles the restart delay for each consecutive
// activation failure, capped at restartMaxDelay, so a
// persistently broken Service does not spin the Lease and VIP in a tight
// add/delete loop. The caller must hold e.mutex.
func (e *Coordinator) restartDelayLocked() time.Duration {
	delay := restartBaseDelay
	for i := 0; i < e.restartFailures && delay < restartMaxDelay; i++ {
		delay *= 2
	}
	if delay > restartMaxDelay {
		delay = restartMaxDelay
	}
	return delay
}
