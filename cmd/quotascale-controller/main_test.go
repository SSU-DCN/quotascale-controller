package main

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestMergeWatchEventsForwardsBothReasons(t *testing.T) {
	failedCreate := watch.NewFake()
	failedScheduling := watch.NewFake()
	merged := mergeWatchEvents(failedCreate, failedScheduling)

	go func() {
		failedCreate.Add(&v1.Event{Reason: "FailedCreate"})
		failedScheduling.Add(&v1.Event{Reason: "FailedScheduling"})
		failedCreate.Stop()
		failedScheduling.Stop()
	}()

	reasons := map[string]bool{}
	for event := range merged {
		reasons[event.Object.(*v1.Event).Reason] = true
	}
	if !reasons["FailedCreate"] || !reasons["FailedScheduling"] {
		t.Fatalf("expected both event reasons, got %#v", reasons)
	}
}
