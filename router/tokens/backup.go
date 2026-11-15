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
//
// The Panel issues tokens without a user when one node downloads a backup from
// another, to restore it into a new server. Those are not tied to a user's
// access, so only the time they were issued is checked.
func (p *BackupPayload) Denylisted() bool {
	if p.UserUuid == "" {
		return p.IssuedAt == nil || p.ServerUuid == "" || p.IssuedAt.Time.Before(wingsBootTime)
	}
	return isDenylisted(&p.Payload, p.ServerUuid, p.UserUuid)
}