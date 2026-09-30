package lease

import (
	"context"
	"fmt"
	log "log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
)

// Manager is used to manage leases.
type Manager struct {
	leases map[string]*Lease
	lock   sync.Mutex
}

// NewManager creates new lease manager.
func NewManager() *Manager {
	return &Manager{
		leases: make(map[string]*Lease),
	}
}

// Add creates or retrieves the lease identified by id.
func (m *Manager) Add(ctx context.Context, id ID) *Lease {
	m.lock.Lock()
	defer m.lock.Unlock()
	return m.addLocked(ctx, id)
}

// Acquire creates or retrieves a lease and atomically registers objectName
// together with its current VIP ownership provider. The returned bool reports
// whether this object was newly registered.
func (m *Manager) Acquire(ctx context.Context, id ID, objectName string,
	vipProvider VIPProvider) (*Lease, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	lease := m.addLocked(ctx, id)
	return lease, lease.AddWithVIPProvider(objectName, vipProvider)
}

// Claim atomically registers objectName against an existing lease. It returns
// nil when the lease was retired before the caller could join it.
func (m *Manager) Claim(id ID, objectName string) (*Lease, bool) {
	return m.ClaimWithVIPProvider(id, objectName, nil)
}

// ClaimWithVIPProvider atomically registers objectName and its VIP ownership
// provider against an existing lease.
func (m *Manager) ClaimWithVIPProvider(id ID, objectName string, vipProvider VIPProvider) (*Lease, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	lease, exists := m.leases[id.NamespacedName()]
	if !exists {
		return nil, false
	}
	return lease, lease.AddWithVIPProvider(objectName, vipProvider)
}

func (m *Manager) addLocked(ctx context.Context, id ID) *Lease {

	// A lease whose context is already cancelled cannot be handed out again:
	// anything derived from it would be cancelled straight away. Replace it.
	if l, exists := m.leases[id.NamespacedName()]; !exists || l.Ctx.Err() != nil {
		leaseCtx, leaseCancel := context.WithCancel(ctx)
		m.leases[id.NamespacedName()] = newLease(leaseCtx, leaseCancel)
	}

	return m.leases[id.NamespacedName()]
}

// Delete removes the object from the lease it was added to and cancels that lease
// once its last object is gone. It reports whether the lease was retired. With a
// common lease, the siblings that still use it keep it alive.
//
// The lease the caller was given has to be passed in, because cleanup is usually
// deferred to a goroutine that runs long after the object went away. By then the
// lease of that name may already have been replaced, for instance because the
// service was torn down and rebuilt, and cancelling the replacement would leave
// the service unhandled. A stale caller is therefore ignored.
//
// Teardown paths have to call this synchronously rather than leaving it to the
// deferred cleanup: until the lease is out of the map, Add hands the same
// instance back, so a service that is rebuilt straight away gets parented to a
// lease that the pending cleanup is about to cancel.
func (m *Manager) Delete(id ID, objectName string, l *Lease) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	current := m.currentFor(id, l)
	if current == nil {
		return false
	}

	current.delete(objectName)
	if current.cnt.Load() < 1 {
		m.retire(id, current)
		return true
	}
	return false
}

// currentFor returns the registered lease for id, or nil when the caller is
// stale, meaning the lease it holds is no longer the registered one. Callers have
// to hold m.lock.
func (m *Manager) currentFor(id ID, l *Lease) *Lease {
	current, exist := m.leases[id.NamespacedName()]
	if !exist || (l != nil && current != l) {
		return nil
	}
	return current
}

// retire cancels the lease and drops it from the manager. Callers have to hold
// m.lock.
func (m *Manager) retire(id ID, l *Lease) {
	l.Cancel()
	delete(m.leases, id.NamespacedName())
}

// Get returns lease for the service.
func (m *Manager) Get(id ID) *Lease {
	m.lock.Lock()
	defer m.lock.Unlock()

	if lease, exist := m.leases[id.NamespacedName()]; exist {
		return lease
	}
	return nil
}

// Lease holds lease data.
type Lease struct {
	Ctx      context.Context
	Cancel   context.CancelFunc
	services sync.Map
	cnt      atomic.Int64
	// Elected is kept temporarily for compatibility with callers being migrated
	// to IsLeading and generation-scoped ElectionSession values.
	Elected    atomic.Bool
	stateMu    sync.Mutex
	phase      electionPhase
	generation uint64
	changed    chan struct{}
}

type electionPhase uint8

const (
	electionIdle electionPhase = iota
	electionCampaigning
	electionLeading
)

// ElectionSession identifies one generation of the local runner coordinating
// a shared Lease. Only the session that started a generation may change its
// state; delayed callbacks from older generations are ignored.
type ElectionSession struct {
	lease      *Lease
	generation uint64
	owner      bool
}

// VIPProvider returns the VIPs currently owned by one local Lease member.
// Implementations must be safe for concurrent use and must not return mutable
// state that can change while the caller is reading it.
type VIPProvider func() []string

