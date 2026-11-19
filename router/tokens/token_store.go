package tokens

import (
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
)

type TokenStore struct {
	sync.Mutex
	cache *cache.Cache
}

var (
	_tokens     *TokenStore
	_tokensOnce sync.Once
)

// Returns the global unique token store cache. This is used to validate
// one time token usage by storing any received tokens in a local memory
// cache until they are ready to expire.
func getTokenStore() *TokenStore {
	_tokensOnce.Do(func() {
		_tokens = &TokenStore{
			cache: cache.New(time.Minute*60, time.Minute*5),
		}
	})

	return _tokens
}

// Checks if a token is valid or not. A token is only valid the first time it
// is seen; adding it fails if it is already stored, so two concurrent requests
// with the same token cannot both be accepted.
func (t *TokenStore) IsValidToken(token string) bool {
	t.Lock()
	defer t.Unlock()

	return t.cache.Add(token, "", time.Minute*60) == nil
}
