package serviceelection

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type testAdapter struct {
	mutex              sync.Mutex
	current            map[types.UID]*servicecontext.Context
	activations        []*v1.Service
	cleanups           []*v1.Service
	activate           func(*v1.Service) error
	activateErr        error
	cleanupStarted     chan struct{}
	cleanupStartedOnce sync.Once
	releaseCleanup     <-chan struct{}
}

func (a *testAdapter) IsCurrent(service *v1.Service, ctx *servicecontext.Context,
	generation servicecontext.ReadinessGeneration) bool {
	a.mutex.Lock()
	current := a.current[service.UID]
	a.mutex.Unlock()
	return current == ctx && ctx.Ctx.Err() == nil && ctx.ReadinessGenerationCurrent(generation)
}

func (a *testAdapter) Activate(_ context.Context, service *v1.Service, _ *servicecontext.Context, _ *sync.WaitGroup) error {
	a.mutex.Lock()
	a.activations = append(a.activations, service)
	activate := a.activate
	activateErr := a.activateErr
	a.mutex.Unlock()
	if activate != nil {
		return activate(service)
	}
	return activateErr
}

func (a *testAdapter) Cleanup(_ context.Context, service *v1.Service, _ *servicecontext.Context, current func() bool) error {
	if !current() {
		return nil
	}
	if a.cleanupStarted != nil {
		a.cleanupStartedOnce.Do(func() { close(a.cleanupStarted) })
	}
	if a.releaseCleanup != nil {
		<-a.releaseCleanup
	}
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.cleanups = append(a.cleanups, service)
	return nil
}

func (a *testAdapter) RunCampaign(ctx context.Context, run *election.RunConfig) error {
	run.OnStartedLeading(ctx)
	<-ctx.Done()
	run.OnStoppedLeading()
	return nil
}

func (a *testAdapter) ScheduleRestart(_ context.Context, _ time.Duration, _ *sync.WaitGroup, restart func()) {
	restart()
}

type queuedRestartScheduler struct {
	mutex    sync.Mutex
	restarts []func()
}

func (s *queuedRestartScheduler) ScheduleRestart(_ context.Context, _ time.Duration,
	_ *sync.WaitGroup, restart func()) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.restarts = append(s.restarts, restart)
}

func (s *queuedRestartScheduler) count() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.restarts)
}

func (s *queuedRestartScheduler) runNext(t *testing.T) {
	t.Helper()
	s.mutex.Lock()
	if len(s.restarts) == 0 {
		s.mutex.Unlock()
		t.Fatal("no scheduled restart to run")
	}
	restart := s.restarts[0]
	s.restarts = s.restarts[1:]
	s.mutex.Unlock()
	restart()
}

func newTestManager() (*Manager, *testAdapter, *lease.Manager) {
	return newTestManagerWithScheduler(nil)
}

func newTestManagerWithScheduler(scheduler RestartScheduler) (*Manager, *testAdapter, *lease.Manager) {
	adapter := &testAdapter{current: make(map[types.UID]*servicecontext.Context)}
	leaseMgr := lease.NewManager()
	if scheduler == nil {
		scheduler = adapter
	}
	manager, err := NewManager(&Dependencies{
		Config: &kubevip.Config{}, Leases: leaseMgr,
		State: adapter, Datapath: adapter, Runner: adapter, Scheduler: scheduler,
	})
	if err != nil {
		panic(err)
	}
	return manager, adapter, leaseMgr
}

