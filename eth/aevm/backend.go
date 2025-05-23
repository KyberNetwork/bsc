package aevm

import (
	"context"
	"fmt"
	"math/big"

	"github.com/KyberNetwork/aevm"
	"github.com/KyberNetwork/aevm/aevmserver"
	aevmcommon "github.com/KyberNetwork/aevm/common"
	aevmevent "github.com/KyberNetwork/aevm/event"
	aevmtypes "github.com/KyberNetwork/aevm/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/state/readonlystate"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

func NewAEVMServer(ethereum IEthereum, apiBackend IAPIBackend) (*aevmserver.AEVMServer, error) {
	aevmServer, err := aevmserver.NewAEVMServer(&aevmBackend{ethereum: ethereum, apiBackend: apiBackend}, aevmserver.GetListenerAddressFromEnv())
	if err != nil {
		return nil, err
	}
	return aevmServer, nil
}

type IEthereum interface {
	BlockChain() *core.BlockChain
	ChainDb() ethdb.Database
}

type IAPIBackend interface {
	StateAtBlock(ctx context.Context, block *types.Block, reexec uint64, base *state.StateDB, readOnly bool, preferDisk bool) (*state.StateDB, tracers.StateReleaseFunc, error)
	ChainConfig() *params.ChainConfig
	ethapi.ChainContextBackend
}

type aevmBackend struct {
	ethereum   IEthereum
	apiBackend IAPIBackend
}

func (b *aevmBackend) toHeader(header *types.Header) *aevmtypes.Header {
	var (
		blobBaseFee *big.Int
	)
	if header.ExcessBlobGas != nil {
		blobBaseFee = eip4844.CalcBlobFee(b.apiBackend.ChainConfig(), header)
	}
	return &aevmtypes.Header{
		Hash:           aevmcommon.Hash(header.Hash()),
		ParentHash:     aevmcommon.Hash(header.ParentHash),
		Coinbase:       aevmcommon.Address(header.Coinbase),
		Root:           aevmcommon.Hash(header.Root),
		Difficulty:     header.Difficulty,
		Number:         header.Number,
		GasLimit:       header.GasLimit,
		Time:           header.Time,
		MixDigest:      aevmcommon.Hash(header.MixDigest),
		BaseFee:        header.BaseFee,
		BlobBaseFee:    blobBaseFee,
		OriginalHeader: header,
	}
}

func (b *aevmBackend) CurrentBlock() *aevmtypes.Header {
	return b.toHeader(b.ethereum.BlockChain().CurrentBlock())
}

func (b *aevmBackend) GetBlockByHash(hash aevmcommon.Hash) *aevmtypes.Header {
	return b.toHeader(b.ethereum.BlockChain().GetBlockByHash(common.Hash(hash)).Header())
}

func (b *aevmBackend) NewReadonlyStateDB(header *aevmtypes.Header) (aevm.StateDB, error) {
	db := b.ethereum.BlockChain().StateCache()
	readonlyStateDB, err := readonlystate.New(common.Hash(header.Root), db)
	log.Info(">>>>>>>>>>>>>>> readonlystate.New()", "blockNumber", header.Number)
	if err != nil {
		log.Error(">>>>>>>>>>>>>>> could not new ReadonlyStateDB", "blockNumber", header.Number, "error", err)
		return nil, err
	}
	return readonlyStateDB, nil
}

func (b *aevmBackend) NewGetHashFunc(aevmHeader *aevmtypes.Header) aevm.GetHashFunc {
	header := &types.Header{
		ParentHash: common.Hash(aevmHeader.ParentHash),
		Root:       common.Hash(aevmHeader.Root),
		Number:     new(big.Int).Set(aevmHeader.Number),
	}
	getHashFn := core.GetHashFn(header, b.ethereum.BlockChain())
	return func(number uint64) aevmcommon.Hash { return aevmcommon.Hash(getHashFn(number)) }
}

func (*aevmBackend) VMConfig() *aevm.Config {
	return &aevm.Config{}
}

func (b *aevmBackend) ChainRules(header *aevmtypes.Header) *aevm.ChainRules {
	cfg := b.ethereum.BlockChain().Config()
	rules := cfg.Rules(header.Number, true, header.Time)
	return &aevm.ChainRules{
		ChainID:          cfg.ChainID,
		IsHomestead:      rules.IsHomestead,
		IsEIP150:         rules.IsEIP150,
		IsEIP155:         rules.IsEIP155,
		IsEIP158:         rules.IsEIP158,
		IsByzantium:      rules.IsByzantium,
		IsConstantinople: rules.IsConstantinople,
		IsPetersburg:     rules.IsPetersburg,
		IsIstanbul:       rules.IsIstanbul,
		IsBerlin:         rules.IsBerlin,
		IsLondon:         rules.IsLondon,
		IsMerge:          rules.IsMerge,
		IsShanghai:       rules.IsShanghai,
		IsCancun:         rules.IsCancun,
		IsPrague:         rules.IsPrague,
	}
}

type wrappedChainHeadSub struct {
	ch  chan core.ChainHeadEvent
	sub event.Subscription
}

func (s *wrappedChainHeadSub) Err() <-chan error { return s.sub.Err() }

func (s *wrappedChainHeadSub) Unsubscribe() {
	s.sub.Unsubscribe()
	close(s.ch)
}

func (b *aevmBackend) SubscribeChainHeadEvent(ch chan<- *aevmtypes.Header) aevmevent.Subscription {
	// this channel will forward to ch ASAP so capacity = 1 is enough
	_ch := make(chan core.ChainHeadEvent, 1)
	go func() {
		for evt := range _ch {

			ch <- b.toHeader(evt.Header)
		}
	}()
	sub := b.ethereum.BlockChain().SubscribeChainHeadEvent(_ch)
	return &wrappedChainHeadSub{ch: _ch, sub: sub}
}

func (b *aevmBackend) EncodeBlockHeader(_header interface{}) ([]byte, error) {
	header, ok := _header.(*types.Header)
	if !ok {
		return nil, fmt.Errorf("expected header to be *types.Header")
	}
	encoded, err := header.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("could not (*types.Header).MarshalJSON: %w", err)
	}
	return encoded, nil
}

// TraceBlockStateDiffs return pre-state Overrides and post-state Overrides and an error if there is
func (b *aevmBackend) TraceBlockStateDiffs(ctx context.Context, header *aevmtypes.Header) (*aevmtypes.TraceBlockResult, error) {
	res, err := b.TraceDiff(ctx, common.Hash(header.Hash))
	if err != nil {
		return nil, fmt.Errorf("TraceDiff returns an error: %w", err)
	}

	return &aevmtypes.TraceBlockResult{
		PreState:  res.Pre.ToStateOverrides(),
		PostState: res.Post.ToStateOverrides(),
	}, nil
}

var _ aevmserver.BackendForAEVM = &aevmBackend{}
