package aevm

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KyberNetwork/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers"
	_ "github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/tests"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

func genTestDB(g *core.Genesis) (ethdb.Database, error) {
	db := rawdb.NewMemoryDatabase()
	triedb := triedb.NewDatabase(db, triedb.HashDefaults)
	defer triedb.Close()
	config := g.Config
	if config == nil {
		config = params.AllEthashProtocolChanges
	}

	block := g.ToBlock()

	// Marshal the genesis state specification and persist.
	blob, err := json.Marshal(g.Alloc)
	if err != nil {
		return nil, err
	}
	rawdb.WriteGenesisStateSpec(db, block.Hash(), blob)
	rawdb.WriteBlock(db, block)
	rawdb.WriteReceipts(db, block.Hash(), block.NumberU64(), nil)
	rawdb.WriteCanonicalHash(db, block.Hash(), block.NumberU64())
	rawdb.WriteHeadBlockHash(db, block.Hash())
	rawdb.WriteHeadFastBlockHash(db, block.Hash())
	rawdb.WriteHeadHeaderHash(db, block.Hash())
	rawdb.WriteChainConfig(db, block.Hash(), config)
	return db, nil
}
func newTestAccountManager(t *testing.T) (*accounts.Manager, accounts.Account) {
	var (
		dir        = t.TempDir()
		am         = accounts.NewManager(&accounts.Config{InsecureUnlockAllowed: true})
		b          = keystore.NewKeyStore(dir, 2, 1)
		testKey, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	)
	acc, err := b.ImportECDSA(testKey, "")
	if err != nil {
		t.Fatalf("failed to create test account: %v", err)
	}
	if err := b.Unlock(acc, ""); err != nil {
		t.Fatalf("failed to unlock account: %v\n", err)
	}
	am.AddBackend(b)
	return am, acc
}

func (t *testBackend) BlockChain() *core.BlockChain {
	return t.chain
}

func (t *testBackend) ChainDb() ethdb.Database {
	return t.db
}

func (t *testBackend) StateAtBlock(ctx context.Context, block *types.Block, reexec uint64, base *state.StateDB, readOnly bool, preferDisk bool) (*state.StateDB, tracers.StateReleaseFunc, error) {
	//TODO implement me
	panic("implement me")
}

func (t *testBackend) ChainConfig() *params.ChainConfig {
	return t.BlockChain().Config()
}

func (t *testBackend) Engine() consensus.Engine {
	return ethash.NewFaker()

}

func (t *testBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	//TODO implement me
	panic("implement me")
}

type testBackend struct {
	db    ethdb.Database
	chain *core.BlockChain
}

func newTestBackend(t *testing.T, n int, gspec *core.Genesis, engine consensus.Engine) *testBackend {
	var (
		cacheConfig = &core.CacheConfig{
			TrieCleanLimit:    256,
			TrieDirtyLimit:    256,
			TrieTimeLimit:     5 * time.Minute,
			SnapshotLimit:     0,
			TrieDirtyDisabled: true, // Archive mode
		}
	)
	// Generate blocks for testing
	db, blocks, _ := core.GenerateChainWithGenesis(gspec, engine, n, func(i int, gen *core.BlockGen) {
		//purposely empty since we dont need new Block
	})
	txlookupLimit := uint64(0)
	chain, err := core.NewBlockChain(db, cacheConfig, gspec, nil, engine, vm.Config{}, &txlookupLimit)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	if n, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	backend := &testBackend{db: db, chain: chain}
	return backend
}

type callContext struct {
	Number     math.HexOrDecimal64   `json:"number"`
	Difficulty *math.HexOrDecimal256 `json:"difficulty"`
	Time       math.HexOrDecimal64   `json:"timestamp"`
	GasLimit   math.HexOrDecimal64   `json:"gasLimit"`
	Miner      common.Address        `json:"miner"`
	BaseFee    *math.HexOrDecimal256 `json:"baseFeePerGas"`
}

type testcase struct {
	Genesis      *core.Genesis   `json:"genesis"`
	Context      *callContext    `json:"context"`
	Input        string          `json:"input"`
	TracerConfig json.RawMessage `json:"tracerConfig"`
	Result       interface{}     `json:"result"`
}

