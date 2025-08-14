package lease

import (
	"context"
	"fmt"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
)

type Manager struct {
	leases map[string]*Lease
	lock   sync.Mutex
}

func NewManager() *Manager {
	return &Manager{
		leases: make(map[string]*Lease),
	}
}

func (m *Manager) Add(service *v1.Service) bool {
	m.lock.Lock()
	defer m.lock.Unlock()
	name := GetName(service)
	if _, exist := m.leases[name]; !exist {
		ctx, cancel := context.WithCancel(context.Background())
		m.leases[name] = newLease(ctx, cancel)
		return true
	}

	m.leases[name].increment()
	return false
}

func (m *Manager) Delete(service *v1.Service) {
	m.lock.Lock()
	defer m.lock.Unlock()
	name := GetName(service)
	if _, exist := m.leases[name]; exist {
		m.leases[name].decrement()
	}
}

func (m *Manager) Get(service *v1.Service) *Lease {
	m.lock.Lock()
	defer m.lock.Unlock()
	name := GetName(service)
	if lease, exist := m.leases[name]; exist {
		return lease
	}
	return nil
}

func (m *Manager) GetLeaderContext(service *v1.Service) context.Context {
	m.lock.Lock()
	defer m.lock.Unlock()
	name := GetName(service)
	if _, ok := m.leases[name]; !ok {
		return nil
	}
	return m.leases[name].Ctx
}

type Lease struct {
	cnt    uint
	Lock   *sync.Mutex
	Ctx    context.Context
	Cancel context.CancelFunc
}

func newLease(ctx context.Context, cancel context.CancelFunc) *Lease {
	return &Lease{
		Ctx:    ctx,
		Cancel: cancel,
		cnt:    1,
		Lock:   new(sync.Mutex),
	}
}

func (l *Lease) increment() {
	l.Lock.Lock()
	defer l.Lock.Unlock()
	l.cnt++
}

func (l *Lease) decrement() {
	l.Lock.Lock()
	defer l.Lock.Unlock()
	if l.cnt == 0 {
		return
	}
	l.cnt--
	if l.cnt < 1 {
		l.Cancel()
	}
}

func GetName(service *v1.Service) string {
	serviceLease, exists := service.Annotations[kubevip.ServiceLease]
	if !exists {
		serviceLease = fmt.Sprintf("kubevip-%s", service.Name)
	}
	return serviceLease
}
