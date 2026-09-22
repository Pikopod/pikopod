package agent

import (
	"testing"
	"time"
)

func startPipeline(t *testing.T, a *Agent) {
	t.Helper()
	go a.Recorder.Run(a.Proxy.Captures())
	t.Cleanup(func() {
		a.Proxy.Close()
		quiesce(a)
		a.Alerter.Close()
	})
}

func quiesce(a *Agent) {
	select {
	case <-a.Recorder.Done():
	case <-time.After(2 * time.Second):
	}
}
