package canoliq

import (
	"bytes"
	"sort"

	"github.com/canopy-network/go-plugin/contract"
)

// registry.go keeps the plugin's ValidatorRegistry in step with Canopy's live
// committee membership.
//
// Why this exists — the registry used to be written exactly once, by
// genesis.go::applyGenesisValidatorRegistry, from the `validatorRegistry`
// block of the canoLiq genesis file. Nothing else ever added an entry
// (ejectValidator only removes). A validator that joined committee `chainId`
// after genesis the normal way — a native `tx-stake` / `MessageEditStake`
// listing the committee — was therefore invisible to reward accounting
// forever: ProcessRewards observes stake growth summed over the registry, so
// an empty (or stale) registry means an observed reward of 0, a frozen
// `total_pooled_cnpy`, and a cCNPY/CNPY exchange rate pinned at 1.0 no matter
// how much genuinely bonds and compounds at the FSM level.
//
// Canopy itself derives committee membership by scanning every validator
// record and filtering on `Validator.committees[]` (fsm/validator.go's
// getValidatorSet over getCurrentValidators — from protocol v2 the legacy
// per-committee index keys are no longer written, so that scan is the only
// authority). syncCommitteeRegistry does exactly the same scan through the
// plugin's range-read channel, and ProcessRewards calls it every block before
// observing.
//
// Two properties matter for reward correctness and are why the registry
// carries a per-validator stake rather than just a member list:
//
//  1. A membership change must not read as reward. The observation is
//     block-over-block growth; if a validator joins the committee with a
//     1,000 CNPY bond, the aggregate committee stake jumps by 1,000 CNPY, and
//     an aggregate-only diff would credit that entire bond to cCNPY holders as
//     yield. Keeping each member's last-observed stake in its registry entry
//     lets the observation sum *per-validator* growth instead, so a newcomer
//     contributes 0 on the block it is admitted and only compounding counts.
//  2. Governance ejection must survive the sync. ejectValidator writes a
//     tombstone at KeyForEjectedValidator; sync skips tombstoned addresses so
//     a passed F12 proposal is not undone by the next block's reconciliation.

// committeeObservation is the result of one reconciliation pass: the registry
// as it should now be persisted (entries carry the *live* stake, which is both
// the next block's per-validator baseline and the pro-rata weight used by
// distributeValidatorShare), the reward observed since the last pass, and the
// aggregate committee stake.
type committeeObservation struct {
	// registry is the reconciled member set, sorted by address.
	registry *contract.ValidatorRegistry
	// reward is the sum of per-validator stake growth over members that were
	// already registered at the previous observation. Members admitted by this
	// pass contribute 0.
	reward uint64
	// total is the aggregate live stake of the reconciled member set, stored
	// in globals.last_processed_reward_pool as the observation watermark.
	total uint64
}

// syncCommitteeRegistry reconciles the stored ValidatorRegistry against
// Canopy's live validator set and returns the resulting observation. It does
// not write: the caller batches the registry set op with the rest of the
// reward sweep so a block either applies both or neither.
func (c *Canoliq) syncCommitteeRegistry() (*committeeObservation, *contract.PluginError) {
	// One round-trip for all three inputs: the stored registry (last-observed
	// stake per member), the ejection tombstones, and the live validator set.
	qReg, qEject, qVals := qid(), qid(), qid()
	resp, err := c.plugin.StateRead(c, &contract.PluginStateReadRequest{
		Keys: []*contract.PluginKeyRead{{QueryId: qReg, Key: KeyForValidatorRegistry()}},
		Ranges: []*contract.PluginRangeRead{
			{QueryId: qEject, Prefix: EjectedValidatorPrefix()},
			{QueryId: qVals, Prefix: contract.ValidatorPrefix()},
		},
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	baseline := make(map[string]uint64)
	ejected := make(map[string]struct{})
	var live []*contract.Validator
	for _, r := range resp.Results {
		switch r.QueryId {
		case qReg:
			if len(r.Entries) == 0 {
				continue
			}
			reg := new(contract.ValidatorRegistry)
			if e := contract.Unmarshal(r.Entries[0].Value, reg); e != nil {
				return nil, e
			}
			for _, e := range reg.Entries {
				baseline[string(e.Address)] = e.Stake
			}
		case qEject:
			for _, e := range r.Entries {
				if addr, ok := ParseEjectedValidator(e.Key); ok {
					ejected[string(addr)] = struct{}{}
				}
			}
		case qVals:
			live, err = committeeValidators(r.Entries, c.Config.ChainId)
			if err != nil {
				return nil, err
			}
		}
	}
	obs := &committeeObservation{
		registry: &contract.ValidatorRegistry{Entries: make([]*contract.ValidatorRegistryEntry, 0, len(live))},
	}
	for _, val := range live {
		if _, gone := ejected[string(val.Address)]; gone {
			continue
		}
		obs.total += val.StakedAmount
		// Only stake growth of a member we already observed is reward; a
		// newly admitted member is baselined at its current bond (see the
		// file comment). Shrinkage (unstake / slash) contributes nothing
		// rather than netting against another member's genuine reward.
		if prev, known := baseline[string(val.Address)]; known && val.StakedAmount > prev {
			obs.reward += val.StakedAmount - prev
		}
		obs.registry.Entries = append(obs.registry.Entries, &contract.ValidatorRegistryEntry{
			Address: val.Address,
			Stake:   val.StakedAmount,
		})
	}
	return obs, nil
}

// committeeValidators decodes the validator records returned by the
// full-prefix range read and keeps those bonded to `chainId`, sorted by
// address so the persisted registry is deterministic across nodes.
//
// Cost: one full-prefix range read per block. That is the same scan the FSM
// runs in-process for its own committee derivation, and there is no cheaper
// discovery path — the per-committee index keys (fsm/key.go's KeyForCommittee)
// stopped being written at protocol v2. Since the entries we keep are exactly
// the ones whose stake we would otherwise have to point-read anyway, the scan
// replaces the per-member reads rather than adding to them.
//
// Note on filtering: membership is taken straight from `committees[]`, the
// same field Canopy pays committee rewards against. Unstaking / paused /
// non-compounding operators are deliberately not filtered out — the plugin's
// view of a Canopy validator (contract.Validator) carries only address, stake
// and committees, and none of those states can manufacture reward anyway: a
// frozen or non-compounding position simply shows no stake growth and so
// observes as 0.
func committeeValidators(entries []*contract.PluginStateEntry, chainId uint64) ([]*contract.Validator, *contract.PluginError) {
	out := make([]*contract.Validator, 0, len(entries))
	for _, e := range entries {
		val := new(contract.Validator)
		if err := contract.Unmarshal(e.Value, val); err != nil {
			return nil, err
		}
		if len(val.Address) != 20 || !validatorOnCommittee(val, chainId) {
			continue
		}
		out = append(out, val)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].Address, out[j].Address) < 0 })
	return out, nil
}

// registrySetOp marshals the reconciled registry into a set op for the
// caller's write batch.
func registrySetOp(reg *contract.ValidatorRegistry) (*contract.PluginSetOp, *contract.PluginError) {
	bz, err := contract.Marshal(reg)
	if err != nil {
		return nil, err
	}
	return &contract.PluginSetOp{Key: KeyForValidatorRegistry(), Value: bz}, nil
}
