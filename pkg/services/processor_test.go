package services

// newTestServiceLocks supplies the invariant normally established by
// NewServicesProcessor to tests that need a partially configured Processor.
func newTestServiceLocks() *ServiceLock {
	return NewServiceLock()
}
