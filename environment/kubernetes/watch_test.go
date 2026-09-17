package kubernetes

import (
	"context"
	"testing"
	"time"

	. "github.com/franela/goblin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWatchUntil(t *testing.T) {
	g := Goblin(t)

	podGetter := func(client *fake.Clientset, name string) func(ctx context.Context) (runtime.Object, error) {
		return func(ctx context.Context) (runtime.Object, error) {
			return client.CoreV1().Pods("pelican").Get(ctx, name, metav1.GetOptions{})
		}
	}
	podWatcher := func(client *fake.Clientset, name string) func(ctx context.Context) (watch.Interface, error) {
		return func(ctx context.Context) (watch.Interface, error) {
			return client.CoreV1().Pods("pelican").Watch(ctx, metav1.ListOptions{
				FieldSelector: "metadata.name=" + name,
			})
		}
	}
	runningCheck := func(evt watch.EventType, obj runtime.Object) (bool, error) {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return false, nil
		}
		return pod.Status.Phase == corev1.PodRunning, nil
	}

	g.Describe("watchUntil", func() {
		g.It("returns without watching when the object is already terminal on the first get", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "done-pod", Namespace: "pelican"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}
			client := fake.NewClientset(pod)

			err := watchUntil(context.Background(), podGetter(client, "done-pod"), func(ctx context.Context) (watch.Interface, error) {
				g.Fail("watch should not be established")
				return nil, nil
			}, runningCheck)
			g.Assert(err).IsNil()
		})

		g.It("returns when a matching event arrives on the watch", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pending-pod", Namespace: "pelican"},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			}
			client := fake.NewClientset(pod)

			done := make(chan error, 1)
			go func() {
				done <- watchUntil(context.Background(), podGetter(client, "pending-pod"), podWatcher(client, "pending-pod"), runningCheck)
			}()

			// Give the watch a moment to establish, then flip the phase.
			time.Sleep(50 * time.Millisecond)
			pod.Status.Phase = corev1.PodRunning
			_, err := client.CoreV1().Pods("pelican").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
			g.Assert(err).IsNil()

			select {
			case err := <-done:
				g.Assert(err).IsNil()
			case <-time.After(5 * time.Second):
				g.Fail("watchUntil did not return after the event")
			}
		})

		g.It("returns ctx.Err() when the context is cancelled", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "stuck-pod", Namespace: "pelican"},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			}
			client := fake.NewClientset(pod)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- watchUntil(ctx, podGetter(client, "stuck-pod"), podWatcher(client, "stuck-pod"), runningCheck)
			}()

			time.Sleep(50 * time.Millisecond)
			cancel()

			select {
			case err := <-done:
				g.Assert(err).Equal(context.Canceled)
			case <-time.After(5 * time.Second):
				g.Fail("watchUntil did not return after context cancellation")
			}
		})
	})
}