func TestSharedCampaignRetriesOnlyFailedMember(t *testing.T) {
	scheduler := &queuedRestartScheduler{}
	manager, adapter, _ := newTestManagerWithScheduler(scheduler)
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	failingService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "failing", Namespace: "default", UID: "failing", Annotations: annotations,
	}}
	healthyService := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "healthy", Namespace: "default", UID: "healthy", Annotations: annotations,
	}}
	failingMember := readyMember(t, manager, adapter, failingService)
	healthyMember := readyMember(t, manager, adapter, healthyService)
	if failingMember.coordinator != healthyMember.coordinator {
		t.Fatal("members with a shared lease got different coordinators")
	}

	attempts := make(map[types.UID]int)
	var attemptsMutex sync.Mutex
	adapter.activate = func(service *v1.Service) error {
		attemptsMutex.Lock()
		defer attemptsMutex.Unlock()
		attempts[service.UID]++
		if service.UID == failingService.UID && attempts[service.UID] == 1 {
			return errors.New("transient datapath failure")
		}
		return nil
	}

	coordinator := failingMember.coordinator
	start := coordinator.newCampaignCandidate()
	start.campaign.election.Started()
	coordinator.activateMembers(context.Background(), start.lease, start.campaign, &sync.WaitGroup{})

	if failingMember.active {
		t.Fatal("failed member remained active")
	}
	if !healthyMember.active {
		t.Fatal("healthy member was not activated")
	}
	if start.campaign.ctx.Err() != nil {
		t.Fatal("one failed member cancelled a campaign with a healthy sibling")
	}
	if got := scheduler.count(); got != 1 {
		t.Fatalf("scheduled retries = %d, want 1", got)
	}

	scheduler.runNext(t)
	if !failingMember.active {
		t.Fatal("failed member was not activated by its retry")
	}
	if !healthyMember.active {
		t.Fatal("retry of failed member deactivated its healthy sibling")
	}
	attemptsMutex.Lock()
	failingAttempts := attempts[failingService.UID]
	healthyAttempts := attempts[healthyService.UID]
	attemptsMutex.Unlock()
	if failingAttempts != 2 || healthyAttempts != 1 {
		t.Fatalf("activation attempts = failing:%d healthy:%d, want failing:2 healthy:1",
			failingAttempts, healthyAttempts)
	}

	failingMember.coordinator.closeMember(failingMember)
	healthyMember.coordinator.closeMember(healthyMember)
}

func TestSharedCampaignRestartsWhenEveryMemberActivationFails(t *testing.T) {
	manager, adapter, _ := newTestManager()
	adapter.activateErr = errors.New("datapath failed")
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	first := readyMember(t, manager, adapter, &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "first", Namespace: "default", UID: "first", Annotations: annotations,
	}})
	second := readyMember(t, manager, adapter, &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "second", Namespace: "default", UID: "second", Annotations: annotations,
	}})
	coordinator := first.coordinator
	if coordinator != second.coordinator {
		t.Fatal("members with a shared lease got different coordinators")
	}

	start := coordinator.newCampaignCandidate()
	start.campaign.election.Started()
	coordinator.activateMembers(context.Background(), start.lease, start.campaign, &sync.WaitGroup{})

	if start.campaign.ctx.Err() == nil {
		t.Fatal("campaign with no active members was not cancelled")
	}
	if got := coordinator.campaigns.restartFailures; got != 1 {
		t.Fatalf("restartFailures = %d, want 1", got)
	}
	if first.active || second.active {
		t.Fatal("member remained active after every activation failed")
	}

	first.coordinator.closeMember(first)
	second.coordinator.closeMember(second)
}

func TestNewManagerRejectsMissingDependencies(t *testing.T) {
	manager, err := NewManager(nil)
	if err == nil {
		t.Fatal("NewManager() accepted nil dependencies")
	}
	if manager != nil {
		t.Fatal("NewManager() returned a manager after rejecting nil dependencies")
	}

	adapter := &testAdapter{current: make(map[types.UID]*servicecontext.Context)}
	valid := func() Dependencies {
		return Dependencies{
			Config: &kubevip.Config{}, Leases: lease.NewManager(),
			State: adapter, Datapath: adapter, Runner: adapter, Scheduler: adapter,
		}
	}

	tests := []struct {
		name   string
		modify func(*Dependencies)
	}{
		{name: "config", modify: func(dependencies *Dependencies) { dependencies.Config = nil }},
		{name: "lease store", modify: func(dependencies *Dependencies) { dependencies.Leases = nil }},
		{name: "service state", modify: func(dependencies *Dependencies) { dependencies.State = nil }},
		{name: "datapath", modify: func(dependencies *Dependencies) { dependencies.Datapath = nil }},
		{name: "campaign runner", modify: func(dependencies *Dependencies) { dependencies.Runner = nil }},
		{name: "restart scheduler", modify: func(dependencies *Dependencies) { dependencies.Scheduler = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies := valid()
			test.modify(&dependencies)
			manager, err := NewManager(&dependencies)
			if err == nil {
				t.Fatal("NewManager() accepted missing dependency")
			}
			if manager != nil {
				t.Fatal("NewManager() returned a manager after rejecting its dependencies")
			}
		})
	}
}

