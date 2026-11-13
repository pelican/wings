package models

import "time"

// PluginStore is one key/value pair belonging to a Wings plugin.
//
// Plugins get durable storage so they can keep state that has to outlive a
// restart without inventing their own file format next to their source. Rows
// are keyed by plugin as well as by key, so two plugins can both store "state"
// without colliding, and uninstalling a plugin can discard exactly its rows.
type PluginStore struct {
	// Plugin is the id of the plugin that owns this row.
	Plugin string `gorm:"primaryKey;size:64;not null" json:"plugin"`

	// Key is the plugin's own name for the value.
	Key string `gorm:"primaryKey;size:255;not null" json:"key"`

	// Value is opaque to Wings. Plugins encode whatever they like into it.
	Value []byte `json:"value"`

	// ExpiresAt marks a value that should be treated as absent once passed,
	// for plugins using the store as a cache. A null value never expires.
	//
	// Expiry is applied on read and swept periodically rather than by a timer
	// per row, so a value can briefly outlive its deadline on disk. It is
	// never returned to a plugin after expiring.
	ExpiresAt *time.Time `gorm:"index" json:"expires_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is set explicitly so the table does not depend on how gorm happens
// to pluralise the struct name.
func (PluginStore) TableName() string {
	return "plugin_store"
}