// StaticVIPProvider returns a provider backed by an immutable copy of vips.
func StaticVIPProvider(vips []string) VIPProvider {
	owned := append([]string(nil), vips...)
	return func() []string {
		return append([]string(nil), owned...)
	}
}

type member struct {
	vipProvider VIPProvider
}

func newLease(ctx context.Context, cancel context.CancelFunc) *Lease {
	return &Lease{
		Ctx:     ctx,
		Cancel:  cancel,
		changed: make(chan struct{}),
	}
}

// NewElectionContext returns a context for one election runner. Cancelling it
// stops only that runner; the Lease context remains live until its final member
// is deleted from the Manager.
func (l *Lease) NewElectionContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(l.Ctx)
	stopParent := context.AfterFunc(parent, cancel)
	return ctx, func() {
		stopParent()
		cancel()
	}
}

// Add adds the object to the lease and increments counter
// it will return true if object was added
func (l *Lease) Add(name string) bool {
	return l.AddWithVIPProvider(name, nil)
}

// AddWithVIPProvider adds an object and its VIP ownership provider to the
// lease. Re-adding the same object leaves the original registration intact.
func (l *Lease) AddWithVIPProvider(name string, vipProvider VIPProvider) bool {
	if _, exists := l.services.LoadOrStore(name, member{vipProvider: vipProvider}); !exists {
		l.cnt.Add(1)
		return true
	}
	return false
}

// OwnedVIPs returns a stable, deduplicated snapshot of VIPs contributed by all
// local members sharing this Lease.
func (l *Lease) OwnedVIPs() []string {
	providers := make([]VIPProvider, 0, l.cnt.Load())
	l.services.Range(func(_, value any) bool {
		registered, ok := value.(member)
		if ok && registered.vipProvider != nil {
			providers = append(providers, registered.vipProvider)
		}
		return true
	})

	unique := make(map[string]struct{})
	for _, provider := range providers {
		for _, vip := range provider() {
			if vip != "" {
				unique[vip] = struct{}{}
			}
		}
	}
	vips := make([]string, 0, len(unique))
	for vip := range unique {
		vips = append(vips, vip)
	}
	slices.Sort(vips)
	return vips
}

// delete removes the service from the lease and decrements the counter.
func (l *Lease) delete(service string) {
	if _, exists := l.services.LoadAndDelete(service); exists {
		l.cnt.Add(-1)
	}
}

// AcquireElection starts a new election generation when the Lease is idle, or
// returns a session observing the current generation. The returned bool is true
// only for the caller responsible for running that election.
func (l *Lease) AcquireElection() (*ElectionSession, bool) {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()

	owner := l.phase == electionIdle
	if owner {
		l.generation++
		l.phase = electionCampaigning
		l.signalStateLocked()
	}
	return &ElectionSession{lease: l, generation: l.generation, owner: owner}, owner
}

// Started marks this session as leading. It returns false for observers,
// already-stopped sessions, and sessions superseded by a newer generation.
func (s *ElectionSession) Started() bool {
	if s == nil || s.lease == nil || !s.owner {
		return false
	}
	l := s.lease
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.generation != s.generation || l.phase != electionCampaigning {
		return false
	}
	l.phase = electionLeading
	l.Elected.Store(true)
	l.signalStateLocked()
	return true
}

// Stopped ends this session. It is safe to call repeatedly: once another
// generation starts, a delayed call from this session cannot stop it.
func (s *ElectionSession) Stopped() bool {
	if s == nil || s.lease == nil || !s.owner {
		return false
	}
	l := s.lease
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.generation != s.generation || l.phase == electionIdle {
		return false
	}
	l.phase = electionIdle
	l.Elected.Store(false)
	l.signalStateLocked()
	return true
}

// IsLeading reports whether this exact election generation is still leading.
func (s *ElectionSession) IsLeading() bool {
	if s == nil || s.lease == nil {
		return false
	}
	phase, generation, _ := s.lease.electionState()
	return generation == s.generation && phase == electionLeading
}

// WaitForLeader waits for this election generation to either become leader or
// end. A replacement generation is not silently adopted.
func (s *ElectionSession) WaitForLeader(ctx context.Context) bool {
	if s == nil || s.lease == nil {
		return false
	}
	for {
		phase, generation, changed := s.lease.electionState()
		if generation != s.generation {
			return false
		}
		switch phase {
		case electionLeading:
			return true
		case electionIdle:
			return false
		}

		select {
		case <-ctx.Done():
			return false
		case <-s.lease.Ctx.Done():
			return false
		case <-changed:
		}
	}
}

// WaitForEnd waits until this election generation is no longer leading.
func (s *ElectionSession) WaitForEnd(ctx context.Context) {
	if s == nil || s.lease == nil {
		return
	}
	for {
		phase, generation, changed := s.lease.electionState()
		if generation != s.generation || phase != electionLeading {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-s.lease.Ctx.Done():
			return
		case <-changed:
		}
	}
}

// IsLeading reports whether the Lease's current election generation is
// leading. Callers that participate in an election should prefer the
// generation-scoped ElectionSession.IsLeading method.
func (l *Lease) IsLeading() bool {
	phase, _, _ := l.electionState()
	return phase == electionLeading
}

