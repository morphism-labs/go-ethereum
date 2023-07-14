package core

import (
	"errors"
	"math/big"
	"time"

	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/consensus"
	"github.com/scroll-tech/go-ethereum/core/rawdb"
	"github.com/scroll-tech/go-ethereum/core/state"
	"github.com/scroll-tech/go-ethereum/core/types"
	"github.com/scroll-tech/go-ethereum/ethdb"
	"github.com/scroll-tech/go-ethereum/log"
)

func (bc *BlockChain) ProcessBlock(block *types.Block, parent *types.Header) (*state.StateDB, types.Receipts, uint64, time.Duration, error) {
	statedb, err := state.New(parent.Root, bc.stateCache, bc.snaps)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	// Enable prefetching to pull in trie node paths while processing transactions
	statedb.StartPrefetcher("chain")

	// Process block using the parent state as reference point
	start := time.Now()
	receipts, _, usedGas, err := bc.processor.Process(block, statedb, bc.vmConfig)
	if err != nil {
		bc.reportBlock(block, receipts, err)
		return nil, nil, 0, 0, err
	}
	// Update the metrics touched during block processing
	accountReadTimer.Update(statedb.AccountReads)                 // Account reads are complete, we can mark them
	storageReadTimer.Update(statedb.StorageReads)                 // Storage reads are complete, we can mark them
	accountUpdateTimer.Update(statedb.AccountUpdates)             // Account updates are complete, we can mark them
	storageUpdateTimer.Update(statedb.StorageUpdates)             // Storage updates are complete, we can mark them
	snapshotAccountReadTimer.Update(statedb.SnapshotAccountReads) // Account reads are complete, we can mark them
	snapshotStorageReadTimer.Update(statedb.SnapshotStorageReads) // Storage reads are complete, we can mark them
	triehash := statedb.AccountHashes + statedb.StorageHashes     // Save to not double count in validation
	trieproc := statedb.SnapshotAccountReads + statedb.AccountReads + statedb.AccountUpdates
	trieproc += statedb.SnapshotStorageReads + statedb.StorageReads + statedb.StorageUpdates

	blockExecutionTimer.Update(time.Since(start) - trieproc - triehash)

	// Validate the state using the default validator
	substart := time.Now()
	if err := bc.validator.ValidateState(block, statedb, receipts, usedGas); err != nil {
		bc.reportBlock(block, receipts, err)
		return nil, nil, 0, 0, err
	}
	blockValidationTimer.Update(time.Since(substart) - (statedb.AccountHashes + statedb.StorageHashes - triehash))

	proctime := time.Since(start)

	// Update the metrics touched during block validation
	accountHashTimer.Update(statedb.AccountHashes) // Account hashes are complete, we can mark them
	storageHashTimer.Update(statedb.StorageHashes) // Storage hashes are complete, we can mark them

	return statedb, receipts, usedGas, proctime, nil
}

