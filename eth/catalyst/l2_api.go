package catalyst

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/core/state"
	"github.com/scroll-tech/go-ethereum/core/types"
	"github.com/scroll-tech/go-ethereum/eth"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/scroll-tech/go-ethereum/node"
	"github.com/scroll-tech/go-ethereum/rollup/rcfg"
	"github.com/scroll-tech/go-ethereum/rollup/withdrawtrie"
	"github.com/scroll-tech/go-ethereum/rpc"
	"github.com/scroll-tech/go-ethereum/trie"
)

func RegisterL2Engine(stack *node.Node, backend *eth.Ethereum) error {
	chainconfig := backend.BlockChain().Config()
	if chainconfig.TerminalTotalDifficulty == nil {
		return errors.New("catalyst started without valid total difficulty")
	}

	stack.RegisterAPIs([]rpc.API{
		{
			Namespace:     "engine",
			Version:       "1.0",
			Service:       newL2ConsensusAPI(backend),
			Public:        true,
			Authenticated: true,
		},
	})
	return nil
}

type l2ConsensusAPI struct {
	eth               *eth.Ethereum
	verified          map[common.Hash]executionResult // stored execution result of the next block that to be committed
	verifiedMapLock   *sync.RWMutex
	validateBlockLock *sync.Mutex
	newBlockLock      *sync.Mutex
}

func newL2ConsensusAPI(eth *eth.Ethereum) *l2ConsensusAPI {
	return &l2ConsensusAPI{
		eth:               eth,
		verified:          make(map[common.Hash]executionResult),
		verifiedMapLock:   &sync.RWMutex{},
		validateBlockLock: &sync.Mutex{},
		newBlockLock:      &sync.Mutex{},
	}
}

type executionResult struct {
	block            *types.Block
	state            *state.StateDB
	receipts         types.Receipts
	procTime         time.Duration
	withdrawTrieRoot common.Hash
}

func (api *l2ConsensusAPI) AssembleL2Block(params AssembleL2BlockParams) (*ExecutableL2Data, error) {
	log.Info("Assembling block", "block number", params.Number)
	parent := api.eth.BlockChain().CurrentHeader()
	expectedBlockNumber := parent.Number.Uint64() + 1
	if params.Number != expectedBlockNumber {
		log.Warn("Cannot assemble block with discontinuous block number", "expected number", expectedBlockNumber, "actual number", params.Number)
		return nil, fmt.Errorf("cannot assemble block with discontinuous block number %d, expected number is %d", params.Number, expectedBlockNumber)
	}
	transactions := make(types.Transactions, 0, len(params.Transactions))
	for i, otx := range params.Transactions {
		var tx types.Transaction
		if err := tx.UnmarshalBinary(otx); err != nil {
			return nil, fmt.Errorf("transaction %d is not valid: %v", i, err)
		}
		transactions = append(transactions, &tx)
	}

	start := time.Now()
	block, state, receipts, err := api.eth.Miner().GetSealingBlockAndState(parent.Hash(), time.Now(), transactions)
	if err != nil {
		return nil, err
	}

	// Do not produce new block if no transaction is involved
	// if block.TxHash() == types.EmptyRootHash {
	//	 return nil, nil
	// }
	procTime := time.Since(start)
	withdrawTrieRoot := api.writeVerified(state, block, receipts, procTime)
	return &ExecutableL2Data{
		ParentHash:   block.ParentHash(),
		Number:       block.NumberU64(),
		Miner:        block.Coinbase(),
		Timestamp:    block.Time(),
		GasLimit:     block.GasLimit(),
		BaseFee:      block.BaseFee(),
		Transactions: encodeTransactions(block.Transactions()),

		StateRoot:        block.Root(),
		GasUsed:          block.GasUsed(),
		ReceiptRoot:      block.ReceiptHash(),
		LogsBloom:        block.Bloom().Bytes(),
		WithdrawTrieRoot: withdrawTrieRoot,

		Hash: block.Hash(),
	}, nil
}

func (api *l2ConsensusAPI) writeVerified(state *state.StateDB, block *types.Block, receipts types.Receipts, procTime time.Duration) common.Hash {
	withdrawTrieRoot := withdrawtrie.ReadWTRSlot(rcfg.L2MessageQueueAddress, state)
	api.verifiedMapLock.Lock()
	defer api.verifiedMapLock.Unlock()
	api.verified[block.Hash()] = executionResult{
		block:            block,
		state:            state,
		receipts:         receipts,
		procTime:         procTime,
		withdrawTrieRoot: withdrawTrieRoot,
	}
	return withdrawTrieRoot
}