func readyMember(t *testing.T, manager *Manager, adapter *testAdapter, service *v1.Service) *member {
	t.Helper()
	ctx := servicecontext.New(context.Background())
	adapter.mutex.Lock()
	adapter.current[service.UID] = ctx
	adapter.mutex.Unlock()
	ctx.SignalReadiness()
	generation := ctx.CurrentReadiness()
	member, joined := manager.join(ctx, service, generation)
	if !joined {
		t.Fatal("ready Service did not join its coordinator")
	}
	return member
}

func TestManagerIssuesNewClaimForEachReadinessGeneration(t *testing.T) {
	manager, adapter, leaseMgr := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	first := readyMember(t, manager, adapter, service)
	firstToken := first.registration.Name()
	firstContext := first.serviceContext
	first.coordinator.closeMember(first)

	generation := firstContext.CurrentReadiness()
	if !firstContext.ResetReadinessGeneration(generation) {
		t.Fatal("failed to reset readiness generation")
	}
	firstContext.SignalReadiness()
	secondGeneration := firstContext.CurrentReadiness()
	second, joined := manager.join(firstContext, service, secondGeneration)
	if !joined {
		t.Fatal("new readiness generation did not join")
	}
	if second.registration.Name() == firstToken {
		t.Fatal("new readiness generation reused a claim token")
	}

	first.coordinator.closeMember(first)
	if leaseMgr.Get(second.coordinator.id) == nil {
		t.Fatal("stale member retired the replacement lease")
	}
	second.coordinator.closeMember(second)
}

func TestLastMemberRetirementRemovesCoordinatorFromRegistry(t *testing.T) {
	manager, adapter, _ := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: "service",
	}}
	member := readyMember(t, manager, adapter, service)
	coordinator := member.coordinator

	if current := manager.coordinatorMgr.current(coordinator.id); current != coordinator {
		t.Fatal("joined coordinator is not registered")
	}

	member.coordinator.closeMember(member)

	if current := manager.coordinatorMgr.current(coordinator.id); current != nil {
		t.Fatal("retired coordinator remained registered")
	}
}

func TestStaleCoordinatorCannotRemoveReplacementFromRegistry(t *testing.T) {
	manager, _, _ := newTestManager()
	id := lease.NewID("kubernetes", "default", "shared")
	stale := manager.coordinatorMgr.getOrCreate(id)
	manager.coordinatorMgr.remove(stale)
	replacement := manager.coordinatorMgr.getOrCreate(id)

	manager.coordinatorMgr.remove(stale)

	if current := manager.coordinatorMgr.current(id); current != replacement {
		t.Fatal("stale coordinator removed its replacement")
	}
}

func TestSharedCoordinatorAggregatesCurrentMemberVIPs(t *testing.T) {
	manager, adapter, _ := newTestManager()
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	firstService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "default", UID: "first", Annotations: annotations},
		Spec:       v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"},
	}
	secondService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "default", UID: "second", Annotations: annotations},
		Spec:       v1.ServiceSpec{LoadBalancerIP: "192.0.2.20"},
	}
	first := readyMember(t, manager, adapter, firstService)
	second := readyMember(t, manager, adapter, secondService)

	if first.coordinator != second.coordinator {
		t.Fatal("members with a shared lease got different coordinators")
	}
	if got, want := first.coordinator.membership.lease.OwnedVIPs(), []string{"192.0.2.10", "192.0.2.20"}; !slices.Equal(got, want) {
		t.Fatalf("OwnedVIPs() = %v, want %v", got, want)
	}
	first.coordinator.closeMember(first)
	if got, want := second.coordinator.membership.lease.OwnedVIPs(), []string{"192.0.2.20"}; !slices.Equal(got, want) {
		t.Fatalf("OwnedVIPs() after leave = %v, want %v", got, want)
	}
	second.coordinator.closeMember(second)
}

