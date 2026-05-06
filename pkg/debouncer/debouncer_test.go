package debouncer

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestTimeSetting(t *testing.T) {
	tcs := []struct {
		name       string
		configured string
		expected   string
	}{
		{
			name:       "configured proper value 10s",
			configured: "10s",
			expected:   "10s",
		},
		{
			name:       "configured value less than 200ms",
			configured: "0s",
			expected:   minimalTime.String(),
		},
		{
			name:       "configured proper value 1s",
			configured: "1s",
			expected:   "1s",
		},
		{
			name:       "configured proper value 1500ms",
			configured: "1500ms",
			expected:   "1.5s",
		},
		{
			name:       "configured invalid value that cannot be parsed",
			configured: "invalid",
			expected:   DefaultTime,
		},
		{
			name:       "configured negative value",
			configured: "-1s",
			expected:   minimalTime.String(),
		},
	}

	input := make(chan watch.Event)
	defer close(input)

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			d, err := New(input, func() {}, tc.configured)

			if err != nil {
				t.Fatalf("failed to create debouncer with debounce time %q", tc.configured)
			}

			if d.debounceTime.String() != tc.expected {
				t.Fatalf("invalid debounce time %q was configured instead of expected %q", d.debounceTime.String(), tc.expected)
			}
		})
	}
}

func TestStartStop(t *testing.T) {
	t.Run("Run and stop the debouncer without issues", func(t *testing.T) {
		input := make(chan watch.Event)
		defer close(input)

		expected := "200ms"

		rwStopped := false

		rwStop := func() {
			rwStopped = true
		}

		d, err := New(input, rwStop, expected)

		if err != nil {
			t.Fatalf("failed to create debouncer with debounce time %q", expected)
		}

		if d.debounceTime.String() != expected {
			t.Fatalf("invalid debounce time %q was configured instead of expected %q", d.debounceTime.String(), expected)
		}

		ctx, cancel := context.WithCancel(context.Background())

		wg := sync.WaitGroup{}

		wg.Go(func() {
			d.Start(ctx)
		})

		cancel()

		timedOut := waitTimeout(&wg, time.Second*3)

		if timedOut {
			t.Fatal("debouncer was not closed before timeout")
		}

		if !rwStopped {
			t.Fatal("retry watcher is note being stopped on the debouncer exit")
		}
	})
}

func TestDebouncing(t *testing.T) {

	tcs := []string{"endpointslices", "endpoints"}

	for _, tc := range tcs {
		t.Run(fmt.Sprintf("Get the newest event as the only one when using %s", tc), func(t *testing.T) {
			// input := make(chan watch.Event)
			// defer close(input)

			expected := "2s"

			fw := watch.NewFake()

			input := fw.ResultChan()

			rwStopped := false

			rwStop := func() {
				rwStopped = true
			}

			d, err := New(input, rwStop, expected)

			if err != nil {
				t.Fatalf("failed to create debouncer with debounce time %q", expected)
			}

			if d.debounceTime.String() != expected {
				t.Fatalf("invalid debounce time %q was configured instead of expected %q", d.debounceTime.String(), expected)
			}

			ctx, cancel := context.WithCancel(context.Background())

			wg := sync.WaitGroup{}

			wg.Go(func() {
				d.Start(ctx)
			})

			addrEpslices := []string{}
			var epslice *discoveryv1.EndpointSlice

			addrEp := []v1.EndpointAddress{}
			var ep *v1.Endpoints

			numberOfEndpoints := 100

			if tc == "endpointslices" {

				epslice = &discoveryv1.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
						OwnerReferences: []metav1.OwnerReference{
							{
								Name: "test-svc",
							},
						},
					},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: addrEpslices,
						},
					},
				}

				for i := range numberOfEndpoints {
					addrEpslices = append(addrEpslices, strconv.Itoa(i))
					epslice.Endpoints[0].Addresses = addrEpslices
					fw.Add(epslice)
				}
			} else {
				ep = &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
						OwnerReferences: []metav1.OwnerReference{
							{
								Name: "test-svc",
							},
						},
					},
					Subsets: []v1.EndpointSubset{
						v1.EndpointSubset{
							Addresses: addrEp,
						},
					},
				}

				for i := range numberOfEndpoints {
					addrEp = append(addrEp, v1.EndpointAddress{IP: strconv.Itoa(i)})
					ep.Subsets[0].Addresses = addrEp
					fw.Add(ep)
				}
			}

			out := <-d.output
			// t.Fatal("OUT", out)

			if tc == "endpointslices" {
				outEps, ok := out.Last.Object.(*discoveryv1.EndpointSlice)
				if !ok {
					t.Fatal("got different type of object than EndpointSlice, failed to cast")
				}

				if len(outEps.Endpoints[0].Addresses) != numberOfEndpoints {
					t.Fatalf("expected to aggregate %d events, but got %d", numberOfEndpoints, len(outEps.Endpoints[0].Addresses))
				}
			} else {
				outEps, ok := out.Last.Object.(*v1.Endpoints)
				if !ok {
					t.Fatal("got different type of object than EndpointSlice, failed to cast")
				}

				if len(outEps.Subsets[0].Addresses) != numberOfEndpoints {
					t.Fatalf("expected to aggregate %d events, but got %d", numberOfEndpoints, len(outEps.Subsets[0].Addresses))
				}
			}

			cancel()

			timedOut := waitTimeout(&wg, time.Second*3)

			if timedOut {
				t.Fatal("debouncer was not closed before timeout")
			}

			if !rwStopped {
				t.Fatal("retry watcher is note being stopped on the debouncer exit")
			}
		})
	}
}

// waitTimeout waits for the waitgroup for the specified max timeout.
// Returns true if waiting timed out.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		return false // completed normally
	case <-time.After(timeout):
		return true // timed out
	}
}
