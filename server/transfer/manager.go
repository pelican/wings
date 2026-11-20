package transfer

import (
	"sync"
)

var (
	incomingTransfers = NewManager()
	outgoingTransfers = NewManager()
)

// Incoming returns a transfer manager for incoming transfers.
func Incoming() *Manager {
	return incomingTransfers
}

// Outgoing returns a transfer manager for outgoing transfers.
func Outgoing() *Manager {
	return outgoingTransfers
}

// Manager manages transfers.
type Manager struct {
	mu        sync.RWMutex
	transfers map[string]*Transfer
}

// NewManager returns a new transfer manager.
func NewManager() *Manager {
	return &Manager{
		transfers: make(map[string]*Transfer),
	}
}

// Add adds a transfer to the manager.
func (m *Manager) Add(transfer *Transfer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.transfers[transfer.Server.ID()] = transfer
}

// Remove removes a transfer from the manager. Nothing is removed if another
// transfer of the same server has replaced it since, so that a transfer that
// finishes late cannot remove the entry of a newer one.
func (m *Manager) Remove(transfer *Transfer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := transfer.Server.ID()
	if m.transfers[id] == transfer {
		delete(m.transfers, id)
	}
}

// Get gets a transfer from the manager using a server ID.
func (m *Manager) Get(id string) *Transfer {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.transfers[id]
}
