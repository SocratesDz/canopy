package canoliq

import (
	"bytes"

	"github.com/canopy-network/go-plugin/contract"
)

// ProcessRewards is the EndBlock hook that observes canoLiq's received
// committee reward and applies the 12% protocol fee with the canonical
// 40/30/15/15 split.
//
// Observation source — canoLiq's committee stake, NOT the committee fee pool.
// Canopy funds the committee reward pool (KeyForFeePool(chainId)) in BeginBlock
// and then fully distributes + zeroes it in EndBlock's DistributeCommitteeRewards,
// which runs *before* this plugin EndBlock hook. So the pool is always 0 by the
// time we look — it cannot be swept. Instead, Canopy compounds each block's
// committee reward into the bonded StakedAmount of the committee's validators
// (DistributeCommitteeReward, Compound=true). We therefore observe the
// block-over-block growth of canoLiq's committee validator stake (summed from
// the ValidatorRegistry) as the received reward R, and store the last observed
// aggregate in globals.last_processed_reward_pool (repurposed to mean "last
// observed committee stake"). canoLiq's own protocol tx-fees are tracked
// separately in KeyForTxFeeAccrual and route straight to the DAO treasury.
func (c *Canoliq) ProcessRewards(req *contract.PluginEndRequest) *contract.PluginError {
	params, err := c.LoadParams()
	if err != nil {
		return err
	}
	if params.FeeBps == 0 {
		return nil
	}
	globalsKey := KeyForGlobals()
	escrowKey := KeyForEscrowPool()
	gQ, eQ := qid(), qid()
	resp, err := c.plugin.StateRead(c, &contract.PluginStateReadRequest{
		Keys: []*contract.PluginKeyRead{
			{QueryId: gQ, Key: globalsKey},
			{QueryId: eQ, Key: escrowKey},
		},
	})
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	globals := new(contract.CanoliqGlobals)
	escrow := new(contract.Pool)
	for _, r := range resp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case gQ:
			if e := contract.Unmarshal(r.Entries[0].Value, globals); e != nil {
				return e
			}
		case eQ:
			if e := contract.Unmarshal(r.Entries[0].Value, escrow); e != nil {
				return e
			}
		}
	}
	if !globals.GenesisComplete {
		// Genesis has not run yet; nothing to do.
		return nil
	}
	// Sum the live Canopy stake of canoLiq's committee validators.
	observedStake, err := c.observedCommitteeStake()
	if err != nil {
		return err
	}
	baseline := globals.LastProcessedRewardPool
	// Seed-and-return when there is no positive growth to distribute:
	//   - baseline == 0: the first observation (fresh node / post-upgrade).
	//     Adopt the current stake as the baseline so pre-existing bonded stake
	//     is not mistaken for one giant reward on the next block.
	//   - observedStake <= baseline: an unstake / slash shrank the position, so
	//     there is no reward this block. Reset the baseline so growth resumes
	//     cleanly from the new (lower) level.
	// Either branch still advances the peak-TVL high-water mark (T4).
	if baseline == 0 || observedStake <= baseline {
		if globals.TotalPooledCnpy > globals.PeakTvlUcnpy {
			globals.PeakTvlUcnpy = globals.TotalPooledCnpy
		}
		globals.LastProcessedRewardPool = observedStake
		return c.SaveGlobals(globals)
	}
	// rewardDelta is pure Canopy committee reward — the growth of the bonded
	// committee stake since the last observation.
	rewardDelta := observedStake - baseline
	// L3: canoLiq's own protocol tx-fees accrue in their own scalar (every
	// handler credits it) and route straight to the DAO treasury. They are
	// protocol revenue, not committee reward, so they are NOT part of the 12%
	// fee + 40/30/15/15 split applied to rewardDelta.
	txFees := c.readScalar(KeyForTxFeeAccrual())
	fee := FeeOnReward(rewardDelta, params.FeeBps)
	netToUsers := rewardDelta - fee
	split := SplitFee(fee, &FeeSplitParams{
		UserRebateBps: params.UserRebateBps,
		TreasuryBps:   params.TreasuryBps,
		ValidatorBps:  params.ValidatorBps,
		BuybackBps:    params.BuybackBps,
	})
	// User accrual: net rewards plus the user-rebate slice flow into the
	// pooled CNPY backing cCNPY, lifting the cCNPY/CNPY exchange rate.
	userSlice := netToUsers + split.UserRebate
	globals.TotalPooledCnpy += userSlice
	// Advance the peak-TVL high water mark (T4) post-accrual.
	if globals.TotalPooledCnpy > globals.PeakTvlUcnpy {
		globals.PeakTvlUcnpy = globals.TotalPooledCnpy
	}

	// H1: credit the user slice into the escrow pool so cCNPY holders can
	// redeem against real CNPY. Keeps escrow == TotalPooledCnpy + PendingRedemptionCnpy.
	// (The reward CNPY itself lives in the bonded committee stake; escrow becomes
	// a claim on it, redeemable once the position is unbonded.)
	escrow.Amount += userSlice
	// Record the observed committee stake so the next block isolates only the
	// fresh growth (reward) as delta.
	globals.LastProcessedRewardPool = observedStake

	gBz, e := contract.Marshal(globals)
	if e != nil {
		return e
	}
	escrowBz, e := contract.Marshal(escrow)
	if e != nil {
		return e
	}
	sets := []*contract.PluginSetOp{
		{Key: globalsKey, Value: gBz},
		{Key: escrowKey, Value: escrowBz},
	}
	// Treasury & buyback go into plugin-owned scalar keys. WP §9.2 (slashing
	// risk) prescribes seeding an insurance pool from treasury inflow: skim
	// insurance_bps off the treasury reward slice so every credit auto-routes a
	// fraction. The L3 tx-fee revenue is added to the treasury credit here too
	// but is NOT subject to the insurance skim (it is not committee reward).
	insurance := uint64(0)
	if split.Treasury > 0 && params.InsuranceBps > 0 {
		insurance = mulDiv(split.Treasury, params.InsuranceBps, 10_000)
		// T4: once the reserve reaches its target (insurance_target_bps of peak
		// TVL), stop skimming — the would-be insurance amount stays in the
		// treasury so the fee-conservation invariant still holds. A target of 0
		// disables the gate (skim always on).
		if params.InsuranceTargetBps > 0 {
			target := mulDiv(globals.PeakTvlUcnpy, params.InsuranceTargetBps, 10_000)
			if c.readScalar(KeyForInsurancePool()) >= target {
				insurance = 0
			}
		}
	}
	treasuryDelta := (split.Treasury - insurance) + txFees
	if treasuryDelta > 0 {
		treasuryKey := KeyForTreasuryCNPY()
		sets = append(sets, &contract.PluginSetOp{
			Key:   treasuryKey,
			Value: EncodeUint64(c.readScalar(treasuryKey) + treasuryDelta),
		})
	}
	if insurance > 0 {
		insuranceKey := KeyForInsurancePool()
		sets = append(sets, &contract.PluginSetOp{
			Key:   insuranceKey,
			Value: EncodeUint64(c.readScalar(insuranceKey) + insurance),
		})
	}
	// L3: the accrued tx-fees have now been routed to the treasury; zero the
	// accumulator so the next sweep starts fresh.
	if txFees > 0 {
		sets = append(sets, &contract.PluginSetOp{Key: KeyForTxFeeAccrual(), Value: EncodeUint64(0)})
	}
	if split.Buyback > 0 {
		buybackKey := KeyForBuybackPool()
		sets = append(sets, &contract.PluginSetOp{
			Key:   buybackKey,
			Value: EncodeUint64(c.readScalar(buybackKey) + split.Buyback),
		})
	}
	if split.Validators > 0 {
		valSets, err := c.distributeValidatorShare(split.Validators)
		if err != nil {
			return err
		}
		sets = append(sets, valSets...)
	}
	if _, err := c.plugin.StateWrite(c, &contract.PluginStateWriteRequest{Sets: sets}); err != nil {
		return err
	}
	_ = req
	return nil
}

