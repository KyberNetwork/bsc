package readonlystate

import (
	"fmt"
	"math/big"
	"os"

	"github.com/KyberNetwork/aevm"
	aevmcommon "github.com/KyberNetwork/aevm/common"
	cmap "github.com/orcaman/concurrent-map/v2"
	"github.com/tarndt/shardedsingleflight"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
)

// ReadonlyStateDB this is a stripped down version of go-ethereum's state.StateDB struct whose operations are all readonly.
// This struct is thread-safe.
type ReadonlyStateDB struct {
	db     state.Database
	trie   state.Trie
	reader *trieReader

	// originalRoot is the pre-state root, before any changes were made.
	// It will be updated when the Commit is called.
	originalRoot common.Hash

	// This map holds 'live' objects, which will get modified while processing a state transition.
	stateObjects      cmap.ConcurrentMap[common.Address, *stateObject]
	stateObjectsGroup *shardedsingleflight.ShardedGroup
}

// New creates a new state from a given trie.
func New(root common.Hash, db state.Database) (*ReadonlyStateDB, error) {
	tr, err := db.OpenTrie(root) // return a *trie.StateTrie
	if err != nil {
		return nil, err
	}
	// see core/state.CachingDB.Reader()
	reader, err := newTrieReader(root, db.TrieDB(), db.PointCache())
	if err != nil {
		return nil, err
	}
	sdb := &ReadonlyStateDB{
		db:                db,
		trie:              tr,
		reader:            reader,
		originalRoot:      root,
		stateObjects:      cmap.NewWithCustomShardingFunction[common.Address, *stateObject](fnv32Address),
		stateObjectsGroup: shardedsingleflight.NewShardedGroup(),
	}
	return sdb, nil
}

// Exist reports whether the given account address exists in the state.
// Notably this also returns true for suicided accounts.
func (s *ReadonlyStateDB) Exist(addr aevmcommon.Address) bool {
	return s.getStateObject(common.Address(addr)) != nil
}

// Empty returns whether the state object is either non-existent
// or empty according to the EIP161 specification (balance = nonce = code = 0)
func (s *ReadonlyStateDB) Empty(addr aevmcommon.Address) bool {
	so := s.getStateObject(common.Address(addr))
	return so == nil || so.empty()
}

// GetBalance retrieves the balance from the given address or 0 if object not found
func (s *ReadonlyStateDB) GetBalance(addr aevmcommon.Address) *big.Int {
	stateObject := s.getStateObject(common.Address(addr))
	if stateObject != nil {
		return stateObject.Balance().ToBig()
	}
	return common.Big0
}

func (s *ReadonlyStateDB) GetNonce(addr aevmcommon.Address) uint64 {
	stateObject := s.getStateObject(common.Address(addr))
	if stateObject != nil {
		return stateObject.Nonce()
	}

	return 0
}

func (s *ReadonlyStateDB) GetCode(addr aevmcommon.Address) []byte {
	stateObject := s.getStateObject(common.Address(addr))
	if stateObject != nil {
		return stateObject.Code()
	}
	return nil
}

func (s *ReadonlyStateDB) GetCodeSize(addr aevmcommon.Address) int {
	stateObject := s.getStateObject(common.Address(addr))
	if stateObject != nil {
		return stateObject.CodeSize()
	}
	return 0
}

func (s *ReadonlyStateDB) GetCodeHash(addr aevmcommon.Address) aevmcommon.Hash {
	stateObject := s.getStateObject(common.Address(addr))
	if stateObject != nil {
		return aevmcommon.BytesToHash(stateObject.CodeHash())
	}
	return aevmcommon.Hash{}
}

// GetState retrieves a value from the given account's storage trie.
func (s *ReadonlyStateDB) GetState(addr aevmcommon.Address, hash aevmcommon.Hash) aevmcommon.Hash {
	stateObject := s.getStateObject(common.Address(addr))
	if stateObject != nil {
		return aevmcommon.Hash(stateObject.GetState(common.Hash(hash)))
	}
	return aevmcommon.Hash{}
}

// GetCommittedState retrieves a value from the given account's committed storage trie.
func (s *ReadonlyStateDB) GetCommittedState(addr aevmcommon.Address, hash aevmcommon.Hash) aevmcommon.Hash {
	stateObject := s.getStateObject(common.Address(addr))
	if stateObject != nil {
		return aevmcommon.Hash(stateObject.GetCommittedState(common.Hash(hash)))
	}
	return aevmcommon.Hash{}
}

// getStateObject retrieves a state object given by the address, returning nil if
// the object is not found or was deleted in this execution context.
func (s *ReadonlyStateDB) getStateObject(addr common.Address) *stateObject {
	// Prefer live objects if any is available
	if obj, ok := s.stateObjects.Get(addr); ok {
		return obj
	}

	_acct, err, _ := s.stateObjectsGroup.Do(fmt.Sprintf("reader.Account(%s)", addr), func() (interface{}, error) {
		return s.reader.Account(addr)
	})
	if err != nil {
		s.setError(fmt.Errorf("getStateObject (%x) error: %w", addr.Bytes(), err))
		return nil
	}
	acct := _acct.(*types.StateAccount)

	// Short circuit if the account is not found
	if acct == nil {
		return nil
	}

	// Insert into the live set
	obj := newObject(s, addr, acct)
	s.setStateObject(obj)
	return obj
}

func (s *ReadonlyStateDB) setStateObject(object *stateObject) {
	s.stateObjects.Set(object.Address(), object)
}

func (*ReadonlyStateDB) setError(err error) {
	fmt.Fprintf(os.Stderr, "core/state/readonlystate.ReadonlyStateDB error: %s", err)
}

var _ aevm.StateDB = &ReadonlyStateDB{}