func TestReplacingUIDGenerationKeepsSiblingClaim(t *testing.T) {
	manager, adapter, _ := newTestManager()
	annotations := map[string]string{kubevip.ServiceLease: "shared"}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service", Annotations: annotations}}
	siblingService := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "sibling", Namespace: "default", UID: "sibling", Annotations: annotations}}
	old := readyMember(t, manager, adapter, service)
	sibling := readyMember(t, manager, adapter, siblingService)

	replacementContext := servicecontext.New(context.Background())
	adapter.mutex.Lock()
	adapter.current[service.UID] = replacementContext
	adapter.mutex.Unlock()
	replacementContext.SignalReadiness()
	generation := replacementContext.CurrentReadiness()
	replacement, joined := manager.join(replacementContext, service, generation)
	if !joined {
		t.Fatal("replacement generation did not join")
	}
	old.coordinator.closeMember(old)
	if replacement.coordinator.currentMember(service.UID) != replacement ||
		replacement.coordinator.currentMember(siblingService.UID) != sibling {
		t.Fatal("stale generation removed a current shared-lease member")
	}
	replacement.coordinator.closeMember(replacement)
	sibling.coordinator.closeMember(sibling)
}

func TestActivationFailureCancelsCampaignAndRecordsBackoff(t *testing.T) {
	manager, adapter, _ := newTestManager()
	adapter.activateErr = errors.New("datapath failed")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	member := readyMember(t, manager, adapter, service)
	coordinator := member.coordinator

	start := coordinator.newCampaignCandidate()
	start.campaign.election.Started()
	coordinator.activateMembers(context.Background(), start.lease, start.campaign, &sync.WaitGroup{})
	if coordinator.campaigns.restartFailures != 1 {
		t.Fatalf("restartFailures = %d, want 1", coordinator.campaigns.restartFailures)
	}
	if got, want := coordinator.restartDelayLocked(), 2*restartBaseDelay; got != want {
		t.Fatalf("restart delay = %v, want %v", got, want)
	}
	member.coordinator.closeMember(member)
}

func TestCloseActiveMemberCleansUpExactlyOnce(t *testing.T) {
	manager, adapter, leaseMgr := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	member := readyMember(t, manager, adapter, service)
	coordinator := member.coordinator
	serviceLease := coordinator.membership.lease

	coordinator.mutex.Lock()
	coordinator.campaigns.current = newCampaign(context.Background(), serviceLease, memberVIPs(coordinator.membersLocked()))
	currentCampaign := coordinator.campaigns.current
	coordinator.mutex.Unlock()
	currentCampaign.election.Started()

	coordinator.activateMember(context.Background(), member, serviceLease, currentCampaign, &sync.WaitGroup{})
	if !member.active {
		t.Fatal("member was not activated")
	}

	member.coordinator.closeMember(member)
	member.coordinator.closeMember(member)

	adapter.mutex.Lock()
	cleanups := len(adapter.cleanups)
	adapter.mutex.Unlock()
	if cleanups != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanups)
	}
	if leaseMgr.Get(coordinator.id) != nil {
		t.Fatal("last member withdrawal did not retire the lease")
	}
}

func TestCloseMemberPreventsConcurrentReactivation(t *testing.T) {
	manager, adapter, _ := newTestManager()
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	adapter.cleanupStarted = cleanupStarted
	adapter.releaseCleanup = releaseCleanup
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	member := readyMember(t, manager, adapter, service)
	coordinator := member.coordinator
	serviceLease := coordinator.membership.lease

	coordinator.mutex.Lock()
	coordinator.campaigns.current = newCampaign(context.Background(), serviceLease, memberVIPs(coordinator.membersLocked()))
	currentCampaign := coordinator.campaigns.current
	coordinator.mutex.Unlock()
	currentCampaign.election.Started()
	coordinator.activateMember(context.Background(), member, serviceLease, currentCampaign, &sync.WaitGroup{})

	closeDone := make(chan struct{})
	go func() {
		coordinator.closeMember(member)
		close(closeDone)
	}()
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("closeMember() did not start datapath cleanup")
	}

	activateDone := make(chan struct{})
	go func() {
		coordinator.activateMember(context.Background(), member, serviceLease, currentCampaign, &sync.WaitGroup{})
		close(activateDone)
	}()
	select {
	case <-activateDone:
		t.Fatal("activation completed while closeMember held the member operation lock")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseCleanup)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("closeMember() did not finish after cleanup was released")
	}
	select {
	case <-activateDone:
	case <-time.After(time.Second):
		t.Fatal("activation did not finish after closeMember released the operation lock")
	}

	if current := coordinator.currentMember(service.UID); current != nil {
		t.Fatal("closed member remained registered")
	}
	coordinator.mutex.Lock()
	active := member.active
	coordinator.mutex.Unlock()
	if active {
		t.Fatal("closed member was reactivated")
	}
	adapter.mutex.Lock()
	activations := len(adapter.activations)
	cleanups := len(adapter.cleanups)
	adapter.mutex.Unlock()
	if activations != 1 {
		t.Fatalf("activation calls = %d, want 1", activations)
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanups)
	}
}