// observedCommitteeStake sums the live Canopy StakedAmount of every canoLiq
// committee validator listed in the ValidatorRegistry whose Committees include
// this chain. Canopy compounds each block's committee reward into these bonded
// positions, so the block-over-block growth of this sum is canoLiq's received
// reward (see ProcessRewards). Returns 0 when the registry is empty/absent —
// with no observable position there is no reward to distribute.
func (c *Canoliq) observedCommitteeStake() (uint64, *contract.PluginError) {
	registry, err := c.loadValidatorRegistry()
	if err != nil {
		return 0, err
	}
	if registry == nil || len(registry.Entries) == 0 {
		return 0, nil
	}
	keys := make([]*contract.PluginKeyRead, 0, len(registry.Entries))
	for _, e := range registry.Entries {
		keys = append(keys, &contract.PluginKeyRead{QueryId: qid(), Key: contract.KeyForValidator(e.Address)})
	}
	resp, err := c.plugin.StateRead(c, &contract.PluginStateReadRequest{Keys: keys})
	if err != nil {
		return 0, err
	}
	if resp.Error != nil {
		return 0, resp.Error
	}
	total := uint64(0)
	for _, r := range resp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		val := new(contract.Validator)
		if e := contract.Unmarshal(r.Entries[0].Value, val); e != nil {
			return 0, e
		}
		// Only count stake actually bonded to this committee — a validator that
		// has left committee `chainId` no longer earns its reward here.
		if !validatorOnCommittee(val, c.Config.ChainId) {
			continue
		}
		total += val.StakedAmount
	}
	return total, nil
}

// validatorOnCommittee reports whether val is a member of the given committee.
func validatorOnCommittee(val *contract.Validator, chainId uint64) bool {
	for _, id := range val.Committees {
		if id == chainId {
			return true
		}
	}
	return false
}

