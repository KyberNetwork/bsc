package readonlystate

import (
	"bytes"
	"fmt"
	"sync/atomic"

	"github.com/holiman/uint256"
	cmap "github.com/orcaman/concurrent-map/v2"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// this is a stripped down version of go-ethereum's state.stateObject struct whose operations are all readonly.
// This struct is also thread-safe.
type stateObject struct {
	db       *ReadonlyStateDB
	address  common.Address
	addrHash common.Hash // hash of ethereum address of the account
	data     types.StateAccount

	// Write caches.
	trie atomic.Value // `state.Trie` storage trie, which becomes non-nil on first access
	code atomic.Value // `Code` contract bytecode, which gets set when code is loaded

	originStorage cmap.ConcurrentMap[common.Hash, common.Hash] // Storage cache of original entries to dedup rewrites
}

// empty returns whether the account is considered empty.
func (s *stateObject) empty() bool {
	return s.data.Nonce == 0 && s.data.Balance.IsZero() && bytes.Equal(s.data.CodeHash, types.EmptyCodeHash.Bytes())
}

// newObject creates a state object.
func newObject(db *ReadonlyStateDB, address common.Address, acct *types.StateAccount) *stateObject {
	if acct == nil {
		acct = types.NewEmptyStateAccount()
	}
	return &stateObject{
		db:            db,
		address:       address,
		addrHash:      crypto.Keccak256Hash(address[:]),
		data:          *acct,
		originStorage: cmap.NewWithCustomShardingFunction[common.Hash, common.Hash](fnv32Hash),
	}
}

func (s *stateObject) getTrie() (state.Trie, error) {
	if s.trie.Load() == nil {
		tr, err, _ := s.db.stateObjectsGroup.Do(fmt.Sprintf("OpenStorageTrie(%s,%s,%s)", s.db.originalRoot, s.addrHash, s.data.Root), func() (interface{}, error) {
			return s.db.db.OpenStorageTrie(s.db.originalRoot, s.address, s.data.Root, s.db.trie)
		})
		if err != nil {
			return nil, err
		}
		s.trie.Store(tr)
	}
	return s.trie.Load().(state.Trie), nil
}

// GetState retrieves a value from the account storage trie.
func (s *stateObject) GetState(key common.Hash) common.Hash {
	// Otherwise return the entry's original value
	return s.GetCommittedState(key)
}

// GetCommittedState retrieves a value from the committed account storage trie.
func (s *stateObject) GetCommittedState(key common.Hash) common.Hash {
	if value, cached := s.originStorage.Get(key); cached {
		return value
	}

	_value, err, _ := s.db.stateObjectsGroup.Do(fmt.Sprintf("reader.Storage(%s,%s)", s.address, key), func() (interface{}, error) {
		return s.db.reader.Storage(s.address, key)
	})
	if err != nil {
		s.db.setError(err)
		return common.Hash{}
	}
	value := _value.(common.Hash)

	s.originStorage.Set(key, value)
	return value
}

//
// Attribute accessors
//

// Address returns the address of the contract/account
func (s *stateObject) Address() common.Address {
	return s.address
}

// Code returns the contract code associated with this object, if any.
func (s *stateObject) Code() []byte {
	if s.code.Load() != nil {
		return s.code.Load().([]byte)
	}
	if bytes.Equal(s.CodeHash(), types.EmptyCodeHash.Bytes()) {
		return nil
	}
	code, err, _ := s.db.stateObjectsGroup.Do(fmt.Sprintf("ContractCode(%s,%s)", s.addrHash, common.BytesToHash(s.CodeHash())), func() (interface{}, error) {
		r, err := s.db.db.Reader(s.db.originalRoot)
		if err != nil {
			s.db.setError(fmt.Errorf("can't load code hash %x: %v", s.CodeHash(), err))
		} else {
			return r.Code(s.address, common.BytesToHash(s.CodeHash()))
		}
		return nil, nil
	})
	if err != nil {
		s.db.setError(fmt.Errorf("can't load code hash %x: %v", s.CodeHash(), err))
	}
	s.code.Store(code)
	return code.([]byte)
}

// CodeSize returns the size of the contract code associated with this object,
// or zero if none. This method is an almost mirror of Code, but uses a cache
// inside the database to avoid loading codes seen recently.
func (s *stateObject) CodeSize() int {
	if s.code.Load() != nil {
		return len(s.code.Load().([]byte))
	}
	if bytes.Equal(s.CodeHash(), types.EmptyCodeHash.Bytes()) {
		return 0
	}
	r, err := s.db.db.Reader(s.db.originalRoot)
	if err != nil {
		s.db.setError(fmt.Errorf("can't load code size %x: %v", s.CodeHash(), err))
	}
	size, err := r.CodeSize(s.address, common.BytesToHash(s.CodeHash()))
	if err != nil {
		s.db.setError(fmt.Errorf("can't load code size %x: %v", s.CodeHash(), err))
	}
	return size
}

func (s *stateObject) CodeHash() []byte {
	return s.data.CodeHash
}

func (s *stateObject) Balance() *uint256.Int {
	return s.data.Balance
}

func (s *stateObject) Nonce() uint64 {
	return s.data.Nonce
}
