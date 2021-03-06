package slashing

import (
	"bytes"
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/runtime"
	"github.com/pkg/errors"

	"github.com/filecoin-project/go-filecoin/internal/pkg/block"
	"github.com/filecoin-project/go-filecoin/internal/pkg/chain"
	"github.com/filecoin-project/go-filecoin/internal/pkg/encoding"
	"github.com/filecoin-project/go-filecoin/internal/pkg/state"
)

type FaultStateView interface {
	state.AccountStateView
	MinerControlAddresses(ctx context.Context, maddr address.Address) (owner, worker address.Address, err error)
}

// Chain state required for checking consensus fault reports.
type chainReader interface {
	GetTipSet(block.TipSetKey) (block.TipSet, error)
}

// Checks the validity of reported consensus faults.
type ConsensusFaultChecker struct {
	chain chainReader
}

func NewFaultChecker(chain chainReader) *ConsensusFaultChecker {
	return &ConsensusFaultChecker{chain: chain}
}

// Checks the validity of a consensus fault reported by serialized block headers h1, h2, and optional
// common-ancestor witness h3.
func (s *ConsensusFaultChecker) VerifyConsensusFault(ctx context.Context, h1, h2, extra []byte, head block.TipSetKey, view FaultStateView, earliest abi.ChainEpoch) (*runtime.ConsensusFault, error) {
	if bytes.Equal(h1, h2) {
		return nil, fmt.Errorf("no consensus fault: blocks identical")
	}

	var b1, b2, b3 block.Block
	innerErr := encoding.Decode(h1, &b1)
	if innerErr != nil {
		return nil, errors.Wrapf(innerErr, "failed to decode h1")
	}
	innerErr = encoding.Decode(h2, &b2)
	if innerErr != nil {
		return nil, errors.Wrapf(innerErr, "failed to decode h2")
	}
	if len(extra) > 0 {
		innerErr = encoding.Decode(extra, &b3)
		if innerErr != nil {
			return nil, errors.Wrapf(innerErr, "failed to decode extra")
		}
	}
	// Block syntax is not validated. This implements the strictest check possible, and is also the simplest check
	// possible.
	// This means that blocks that could never have been included in the chain (e.g. with an empty parent state)
	// are still fault-able.

	if b1.Miner != b2.Miner {
		return nil, fmt.Errorf("no consensus fault: miners differ")
	}
	if b1.Height > b2.Height {
		return nil, fmt.Errorf("no consensus fault: first block is higher than second")
	}

	// Check the basic fault conditions first, defer the (expensive) signature and chain history check until last.
	var fault *runtime.ConsensusFault

	// Double-fork mining fault: two blocks at the same epoch.
	// It is not necessary to present a common ancestor of the blocks.
	if b1.Height == b2.Height {
		fault = &runtime.ConsensusFault{
			Target: b1.Miner,
			Epoch:  b2.Height,
			Type:   runtime.ConsensusFaultDoubleForkMining,
		}
	}
	// Time-offset mining fault: two blocks with the same parent but different epochs.
	// The height check is redundant at time of writing, but included for robustness to future changes to this method.
	// The blocks have a common ancestor by definition (the parent).
	if b1.Parents.Equals(b2.Parents) && b1.Height != b2.Height {
		fault = &runtime.ConsensusFault{
			Target: b1.Miner,
			Epoch:  b2.Height,
			Type:   runtime.ConsensusFaultTimeOffsetMining,
		}
	}
	// Parent-grinding fault: one block’s parent is a tipset that provably should have included some block but does not.
	// The provable case is that two blocks are mined in consecutive epochs and the later one does not include the
	// earlier one as a parent.
	// B3 must prove that the higher block (B2) has grandparent equal to B1's parent.
	if b1.Height+1 == b2.Height && !b2.Parents.Has(b1.Cid()) && b2.Parents.Has(b3.Cid()) && b3.Parents.Equals(b1.Parents) {
		fault = &runtime.ConsensusFault{
			Target: b1.Miner,
			Epoch:  b2.Height,
			Type:   runtime.ConsensusFaultParentGrinding,
		}
	}
	if fault == nil {
		return nil, fmt.Errorf("no consensus fault: blocks are ok")
	}

	// Expensive validation: signatures and chain history.

	err := verifyBlockSignature(ctx, view, b1)
	if err != nil {
		return nil, err
	}
	err = verifyBlockSignature(ctx, view, b2)
	if err != nil {
		return nil, err
	}
	err = verifyOneBlockInChain(ctx, s.chain, head, b1, b2, earliest)
	if err != nil {
		return nil, err
	}

	return fault, nil
}

// Checks whether a block header is correctly signed in the context of the parent state to which it refers.
func verifyBlockSignature(ctx context.Context, view FaultStateView, blk block.Block) error {
	_, worker, err := view.MinerControlAddresses(ctx, blk.Miner)
	if err != nil {
		panic(errors.Wrapf(err, "failed to inspect miner addresses"))
	}
	err = state.NewSignatureValidator(view).ValidateSignature(ctx, blk.SignatureData(), worker, blk.BlockSig)
	if err != nil {
		return errors.Wrapf(err, "no consensus fault: block %s signature invalid", blk.Cid())
	}
	return err
}

// Checks whether at least one of b1, b2 appear in the chain defined by `head`.
func verifyOneBlockInChain(ctx context.Context, chn chainReader, head block.TipSetKey, b1 block.Block, b2 block.Block, earliest abi.ChainEpoch) error {
	if chainHasB1, err := chainContainsBlock(ctx, chn, head, b1, earliest); err != nil {
		panic(errors.Wrapf(err, "failed to inspect chain")) // This idiosyncratic failure shouldn't go on chain
	} else if chainHasB1 {
		return nil
	}
	if chainHasB2, err := chainContainsBlock(ctx, chn, head, b2, earliest); err != nil {
		panic(errors.Wrapf(err, "failed to inspect chain"))
	} else if chainHasB2 {
		return nil
	}
	return fmt.Errorf("no consensus fault: neither block in chain since %d", earliest)
}

func chainContainsBlock(ctx context.Context, chn chainReader, head block.TipSetKey, blk block.Block, earliest abi.ChainEpoch) (bool, error) {
	if blk.Height < earliest { // Short-circuit
		return false, nil
	}
	ts, err := chn.GetTipSet(head)
	if err != nil {
		return false, err
	}

	itr := chain.IterAncestors(ctx, chn, ts)
	for ts := itr.Value(); !itr.Complete(); err = itr.Next() {
		if err != nil {
			return false, err
		}
		height, err := ts.Height()
		if err != nil {
			return false, err
		}
		if height < earliest {
			return false, nil
		}
		if ts.Key().Has(blk.Cid()) {
			return true, nil
		}
	}
	return false, nil
}