// readScalar is a small convenience for reading a uint64 stored under `key`,
// returning 0 if absent. Used by ProcessRewards to read-modify-write
// treasury/buyback/validator accumulators in one batch.
func (c *Canoliq) readScalar(key []byte) uint64 {
	q := qid()
	resp, err := c.plugin.StateRead(c, &contract.PluginStateReadRequest{
		Keys: []*contract.PluginKeyRead{{QueryId: q, Key: key}},
	})
	if err != nil || resp == nil || resp.Error != nil {
		return 0
	}
	if len(resp.Results) == 0 || len(resp.Results[0].Entries) == 0 {
		return 0
	}
	return DecodeUint64(resp.Results[0].Entries[0].Value)
}

// committeeAggregatorAddr returns a synthetic "address" used to aggregate
// validator-incentive accruals when the canoLiq committee validator set is
// unknown (empty registry). Phase 2 prefers ValidatorRegistry-driven
// pro-rata in distributeValidatorShare; this address is the legacy fallback.
func (c *Canoliq) committeeAggregatorAddr() []byte {
	addr := make([]byte, 20)
	for i := range addr {
		addr[i] = 0xCA
	}
	return addr
}

// distributeValidatorShare splits the validator-incentive slice across the
// canoLiq committee validator set proportional to per-validator stake. Empty
// registry falls back to a single committee-wide aggregator key — the
// Phase 1 behavior — so Phase 1 tests continue to pass unchanged. Rounding
// remainder is credited to the largest-stake validator so the credited
// total exactly equals the input share.
func (c *Canoliq) distributeValidatorShare(share uint64) ([]*contract.PluginSetOp, *contract.PluginError) {
	if share == 0 {
		return nil, nil
	}
	registry, err := c.loadValidatorRegistry()
	if err != nil {
		return nil, err
	}
	if registry == nil || len(registry.Entries) == 0 {
		// Legacy aggregator path.
		key := KeyForValidatorIncentives(c.committeeAggregatorAddr())
		return []*contract.PluginSetOp{
			{Key: key, Value: EncodeUint64(c.readScalar(key) + share)},
		}, nil
	}
	totalStake := uint64(0)
	largestIdx := 0
	for i, e := range registry.Entries {
		totalStake += e.Stake
		if e.Stake > registry.Entries[largestIdx].Stake {
			largestIdx = i
		}
	}
	if totalStake == 0 {
		key := KeyForValidatorIncentives(c.committeeAggregatorAddr())
		return []*contract.PluginSetOp{
			{Key: key, Value: EncodeUint64(c.readScalar(key) + share)},
		}, nil
	}
	credits := make([]uint64, len(registry.Entries))
	allocated := uint64(0)
	for i, e := range registry.Entries {
		credits[i] = mulDiv(share, e.Stake, totalStake)
		allocated += credits[i]
	}
	if allocated < share {
		credits[largestIdx] += share - allocated
	}
	sets := make([]*contract.PluginSetOp, 0, len(registry.Entries))
	for i, e := range registry.Entries {
		if credits[i] == 0 {
			continue
		}
		key := KeyForValidatorIncentives(e.Address)
		sets = append(sets, &contract.PluginSetOp{
			Key:   key,
			Value: EncodeUint64(c.readScalar(key) + credits[i]),
		})
	}
	return sets, nil
}

// ejectValidator removes addr from the committee registry and clears its
// accrued validator-incentive balance (F12). Idempotent: a no-op when addr is
// absent so a passed eject proposal can never halt BeginBlock. Future reward
// sweeps redistribute pro-rata over the remaining registry entries, so the
// ejected validator simply stops receiving a share.
func (c *Canoliq) ejectValidator(addr []byte) *contract.PluginError {
	registry, err := c.loadValidatorRegistry()
	if err != nil {
		return err
	}
	// Always clear any accrued incentives for the ejected address.
	deletes := []*contract.PluginDeleteOp{{Key: KeyForValidatorIncentives(addr)}}
	var sets []*contract.PluginSetOp
	if registry != nil {
		kept := registry.Entries[:0]
		for _, e := range registry.Entries {
			if bytes.Equal(e.Address, addr) {
				continue
			}
			kept = append(kept, e)
		}
		registry.Entries = kept
		bz, e := contract.Marshal(registry)
		if e != nil {
			return e
		}
		sets = append(sets, &contract.PluginSetOp{Key: KeyForValidatorRegistry(), Value: bz})
	}
	if _, err := c.plugin.StateWrite(c, &contract.PluginStateWriteRequest{Sets: sets, Deletes: deletes}); err != nil {
		return err
	}
	return nil
}

// loadValidatorRegistry reads the singleton validator registry. Returns
// (nil, nil) when absent.
func (c *Canoliq) loadValidatorRegistry() (*contract.ValidatorRegistry, *contract.PluginError) {
	q := qid()
	resp, err := c.plugin.StateRead(c, &contract.PluginStateReadRequest{
		Keys: []*contract.PluginKeyRead{{QueryId: q, Key: KeyForValidatorRegistry()}},
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	if len(resp.Results) == 0 || len(resp.Results[0].Entries) == 0 {
		return nil, nil
	}
	reg := new(contract.ValidatorRegistry)
	if e := contract.Unmarshal(resp.Results[0].Entries[0].Value, reg); e != nil {
		return nil, e
	}
	return reg, nil
}