func TestStateKeeper_traceDiff(t *testing.T) {
	logger.SetLogLevel("debug")
	files, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("failed to retrieve tracer test suite: %v", err)
	}
	type args struct {
		ctx   context.Context
		block *types.Block
	}

	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		file := file // capture range variable
		t.Run(strings.TrimSuffix(file.Name(), ".json"), func(t *testing.T) {
			var (
				test = new(testcase)
				tx   = new(types.Transaction)
			)
			// Call tracer test found, read if from disk
			if blob, err := os.ReadFile(filepath.Join("testdata", file.Name())); err != nil {
				t.Fatalf("failed to read testcase: %v", err)
			} else if err := json.Unmarshal(blob, test); err != nil {
				t.Fatalf("failed to parse testcase: %v", err)
			}
			if err := tx.UnmarshalBinary(common.FromHex(test.Input)); err != nil {
				t.Fatalf("failed to parse testcase input: %v", err)
			}
			configStr, mErr := test.TracerConfig.MarshalJSON()
			if mErr != nil {
				t.Fatalf("failed to marshal tracer config %v", err)
			}
			println("t.TracerConfig", string(configStr))
			db, sErr := genTestDB(test.Genesis)
			if sErr != nil {
				t.Fatalf("cannot gen testDB, err: %v", sErr)
			}
			var (
				cacheConfig = &core.CacheConfig{
					TrieCleanLimit:    256,
					TrieDirtyLimit:    256,
					TrieTimeLimit:     5 * time.Minute,
					SnapshotLimit:     0,
					TrieDirtyDisabled: true, // Archive mode
				}
			)

			txlookupLimit := uint64(0)
			tmp := test.Genesis.Number
			//hack: by pass genesis number = 0
			test.Genesis.Number = 0
			chain, sErr := core.NewBlockChain(db, cacheConfig, test.Genesis, nil, beacon.New(ethash.NewFaker()), vm.Config{}, &txlookupLimit)
			if sErr != nil {
				t.Fatalf("cannot gen chain, err: %v", sErr)
			}
			test.Genesis.Number = tmp

			be := testBackend{
				db:    db,
				chain: chain,
			}
			currentState := tests.MakePreState(rawdb.NewMemoryDatabase(), test.Genesis.Alloc, false, rawdb.HashScheme)

			println("is statenil", currentState.StateDB)

			sk := &aevmBackend{
				ethereum:   &be,
				apiBackend: &be,
			}
			currentHeader := be.BlockChain().CurrentBlock()
			//currentBlock := be.BlockChain().GetBlock(currentHeader.Hash(), 0)
			body := &types.Body{
				Transactions: []*types.Transaction{tx},
				Uncles:       nil,
				Withdrawals:  nil,
			}
			currentHeader.Number = big.NewInt(int64(test.Context.Number))
			currentHeader.Time = uint64(test.Context.Time)
			currentHeader.Coinbase = common.HexToAddress("0x1585936b53834b021f68cc13eeefdec2efc8e724")
			block := types.NewBlock(currentHeader, body, nil, trie.NewStackTrie(nil))
			res, rErr := sk.traceDiff(context.Background(), currentState.StateDB, block)
			assert.NoError(t, rErr, "must be able to traceDiff")

			resStr, mErr := json.Marshal(res.Post)
			if mErr != nil {
				t.Fatalf("failed to marshal test: %v", mErr)

			}
			wantStr, mErr := json.Marshal(test.Result)
			if mErr != nil {
				t.Fatalf("failed to marshal test: %v", mErr)
			}
			var want TraceResult
			mErr = json.Unmarshal(wantStr, &want)
			assert.NoError(t, mErr, "cannot marshal/unmarshal")

			wantStr, mErr = json.Marshal(want.Post)
			assert.NoError(t, mErr, "cannot marshal/unmarshal")

			assert.Equal(t, string(resStr), string(wantStr), "must got the same res")
		})

	}

}

