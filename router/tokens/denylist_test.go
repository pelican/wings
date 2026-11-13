package tokens

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
)

type denylistable interface {
	Denylisted() bool
}

func resetDenylistState(t *testing.T, bootTime time.Time) {
	t.Helper()

	originalBootTime := wingsBootTime
	wingsBootTime = bootTime
	denylist.Clear()
	userDenylist.Clear()

	t.Cleanup(func() {
		wingsBootTime = originalBootTime
		denylist.Clear()
		userDenylist.Clear()
	})
}

func payloadsIssuedAt(issuedAt time.Time, serverUUID, userUUID string) map[string]denylistable {
	payload := func() jwt.Payload {
		return jwt.Payload{
			IssuedAt: jwt.NumericDate(issuedAt),
			JWTID:    "token-id",
		}
	}

	return map[string]denylistable{
		"websocket": &WebsocketPayload{
			Payload:    payload(),
			ServerUUID: serverUUID,
			UserUUID:   userUUID,
		},
		"file download": &FilePayload{
			Payload:    payload(),
			ServerUuid: serverUUID,
			UserUuid:   userUUID,
		},
		"backup download": &BackupPayload{
			Payload:    payload(),
			ServerUuid: serverUUID,
			UserUuid:   userUUID,
		},
		"file upload": &UploadPayload{
			Payload:    payload(),
			ServerUuid: serverUUID,
			UserUuid:   userUUID,
		},
	}
}

func TestUserBoundPayloadsHonorServerRevocation(t *testing.T) {
	issuedAt := time.Now().Add(-time.Minute)
	resetDenylistState(t, issuedAt.Add(-time.Minute))

	payloads := payloadsIssuedAt(issuedAt, "server", "user")
	for name, payload := range payloads {
		if payload.Denylisted() {
			t.Fatalf("expected fresh %s token to be accepted", name)
		}
	}

	DenyForServer("server", "user")

	for name, payload := range payloads {
		if !payload.Denylisted() {
			t.Fatalf("expected revoked %s token to be denied", name)
		}
	}
}

func TestUserBoundPayloadsAllowTokensIssuedAfterRevocation(t *testing.T) {
	revokedAt := time.Now()
	resetDenylistState(t, revokedAt.Add(-time.Hour))
	userDenylist.Store("server:user", revokedAt)

	for name, payload := range payloadsIssuedAt(revokedAt, "server", "user") {
		if !payload.Denylisted() {
			t.Fatalf("expected %s token issued at revocation time to be denied", name)
		}
	}

	for name, payload := range payloadsIssuedAt(revokedAt.Add(time.Second), "server", "user") {
		if payload.Denylisted() {
			t.Fatalf("expected reissued %s token to be accepted", name)
		}
	}
}

func TestDownloadPayloadsDecodeUserUUID(t *testing.T) {
	const encoded = `{"server_uuid":"server","user_uuid":"user"}`

	var file FilePayload
	if err := json.Unmarshal([]byte(encoded), &file); err != nil {
		t.Fatal(err)
	}
	if file.UserUuid != "user" {
		t.Fatalf("expected file payload user UUID to be decoded, got %q", file.UserUuid)
	}

	var backup BackupPayload
	if err := json.Unmarshal([]byte(encoded), &backup); err != nil {
		t.Fatal(err)
	}
	if backup.UserUuid != "user" {
		t.Fatalf("expected backup payload user UUID to be decoded, got %q", backup.UserUuid)
	}
}

func TestUserBoundPayloadsFailClosedWithoutRevocationClaims(t *testing.T) {
	issuedAt := time.Now()
	resetDenylistState(t, issuedAt.Add(-time.Hour))

	tests := map[string]denylistable{
		"missing issued at": &FilePayload{
			ServerUuid: "server",
			UserUuid:   "user",
		},
		"missing server UUID": &BackupPayload{
			Payload:  jwt.Payload{IssuedAt: jwt.NumericDate(issuedAt)},
			UserUuid: "user",
		},
		"missing user UUID": &UploadPayload{
			Payload:    jwt.Payload{IssuedAt: jwt.NumericDate(issuedAt)},
			ServerUuid: "server",
		},
	}

	for name, payload := range tests {
		if !payload.Denylisted() {
			t.Fatalf("expected token with %s to be denied", name)
		}
	}
}

func TestUserBoundPayloadsRejectTokensIssuedBeforeBoot(t *testing.T) {
	bootTime := time.Now()
	resetDenylistState(t, bootTime)

	for name, payload := range payloadsIssuedAt(bootTime.Add(-time.Second), "server", "user") {
		if !payload.Denylisted() {
			t.Fatalf("expected pre-boot %s token to be denied", name)
		}
	}
}

func TestUserRevocationIsScopedToServerAndUser(t *testing.T) {
	issuedAt := time.Now().Add(-time.Minute)
	resetDenylistState(t, issuedAt.Add(-time.Minute))
	DenyForServer("server", "user")

	for name, payload := range payloadsIssuedAt(issuedAt, "other-server", "user") {
		if payload.Denylisted() {
			t.Fatalf("expected %s token for another server to be accepted", name)
		}
	}

	for name, payload := range payloadsIssuedAt(issuedAt, "server", "other-user") {
		if payload.Denylisted() {
			t.Fatalf("expected %s token for another user to be accepted", name)
		}
	}
}
