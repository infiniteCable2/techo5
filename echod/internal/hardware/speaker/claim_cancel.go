package speaker

import "time"

// CancelClaim revokes this transport's own playback without silencing a newer
// announcement, alarm or reply which may have taken the speaker meanwhile.
func (d *Driver) CancelClaim(expected *Claim) {
	if expected == nil || expected.Finished() {
		return
	}
	expected.mu.Lock()
	expected.stopped = true
	expected.mu.Unlock()
	expected.cancel()
	select {
	case <-expected.done:
	case <-time.After(time.Second):
	}
	d.mu.Lock()
	if d.now == nil || d.now == expected {
		d.p.Drain()
		if d.now == expected {
			d.now = nil
		}
	}
	d.mu.Unlock()
	d.settle()
}
