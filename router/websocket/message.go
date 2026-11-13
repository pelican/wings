package websocket

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
)

type Event string

const (
	AuthenticationSuccessEvent = "auth success"
	TokenExpiringEvent         = "token expiring"
	TokenExpiredEvent          = "token expired"
	AuthenticationEvent        = "auth"
	SetStateEvent              = "set state"
	SendServerLogsEvent        = "send logs"
	SendCommandEvent           = "send command"
	SendStatsEvent             = "send stats"
	ErrorEvent                 = "daemon error"
	JwtErrorEvent              = "jwt error"
	ThrottledEvent             = Event("throttled")
)

type Message struct {
	// The event to perform.
	Event Event `json:"event"`

	// The data to pass along, only used by power/command currently. Other requests
	// should either omit the field or pass an empty value as it is ignored.
	Args []string `json:"args,omitempty"`
}

// messageDecodeOptions makes encoding/json/v2 accept exactly what
// encoding/json v1 accepted from clients: the last of any duplicate keys wins,
// invalid UTF-8 is replaced with U+FFFD instead of rejected, and field names
// match without regard to case.
var messageDecodeOptions = jsonv2.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
	jsonv2.MatchCaseInsensitiveNames(true),
)

// DecodeMessage decodes an inbound websocket frame into a Message.
func DecodeMessage(p []byte) (Message, error) {
	var m Message
	err := jsonv2.Unmarshal(p, &m, messageDecodeOptions)
	return m, err
}
