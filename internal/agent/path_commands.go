package agent

import (
	"errors"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
)

// Commands remain unacknowledged until the mount change finishes. Keeping the
// suffix in arrival order prevents a cancellation from overtaking a queued run.
type pathCommandQueue struct{ pending []*xirangv1.ControlMessage }

func startsWork(message *xirangv1.ControlMessage) bool {
	return message.GetRunAssignment() != nil || message.GetRunNativeBuild() != nil || message.GetOpenTerminal() != nil
}

func (queue *pathCommandQueue) dispatch(message *xirangv1.ControlMessage, changing bool, apply func(*xirangv1.ControlMessage) error) error {
	if len(queue.pending) == 0 && !(changing && startsWork(message)) {
		return apply(message)
	}
	if len(queue.pending) >= 128 {
		return errors.New("pending shared path command limit reached")
	}
	queue.pending = append(queue.pending, message)
	return nil
}

func (queue *pathCommandQueue) drain(changing func() bool, apply func(*xirangv1.ControlMessage) error) error {
	for len(queue.pending) > 0 {
		// A completed-operation replay can arrive while another mount runs.
		// A queued command may also start the next path change.
		if changing() {
			return nil
		}
		if err := apply(queue.pending[0]); err != nil {
			return err
		}
		queue.pending = queue.pending[1:]
	}
	return nil
}