func TestMergePostState(t *testing.T) {
	toMerge := []*account{
		{
			Balance: big.NewInt(9999999999),
			Nonce:   1,
			Code:    []byte{1, 2, 3, 4, 5, 6, 7, 8, 9},
		},
		{
			Balance: big.NewInt(10000000000000000),
			Nonce:   2,
		},
		{
			Storage: map[common.Hash]common.Hash{
				common.HexToHash("0xaaaa"): common.HexToHash("0xbbbb"),
				common.HexToHash("0xcccc"): common.HexToHash("0xdddd"),
			},
		},
		{
			Storage: map[common.Hash]common.Hash{
				common.HexToHash("0xcccc"): common.HexToHash("0xeeee"),
				common.HexToHash("0xeeee"): common.HexToHash("0xffff"),
			},
		},
	}
	expected := &account{
		Balance: big.NewInt(10000000000000000),
		Nonce:   2,
		Code:    []byte{1, 2, 3, 4, 5, 6, 7, 8, 9},
		Storage: map[common.Hash]common.Hash{
			common.HexToHash("0xaaaa"): common.HexToHash("0xbbbb"),
			common.HexToHash("0xcccc"): common.HexToHash("0xeeee"),
			common.HexToHash("0xeeee"): common.HexToHash("0xffff"),
		},
	}
	var merged *account
	for _, acc := range toMerge {
		merged = mergePostState(merged, acc)
	}

	require.Equal(t, expected, merged)
}

func TestMergePreState(t *testing.T) {
	toMerge := []*account{
		{
			Balance: big.NewInt(9999999999),
			Nonce:   1,
			Code:    []byte{1, 2, 3, 4, 5, 6, 7, 8, 9},
		},
		{
			Balance: big.NewInt(10000000000000000),
			Nonce:   2,
		},
		{
			Storage: map[common.Hash]common.Hash{
				common.HexToHash("0xaaaa"): common.HexToHash("0xbbbb"),
				common.HexToHash("0xcccc"): common.HexToHash("0xdddd"),
			},
		},
		{
			Storage: map[common.Hash]common.Hash{
				common.HexToHash("0xcccc"): common.HexToHash("0xeeee"),
				common.HexToHash("0xeeee"): common.HexToHash("0xffff"),
			},
		},
	}
	expected := &account{
		Balance: big.NewInt(9999999999),
		Nonce:   1,
		Code:    []byte{1, 2, 3, 4, 5, 6, 7, 8, 9},
		Storage: map[common.Hash]common.Hash{
			common.HexToHash("0xaaaa"): common.HexToHash("0xbbbb"),
			common.HexToHash("0xcccc"): common.HexToHash("0xdddd"),
			common.HexToHash("0xeeee"): common.HexToHash("0xffff"),
		},
	}
	var merged *account
	for _, acc := range toMerge {
		merged = mergePreState(acc, merged)
	}

	require.Equal(t, expected, merged)
}

func TestReconstructPostStateZeroSlots(t *testing.T) {
	pre := stateMap{
		common.HexToAddress("0xaa"): &account{
			Storage: map[common.Hash]common.Hash{
				common.HexToHash("0xaaaa"): common.HexToHash("0xbbbb"),
				common.HexToHash("0xcccc"): common.HexToHash("0xdddd"),
			},
		},
		common.HexToAddress("0xbb"): &account{
			Storage: map[common.Hash]common.Hash{
				common.HexToHash("0xaaaa"): common.HexToHash("0xbbbb"),
			},
		},
	}
	post := stateMap{
		common.HexToAddress("0xaa"): &account{
			Storage: map[common.Hash]common.Hash{
				common.HexToHash("0xcccc"): common.HexToHash("0xeeee"),
			},
		},
	}
	expectedPost := stateMap{
		common.HexToAddress("0xaa"): &account{
			Storage: map[common.Hash]common.Hash{
				common.HexToHash("0xaaaa"): common.HexToHash("0x0"),
				common.HexToHash("0xcccc"): common.HexToHash("0xeeee"),
			},
		},
		common.HexToAddress("0xbb"): &account{
			Storage: map[common.Hash]common.Hash{
				common.HexToHash("0xaaaa"): common.HexToHash("0x0"),
			},
		},
	}

	reconstructPostStateZeroSlots(pre, post)

	require.Equal(t, expectedPost, post)
}