func (api *l2ConsensusAPI) isVerified(blockHash common.Hash) (executionResult, bool) {
	api.verifiedMapLock.RLock()
	defer api.verifiedMapLock.RUnlock()
	er, found := api.verified[blockHash]
	return er, found
}

func (api *l2ConsensusAPI) ValidateL2Block(params ExecutableL2Data) (*GenericResponse, error) {
	parent := api.eth.BlockChain().CurrentBlock()
	expectedBlockNumber := parent.NumberU64() + 1
	if params.Number != expectedBlockNumber {
		log.Warn("Cannot validate block with discontinuous block number", "expected number", expectedBlockNumber, "actual number", params.Number)
		return nil, fmt.Errorf("cannot validate block with discontinuous block number %d, expected number is %d", params.Number, expectedBlockNumber)
	}
	if params.ParentHash != parent.Hash() {
		log.Warn("Wrong parent hash", "expected block hash", parent.TxHash().Hex(), "actual block hash", params.ParentHash.Hex())
		return nil, fmt.Errorf("wrong parent hash: %s, expected parent hash is %s", params.ParentHash, parent.Hash())
	}

	block, err := api.executableDataToBlock(params, types.BLSData{})
	if err != nil {
		return nil, err
	}

	api.validateBlockLock.Lock()
	defer api.validateBlockLock.Unlock()

	er, verified := api.isVerified(block.Hash())
	if verified {
		if (params.WithdrawTrieRoot != common.Hash{} && params.WithdrawTrieRoot != er.withdrawTrieRoot) {
			log.Error("ValidateL2Block failed, wrong withdrawTrieRoot", "expected", er.withdrawTrieRoot.Hex(), "actual", params.WithdrawTrieRoot.Hex())
			return &GenericResponse{
				false,
			}, nil
		}
		return &GenericResponse{
			true,
		}, nil
	}

	if err := api.VerifyBlock(block); err != nil {
		return &GenericResponse{
			false,
		}, nil
	}

	stateDB, receipts, _, procTime, err := api.eth.BlockChain().ProcessBlock(block, parent.Header())
	if err != nil {
		log.Error("error processing block", "error", err)
		return &GenericResponse{
			false,
		}, nil
	}
	if (params.WithdrawTrieRoot != common.Hash{}) && params.WithdrawTrieRoot != withdrawtrie.ReadWTRSlot(rcfg.L2MessageQueueAddress, stateDB) {
		log.Error("ValidateL2Block failed, wrong withdraw trie root")
		return &GenericResponse{
			false,
		}, nil
	}
	api.writeVerified(stateDB, block, receipts, procTime)
	return &GenericResponse{
		true,
	}, nil
}

func (api *l2ConsensusAPI) NewL2Block(params ExecutableL2Data, bls types.BLSData) (err error) {
	api.newBlockLock.Lock()
	defer api.newBlockLock.Unlock()

	parent := api.eth.BlockChain().CurrentBlock()
	expectedBlockNumber := parent.NumberU64() + 1
	if params.Number != expectedBlockNumber {
		if params.Number < expectedBlockNumber {
			log.Warn("ignore the past block number", "block number", params.Number, "current block number", parent.NumberU64())
			return nil
		}
		log.Warn("Cannot new block with discontinuous block number", "expected number", expectedBlockNumber, "actual number", params.Number)
		return fmt.Errorf("cannot new block with discontinuous block number %d, expected number is %d", params.Number, expectedBlockNumber)
	}
	if params.ParentHash != parent.Hash() {
		log.Warn("Wrong parent hash", "expected block hash", parent.Hash().Hex(), "actual block hash", params.ParentHash.Hex())
		return fmt.Errorf("wrong parent hash: %s, expected parent hash is %s", params.ParentHash, parent.Hash())
	}

	block, err := api.executableDataToBlock(params, bls)
	if err != nil {
		return err
	}

	defer func() {
		if err == nil {
			api.verified = make(map[common.Hash]executionResult) // clear cached pending block
		}
	}()

	bas, verified := api.isVerified(block.Hash())
	if verified {
		return api.eth.BlockChain().WriteStateAndSetHead(block, bas.receipts, bas.state, bas.procTime)
	}

	if err = api.VerifyBlock(block); err != nil {
		log.Error("failed to verify block", "error", err)
		return err
	}

	stateDB, receipts, _, procTime, err := api.eth.BlockChain().ProcessBlock(block, parent.Header())
	if err != nil {
		return err
	}
	return api.eth.BlockChain().WriteStateAndSetHead(block, receipts, stateDB, procTime)
}

