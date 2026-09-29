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
	mutex       sync.Mutex
	current     map[types.UID]*servicecontext.Context
	activations []*Member
	cleanups    []*Member
	activateErr error
}

func (a *testAdapter) IsCurrent(member *Member) bool {
	a.mutex.Lock()
	current := a.current[member.Service().UID]
	a.mutex.Unlock()
	ctx := member.ServiceContext()
	if current != ctx || ctx.Ctx.Err() != nil || !ctx.ReadinessGenerationCurrent(member.ReadinessGeneration()) {
		return false
	}
	return member.Coordinator() == nil || member.Coordinator().Contains(member)
}

func (a *testAdapter) ActivateMember(_ context.Context, member *Member, _ *lease.Lease, _ *sync.WaitGroup) error {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.activations = append(a.activations, member)
	return a.activateErr
}

func (a *testAdapter) CleanupMember(member *Member, _ *lease.Lease) error {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.cleanups = append(a.cleanups, member)
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

func newTestManager() (*Manager, *testAdapter, *lease.Manager) {
	adapter := &testAdapter{current: make(map[types.UID]*servicecontext.Context)}
	leaseMgr := lease.NewManager()
	manager := NewManager(&kubevip.Config{}, leaseMgr, nil, adapter, adapter, adapter, adapter)
	return manager, adapter, leaseMgr
}

func readyMember(t *testing.T, manager *Manager, adapter *testAdapter, service *v1.Service) *Member {
	t.Helper()
	ctx := servicecontext.New(context.Background())
	adapter.mutex.Lock()
	adapter.current[service.UID] = ctx
	adapter.mutex.Unlock()
	ctx.SignalReadiness()
	generation, _, _, _ := ctx.ReadinessState()
	member, joined := manager.Join(ctx, service, generation)
	if !joined {
		t.Fatal("ready Service did not join its coordinator")
	}
	return member
}

func TestManagerIssuesNewClaimForEachReadinessGeneration(t *testing.T) {
	manager, adapter, leaseMgr := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	first := readyMember(t, manager, adapter, service)
	firstToken := first.ClaimToken()
	firstContext := first.ServiceContext()
	manager.Leave(first)

	generation, _, _, ready := firstContext.ReadinessState()
	if !ready || !firstContext.ResetReadinessGeneration(generation) {
		t.Fatal("failed to reset readiness generation")
	}
	firstContext.SignalReadiness()
	secondGeneration, _, _, _ := firstContext.ReadinessState()
	second, joined := manager.Join(firstContext, service, secondGeneration)
	if !joined {
		t.Fatal("new readiness generation did not join")
	}
	if second.ClaimToken() == firstToken {
		t.Fatal("new readiness generation reused a claim token")
	}

	manager.Leave(first)
	if leaseMgr.Get(second.Coordinator().id) == nil {
		t.Fatal("stale member retired the replacement lease")
	}
	manager.Leave(second)
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

	if first.Coordinator() != second.Coordinator() {
		t.Fatal("members with a shared lease got different coordinators")
	}
	if got, want := first.Coordinator().Lease().OwnedVIPs(), []string{"192.0.2.10", "192.0.2.20"}; !slices.Equal(got, want) {
		t.Fatalf("OwnedVIPs() = %v, want %v", got, want)
	}
	manager.Leave(first)
	if got, want := second.Coordinator().Lease().OwnedVIPs(), []string{"192.0.2.20"}; !slices.Equal(got, want) {
		t.Fatalf("OwnedVIPs() after leave = %v, want %v", got, want)
	}
	manager.Leave(second)
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
	generation, _, _, _ := replacementContext.ReadinessState()
	replacement, joined := manager.Join(replacementContext, service, generation)
	if !joined {
		t.Fatal("replacement generation did not join")
	}
	manager.Leave(old)
	if replacement.Coordinator().CurrentMember(service.UID) != replacement ||
		replacement.Coordinator().CurrentMember(siblingService.UID) != sibling {
		t.Fatal("stale generation removed a current shared-lease member")
	}
	manager.Leave(replacement)
	manager.Leave(sibling)
}

func TestActivationFailureCancelscampaignAndRecordsBackoff(t *testing.T) {
	manager, adapter, _ := newTestManager()
	adapter.activateErr = errors.New("datapath failed")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	member := readyMember(t, manager, adapter, service)
	coordinator := member.Coordinator()

	start := coordinator.prepareCampaign()
	start.lease.ElectionStarted()
	coordinator.activateMember(context.Background(), member, start.lease, start.campaign, &sync.WaitGroup{})
	if coordinator.restartFailures != 1 {
		t.Fatalf("restartFailures = %d, want 1", coordinator.restartFailures)
	}
	if got, want := coordinator.restartDelayLocked(), 2*restartBaseDelay; got != want {
		t.Fatalf("restart delay = %v, want %v", got, want)
	}
	manager.Leave(member)
}

func TestDeactivateAndLeaveActiveMemberCleansUpExactlyOnce(t *testing.T) {
	manager, adapter, leaseMgr := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	member := readyMember(t, manager, adapter, service)
	coordinator := member.Coordinator()
	serviceLease := coordinator.Lease()

	coordinator.mutex.Lock()
	campaignCtx, cancelCampaign := context.WithCancel(context.Background())
	coordinator.campaign = &campaign{ctx: campaignCtx, cancel: cancelCampaign}
	currentCampaign := coordinator.campaign
	coordinator.mutex.Unlock()
	serviceLease.ElectionStarted()

	coordinator.activateMember(context.Background(), member, serviceLease, currentCampaign, &sync.WaitGroup{})
	if !member.Active() {
		t.Fatal("member was not activated")
	}

	coordinator.DeactivateMember(member)
	coordinator.DeactivateMember(member)
	manager.Leave(member)
	manager.Leave(member)

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

func TestLeavingInactiveMemberDoesNotCleanupDatapath(t *testing.T) {
	manager, adapter, _ := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	member := readyMember(t, manager, adapter, service)

	manager.Leave(member)

	adapter.mutex.Lock()
	cleanups := len(adapter.cleanups)
	adapter.mutex.Unlock()
	if cleanups != 0 {
		t.Fatalf("cleanup calls = %d, want 0", cleanups)
	}
}

func TestManagerRejectsStaleReadinessGeneration(t *testing.T) {
	manager, adapter, _ := newTestManager()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"}}
	ctx := servicecontext.New(context.Background())
	adapter.current[service.UID] = ctx
	ctx.SignalReadiness()
	generation, _, _, _ := ctx.ReadinessState()
	if !ctx.ResetReadinessGeneration(generation) {
		t.Fatal("failed to advance readiness generation")
	}
	if member, joined := manager.Join(ctx, service, generation); joined || member != nil {
		t.Fatal("stale readiness generation joined a coordinator")
	}
}
