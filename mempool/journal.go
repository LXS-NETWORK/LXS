package mempool

import (
	"bufio"
	"encoding/json"
	"os"

	"lxs/types"
)

// Local-transaction journal. A user's own tx (submitted through this node's RPC or
// faucet) is written to disk so a process restart does not silently drop it —
// exactly the failure we hit live when restarting the seed emptied its mempool and
// pending transfers vanished. Gossiped txs are deliberately NOT journaled: they
// survive on the node that originated them, and journaling every relayed tx would
// turn the file into the whole network's backlog. This mirrors go-ethereum's
// local-tx journal. The journal never touches consensus — replaying it on startup
// is identical to the user re-submitting, and every tx is re-validated by Add.

// EnableJournal points the pool at a disk journal and replays it. Call once at
// startup, before the node accepts new txs. Returns how many journaled txs were
// re-admitted (already-mined or now-invalid ones are dropped harmlessly).
func (m *Mempool) EnableJournal(path string, chainID uint64) (loaded int) {
	m.mu.Lock()
	m.journalPath = path
	m.mu.Unlock()

	if f, err := os.Open(path); err == nil {
		var txs []*types.Transaction
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			var tx types.Transaction
			if len(sc.Bytes()) > 0 && json.Unmarshal(sc.Bytes(), &tx) == nil {
				txs = append(txs, &tx)
			}
		}
		f.Close()
		for _, tx := range txs {
			if m.AddLocal(tx, chainID) == nil {
				loaded++
			}
		}
	}
	// Compact + open the file for appends, whether or not it existed.
	m.mu.Lock()
	m.rewriteJournalLocked()
	m.mu.Unlock()
	return loaded
}

// AddLocal admits a locally-originated tx (RPC/faucet) exactly like Add, then
// records it in the journal so it survives a restart. Gossiped txs use Add.
func (m *Mempool) AddLocal(tx *types.Transaction, chainID uint64) error {
	if err := m.Add(tx, chainID); err != nil {
		return err
	}
	m.mu.Lock()
	m.local[tx.Hash()] = struct{}{}
	m.appendJournalLocked(tx)
	m.mu.Unlock()
	return nil
}

// appendJournalLocked writes one tx as a JSON line. Caller holds m.mu.
func (m *Mempool) appendJournalLocked(tx *types.Transaction) {
	if m.journalFile == nil {
		return
	}
	if b, err := json.Marshal(tx); err == nil {
		_, _ = m.journalFile.Write(append(b, '\n'))
	}
}

// rewriteJournalLocked atomically rewrites the journal to hold only the local txs
// still pending (dropping mined/invalid ones), then reopens it for appends. Caller
// holds m.mu. Atomic tmp+rename so a crash mid-write can't corrupt the journal.
func (m *Mempool) rewriteJournalLocked() {
	if m.journalPath == "" {
		return
	}
	if m.journalFile != nil {
		_ = m.journalFile.Close()
		m.journalFile = nil
	}
	tmp := m.journalPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for h := range m.local {
		tx, ok := m.all[h]
		if !ok {
			delete(m.local, h) // no longer pending — drop
			continue
		}
		if b, err := json.Marshal(tx); err == nil {
			_, _ = w.Write(append(b, '\n'))
		}
	}
	_ = w.Flush()
	_ = f.Close()
	if os.Rename(tmp, m.journalPath) != nil {
		return
	}
	m.journalFile, _ = os.OpenFile(m.journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
}