func (api *l2ConsensusAPI) NewSafeL2Block(params SafeL2Data, bls types.BLSData) (header *types.Header, err error) {
	parent := api.eth.BlockChain().CurrentBlock()
	expectedBlockNumber := parent.NumberU64() + 1
	if params.Number != expectedBlockNumber {
		log.Warn("Cannot assemble block with discontinuous block number", "expected number", expectedBlockNumber, "actual number", params.Number)
		return nil, fmt.Errorf("cannot assemble block with discontinuous block number %d, expected number is %d", params.Number, expectedBlockNumber)
	}
	if params.ParentHash != parent.Hash() {
		log.Warn("Wrong parent hash", "expected block hash", parent.Hash().Hex(), "actual block hash", params.ParentHash.Hex())
		return nil, fmt.Errorf("wrong parent hash: %s, expected parent hash is %s", params.ParentHash, parent.Hash())
	}
	block, err := api.safeDataToBlock(params, bls)
	if err != nil {
		return nil, err
	}
	stateDB, receipts, usedGas, procTime, err := api.eth.BlockChain().ProcessBlock(block, parent.Header())
	if err != nil {
		return nil, err
	}
	// reconstruct the block with the execution result
	header = block.Header()
	header.GasUsed = usedGas
	header.Bloom = types.CreateBloom(receipts)
	header.ReceiptHash = types.DeriveSha(receipts, trie.NewStackTrie(nil))
	header.Root = stateDB.IntermediateRoot(true)
	block = types.NewBlockWithHeader(header).WithBody(block.Transactions(), nil)

	// refill the receipts with real block hash
	for _, receipt := range receipts {
		receipt.BlockHash = block.Hash()
		for _, txLog := range receipt.Logs {
			txLog.BlockHash = block.Hash()
		}
	}
	return header, api.eth.BlockChain().WriteStateAndSetHead(block, receipts, stateDB, procTime)
}

func (api *l2ConsensusAPI) safeDataToBlock(params SafeL2Data, blsData types.BLSData) (*types.Block, error) {
	header := &types.Header{
		ParentHash: params.ParentHash,
		Number:     big.NewInt(int64(params.Number)),
		GasLimit:   params.GasLimit,
		Time:       params.Timestamp,
		BLSData:    blsData,
		BaseFee:    params.BaseFee,
	}
	api.eth.Engine().Prepare(api.eth.BlockChain(), header)
	txs, err := decodeTransactions(params.Transactions)
	if err != nil {
		return nil, err
	}
	header.TxHash = types.DeriveSha(types.Transactions(txs), trie.NewStackTrie(nil))
	return types.NewBlockWithHeader(header).WithBody(txs, nil), nil
}

func (api *l2ConsensusAPI) executableDataToBlock(params ExecutableL2Data, blsData types.BLSData) (*types.Block, error) {
	header := &types.Header{
		ParentHash: params.ParentHash,
		Number:     big.NewInt(int64(params.Number)),
		GasUsed:    params.GasUsed,
		GasLimit:   params.GasLimit,
		Time:       params.Timestamp,
		Coinbase:   params.Miner,
		BLSData:    blsData,
		BaseFee:    params.BaseFee,
	}
	api.eth.Engine().Prepare(api.eth.BlockChain(), header)

	txs, err := decodeTransactions(params.Transactions)
	if err != nil {
		return nil, err
	}
	header.TxHash = types.DeriveSha(types.Transactions(txs), trie.NewStackTrie(nil))
	header.ReceiptHash = params.ReceiptRoot
	header.Root = params.StateRoot
	header.Bloom = types.BytesToBloom(params.LogsBloom)
	return types.NewBlockWithHeader(header).WithBody(txs, nil), nil
}

func (api *l2ConsensusAPI) VerifyBlock(block *types.Block) error {
	if err := api.eth.Engine().VerifyHeader(api.eth.BlockChain(), block.Header(), false); err != nil {
		log.Warn("failed to verify header", "error", err)
		return err
	}
	if err := api.eth.BlockChain().Validator().ValidateBody(block); err != nil {
		log.Error("error validating body", "error", err)
		return err
	}
	return nil
}
