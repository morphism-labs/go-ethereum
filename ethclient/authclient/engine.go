package authclient

import (
	"context"
	"fmt"
	"math/big"

	"github.com/scroll-tech/go-ethereum/core/types"
	"github.com/scroll-tech/go-ethereum/eth/catalyst"
)

// AssembleL2Block assembles L2 Block used for L2 sequencer to propose a block in L2 consensus progress
func (ec *Client) AssembleL2Block(ctx context.Context, number *big.Int, transactions types.Transactions) (*catalyst.ExecutableL2Data, error) {
	txs := make([][]byte, 0, len(transactions))
	for i, tx := range transactions {
		bz, err := tx.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("failed to marshal tx, index: %d, error: %v", i, err)
		}
		txs = append(txs, bz)
	}
	var result catalyst.ExecutableL2Data
	err := ec.c.CallContext(ctx, &result, "engine_assembleL2Block", &catalyst.AssembleL2BlockParams{
		Number:       number.Uint64(),
		Transactions: txs,
	})
	return &result, err
}

// ValidateL2Block validates a L2 Block
func (ec *Client) ValidateL2Block(ctx context.Context, executableL2Data *catalyst.ExecutableL2Data) (bool, error) {
	var result catalyst.GenericResponse
	err := ec.c.CallContext(ctx, &result, "engine_validateL2Block", executableL2Data)
	return result.Success, err
}

// NewL2Block executes L2 Block, and set the block to chain
func (ec *Client) NewL2Block(ctx context.Context, executableL2Data *catalyst.ExecutableL2Data, blsData *types.BLSData) error {
	return ec.c.CallContext(ctx, nil, "engine_newL2Block", executableL2Data, blsData)
}

// NewSafeL2Block executes a safe L2 Block, and set the block to chain
func (ec *Client) NewSafeL2Block(ctx context.Context, safeL2Data *catalyst.SafeL2Data, blsData *types.BLSData) (*types.Header, error) {
	var header types.Header
	err := ec.c.CallContext(ctx, &header, "engine_newSafeL2Block", safeL2Data, blsData)
	return &header, err
}
