package debauncer

import (
	"context"
	"log/slog"
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

func New(rw *watchtools.RetryWatcher) *Debauncer {
	return &Debauncer{
		rw:       rw,
		input:    rw.ResultChan(),
		output:   make(chan AggregatedEvent),
		stopChan: make(chan any),
		events:   make(map[string]map[string]*item),
	}
}

type item struct {
	input    chan watch.Event
	output   chan<- AggregatedEvent
	stopChan chan any
	stopOnce sync.Once
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
		slog.Info("DEBAUNCER - Defere")
		d.rw.Stop()
		cancel()
		slog.Info("DEBAUNCER - waiting for goroutines")
		wg.Wait()
		slog.Info("DEBAUNCER - goroutines finished")
		close(d.output)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopChan:
			return
		case tmp := <-d.input:
			if tmp.Type == "" {
				slog.Info("DEBAUNCER - return no type")
				continue
			}

			var namespace, name string
			switch v := tmp.Object.(type) {
			case *discoveryv1.EndpointSlice:
				slog.Info("DEBAUNCER - endpointslice")
				namespace = v.Namespace
				name = v.OwnerReferences[0].Name
			case *v1.Service:
				slog.Info("DEBAUNCER - service")
				namespace = v.Namespace
				name = v.OwnerReferences[0].Name
			default:
				return
			}

			slog.Info("DEBAUNCER - event for", "namespace", namespace, "name", name)

			nsEvent, exists := d.events[namespace]
			if !exists {
				slog.Info("DEBAUNCER - event for - namespace not exists", "namespace", namespace, "name", name)
				d.events[namespace] = make(map[string]*item)
				nsEvent = d.events[namespace]
			}

			nameEvent, exists := nsEvent[name]
			if !exists {
				slog.Info("DEBAUNCER - event for - name not exists", "namespace", namespace, "name", name)
				nsEvent[name] = newItem(d.output)
				nameEvent = nsEvent[name]

				wg.Go(func() {
					slog.Info("DEBAUNCER - event for - starting loop for name", "namespace", namespace, "name", name)
					nameEvent.start(debauncerCtx)
					slog.Info("DEBAUNCER - event for - finished loop for name, deleting for name", "namespace", namespace, "name", name)
					delete(d.events[namespace], name)
					if len(d.events[namespace]) == 0 {
						slog.Info("DEBAUNCER - event for - finished loop for name, deleting for namespace", "namespace", namespace, "name", name)
						delete(d.events, namespace)
					}
				})
			}

			slog.Info("DEBAUNCER - event for - sending data in", "namespace", namespace, "name", name)
			nameEvent.input <- tmp

			if tmp.Type == watch.Deleted {
				slog.Info("DEBAUNCER - event for - type deleted, stopping", "namespace", namespace, "name", name)
				nameEvent.stop()
			}
		}
	}
}

func (d *Debauncer) Stop() {
	d.stopOnce.Do(func() {
		slog.Info("DEBAUNCER - context stop")
		close(d.stopChan)
	})
}

func (d *Debauncer) Output() chan AggregatedEvent {
	return d.output
}

func (i *item) start(ctx context.Context) {
	slog.Info("DEBAUNCER ITEM - start")
	t := time.NewTicker(debaunceTime)

	var aggregated *AggregatedEvent

	defer func() {
		if aggregated != nil {
			slog.Info("DEBAUNCER ITEM - sending aggregated - defer")
			i.output <- *aggregated
			aggregated = nil
		}
	}()

	for {
		select {
		case <-ctx.Done():
			slog.Info("DEBAUNCER ITEM - context cancelled")
			return
		case <-i.stopChan:
			slog.Info("DEBAUNCER ITEM - stop signal")
			return
		case tmp := <-i.input:
			slog.Info("DEBAUNCER ITEM", "event", tmp)
			if aggregated != nil {
				if tmp.Type != aggregated.Type {
					slog.Info("DEBAUNCER ITEM - sending aggregated - change")
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
				slog.Info("DEBAUNCER ITEM - sending aggregated - timer")
				i.output <- *aggregated
				aggregated = nil
			}
		}
	}
}

func (i *item) stop() {
	i.stopOnce.Do(func() {
		slog.Info("DEBAUNCER ITEM - stop")
		close(i.stopChan)
	})
}
