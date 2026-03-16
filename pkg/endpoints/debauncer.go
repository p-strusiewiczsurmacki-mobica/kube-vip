package endpoints

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
)

type AggregatedEvent struct {
	Type   watch.EventType
	Events []*watch.Event
}

type Debauncer struct {
	rw       *watchtools.RetryWatcher
	input    <-chan watch.Event
	output   chan AggregatedEvent
	stopChan chan any
	stopOnce sync.Once
}

func NewDebauncer(rw *watchtools.RetryWatcher) *Debauncer {
	return &Debauncer{
		rw:       rw,
		input:    rw.ResultChan(),
		output:   make(chan AggregatedEvent),
		stopChan: make(chan any),
	}
}

const debaunceTime = time.Second * 2

func (d *Debauncer) Start(ctx context.Context) {
	t := time.NewTicker(debaunceTime)
	defer func() {
		close(d.output)
		d.rw.Stop()
	}()

	var aggregated *AggregatedEvent

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopChan:
			return
		case tmp := <-d.input:
			if tmp.Type == "" {
				return
			}
			if aggregated != nil {
				if tmp.Type != aggregated.Type {
					d.output <- *aggregated
					aggregated = nil
				}
			}
			if aggregated == nil {
				aggregated = &AggregatedEvent{
					Type: tmp.Type,
				}
			}
			aggregated.Events = append(aggregated.Events, &tmp)
			t.Reset(debaunceTime)
		case <-t.C:
			if aggregated != nil {
				d.output <- *aggregated
				aggregated = nil
			}
		}
	}
}

func (d *Debauncer) Stop() {
	d.stopOnce.Do(func() {
		close(d.stopChan)
	})
}

func (d *Debauncer) Output() chan AggregatedEvent {
	return d.output
}
