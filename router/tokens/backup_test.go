package tokens

import (
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
)

// The Panel issues backup download tokens without a user for one node to
// download a backup from another. Those are accepted, unless they were issued
// before Wings booted or have no server.
func TestBackupPayloadWithoutUser(t *testing.T) {
	bootTime := time.Now()
	resetDenylistState(t, bootTime.Add(-time.Minute))

	tests := map[string]struct {
		payload BackupPayload
		denied  bool
	}{
		"issued after boot": {
			payload: BackupPayload{Payload: jwt.Payload{IssuedAt: jwt.NumericDate(bootTime)}, ServerUuid: "server"},
		},
		"issued before boot": {
			payload: BackupPayload{Payload: jwt.Payload{IssuedAt: jwt.NumericDate(bootTime.Add(-time.Hour))}, ServerUuid: "server"},
			denied:  true,
		},
		"missing issued at": {
			payload: BackupPayload{ServerUuid: "server"},
			denied:  true,
		},
		"missing server": {
			payload: BackupPayload{Payload: jwt.Payload{IssuedAt: jwt.NumericDate(bootTime)}},
			denied:  true,
		},
	}

	for name, test := range tests {
		if got := test.payload.Denylisted(); got != test.denied {
			t.Errorf("%s: expected denied to be %v, got %v", name, test.denied, got)
		}
	}
}

// Other user-bound tokens still need a user.
func TestFilePayloadWithoutUserIsDenied(t *testing.T) {
	bootTime := time.Now()
	resetDenylistState(t, bootTime.Add(-time.Minute))

	p := FilePayload{Payload: jwt.Payload{IssuedAt: jwt.NumericDate(bootTime)}, ServerUuid: "server"}
	if !p.Denylisted() {
		t.Fatal("expected a file download token without a user to be denied")
	}
}
