package services

import (
	"sync"

	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

type Callback struct {
	Function func(*servicecontext.Context, *v1.Service, *sync.WaitGroup) error
}

func NewCallback(f func(*servicecontext.Context, *v1.Service, *sync.WaitGroup) error) *Callback {
	return &Callback{Function: f}
}

func (c *Callback) Run(svcCtx *servicecontext.Context, svc *v1.Service, wg *sync.WaitGroup) error {
	if c == nil || c.Function == nil {
		return nil
	}
	return c.Function(svcCtx, svc, wg)
}
