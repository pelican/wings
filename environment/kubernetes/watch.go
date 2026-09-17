package kubernetes

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

// watchUntil calls get, then watches via watchFn, invoking check on the
// current object and on every subsequent event until check reports done,
// the watch is closed (in which case it is re-established), or ctx ends.
// A nil object with watch.Deleted is passed to check when the object is
// deleted. Returns ctx.Err() when ctx ends.
func watchUntil(
	ctx context.Context,
	get func(ctx context.Context) (runtime.Object, error),
	watchFn func(ctx context.Context) (watch.Interface, error),
	check func(evt watch.EventType, obj runtime.Object) (done bool, err error),
) error {
	for {
		obj, err := get(ctx)
		if err != nil {
			return err
		}
		if done, err := check(watch.Modified, obj); done {
			return err
		}

		w, err := watchFn(ctx)
		if err != nil {
			return err
		}

		closed := false
		for !closed {
			select {
			case <-ctx.Done():
				w.Stop()
				return ctx.Err()
			case event, ok := <-w.ResultChan():
				if !ok {
					// Watch closed by the apiserver; re-establish it.
					w.Stop()
					closed = true
					continue
				}
				if done, err := check(event.Type, event.Object); done {
					w.Stop()
					return err
				}
			}
		}
	}
}
