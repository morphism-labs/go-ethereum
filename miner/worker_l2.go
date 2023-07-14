package miner

import (
	"fmt"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/consensus/misc"
	"github.com/scroll-tech/go-ethereum/core"
	"github.com/scroll-tech/go-ethereum/core/state"
	"github.com/scroll-tech/go-ethereum/core/types"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/scroll-tech/go-ethereum/params"
)

// generateParams wraps various of settings for generating sealing task.
type generateParams struct {
	timestamp    uint64             // The timstamp for sealing task
	parentHash   common.Hash        // Parent block hash, empty means the latest chain head
	coinbase     common.Address     // The fee recipient address for including transaction
	transactions types.Transactions // L1Message transactions to include at the start of the block
}

// prepareWork constructs the sealing task according to the given parameters,
// either based on the last chain head or specified parent. In this function
// the pending transactions are not filled yet, only the empty task returned.
func (w *worker) prepareWork(genParams *generateParams) (*environment, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	parent := w.chain.CurrentBlock()
	if genParams.parentHash != (common.Hash{}) {
		parent = w.chain.GetBlockByHash(genParams.parentHash)
	}
	if parent == nil {
		return nil, fmt.Errorf("missing parent")
	}

	timestamp := genParams.timestamp
	if parent.Time() >= genParams.timestamp {
		timestamp = parent.Time() + 1
	}
	coinBase := w.coinbase
	if genParams.coinbase != (common.Address{}) {
		coinBase = genParams.coinbase
	}
	header, err := w.makeHeader(parent, timestamp, coinBase)
	if err != nil {
		return nil, err
	}

	env, err := w.makeEnv(parent, header)
	if err != nil {
		log.Error("Failed to create sealing context", "err", err)
		return nil, err
	}
	return env, nil
}

func (w *worker) makeHeader(parent *types.Block, timestamp uint64, coinBase common.Address) (*types.Header, error) {
	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent.GasLimit(), w.config.GasCeil),
		Extra:      w.extra,
		Time:       timestamp,
		Coinbase:   coinBase,
	}
	// Set baseFee and GasLimit if we are on an EIP-1559 chain
	if w.chainConfig.IsLondon(header.Number) {
		if w.chainConfig.Scroll.BaseFeeEnabled() {
			header.BaseFee = misc.CalcBaseFee(w.chainConfig, parent.Header())
		} else {
			// When disabling EIP-2718 or EIP-1559, we do not set baseFeePerGas in RPC response.
			// Setting BaseFee as nil here can help outside SDK calculates l2geth's RLP encoding,
			// otherwise the l2geth's BaseFee is not known from the outside.
			header.BaseFee = nil
		}
		if !w.chainConfig.IsLondon(parent.Number()) {
			parentGasLimit := parent.GasLimit() * params.ElasticityMultiplier
			header.GasLimit = core.CalcGasLimit(parentGasLimit, w.config.GasCeil)
		}
	}
	// Run the consensus preparation with the default or customized consensus engine.
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Error("Failed to prepare header for sealing", "err", err)
		return nil, err
	}
	return header, nil
}

// fillTransactions retrieves the pending transactions from the txpool and fills them
// into the given sealing block. The transaction selection and ordering strategy can
// be customized with the plugin in the future.
func (w *worker) fillTransactions(env *environment, l1Transactions types.Transactions) {
	if len(l1Transactions) > 0 {
		l1Txs := make(map[common.Address]types.Transactions)
		for _, tx := range l1Transactions {
			sender, _ := types.Sender(env.signer, tx)
			senderTxs, ok := l1Txs[sender]
			if ok {
				senderTxs = append(senderTxs, tx)
				l1Txs[sender] = senderTxs
			} else {
				l1Txs[sender] = types.Transactions{tx}
			}
		}
		txs := types.NewTransactionsByPriceAndNonce(env.signer, l1Txs, env.header.BaseFee)
		w.commitTransactions(env, txs, env.header.Coinbase, nil)
	}

	// Split the pending transactions into locals and remotes
	// Fill the block with all available pending transactions.
	pending := w.eth.TxPool().Pending(true)
	localTxs, remoteTxs := make(map[common.Address]types.Transactions), pending
	for _, account := range w.eth.TxPool().Locals() {
		if txs := remoteTxs[account]; len(txs) > 0 {
			delete(remoteTxs, account)
			localTxs[account] = txs
		}
	}
	if len(localTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(env.signer, localTxs, env.header.BaseFee)
		w.commitTransactions(env, txs, env.header.Coinbase, nil) // always return false
	}
	if len(remoteTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(env.signer, remoteTxs, env.header.BaseFee)
		w.commitTransactions(env, txs, env.header.Coinbase, nil) // always return false
	}
}

// generateWork generates a sealing block based on the given parameters.
func (w *worker) generateWork(genParams *generateParams) (*types.Block, *state.StateDB, types.Receipts, error) {
	work, err := w.prepareWork(genParams)
	if err != nil {
		return nil, nil, nil, err
	}
	defer work.discard()
	if work.gasPool == nil {
		work.gasPool = new(core.GasPool).AddGas(work.header.GasLimit)
	}

	//for _, tx := range genParams.transactions {
	//
	//	from, _ := types.Sender(work.signer, tx)
	//	// Start executing the transaction
	//	work.state.Prepare(tx.Hash(), work.tcount)
	//	_, err := w.commitTransaction(work, tx, work.header.Coinbase)
	//	if err != nil {
	//		return nil, nil, nil, fmt.Errorf("failed to force-include tx: %s type: %d sender: %s nonce: %d, err: %w", tx.Hash(), tx.Type(), from, tx.Nonce(), err)
	//	}
	//	work.l1TxCount++
	//}

	w.fillTransactions(work, genParams.transactions)

	block, err := w.engine.FinalizeAndAssemble(w.chain, work.header, work.state, work.txs, nil, work.receipts)
	if err != nil {
		return nil, nil, nil, err
	}
	return block, work.state, work.receipts, nil
}

func (env *environment) discard() {
	if env.state == nil {
		return
	}
	env.state.StopPrefetcher()
}
