package tokens

import (
	"github.com/gbrlsnchs/jwt/v3"
)

type BackupPayload struct {
	jwt.Payload

	ServerUuid string `json:"server_uuid"`
	UserUuid   string `json:"user_uuid"`
	BackupUuid string `json:"backup_uuid"`
	UniqueId   string `json:"unique_id"`
	Scoped
}

// Returns the JWT payload.
func (p *BackupPayload) GetPayload() *jwt.Payload {
	return &p.Payload
}

// Denylisted returns true if this token was issued before the user's access to
// the server was revoked.
func (p *BackupPayload) Denylisted() bool {
	return isDenylisted(&p.Payload, p.ServerUuid, p.UserUuid)
}