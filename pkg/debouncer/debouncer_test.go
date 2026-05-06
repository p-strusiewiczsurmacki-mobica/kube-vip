package debouncer

import (
	"context"
	"sync"
	"testing"
	"time"

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

// func TestEventAggregation(t *testing.T) {
// 	t.Run("Run and stop the debouncer without issues", func(t *testing.T) {
// 		input := make(chan watch.Event)
// 		defer close(input)

// 		expected := "2s"

// 		fw := watch.NewFake()

// 		fw.ResultChan()
// 		fw.Add(&v1.Service{})

// 		rwStopped := false

// 		rwStop := func() {
// 			rwStopped = true
// 		}

// 		d, err := New(input, rwStop, expected)

// 		if err != nil {
// 			t.Fatalf("failed to create debouncer with debounce time %q", expected)
// 		}

// 		if d.debounceTime.String() != expected {
// 			t.Fatalf("invalid debounce time %q was configured instead of expected %q", d.debounceTime.String(), expected)
// 		}

// 		ctx, cancel := context.WithCancel(context.Background())

// 		wg := sync.WaitGroup{}

// 		wg.Go(func() {
// 			d.Start(ctx)
// 		})

// 		cancel()

// 		timedOut := waitTimeout(&wg, time.Second*3)

// 		if timedOut {
// 			t.Fatal("debouncer was not closed before timeout")
// 		}

// 		if !rwStopped {
// 			t.Fatal("retry watcher is note being stopped on the debouncer exit")
// 		}
// 	})
// }

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
