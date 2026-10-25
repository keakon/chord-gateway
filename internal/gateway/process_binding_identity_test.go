package gateway

import "testing"

func BenchmarkProcessBindingIdentity(b *testing.B) {
	p := &ChordProcess{state: ControlState{BindingInstanceID: "binding", InputQueue: &InputQueue{Inputs: make([]InputQueueItem, 128)}}}
	for _, snapshot := range []bool{false, true} {
		name := "identity"
		if snapshot {
			name = "snapshot"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var identity string
				if snapshot {
					identity = p.State().BindingInstanceID
				} else {
					identity = p.BindingInstanceID()
				}
				if identity != "binding" {
					b.Fatal("wrong binding identity")
				}
			}
		})
	}
}
