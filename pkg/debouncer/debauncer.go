package debouncer

import (
	"context"
	"fmt"
	log "log/slog"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/watch"
)

type Event struct {
	Type watch.EventType
	Last *watch.Event
}

type debouncer struct {
	input        <-chan watch.Event
	output       chan Event
	stopChan     chan any
	stopOnce     sync.Once
	events       map[string]map[string]*item
	debounceTime time.Duration
	rwStop       func()
	mtx          sync.Mutex
}

func New(input <-chan watch.Event, rwStop func(), debounceTime string) (*debouncer, error) {
	dt, err := time.ParseDuration(debounceTime)
	if err != nil {
		log.Warn("failed to parse debounce time configuration, will use the default config", "value", DefaultTime, "err", err)
		dt, err = time.ParseDuration(DefaultTime)
		if err != nil {
			return nil, fmt.Errorf("failed to parse default debounce time value: %w", err)
		}
	}
	if dt < minimalTime {
		log.Warn("configured debounce time value is too low, will use minimal value of 200ms", "config value", dt.String())
		dt = minimalTime
	}
	return &debouncer{
		input:        input,
		output:       make(chan Event),
		stopChan:     make(chan any),
		events:       make(map[string]map[string]*item),
		debounceTime: dt,
		rwStop:       rwStop,
	}, nil
}

type item struct {
	input    chan watch.Event
	output   chan<- Event
	stopChan chan any
	stopOnce sync.Once
}

func newItem(output chan<- Event) *item {
	return &item{
		input:    make(chan watch.Event),
		output:   output,
		stopChan: make(chan any),
	}
}

const (
	DefaultTime = "2s"
	minimalTime = time.Millisecond * 200
)

func (d *debouncer) Start(ctx context.Context) {
	wg := sync.WaitGroup{}
	debouncerCtx, cancel := context.WithCancel(ctx)
	defer func() {
		d.rwStop()
		cancel()
		wg.Wait()
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
				continue
			}

			var namespace, name string

			switch v := tmp.Object.(type) {
			case *discoveryv1.EndpointSlice:
				namespace = v.Namespace
				name = v.Name
			case *v1.Endpoints: //nolint:staticcheck
				namespace = v.Namespace
				name = v.Name
			case *v1.Service:
				namespace = v.Namespace
				name = v.Name
			default:
				log.Error(fmt.Sprintf("objects of type %T are not supported", v))
				return
			}

			d.mtx.Lock()
			nsEvent, exists := d.events[namespace]
			if !exists {
				d.events[namespace] = make(map[string]*item)
				nsEvent = d.events[namespace]
			}
			d.mtx.Unlock()

			nameEvent, exists := nsEvent[name]
			if !exists {
				nsEvent[name] = newItem(d.output)
				nameEvent = nsEvent[name]

				wg.Go(func() {
					nameEvent.start(debouncerCtx, d.debounceTime)
					d.mtx.Lock()
					defer d.mtx.Unlock()
					delete(d.events[namespace], name)
					if len(d.events[namespace]) == 0 {
						delete(d.events, namespace)
					}
				})
			}
			nameEvent.input <- tmp

			if tmp.Type == watch.Deleted {
				nameEvent.stop()
			}
		}
	}
}

func (d *debouncer) Stop() {
	d.stopOnce.Do(func() {
		close(d.stopChan)
	})
}

func (d *debouncer) Output() chan Event {
	return d.output
}

func (i *item) start(ctx context.Context, debounceTime time.Duration) {
	t := time.NewTicker(debounceTime)

	var aggregated *Event

	defer func() {
		if aggregated != nil {
			i.output <- *aggregated
			aggregated = nil
		}
	}()

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
				aggregated = &Event{
					Type: tmp.Type,
				}
			}
			aggregated.Last = &tmp
			t.Reset(debounceTime)
		case <-t.C:
			if aggregated != nil {
				i.output <- *aggregated
				aggregated = nil
			}
		}
	}
}

func (i *item) stop() {
	i.stopOnce.Do(func() {
		close(i.stopChan)
	})
}
