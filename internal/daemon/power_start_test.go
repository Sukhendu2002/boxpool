//go:build linux || darwin

package daemon

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type blockedBoot struct {
	*fakeDriver
	entered chan struct{}
	release chan struct{}
	fail    bool
}

func (d *blockedBoot) Start(ctx context.Context, id string) error {
	return d.wait(ctx, id, d.fakeDriver.Start)
}

func (d *blockedBoot) Resume(ctx context.Context, id string) error {
	return d.wait(ctx, id, d.fakeDriver.Resume)
}

func (d *blockedBoot) wait(ctx context.Context, id string, boot func(context.Context, string) error) error {
	if err := boot(ctx, id); err != nil {
		return err
	}
	close(d.entered)
	<-d.release // the VM is running, but the guest agent is not ready yet
	if d.fail {
		d.fakeDriver.Stop(ctx, id)
		return errors.New("guest readiness failed")
	}
	return nil
}

func TestSleepAssertionBeforeBoot(t *testing.T) {
	for _, action := range []string{"create", "start", "resume"} {
		for _, fail := range []bool{false, true} {
			name := action
			if fail {
				name += "/failure"
			}
			t.Run(name, func(t *testing.T) {
				srv, s, d := poolMachine(t, "darwin")
				if action != "create" {
					if code, _, body := post(t, srv, `{"name":"box"}`); code != http.StatusCreated {
						t.Fatal(body)
					}
					stop := "stop"
					if action == "resume" {
						stop = "pause"
					}
					if code, body := call(t, srv, "POST", "/v1/sandboxes/box/"+stop); code != http.StatusOK {
						t.Fatal(body)
					}
				}
				slow := &blockedBoot{fakeDriver: d, entered: make(chan struct{}), release: make(chan struct{}), fail: fail}
				s.d = slow
				var acquired, released atomic.Int32
				s.preventSleep = func() (func() error, error) {
					acquired.Add(1)
					return func() error { released.Add(1); return nil }, nil
				}
				result := make(chan int, 1)
				go func() {
					if action == "create" {
						code, _, _ := post(t, srv, `{"name":"box"}`)
						result <- code
					} else {
						code, _ := call(t, srv, "POST", "/v1/sandboxes/box/"+action)
						result <- code
					}
				}()
				select {
				case <-slow.entered:
					if acquired.Load() != 1 || released.Load() != 0 {
						t.Error("boot is waiting for guest readiness without an assertion")
					}
				case <-time.After(5 * time.Second):
					t.Error("boot did not start")
				}
				close(slow.release)
				want := http.StatusOK
				if action == "create" {
					want = http.StatusCreated
				}
				if fail {
					want = http.StatusInternalServerError
				}
				select {
				case code := <-result:
					if code != want {
						t.Fatalf("boot returned %d, want %d", code, want)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("boot did not finish")
				}
				// Synchronize with the handler's deferred refresh after its response.
				s.refreshSleep(context.Background())
				if !fail {
					call(t, srv, "POST", "/v1/sandboxes/box/stop")
					s.refreshSleep(context.Background())
				}
				if acquired.Load() != 1 || released.Load() != 1 {
					t.Errorf("assertions acquired %d, released %d", acquired.Load(), released.Load())
				}
			})
		}
	}
}
