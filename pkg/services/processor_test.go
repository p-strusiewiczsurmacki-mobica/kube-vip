package services

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

// newTestServiceLocks supplies the invariant normally established by
// NewServicesProcessor to tests that need a partially configured Processor.
func newTestServiceLocks() *ServiceLock {
	return NewServiceLock()
}

// initializeTestElectionCoordinators supplies the invariant normally
// established by NewServicesProcessor after a partial test Processor is built.
func initializeTestElectionCoordinators(processor *Processor) {
	processor.electionCoordinators = newElectionCoordinatorManager(processor)
}

func TestNewServicesProcessorInitializesElectionCoordinators(t *testing.T) {
	processor := NewServicesProcessor(&kubevip.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if processor.electionCoordinators == nil {
		t.Fatal("NewServicesProcessor() did not initialize election coordinators")
	}
}