// BeginElection is the compatibility API for callers not yet migrated to
// AcquireElection.
func (l *Lease) BeginElection() bool {
	_, owner := l.AcquireElection()
	return owner
}

// ElectionStarted is the compatibility API for callers not yet migrated to an
// ElectionSession. It intentionally preserves the old idle-to-leading behavior
// while the remaining callers are migrated.
func (l *Lease) ElectionStarted() {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.phase == electionLeading {
		return
	}
	if l.phase == electionIdle {
		l.generation++
	}
	l.phase = electionLeading
	l.Elected.Store(true)
	l.signalStateLocked()
}

// ElectionStopped is the compatibility API for callers not yet migrated to an
// ElectionSession.
func (l *Lease) ElectionStopped() {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.phase == electionIdle {
		return
	}
	l.phase = electionIdle
	l.Elected.Store(false)
	l.signalStateLocked()
}

// WaitForLeader waits for an in-flight lease election to either elect a leader
// or finish without one. It never holds the lease state mutex while waiting.
func (l *Lease) WaitForLeader(ctx context.Context) bool {
	_, elected := l.WaitForLeaderGeneration(ctx)
	return elected
}

// WaitForLeaderGeneration waits for leadership and returns the election-end
// generation observed atomically with the elected state.
func (l *Lease) WaitForLeaderGeneration(ctx context.Context) (uint64, bool) {
	phase, generation, _ := l.electionState()
	if phase == electionIdle {
		return 0, false
	}
	session := &ElectionSession{lease: l, generation: generation}
	return generation, session.WaitForLeader(ctx)
}

// WaitForElectionEnd waits until an elected lease loses its leader. It never
// holds the lease state mutex while waiting.
func (l *Lease) WaitForElectionEnd(ctx context.Context) {
	_, generation, _ := l.electionState()
	l.WaitForElectionEndAfter(ctx, generation)
}

// WaitForElectionEndAfter waits until the leadership generation returned by
// WaitForLeaderGeneration ends, even if a replacement election starts first.
func (l *Lease) WaitForElectionEndAfter(ctx context.Context, generation uint64) {
	(&ElectionSession{lease: l, generation: generation}).WaitForEnd(ctx)
}

func (l *Lease) electionState() (electionPhase, uint64, <-chan struct{}) {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	return l.phase, l.generation, l.changed
}

func (l *Lease) signalStateLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// ServiceName gets lease name and id for the service.
func ServiceName(service *v1.Service) (string, string) {
	return ServiceNameFor(service.Namespace, service.Name, service.Annotations[kubevip.ServiceLease])
}

func ServiceNameFor(namespace, serviceName, leaseName string) (string, string) {
	name := leaseName
	if name == "" {
		name = fmt.Sprintf("kubevip-%s", serviceName)
	}

	serviceLeaseParts := strings.Split(name, "/")

	if len(serviceLeaseParts) > 1 {
		namespace = serviceLeaseParts[0]
		name = serviceLeaseParts[1]
	}

	return namespace, name
}

func ServiceNamespacedName(service *v1.Service) string {
	return fmt.Sprintf("%s/%s", service.Namespace, service.Name)
}

func ObjectName(id ID, suffix string) string {
	return fmt.Sprintf("%s-%s", id.NamespacedName(), suffix)
}

func NamespaceName(lease string, c *kubevip.Config) (string, string) {
	leaseName := lease
	leasnameParts := strings.Split(lease, "/")
	var ns string
	var err error
	if len(leasnameParts) > 1 {
		ns = leasnameParts[0]
		leaseName = leasnameParts[1]
	} else {
		ns, err = returnNamespace()
		if err != nil {
			log.Warn("unable to auto-detect namespace, dropping to config", "namespace", c.Namespace)
			ns = c.Namespace
		}
	}
	return ns, leaseName
}

func returnNamespace() (string, error) {
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns, nil
		}
		return "", err
	}
	return "", fmt.Errorf("unable to find Namespace")
}

type ID interface {
	Name() string
	Namespace() string
	NamespacedName() string
}

type CommonID struct {
	namespace string
	name      string
}

func NewID(leaseType, namespace, name string) ID {
	if leaseType == "etcd" {
		return newEtcdID(namespace, name)
	}
	return newKubernetesID(namespace, name)
}

func newKubernetesID(namespace, name string) ID {
	return &KubernetesID{
		CommonID: CommonID{
			namespace: namespace,
			name:      name,
		},
	}
}
func newEtcdID(namespace, name string) ID {
	return &EtcdID{
		CommonID: CommonID{
			namespace: namespace,
			name:      name,
		},
	}
}

func (c *CommonID) Name() string {
	return c.name
}

func (c *CommonID) Namespace() string {
	return c.namespace
}

type KubernetesID struct {
	CommonID
}

func (k *KubernetesID) NamespacedName() string {
	return fmt.Sprintf("%s/%s", k.namespace, k.name)
}

type EtcdID struct {
	CommonID
}

func (e *EtcdID) NamespacedName() string {
	return fmt.Sprintf("%s-%s", e.namespace, e.name)
}
