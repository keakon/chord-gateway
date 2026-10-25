package im

import (
	"os"
	"sync"
	"sync/atomic"
)

// MaxGatewayImageBytes bounds one received image file.
const MaxGatewayImageBytes = 32 << 20

// ImageSpoolBytes is the process-wide received-image spool budget shared by every
// receiver. Tests that inspect or replace its value must remain serial to avoid
// interfering with other receivers.
var ImageSpoolBytes atomic.Int64

// ReceivedImage owns one validated temporary original until Close is called.
type ReceivedImage struct {
	BindingInstanceID                                         string
	Path, Reference, CallID, AgentID, SessionID, MIME, SHA256 string
	Size                                                      int64
	Width, Height                                             int
	closeOnce                                                 sync.Once
}

func (i *ReceivedImage) Close() {
	i.closeOnce.Do(func() { _ = os.Remove(i.Path); ImageSpoolBytes.Add(-i.Size) })
}
