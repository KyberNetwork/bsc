package readonlystate

import (
	"fmt"
	"sync"

	cmap "github.com/orcaman/concurrent-map/v2"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/trie/utils"
	"github.com/ethereum/go-ethereum/triedb"
)

// trieReader implements the Reader interface, providing functions to access
// state from the referenced trie.
// this is a concurrent-safe version of state.trieReader
type trieReader struct {
	root     common.Hash
	db       *triedb.Database
	buff     sync.Pool
	mainTrie state.Trie
	subRoots cmap.ConcurrentMap[common.Address, common.Hash]
	subTries cmap.ConcurrentMap[common.Address, state.Trie]
}

// trieReader constructs a trie reader of the specific state. An error will be
// returned if the associated trie specified by root is not existent.
func newTrieReader(root common.Hash, db *triedb.Database, cache *utils.PointCache) (*trieReader, error) {
	var (
		tr  state.Trie
		err error
	)
	if !db.IsVerkle() {
		tr, err = trie.NewStateTrie(trie.StateTrieID(root), db)
	} else {
		tr, err = trie.NewVerkleTrie(root, db, cache)
	}
	if err != nil {
		return nil, err
	}
	return &trieReader{
		root:     root,
		db:       db,
		buff:     sync.Pool{New: func() any { return crypto.NewKeccakState() }},
		mainTrie: tr,
		subRoots: cmap.NewWithCustomShardingFunction[common.Address, common.Hash](fnv32Address),
		subTries: cmap.NewWithCustomShardingFunction[common.Address, state.Trie](fnv32Address),
	}, nil
}

// Account implements Reader, retrieving the account specified by the address.
//
// An error will be returned if the trie state is corrupted. An nil account
// will be returned if it's not existent in the trie.
func (r *trieReader) Account(addr common.Address) (*types.StateAccount, error) {
	var (
		account *types.StateAccount
		err     error
	)
	switch tr := r.mainTrie.(type) {
	case *trie.StateTrie:
		account, err = tr.Copy().GetAccount(addr)
	case *trie.VerkleTrie:
		account, err = tr.Copy().GetAccount(addr)
	default:
		return nil, fmt.Errorf("expect mainTrie to be *trie.StateTrie or *trie.VerkleTrie")
	}
	if err != nil {
		return nil, err
	}
	if account == nil {
		r.subRoots.Set(addr, types.EmptyRootHash)
	} else {
		r.subRoots.Set(addr, account.Root)
	}
	return account, nil
}

// Storage implements Reader, retrieving the storage slot specified by the
// address and slot key.
//
// An error will be returned if the trie state is corrupted. An empty storage
// slot will be returned if it's not existent in the trie.
func (r *trieReader) Storage(addr common.Address, key common.Hash) (common.Hash, error) {
	var (
		tr    state.Trie
		found bool
		value common.Hash
	)
	if r.db.IsVerkle() {
		tr = r.mainTrie
	} else {
		tr, found = r.subTries.Get(addr)
		if !found {
			root, ok := r.subRoots.Get(addr)

			// The storage slot is accessed without account caching. It's unexpected
			// behavior but try to resolve the account first anyway.
			if !ok {
				_, err := r.Account(addr)
				if err != nil {
					return common.Hash{}, err
				}
				root, _ = r.subRoots.Get(addr)
			}
			var err error
			buff := r.buff.Get().(crypto.KeccakState)
			defer r.buff.Put(buff)
			tr, err = trie.NewStateTrie(trie.StorageTrieID(r.root, crypto.HashData(buff, addr.Bytes()), root), r.db)
			if err != nil {
				return common.Hash{}, err
			}
			r.subTries.Set(addr, tr)
		}
	}
	var (
		ret []byte
		err error
	)
	switch tr := tr.(type) {
	case *trie.StateTrie:
		ret, err = tr.Copy().GetStorage(addr, key.Bytes())
	case *trie.VerkleTrie:
		ret, err = tr.Copy().GetStorage(addr, key.Bytes())
	default:
		return common.Hash{}, fmt.Errorf("expect trie to be *trie.StateTrie or *trie.VerkleTrie")
	}
	if err != nil {
		return common.Hash{}, err
	}
	value.SetBytes(ret)
	return value, nil
}

func (*trieReader) Copy() state.Reader {
	panic("unreachable")
}
