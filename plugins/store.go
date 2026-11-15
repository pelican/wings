package plugins

import (
	"strings"
	"time"

	"emperror.dev/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/pelican/wings/internal/models"
)

// maxStoreValueBytes caps a single stored value.
//
// The store shares one SQLite file with the activity log, and that file sits on
// the node's root volume. A plugin treating the store as a place to park
// archives would fill the volume out from under every server on the node, so a
// value that large is refused outright rather than written and regretted.
const maxStoreValueBytes = 1 << 20 // 1 MiB

// store is a plugin's private key/value storage. Every operation is scoped to
// one plugin id, so a plugin cannot read or overwrite another's rows.
type store struct {
	db       *gorm.DB
	pluginID string
}

// Get returns a stored value. The second result reports whether the key exists
// and has not expired.
func (s *store) Get(key string) ([]byte, bool, error) {
	if err := validateStoreKey(key); err != nil {
		return nil, false, err
	}

	var row models.PluginStore
	tx := s.db.Where("plugin = ? AND key = ?", s.pluginID, key).Take(&row)
	if errors.Is(tx.Error, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if tx.Error != nil {
		return nil, false, errors.Wrap(tx.Error, "plugins: failed to read from plugin store")
	}

	// Expiry is enforced here rather than by a background timer, so a value
	// that has passed its deadline is never handed back even if the sweep has
	// not run yet.
	if row.ExpiresAt != nil && row.ExpiresAt.Before(time.Now()) {
		return nil, false, nil
	}

	return row.Value, true, nil
}

// Set stores a value that does not expire.
func (s *store) Set(key string, value []byte) error {
	return s.set(key, value, nil)
}

// SetWithTTL stores a value Wings stops returning once ttl has elapsed.
func (s *store) SetWithTTL(key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("plugins: store ttl must be greater than zero")
	}
	expires := time.Now().Add(ttl)
	return s.set(key, value, &expires)
}

func (s *store) set(key string, value []byte, expires *time.Time) error {
	if err := validateStoreKey(key); err != nil {
		return err
	}
	if len(value) > maxStoreValueBytes {
		return errors.Errorf("plugins: store value for %q is %d bytes, which is over the %d byte limit", key, len(value), maxStoreValueBytes)
	}

	row := models.PluginStore{
		Plugin:    s.pluginID,
		Key:       key,
		Value:     value,
		ExpiresAt: expires,
	}

	// Upsert on the composite key so a plugin overwriting its own value does
	// not have to read first, which would race with itself.
	tx := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "plugin"}, {Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "expires_at", "updated_at"}),
	}).Create(&row)
	if tx.Error != nil {
		return errors.Wrap(tx.Error, "plugins: failed to write to plugin store")
	}
	return nil
}

// Delete removes a key. Deleting a key that is not there is not an error.
func (s *store) Delete(key string) error {
	if err := validateStoreKey(key); err != nil {
		return err
	}
	tx := s.db.Where("plugin = ? AND key = ?", s.pluginID, key).Delete(&models.PluginStore{})
	if tx.Error != nil {
		return errors.Wrap(tx.Error, "plugins: failed to delete from plugin store")
	}
	return nil
}

// Keys lists the plugin's keys carrying prefix, oldest first.
func (s *store) Keys(prefix string) ([]string, error) {
	q := s.db.Model(&models.PluginStore{}).
		Where("plugin = ?", s.pluginID).
		Where("expires_at IS NULL OR expires_at > ?", time.Now())

	if prefix != "" {
		// Escape the LIKE wildcards so a prefix containing % or _ matches
		// literally rather than acting as a pattern.
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
		q = q.Where(`key LIKE ? ESCAPE '\'`, escaped+"%")
	}

	var keys []string
	if tx := q.Order("created_at").Pluck("key", &keys); tx.Error != nil {
		return nil, errors.Wrap(tx.Error, "plugins: failed to list plugin store keys")
	}
	return keys, nil
}

// purge removes every row belonging to the plugin. Called when a plugin is
// uninstalled, so its storage does not outlive it.
func (s *store) purge() error {
	tx := s.db.Where("plugin = ?", s.pluginID).Delete(&models.PluginStore{})
	if tx.Error != nil {
		return errors.Wrap(tx.Error, "plugins: failed to purge plugin store")
	}
	return nil
}

// sweepExpiredStoreEntries deletes rows whose deadline has passed. Reads
// already ignore expired rows, so this only reclaims space.
func sweepExpiredStoreEntries(db *gorm.DB) error {
	tx := db.Where("expires_at IS NOT NULL AND expires_at < ?", time.Now()).Delete(&models.PluginStore{})
	if tx.Error != nil {
		return errors.Wrap(tx.Error, "plugins: failed to sweep expired plugin store entries")
	}
	return nil
}

func validateStoreKey(key string) error {
	if key == "" {
		return errors.New("plugins: store key cannot be empty")
	}
	if len(key) > 255 {
		return errors.New("plugins: store key cannot be longer than 255 characters")
	}
	return nil
}
