package kubernetes

import (
	"context"
	"testing"
	"time"

	. "github.com/franela/goblin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/events"
	"github.com/pelican/wings/system"
)

func TestWaitForStop(t *testing.T) {
	g := Goblin(t)

	config.Update(func(c *config.Configuration) {
		c.Kubernetes.Namespace = "pelican"
	})

	newEnv := func(state string, objects ...runtime.Object) *Environment {
		return &Environment{
			Id:      "test-uuid",
			meta:    &Metadata{},
			client:  fake.NewClientset(objects...),
			st:      system.NewAtomicString(state),
			emitter: events.NewBus(),
		}
	}

	g.Describe("WaitForStop when the server is already offline", func() {
		g.It("returns immediately when the Pod does not exist", func() {
			env := newEnv(environment.ProcessOfflineState)

			done := make(chan error, 1)
			go func() {
				// A long duration would previously block here for the full period.
				done <- env.WaitForStop(context.Background(), time.Minute, true)
			}()

			select {
			case err := <-done:
				g.Assert(err).IsNil()
			case <-time.After(2 * time.Second):
				g.Fail("WaitForStop blocked when the Pod was absent")
			}
		})

		g.It("returns immediately when the Pod exists but is not running", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-uuid", Namespace: "pelican"},
				Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
			}
			// Start from a non-offline state to prove the helper drives it offline.
			env := newEnv(environment.ProcessRunningState, pod)

			done := make(chan error, 1)
			go func() {
				done <- env.WaitForStop(context.Background(), time.Minute, true)
			}()

			select {
			case err := <-done:
				g.Assert(err).IsNil()
				g.Assert(env.State()).Equal(environment.ProcessOfflineState)
			case <-time.After(2 * time.Second):
				g.Fail("WaitForStop blocked when the Pod was not running")
			}
		})
	})
}
