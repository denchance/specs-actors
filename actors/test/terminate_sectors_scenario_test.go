package test_test

import (
	"context"
	"github.com/stretchr/testify/assert"
	"testing"

	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/specs-actors/v2/actors/builtin"
	"github.com/filecoin-project/specs-actors/v2/actors/builtin/miner"
	"github.com/filecoin-project/specs-actors/v2/actors/builtin/power"
	"github.com/filecoin-project/specs-actors/v2/actors/builtin/verifreg"
	"github.com/filecoin-project/specs-actors/v2/actors/runtime/proof"
	tutil "github.com/filecoin-project/specs-actors/v2/support/testing"
	vm "github.com/filecoin-project/specs-actors/v2/support/vm"
)

func TestTerminateSectors(t *testing.T) {
	ctx := context.Background()
	v := vm.NewVMWithSingletons(ctx, t)
	addrs := vm.CreateAccounts(ctx, t, v, 4, big.Mul(big.NewInt(10_000), vm.FIL), 93837778)
	worker, verifier, unverifiedClient, verifiedClient := addrs[0], addrs[1], addrs[2], addrs[3]

	minerBalance := big.Mul(big.NewInt(1_000), vm.FIL)
	sectorNumber := abi.SectorNumber(100)
	sealedCid := tutil.MakeCID("100", &miner.SealedCIDPrefix)
	sealProof := abi.RegisteredSealProof_StackedDrg32GiBV1

	// create miner
	params := power.CreateMinerParams{
		Owner:         worker,
		Worker:        worker,
		SealProofType: sealProof,
		Peer:          abi.PeerID("not really a peer id"),
	}
	ret, code := v.ApplyMessage(addrs[0], builtin.StoragePowerActorAddr, minerBalance, builtin.MethodsPower.CreateMiner, &params)
	require.Equal(t, exitcode.Ok, code)

	minerAddrs, ok := ret.(*power.CreateMinerReturn)
	require.True(t, ok)

	//
	// publish verified and unverified deals
	//

	// register verifier then verified client
	addVerifierParams := verifreg.AddVerifierParams{
		Address:   verifier,
		Allowance: abi.NewStoragePower(32 << 40),
	}
	_, code = v.ApplyMessage(vm.VerifregRoot, builtin.VerifiedRegistryActorAddr, big.Zero(), builtin.MethodsVerifiedRegistry.AddVerifier, &addVerifierParams)
	require.Equal(t, exitcode.Ok, code)

	addClientParams := verifreg.AddVerifiedClientParams{
		Address:   verifiedClient,
		Allowance: abi.NewStoragePower(32 << 40),
	}
	_, code = v.ApplyMessage(verifier, builtin.VerifiedRegistryActorAddr, big.Zero(), builtin.MethodsVerifiedRegistry.AddVerifiedClient, &addClientParams)
	require.Equal(t, exitcode.Ok, code)

	// add market collateral for clients and miner
	collateral := big.Mul(big.NewInt(3), vm.FIL)
	_, code = v.ApplyMessage(unverifiedClient, builtin.StorageMarketActorAddr, collateral, builtin.MethodsMarket.AddBalance, &unverifiedClient)
	require.Equal(t, exitcode.Ok, code)
	_, code = v.ApplyMessage(verifiedClient, builtin.StorageMarketActorAddr, collateral, builtin.MethodsMarket.AddBalance, &verifiedClient)
	require.Equal(t, exitcode.Ok, code)
	collateral = big.Mul(big.NewInt(64), vm.FIL)
	_, code = v.ApplyMessage(worker, builtin.StorageMarketActorAddr, collateral, builtin.MethodsMarket.AddBalance, &minerAddrs.IDAddress)
	require.Equal(t, exitcode.Ok, code)

	// create 3 deals, some verified and some not
	dealIDs := []abi.DealID{}
	dealStart := v.GetEpoch() + miner.MaxProveCommitDuration[sealProof]
	deals := publishDeal(t, v, worker, verifiedClient, minerAddrs.IDAddress, "deal1", 1<<30, true, dealStart, 181*builtin.EpochsInDay)
	dealIDs = append(dealIDs, deals.IDs...)
	deals = publishDeal(t, v, worker, verifiedClient, minerAddrs.IDAddress, "deal2", 1<<32, true, dealStart, 200*builtin.EpochsInDay)
	dealIDs = append(dealIDs, deals.IDs...)
	deals = publishDeal(t, v, worker, unverifiedClient, minerAddrs.IDAddress, "deal3", 1<<34, false, dealStart, 210*builtin.EpochsInDay)
	dealIDs = append(dealIDs, deals.IDs...)

	stats := vm.GetNetworkStats(t, v)
	assert.Equal(t, int64(0), stats.MinerAboveMinPowerCount)
	assert.Equal(t, big.Zero(), stats.TotalRawBytePower)
	assert.Equal(t, big.Zero(), stats.TotalQualityAdjPower)
	assert.Equal(t, big.Zero(), stats.TotalBytesCommitted)
	assert.Equal(t, big.Zero(), stats.TotalQABytesCommitted)
	assert.Equal(t, big.Zero(), stats.TotalPledgeCollateral)
	assert.Equal(t, big.Zero(), stats.TotalClientLockedCollateral)
	assert.Equal(t, big.Zero(), stats.TotalProviderLockedCollateral)
	assert.Equal(t, big.Zero(), stats.TotalClientStorageFee)

	//
	// Precommit, Prove, Verify and PoSt sector with deals
	//

	// precommit sector with deals
	preCommitParams := miner.PreCommitSectorParams{
		SealProof:       sealProof,
		SectorNumber:    sectorNumber,
		SealedCID:       sealedCid,
		SealRandEpoch:   v.GetEpoch() - 1,
		DealIDs:         dealIDs,
		Expiration:      v.GetEpoch() + 220*builtin.EpochsInDay,
		ReplaceCapacity: false,
	}
	_, code = v.ApplyMessage(addrs[0], minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.PreCommitSector, &preCommitParams)
	require.Equal(t, exitcode.Ok, code)

	// advance time to min seal duration
	proveTime := v.GetEpoch() + miner.PreCommitChallengeDelay + 1
	v, _ = vm.AdvanceByDeadlineTillEpoch(t, v, minerAddrs.IDAddress, proveTime)

	// Prove commit sector after max seal duration
	v, err := v.WithEpoch(proveTime)
	require.NoError(t, err)
	proveCommitParams := miner.ProveCommitSectorParams{
		SectorNumber: sectorNumber,
	}
	_, code = v.ApplyMessage(worker, minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.ProveCommitSector, &proveCommitParams)
	require.Equal(t, exitcode.Ok, code)

	// In the same epoch, trigger cron to validate prove commit
	_, code = v.ApplyMessage(builtin.SystemActorAddr, builtin.CronActorAddr, big.Zero(), builtin.MethodsCron.EpochTick, nil)
	require.Equal(t, exitcode.Ok, code)

	// advance to proving period and submit post
	dlInfo, pIdx, v := vm.AdvanceTillProvingDeadline(t, v, minerAddrs.IDAddress, sectorNumber)
	submitParams := miner.SubmitWindowedPoStParams{
		Deadline: dlInfo.Index,
		Partitions: []miner.PoStPartition{{
			Index:   pIdx,
			Skipped: bitfield.New(),
		}},
		Proofs: []proof.PoStProof{{
			PoStProof: abi.RegisteredPoStProof_StackedDrgWindow32GiBV1,
		}},
		ChainCommitEpoch: dlInfo.Challenge,
		ChainCommitRand:  []byte("not really random"),
	}
	_, code = v.ApplyMessage(worker, minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.SubmitWindowedPoSt, &submitParams)
	require.Equal(t, exitcode.Ok, code)

	// proving period cron adds miner power
	v, err = v.WithEpoch(dlInfo.Last())
	require.NoError(t, err)
	_, code = v.ApplyMessage(builtin.SystemActorAddr, builtin.CronActorAddr, big.Zero(), builtin.MethodsCron.EpochTick, nil)
	require.Equal(t, exitcode.Ok, code)

	//
	// Terminate Sector
	//

	terminateParams := miner.TerminateSectorsParams{
		Terminations: []miner.TerminationDeclaration{{
			Deadline:  dlInfo.Index,
			Partition: pIdx,
			Sectors:   bitfield.NewFromSet([]uint64{uint64(sectorNumber)}),
		}},
	}

	_, code = v.ApplyMessage(worker, minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.TerminateSectors, &terminateParams)
	require.Equal(t, exitcode.Ok, code)

	noSubinvocations := []vm.ExpectInvocation{}
	vm.ExpectInvocation{
		To:     minerAddrs.IDAddress,
		Method: builtin.MethodsMiner.TerminateSectors,
		SubInvocations: []vm.ExpectInvocation{
			{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward, SubInvocations: noSubinvocations},
			{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower, SubInvocations: noSubinvocations},
			{To: builtin.BurntFundsActorAddr, Method: builtin.MethodSend, SubInvocations: noSubinvocations},
			{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.UpdatePledgeTotal, SubInvocations: noSubinvocations},
			{To: builtin.StorageMarketActorAddr, Method: builtin.MethodsMarket.OnMinerSectorsTerminate, SubInvocations: noSubinvocations},
			{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.UpdateClaimedPower, SubInvocations: noSubinvocations},
		},
	}.Matches(t, v.LastInvocation())

	// expect power, market and miner to be in base state
	minerBalances := vm.GetMinerBalances(t, v, minerAddrs.IDAddress)
	assert.Equal(t, big.Zero(), minerBalances.InitialPledge)
	assert.Equal(t, big.Zero(), minerBalances.PreCommitDeposit)

	stats = vm.GetNetworkStats(t, v)
	assert.Equal(t, int64(0), stats.MinerAboveMinPowerCount)
	assert.Equal(t, big.Zero(), stats.TotalRawBytePower)
	assert.Equal(t, big.Zero(), stats.TotalQualityAdjPower)
	assert.Equal(t, big.Zero(), stats.TotalBytesCommitted)
	assert.Equal(t, big.Zero(), stats.TotalQABytesCommitted)
	assert.Equal(t, big.Zero(), stats.TotalPledgeCollateral)
	assert.Equal(t, big.Zero(), stats.TotalClientLockedCollateral)
	assert.Equal(t, big.Zero(), stats.TotalProviderLockedCollateral)
	assert.Equal(t, big.Zero(), stats.TotalClientStorageFee)
}
