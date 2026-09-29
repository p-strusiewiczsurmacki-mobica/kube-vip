package serviceelection

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"k8s.io/apimachinery/pkg/types"
)

// Manager coordinates Service readiness with per-lease election state.
type Manager struct {
	config   *kubevip.Config
	state    ServiceState
	registry *registry
}

type registry struct {
	mutex           sync.Mutex
	coordinators    map[string]*coordinator
	nextMemberToken atomic.Uint64
	dependencies    coordinatorDependencies
}

// NewManager creates a Service election manager.
func NewManager(dependencies Dependencies) *Manager {
	registry := &registry{
		coordinators: make(map[string]*coordinator),
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

func (r *registry) coordinatorFor(id lease.ID) *coordinator {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.coordinators == nil {
		r.coordinators = make(map[string]*coordinator)
	}
	key := id.NamespacedName()
	if coordinator := r.coordinators[key]; coordinator != nil {
		return coordinator
	}
	retiredCtx, retire := context.WithCancel(context.Background())
	coordinator := &coordinator{
		dependencies: r.dependencies,
		id:           id,
		members:      make(map[types.UID]*member),
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

func (r *registry) current(id lease.ID) *coordinator {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.coordinators[id.NamespacedName()]
}

func (r *registry) remove(coordinator *coordinator) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.coordinators[coordinator.id.NamespacedName()] == coordinator {
		delete(r.coordinators, coordinator.id.NamespacedName())
	}
}
