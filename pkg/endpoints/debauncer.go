package endpoints

import (
	"context"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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
	events   map[string]map[string]*item
}

func NewDebauncer(rw *watchtools.RetryWatcher) *Debauncer {
	return &Debauncer{
		rw:       rw,
		input:    rw.ResultChan(),
		output:   make(chan AggregatedEvent),
		stopChan: make(chan any),
		events:   make(map[string]map[string]*item),
	}
}

type item struct {
	input     chan watch.Event
	lastEvent *AggregatedEvent
	output    chan<- AggregatedEvent
	stopChan  chan any
	stopOnce  sync.Once
}

func newItem(output chan<- AggregatedEvent) *item {
	return &item{
		input:    make(chan watch.Event),
		output:   output,
		stopChan: make(chan any),
	}
}

const debaunceTime = time.Second * 2

func (d *Debauncer) Start(ctx context.Context) {
	wg := sync.WaitGroup{}
	debauncerCtx, cancel := context.WithCancel(ctx)
	defer func() {
		close(d.output)
		d.rw.Stop()
		cancel()
		wg.Wait()
	}()

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

			var namespace, name string
			switch v := tmp.Object.(type) {
			case *discoveryv1.EndpointSlice:
				namespace = v.Namespace
				name = v.Name
			case *v1.Service:
				namespace = v.Namespace
				name = v.Name
			default:
				return
			}

			nsEvent, exists := d.events[namespace]
			if !exists {
				d.events[namespace] = make(map[string]*item)
				nsEvent = d.events[namespace]
			}

			nameEvent, exists := nsEvent[name]
			if !exists {
				nsEvent[name] = newItem(d.output)
				nameEvent = nsEvent[name]

				wg.Go(func() {
					nameEvent.start(debauncerCtx)
					delete(d.events[namespace], name)
				})
			}

			nameEvent.input <- tmp

			if tmp.Type == watch.Deleted {
				nameEvent.stop()
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

func (i *item) start(ctx context.Context) {

	t := time.NewTicker(debaunceTime)

	var aggregated *AggregatedEvent

	for {
		select {
		case <-ctx.Done():
			return
		case <-i.stopChan:
			return
		case tmp := <-i.input:
			if aggregated != nil {
				if tmp.Type != aggregated.Type {
					i.output <- *aggregated
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
				i.output <- *aggregated
				if aggregated.Type == watch.Deleted {

				}
				aggregated = nil
			}
			t.Reset(debaunceTime)
		}
	}
}

func (i *item) stop() {
	i.stopOnce.Do(func() {
		close(i.stopChan)
	})
}
