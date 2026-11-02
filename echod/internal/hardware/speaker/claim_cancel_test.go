package speaker

import (
	"context"
	"testing"
)

func TestCancelClaimLeavesNewerOwnerRunning(t *testing.T) {
	d := driver()
	old := d.Claim("old reply", func(ctx context.Context, _ *Player) error { <-ctx.Done(); return nil })
	current := d.Claim("announcement", func(ctx context.Context, _ *Player) error { <-ctx.Done(); return nil })
	t.Cleanup(d.Silence)
	d.CancelClaim(old)
	if current.Stopped() || d.Holder() != "announcement" {
		t.Fatal("canceling old output stopped the current owner")
	}
	d.CancelClaim(current)
	waitFor(t, current)
	if !current.Stopped() || d.Busy() {
		t.Fatal("current claim was not revoked")
	}
}
