package aevm

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	aevmcommon "github.com/KyberNetwork/aevm/common"
	aevmtypes "github.com/KyberNetwork/aevm/types"
	"github.com/KyberNetwork/logger"
	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	_ "github.com/ethereum/go-ethereum/eth/tracers/native"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
)

const (
	// defaultTraceReexec is the number of blocks the tracer is willing to go back
	// and reexecute to produce missing historical state necessary to run a specific
	// trace.
	defaultTraceReexec = uint64(128)
)

var (
	tracerCfg = struct {
		DiffMode             bool `json:"diffMode"`
		DiffModeFullPreState bool `json:"diffModeFullPreState"`
	}{
		DiffMode:             true,
		DiffModeFullPreState: true,
	}
	tracerCfgJSON, _ = json.Marshal(tracerCfg)
)

// TraceResult represent the result of tracing
type TraceResult struct {
	Post stateMap `json:"post"`
	Pre  stateMap `json:"pre"`
}

type stateMap (map[common.Address]*account)

type account struct {
	Balance *big.Int                    `json:"balance,omitempty"`
	Code    []byte                      `json:"code,omitempty"`
	Nonce   uint64                      `json:"nonce,omitempty"`
	Storage map[common.Hash]common.Hash `json:"storage,omitempty"`
	empty   bool
}

// MarshalJSON marshals as JSON.
func (a account) MarshalJSON() ([]byte, error) {
	type account struct {
		Balance *hexutil.Big                `json:"balance,omitempty"`
		Code    hexutil.Bytes               `json:"code,omitempty"`
		Nonce   uint64                      `json:"nonce,omitempty"`
		Storage map[common.Hash]common.Hash `json:"storage,omitempty"`
	}
	var enc account
	enc.Balance = (*hexutil.Big)(a.Balance)
	enc.Code = a.Code
	enc.Nonce = a.Nonce
	enc.Storage = a.Storage
	return json.Marshal(&enc)
}

// UnmarshalJSON unmarshals from JSON.
func (a *account) UnmarshalJSON(input []byte) error {
	type account struct {
		Balance *hexutil.Big                `json:"balance,omitempty"`
		Code    *hexutil.Bytes              `json:"code,omitempty"`
		Nonce   *uint64                     `json:"nonce,omitempty"`
		Storage map[common.Hash]common.Hash `json:"storage,omitempty"`
	}
	var dec account
	if err := json.Unmarshal(input, &dec); err != nil {
		return err
	}
	if dec.Balance != nil {
		a.Balance = (*big.Int)(dec.Balance)
	}
	if dec.Code != nil {
		a.Code = *dec.Code
	}
	if dec.Nonce != nil {
		a.Nonce = *dec.Nonce
	}
	if dec.Storage != nil {
		a.Storage = dec.Storage
	}
	return nil
}

func (s stateMap) ToStateOverrides() *aevmtypes.StateOverrides {
	o := new(aevmtypes.StateOverrides)
	for _addr, acc := range s {
		addr := aevmcommon.Address(_addr)
		if acc.Balance != nil {
			balance := new(uint256.Int)
			balance.SetFromBig((*big.Int)(acc.Balance))
			if o.Balances == nil {
				o.Balances = make(map[aevmcommon.Address]*aevmcommon.Uint256)
			}
			o.Balances[addr] = (*aevmcommon.Uint256)(balance)
		}
		if len(acc.Code) > 0 {
			if o.Codes == nil {
				o.Codes = make(map[aevmcommon.Address][]byte)
			}
			o.Codes[addr] = acc.Code
		}
		if acc.Nonce != 0 {
			if o.Nonces == nil {
				o.Nonces = make(map[aevmcommon.Address]uint64)
			}
			o.Nonces[addr] = acc.Nonce
		}
		if len(acc.Storage) > 0 {
			if o.StateDiffs == nil {
				o.StateDiffs = make(map[aevmcommon.Address]map[aevmcommon.Hash]aevmcommon.Hash)
			}
			o.StateDiffs[addr] = make(map[aevmcommon.Hash]aevmcommon.Hash)
			for slot, value := range acc.Storage {
				o.StateDiffs[addr][aevmcommon.Hash(slot)] = aevmcommon.Hash(value)
			}
		}
	}
	return o
}

