// Package session manages the set of active Java Edition play sessions.
// Each connected player in the Play state has one Session entry; adapters for
// other editions (Bedrock, future) will have their own equivalent registries.
package session

import (
	"sync"

	"GoCraft/core/player"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
)

// Session ties a canonical player (from core/) to their live Java connection.
// It is created when the player enters the Play state and removed on disconnect.
type Session struct {
	Player *player.Player
	Conn   *network.ClientConn
}

// Manager is a concurrent-safe registry of active Java play sessions.
// All methods are safe for concurrent use across multiple goroutines.
type Manager struct {
	mu                    sync.RWMutex
	sessions              map[[16]byte]*Session
	external              map[[16]byte]*player.Player
	messageObserver       func(string)
	externalKnockbackFunc func(*player.Player, float64, float64, float64, float64)
}

// NewManager returns an empty, ready-to-use Manager.
func NewManager() *Manager {
	return &Manager{
		sessions: make(map[[16]byte]*Session),
		external: make(map[[16]byte]*player.Player),
	}
}

// ReplaceExternalPlayers publishes the canonical players owned by other
// edition adapters. They are discoverable for entity interaction/combat but
// are deliberately excluded from Java packet broadcasts because they have no
// Java connection.
func (m *Manager) ReplaceExternalPlayers(players []*player.Player) {
	external := make(map[[16]byte]*player.Player, len(players))
	for _, p := range players {
		if p != nil {
			external[p.UUID] = p
		}
	}
	m.mu.Lock()
	m.external = external
	m.mu.Unlock()
}

// PlayerSessionByEntityID resolves both native Java sessions and canonical
// cross-edition players. External players are returned in a lightweight
// Session with Conn == nil, which the health code already supports.
func (m *Manager) PlayerSessionByEntityID(entityID int32) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, candidate := range m.sessions {
		if candidate.Player != nil && candidate.Player.EntityID == entityID {
			return candidate
		}
	}
	for _, p := range m.external {
		if p.EntityID == entityID {
			return &Session{Player: p}
		}
	}
	return nil
}

// SetMessageObserver installs the bridge used to mirror Java-origin system and
// chat messages into other edition adapters.
func (m *Manager) SetMessageObserver(observer func(string)) {
	m.mu.Lock()
	m.messageObserver = observer
	m.mu.Unlock()
}

func (m *Manager) ObserveMessage(message string) {
	m.mu.RLock()
	observer := m.messageObserver
	m.mu.RUnlock()
	if observer != nil {
		observer(message)
	}
}

// SetExternalKnockbackHandler bridges Java attacks against players whose
// network connection belongs to another adapter.
func (m *Manager) SetExternalKnockbackHandler(fn func(*player.Player, float64, float64, float64, float64)) {
	m.mu.Lock()
	m.externalKnockbackFunc = fn
	m.mu.Unlock()
}

func (m *Manager) KnockbackExternal(p *player.Player, sourceX, sourceZ, horizontal, vertical float64) {
	m.mu.RLock()
	fn := m.externalKnockbackFunc
	m.mu.RUnlock()
	if fn != nil {
		fn(p, sourceX, sourceZ, horizontal, vertical)
	}
}

// Add registers s in the manager. Replaces any existing session for the same UUID.
func (m *Manager) Add(s *Session) {
	m.mu.Lock()
	m.sessions[s.Player.UUID] = s
	m.mu.Unlock()
}

// Remove deregisters the session identified by uuid.
func (m *Manager) Remove(uuid [16]byte) {
	m.mu.Lock()
	delete(m.sessions, uuid)
	m.mu.Unlock()
}

// Get returns the session for uuid, or (nil, false) if not present.
func (m *Manager) Get(uuid [16]byte) (*Session, bool) {
	m.mu.RLock()
	s, ok := m.sessions[uuid]
	m.mu.RUnlock()
	return s, ok
}

// ForEachExcept calls fn for every session except the one with excludeUUID.
// The read lock is held for the duration; fn must not call Add or Remove.
func (m *Manager) ForEachExcept(excludeUUID [16]byte, fn func(*Session)) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for uuid, s := range m.sessions {
		if uuid != excludeUUID {
			fn(s)
		}
	}
}

// ForEach calls fn for every session in the manager.
// The read lock is held for the duration; fn must not call Add or Remove.
func (m *Manager) ForEach(fn func(*Session)) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		fn(s)
	}
}

// Snapshot returns a slice of all sessions except excludeUUID, captured under
// the read lock. The caller may iterate the slice and write to connections
// without holding any lock.
func (m *Manager) Snapshot(excludeUUID [16]byte) []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for uuid, s := range m.sessions {
		if uuid != excludeUUID {
			out = append(out, s)
		}
	}
	return out
}

// SnapshotAll returns a slice of all sessions, captured under the read lock.
// The caller may iterate the slice and write to connections without holding
// any lock.
func (m *Manager) SnapshotAll() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// BroadcastExcept writes pkt to every session except excludeUUID.
// It snapshots the session list first so the lock is not held during writes.
// Write errors are silently ignored; each connection's own goroutine handles
// cleanup when the next read or write fails.
func (m *Manager) BroadcastExcept(excludeUUID [16]byte, pkt *protocol.Packet) {
	for _, s := range m.Snapshot(excludeUUID) {
		_ = s.Conn.WritePacket(pkt)
	}
}
