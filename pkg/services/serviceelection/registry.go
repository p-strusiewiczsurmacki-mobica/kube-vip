package serviceelection

import (
	"context"
	"fmt"
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
	dependencies    Dependencies
}

// NewManager creates a Service election manager.
func NewManager(dependencies *Dependencies) (*Manager, error) {
	if dependencies == nil {
		return nil, fmt.Errorf("create service election manager: dependencies are required")
	}
	if err := dependencies.validate(); err != nil {
		return nil, fmt.Errorf("create service election manager: %w", err)
	}
	registry := &registry{
		coordinators: make(map[string]*coordinator),
		dependencies: *dependencies,
	}
	return &Manager{config: dependencies.Config, state: dependencies.State, registry: registry}, nil
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
		registry:     r,
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