// traceDiff return a json Message based on native tracer
// the result is kept in
func (s *aevmBackend) traceDiff(ctx context.Context, state *state.StateDB, block *types.Block) (*TraceResult, error) {
	// https://github.com/KyberNetwork/go-ethereum/blob/c1c905ab12e6d48bf7c80d0175a5222c615af54b/eth/tracers/api.go#L615
	var (
		txs      = block.Transactions()
		blockCtx = core.NewEVMBlockContext(block.Header(), ethapi.NewChainContext(ctx, s.apiBackend), nil)
		signer   = types.MakeSigner(s.apiBackend.ChainConfig(), block.Number(), block.Time())
	)
	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		vmenv := vm.NewEVM(blockCtx, state, s.apiBackend.ChainConfig(), vm.Config{})
		core.ProcessBeaconBlockRoot(*beaconRoot, vmenv)

	}
	if s.apiBackend.ChainConfig().IsPrague(block.Number(), block.Time()) {
		vmenv := vm.NewEVM(blockCtx, state, s.apiBackend.ChainConfig(), vm.Config{})
		core.ProcessParentBlockHash(block.ParentHash(), vmenv)
	}

	var (
		before = make(map[common.Address]*account)
		final  = make(map[common.Address]*account)
	)

	for i, tx := range txs {
		msg, err := core.TransactionToMessage(tx, signer, block.BaseFee())
		if err != nil {
			return nil, err
		}
		tracerCtx := &tracers.Context{
			BlockHash:   block.Hash(),
			BlockNumber: block.Number(),
			TxIndex:     i,
			TxHash:      tx.Hash(),
		}
		res, err := s.traceTx(tx, msg, tracerCtx, blockCtx, state)
		if err != nil {
			return nil, fmt.Errorf("traceTx at %d-th tx returns an error: %w", i, err)
		}

		if !tracerCfg.DiffModeFullPreState {
			// reconstruct slots changed from non-zero to zero in res.Post
			reconstructPostStateZeroSlots(res.Pre, res.Post)
		}

		for acc, data := range res.Pre {
			before[acc] = mergePreState(data, before[acc])
		}
		for acc, data := range res.Post {
			final[acc] = mergePostState(final[acc], data)
		}
	}
	return &TraceResult{Pre: before, Post: final}, nil
}

// https://github.com/KyberNetwork/go-ethereum/blob/c1c905ab12e6d48bf7c80d0175a5222c615af54b/eth/tracers/api.go#L993
func (s *aevmBackend) traceTx(tx *types.Transaction, msg *core.Message, tracerCtx *tracers.Context, blockCtx vm.BlockContext, state *state.StateDB) (*TraceResult, error) {
	tracer, err := tracers.DefaultDirectory.New("prestateTracer", tracerCtx, tracerCfgJSON, s.apiBackend.ChainConfig())
	if err != nil {
		logger.Errorf("cannot get tracer, err:%v", err)
		return nil, err
	}

	evm := vm.NewEVM(blockCtx, state, s.apiBackend.ChainConfig(), vm.Config{Tracer: tracer.Hooks})

	var usedGas uint64
	state.SetTxContext(tracerCtx.TxHash, tracerCtx.TxIndex)
	_, err = core.ApplyTransactionWithEVM(msg, new(core.GasPool).AddGas(msg.GasLimit), state, blockCtx.BlockNumber, tracerCtx.BlockHash, tx, &usedGas, evm)
	if err != nil {
		return nil, err
	}

	jsonResult, err := tracer.GetResult()
	if err != nil {
		return nil, err
	}
	var result TraceResult
	if err := json.Unmarshal(jsonResult, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

func mergePreState(src, dest *account) *account {
	if dest == nil {
		dest = new(account)
	}
	if dest.Balance == nil {
		dest.Balance = new(big.Int).Set(src.Balance)
	}
	if dest.Code == nil {
		dest.Code = append([]byte(nil), src.Code...)
	}
	if dest.Nonce == 0 {
		dest.Nonce = src.Nonce
	}
	for slot, value := range src.Storage {
		if _, ok := dest.Storage[slot]; !ok {
			if dest.Storage == nil {
				dest.Storage = make(map[common.Hash]common.Hash)
			}
			dest.Storage[slot] = value
		}
	}
	return dest
}

func mergePostState(pre *account, post *account) *account {
	if pre == nil {
		return post
	}
	if post == nil {
		return pre
	}
	//if there is no change, these fields will be empty in post
	if post.Nonce == 0 {
		post.Nonce = pre.Nonce
	}
	if post.Code == nil {
		post.Code = pre.Code
	}
	if post.Balance == nil {
		post.Balance = pre.Balance
	}
	if len(post.Storage) == 0 {
		post.Storage = pre.Storage
	}

	if len(post.Storage) > 0 && len(pre.Storage) > 0 {
		// if there is a hash available in pre but not post, set it
		for hash, data := range pre.Storage {
			if _, avail := post.Storage[hash]; !avail {
				post.Storage[hash] = data
			}
		}
	}
	return post
}

// TraceDiff return a json Message based on native tracer
// the result is marshald in TraceResult format
func (s *aevmBackend) TraceDiff(ctx context.Context, blockHash common.Hash) (*TraceResult, error) {
	startTime := time.Now()
	defer func() {
		log.Info("TraceDiff", "took", time.Since(startTime))
	}()

	block := s.ethereum.BlockChain().GetBlockByHash(blockHash)
	parentBlock := s.ethereum.BlockChain().GetBlockByHash(block.ParentHash())
	stateAtBlock, release, err := s.apiBackend.StateAtBlock(context.Background(), parentBlock, defaultTraceReexec, nil, true, false)
	if err != nil {
		logger.Errorf("cannot get newer state, error: %v", err)

	}
	defer release()
	// trace Diff
	return s.traceDiff(ctx, stateAtBlock, block)
}

func reconstructPostStateZeroSlots(pre, post stateMap) {
	for addr, acc := range pre {
		for slot := range acc.Storage {
			if post[addr] == nil {
				post[addr] = &account{}
			}
			if post[addr].Storage == nil {
				post[addr].Storage = make(map[common.Hash]common.Hash)
			}
			if _, ok := post[addr].Storage[slot]; !ok {
				post[addr].Storage[slot] = common.Hash{}
			}
		}
	}

}
