package serviceelection

import (
	"sync"

	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

type member struct {
	coordinator         *coordinator
	service             *v1.Service
	serviceContext      *servicecontext.Context
	readinessGeneration servicecontext.ReadinessGeneration
	claimToken          string
	vipProvider         lease.VIPProvider
	operationMutex      sync.Mutex
	active              bool
}

func newMember(coordinator *coordinator, svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration servicecontext.ReadinessGeneration) *member {
	return &member{
		coordinator: coordinator, service: service.DeepCopy(), serviceContext: svcCtx,
		readinessGeneration: readinessGeneration,
	}
}