func TestLeavingInactiveMemberDoesNotCleanupDatapath(t *testing.T) {
	manager, adapter, _ := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	member := readyMember(t, manager, adapter, service)

	member.coordinator.closeMember(member)

	adapter.mutex.Lock()
	cleanups := len(adapter.cleanups)
	adapter.mutex.Unlock()
	if cleanups != 0 {
		t.Fatalf("cleanup calls = %d, want 0", cleanups)
	}
}

func TestDetachForContextWithdrawsWithoutDatapathCleanup(t *testing.T) {
	manager, adapter, _ := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	member := readyMember(t, manager, adapter, service)
	coordinator := member.coordinator
	serviceLease := coordinator.membership.lease

	coordinator.mutex.Lock()
	coordinator.campaigns.current = newCampaign(context.Background(), serviceLease, memberVIPs(coordinator.membersLocked()))
	currentCampaign := coordinator.campaigns.current
	coordinator.mutex.Unlock()
	currentCampaign.election.Started()
	coordinator.activateMember(context.Background(), member, serviceLease, currentCampaign, &sync.WaitGroup{})
	if !member.active {
		t.Fatal("member was not activated")
	}

	manager.DetachForContext(member.serviceContext, service)

	if current := coordinator.currentMember(service.UID); current != nil {
		t.Fatal("DetachForContext() did not withdraw the member")
	}
	adapter.mutex.Lock()
	cleanups := len(adapter.cleanups)
	adapter.mutex.Unlock()
	if cleanups != 0 {
		t.Fatalf("cleanup calls = %d, want 0", cleanups)
	}
}

func TestExternalElectionEndDeactivatesMember(t *testing.T) {
	manager, adapter, leaseMgr := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", UID: "service",
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(manager.config.LeaderElectionType, namespace, name)
	controlPlaneToken := lease.ObjectName(id, "control-plane")
	member := readyMember(t, manager, adapter, service)
	sharedLease := member.coordinator.membership.lease
	if claimed, _ := leaseMgr.Claim(id, controlPlaneToken); claimed != sharedLease {
		t.Fatal("control plane did not join the Service lease")
	}
	externalParticipation := sharedLease.JoinElection()
	externalElection := externalParticipation.Session
	if !externalParticipation.RunsCampaign() {
		t.Fatal("external election did not start")
	}
	externalElection.Started()

	var wg sync.WaitGroup
	member.coordinator.startCampaign(&wg)
	waitForAdapterCount(t, adapter, func(a *testAdapter) int { return len(a.activations) }, 1, "activation")

	externalElection.Stopped()
	member.coordinator.closeMember(member)
	wg.Wait()
	waitForAdapterCount(t, adapter, func(a *testAdapter) int { return len(a.cleanups) }, 1, "cleanup")
	member.coordinator.mutex.Lock()
	active := member.active
	member.coordinator.mutex.Unlock()
	if active {
		t.Fatal("member remained active after external election ended")
	}

	leaseMgr.Delete(id, controlPlaneToken, sharedLease)
}

func waitForAdapterCount(t *testing.T, adapter *testAdapter, count func(*testAdapter) int, want int, operation string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		adapter.mutex.Lock()
		got := count(adapter)
		adapter.mutex.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s count did not reach %d", operation, want)
}

func TestManagerRejectsStaleReadinessGeneration(t *testing.T) {
	manager, adapter, _ := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	ctx := servicecontext.New(context.Background())
	adapter.current[service.UID] = ctx
	ctx.SignalReadiness()
	generation := ctx.CurrentReadiness()
	if !ctx.ResetReadinessGeneration(generation) {
		t.Fatal("failed to advance readiness generation")
	}
	if member, joined := manager.join(ctx, service, generation); joined || member != nil {
		t.Fatal("stale readiness generation joined a coordinator")
	}
}