// writeBlockStateWithoutHead writes block, metadata and corresponding state data to the
// database.
func (bc *BlockChain) writeBlockStateWithoutHead(block *types.Block, receipts []*types.Receipt, state *state.StateDB) error {
	// Calculate the total difficulty of the block
	ptd := bc.GetTd(block.ParentHash(), block.NumberU64()-1)
	if ptd == nil {
		return consensus.ErrUnknownAncestor
	}
	// Make sure no inconsistent state is leaked during insertion
	externTd := new(big.Int).Add(block.Difficulty(), ptd)

	// Irrelevant of the canonical status, write the block itself to the database.
	//
	// Note all the components of block(td, hash->number map, header, body, receipts)
	// should be written atomically. BlockBatch is used for containing all components.
	blockBatch := bc.db.NewBatch()
	rawdb.WriteTd(blockBatch, block.Hash(), block.NumberU64(), externTd)
	rawdb.WriteBlock(blockBatch, block)
	rawdb.WriteReceipts(blockBatch, block.Hash(), block.NumberU64(), receipts)
	rawdb.WritePreimages(blockBatch, state.Preimages())
	if err := blockBatch.Write(); err != nil {
		log.Crit("Failed to write block into disk", "err", err)
	}
	// Commit all cached state changes into underlying memory database.
	root, err := state.Commit(bc.chainConfig.IsEIP158(block.Number()))
	if err != nil {
		return err
	}

	triedb := bc.stateCache.TrieDB()
	// If we're running an archive node, always flush
	if bc.cacheConfig.TrieDirtyDisabled {
		return triedb.Commit(root, false, nil)
	}
	// Full but not archive node, do proper garbage collection
	triedb.Reference(root, common.Hash{}) // metadata reference to keep trie alive
	bc.triegc.Push(root, -int64(block.NumberU64()))

	current := block.NumberU64()
	// Flush limits are not considered for the first TriesInMemory blocks.
	if current <= TriesInMemory {
		return nil
	}
	// If we exceeded our memory allowance, flush matured singleton nodes to disk
	var (
		nodes, imgs = triedb.Size()
		limit       = common.StorageSize(bc.cacheConfig.TrieDirtyLimit) * 1024 * 1024
	)
	if nodes > limit || imgs > 4*1024*1024 {
		triedb.Cap(limit - ethdb.IdealBatchSize)
	}
	// Find the next state trie we need to commit
	chosen := current - TriesInMemory
	//flushInterval := time.Duration(atomic.LoadInt64(&bc.flushInterval))
	// If we exceeded time allowance, flush an entire trie to disk
	if bc.gcproc > bc.cacheConfig.TrieTimeLimit {
		// If the header is missing (canonical chain behind), we're reorging a low
		// diff sidechain. Suspend committing until this operation is completed.
		header := bc.GetHeaderByNumber(chosen)
		if header == nil {
			log.Warn("Reorg in progress, trie commit postponed", "number", chosen)
		} else {
			// If we're exceeding limits but haven't reached a large enough memory gap,
			// warn the user that the system is becoming unstable.
			if chosen < lastWrite+TriesInMemory && bc.gcproc >= 2*bc.cacheConfig.TrieTimeLimit {
				log.Info("State in memory for too long, committing", "time", bc.gcproc, "allowance", bc.cacheConfig.TrieTimeLimit, "optimum", float64(chosen-lastWrite)/TriesInMemory)
			}
			// Flush an entire trie and restart the counters
			triedb.Commit(header.Root, true, nil)
			lastWrite = chosen
			bc.gcproc = 0
		}
	}
	// Garbage collect anything below our required write retention
	for !bc.triegc.Empty() {
		root, number := bc.triegc.Pop()
		if uint64(-number) > chosen {
			bc.triegc.Push(root, number)
			break
		}
		triedb.Dereference(root.(common.Hash))
	}
	return nil
}

// SetCanonical rewinds the chain to set the new head block as the specified
// block.
func (bc *BlockChain) SetCanonical(head *types.Block) (common.Hash, error) {
	//if !bc.chainmu.TryLock() {
	//	return common.Hash{}, errChainStopped
	//}
	//defer bc.chainmu.Unlock()

	// Re-execute the reorged chain in case the head state is missing.
	if !bc.HasState(head.Root()) {
		//if latestValidHash, err := bc.recoverAncestors(head); err != nil {
		//	return latestValidHash, err
		//}
		//log.Info("Recovered head state", "number", head.Number(), "hash", head.Hash())
		log.Error("Head state is missing", "number", head.Number(), "hash", head.Hash())
		return common.Hash{}, errors.New("head state is missing")
	}
	// Run the reorg if necessary and set the given block as new head.
	start := time.Now()
	if head.ParentHash() != bc.CurrentBlock().Hash() {
		if err := bc.reorg(bc.CurrentBlock(), head); err != nil {
			return common.Hash{}, err
		}
	}
	bc.writeHeadBlock(head)

	// Emit events
	logs := bc.collectLogs(head, false)
	bc.chainFeed.Send(ChainEvent{Block: head, Hash: head.Hash(), Logs: logs})
	if len(logs) > 0 {
		bc.logsFeed.Send(logs)
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Block: head})

	context := []interface{}{
		"number", head.Number(),
		"hash", head.Hash(),
		"root", head.Root(),
		"elapsed", time.Since(start),
	}
	if timestamp := time.Unix(int64(head.Time()), 0); time.Since(timestamp) > time.Minute {
		context = append(context, []interface{}{"age", common.PrettyAge(timestamp)}...)
	}
	log.Info("Chain head was updated", context...)
	return head.Hash(), nil
}

func (bc *BlockChain) WriteStateAndSetHead(block *types.Block, receipts types.Receipts, state *state.StateDB, procTime time.Duration) error {
	if !bc.chainmu.TryLock() {
		return errInsertionInterrupted
	}
	defer bc.chainmu.Unlock()
	bc.gcproc += procTime
	if err := bc.writeBlockStateWithoutHead(block, receipts, state); err != nil {
		return err
	}
	_, err := bc.SetCanonical(block)
	return err
}

// collectLogs collects the logs that were generated or removed during
// the processing of a block. These logs are later announced as deleted or reborn.
func (bc *BlockChain) collectLogs(b *types.Block, removed bool) []*types.Log {
	receipts := rawdb.ReadRawReceipts(bc.db, b.Hash(), b.NumberU64())
	receipts.DeriveFields(bc.chainConfig, b.Hash(), b.NumberU64(), b.Transactions())

	var logs []*types.Log
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			l := *log
			if removed {
				l.Removed = true
			}
			logs = append(logs, &l)
		}
	}
	return logs
}
