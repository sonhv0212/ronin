// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"

	"github.com/ethereum/go-ethereum/common"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

var emptyCodeHash = crypto.Keccak256Hash(nil)

/*
The State Transitioning Model

A state transition is a change made when a transaction is applied to the current world state
The state transitioning model does all the necessary work to work out a valid new state root.

1) Nonce handling
2) Pre pay gas
3) Create a new state object if the recipient is \0*32
4) Value transfer
== If contract creation ==

	4a) Attempt to run transaction data
	4b) If valid, use result as code for the new state object

== end ==
5) Run Script section
6) Derive new state root
*/
type StateTransition struct {
	gp         *GasPool
	msg        Message
	gas        uint64
	gasPrice   *big.Int
	gasFeeCap  *big.Int
	gasTipCap  *big.Int
	initialGas uint64
	value      *big.Int
	data       []byte
	state      vm.StateDB
	evm        *vm.EVM
}

// Message represents a message sent to a contract.
type Message interface {
	From() common.Address
	To() *common.Address

	GasPrice() *big.Int
	GasFeeCap() *big.Int
	GasTipCap() *big.Int
	Gas() uint64
	Value() *big.Int

	Nonce() uint64
	IsFake() bool
	Data() []byte
	AccessList() types.AccessList

	// In legacy transaction, this is the same as From.
	// In sponsored transaction, this is the payer's
	// address recovered from the payer's signature.
	Payer() common.Address
	ExpiredTime() uint64

	BlobGasFeeCap() *big.Int
	BlobHashes() []common.Hash
	AuthList() []types.Authorization
}

// ExecutionResult includes all output after executing given evm
// message no matter the execution itself is successful or not.
type ExecutionResult struct {
	UsedGas     uint64 // Total used gas, not including the refunded gas
	RefundedGas uint64 // Total gas refunded after execution
	Err         error  // Any error encountered during the execution(listed in core/vm/errors.go)
	ReturnData  []byte // Returned data from evm(function result or data supplied with revert opcode)
}

// Unwrap returns the internal evm error which allows us for further
// analysis outside.
func (result *ExecutionResult) Unwrap() error {
	return result.Err
}

// Failed returns the indicator whether the execution is successful or not
func (result *ExecutionResult) Failed() bool { return result.Err != nil }

// Return is a helper function to help caller distinguish between revert reason
// and function return. Return returns the data after execution if no error occurs.
func (result *ExecutionResult) Return() []byte {
	if result.Err != nil {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// Revert returns the concrete revert reason if the execution is aborted by `REVERT`
// opcode. Note the reason can be nil if no data supplied with revert opcode.
func (result *ExecutionResult) Revert() []byte {
	if result.Err != vm.ErrExecutionReverted {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// IntrinsicGas computes the 'intrinsic gas' for a message with the given data.
func IntrinsicGas(data []byte, accessList types.AccessList, authList []types.Authorization, isContractCreation bool, isHomestead, isEIP2028 bool, isEIP3860 bool) (uint64, error) {
	// Set the starting gas for the raw transaction
	var gas uint64
	if isContractCreation && isHomestead {
		gas = params.TxGasContractCreation
	} else {
		gas = params.TxGas
	}
	dataLen := uint64(len(data))
	// Bump the required gas by the amount of transactional data
	if dataLen > 0 {
		// Zero and non-zero bytes are priced differently
		var nz uint64
		for _, byt := range data {
			if byt != 0 {
				nz++
			}
		}
		// Make sure we don't exceed uint64 for all data combinations
		nonZeroGas := params.TxDataNonZeroGasFrontier
		if isEIP2028 {
			nonZeroGas = params.TxDataNonZeroGasEIP2028
		}
		if (math.MaxUint64-gas)/nonZeroGas < nz {
			return 0, ErrGasUintOverflow
		}
		gas += nz * nonZeroGas

		z := dataLen - nz
		if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
			return 0, ErrGasUintOverflow
		}
		gas += z * params.TxDataZeroGas

		if isContractCreation && isEIP3860 {
			lenWords := toWordSize(dataLen)
			if (math.MaxUint64-gas)/params.InitCodeWordGas < lenWords {
				return 0, ErrGasUintOverflow
			}
			gas += lenWords * params.InitCodeWordGas
		}
	}
	if accessList != nil {
		gas += uint64(len(accessList)) * params.TxAccessListAddressGas
		gas += uint64(accessList.StorageKeys()) * params.TxAccessListStorageKeyGas
	}
	if authList != nil {
		gas += uint64(len(authList)) * params.CallNewAccountGas
	}
	return gas, nil
}

// toWordSize returns the ceiled word size required for init code payment calculation.
func toWordSize(size uint64) uint64 {
	if size > math.MaxUint64-31 {
		return math.MaxUint64/32 + 1
	}

	return (size + 31) / 32
}

// NewStateTransition initialises and returns a new state transition object.
func NewStateTransition(evm *vm.EVM, msg Message, gp *GasPool) *StateTransition {
	return &StateTransition{
		gp:        gp,
		evm:       evm,
		msg:       msg,
		gasPrice:  msg.GasPrice(),
		gasFeeCap: msg.GasFeeCap(),
		gasTipCap: msg.GasTipCap(),
		value:     msg.Value(),
		data:      msg.Data(),
		state:     evm.StateDB,
	}
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
func ApplyMessage(evm *vm.EVM, msg Message, gp *GasPool) (*ExecutionResult, error) {
	return NewStateTransition(evm, msg, gp).TransitionDb()
}

// to returns the recipient of the message.
func (st *StateTransition) to() common.Address {
	if st.msg == nil || st.msg.To() == nil /* contract creation */ {
		return common.Address{}
	}
	return *st.msg.To()
}

func (st *StateTransition) buyGas() error {
	msg := st.msg
	gas := new(big.Int).SetUint64(msg.Gas())
	// In transaction types other than dynamic fee transaction,
	// effectiveGasFee is the same as maxGasFee. In dynamic fee
	// transaction, st.gasPrice is the already calculated gas
	// price based on block base fee, gas fee cap and gas tip cap
	effectiveGasFee := new(big.Int).Mul(gas, st.gasPrice)

	// balanceCheck is to calculate the total gas fee spent,
	// used to test against the sender balance.
	var (
		balanceCheck *big.Int
		blobFee      *big.Int
	)
	if st.gasFeeCap != nil {
		balanceCheck = new(big.Int).Mul(gas, st.gasFeeCap)
	} else {
		balanceCheck = new(big.Int).Mul(gas, st.gasPrice)
	}

	if msg.Payer() != msg.From() {
		// This is sponsored transaction, check gas fee with payer's balance and msg.value with sender's balance
		if have, want := st.state.GetBalance(msg.Payer()), balanceCheck; have.Cmp(want) < 0 {
			return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientPayerFunds, msg.Payer().Hex(), have, want)
		}

		if have, want := st.state.GetBalance(msg.From()), st.value; have.Cmp(want) < 0 {
			return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientSenderFunds, msg.From().Hex(), have, want)
		}
	} else {
		// include the logic for blob here
		if msg.BlobHashes() != nil {
			if blobGas := st.blobGasUsed(); blobGas > 0 {
				// Check that the user has enough funds to cover blobGasUsed * tx.BlobGasFeeCap
				blobBalanceCheck := new(big.Int).SetUint64(blobGas)
				blobBalanceCheck.Mul(blobBalanceCheck, msg.BlobGasFeeCap())
				balanceCheck.Add(balanceCheck, blobBalanceCheck)

				// Pay for blobGasUsed * actual blob fee
				blobFee = new(big.Int).SetUint64(blobGas)
				blobFee.Mul(blobFee, st.evm.Context.BlobBaseFee)
				effectiveGasFee.Add(effectiveGasFee, blobFee)
			}
		}
		balanceCheck.Add(balanceCheck, st.value)
		if have, want := st.state.GetBalance(msg.From()), balanceCheck; have.Cmp(want) < 0 {
			return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, msg.From().Hex(), have, want)
		}
	}

	if err := st.gp.SubGas(msg.Gas()); err != nil {
		return err
	}
	st.gas += msg.Gas()

	st.initialGas = msg.Gas()

	// Transfer blob gas fee to Ronin treasury address. If the blob tx fails,
	// the fee will not be refund.
	//
	// Unless the Ronin treasury address is specified, the blob fee amount will be burned.
	if st.evm.ChainConfig().RoninTreasuryAddress != nil && blobFee != nil && blobFee.Cmp(common.Big0) == 1 {
		st.state.AddBalance(*st.evm.ChainConfig().RoninTreasuryAddress, blobFee)
	}

	// Subtract the gas fee from balance of the fee payer,
	// the msg.value is transfered to the recipient in later step.
	st.state.SubBalance(msg.Payer(), effectiveGasFee)
	return nil
}

func (st *StateTransition) preCheck() error {
	msg := st.msg
	// Only check transactions that are not fake
	if !msg.IsFake() {
		// Make sure this transaction's nonce is correct.
		stNonce := st.state.GetNonce(msg.From())
		if msgNonce := msg.Nonce(); stNonce < msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooHigh,
				msg.From().Hex(), msgNonce, stNonce)
		} else if stNonce > msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooLow,
				msg.From().Hex(), msgNonce, stNonce)
		} else if stNonce+1 < stNonce {
			return fmt.Errorf("%w: address %v, nonce: %d", ErrNonceMax,
				msg.From().Hex(), stNonce)
		}
		// Make sure the sender is an EOA
		if codeHash := st.state.GetCodeHash(msg.From()); codeHash != emptyCodeHash && codeHash != (common.Hash{}) {
			return fmt.Errorf("%w: address %v, codehash: %s", ErrSenderNoEOA,
				msg.From().Hex(), codeHash)
		}
	}
	// Make sure that transaction gasFeeCap is greater than the baseFee (post london)
	if !st.evm.Config.IsSystemTransaction && st.evm.ChainConfig().IsLondon(st.evm.Context.BlockNumber) {
		// Skip the checks if gas fields are zero and baseFee was explicitly disabled (eth_call)
		if !st.evm.Config.NoBaseFee || st.gasFeeCap.BitLen() > 0 || st.gasTipCap.BitLen() > 0 {
			if l := st.gasFeeCap.BitLen(); l > 256 {
				return fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", ErrFeeCapVeryHigh,
					msg.From().Hex(), l)
			}
			if l := st.gasTipCap.BitLen(); l > 256 {
				return fmt.Errorf("%w: address %v, maxPriorityFeePerGas bit length: %d", ErrTipVeryHigh,
					msg.From().Hex(), l)
			}
			if st.gasFeeCap.Cmp(st.gasTipCap) < 0 {
				return fmt.Errorf("%w: address %v, maxPriorityFeePerGas: %s, maxFeePerGas: %s", ErrTipAboveFeeCap,
					msg.From().Hex(), st.gasTipCap, st.gasFeeCap)
			}
			// This will panic if baseFee is nil, but basefee presence is verified
			// as part of header validation.
			if st.gasFeeCap.Cmp(st.evm.Context.BaseFee) < 0 {
				return fmt.Errorf("%w: address %v, maxFeePerGas: %s baseFee: %s", ErrFeeCapTooLow,
					msg.From().Hex(), st.gasFeeCap, st.evm.Context.BaseFee)
			}
		}
	}

	// Check expired time, gas fee cap and tip cap in sponsored transaction
	if msg.Payer() != msg.From() {
		expiredTime := msg.ExpiredTime()
		if expiredTime != 0 && expiredTime <= st.evm.Context.Time {
			return fmt.Errorf("%w: expiredTime: %d, blockTime: %d", ErrExpiredSponsoredTx,
				msg.ExpiredTime(), st.evm.Context.Time)
		}

		// Before Venoki (base fee is 0), we have the rule that these 2 fields must be the same
		if !st.evm.ChainConfig().IsVenoki(st.evm.Context.BlockNumber) {
			if msg.GasTipCap().Cmp(msg.GasFeeCap()) != 0 {
				return ErrDifferentFeeCapTipCap
			}
		}
	}

	blobHashes := msg.BlobHashes()
	if blobHashes != nil {
		// The to field of a blob tx type is mandatory, and a `BlobTx` transaction internally
		// has it as a non-nillable value, so any msg derived from blob transaction has it non-nil.
		// However, messages created through RPC (eth_call) don't have this restriction.
		if msg.To() == nil {
			return ErrBlobTxCreate
		}
		if len(blobHashes) == 0 {
			return ErrMissingBlobHashes
		}
		for i, hash := range blobHashes {
			if !kzg4844.IsValidVersionedHash(hash[:]) {
				return fmt.Errorf("blob %d has invalid hash version", i)
			}
		}
	}

	// Check that the user is paying at least the current blob fee
	if st.evm.ChainConfig().IsCancun(st.evm.Context.BlockNumber) {
		if st.blobGasUsed() > 0 {
			// Skip the checks if gas fields are zero and blobBaseFee was explicitly disabled (eth_call)
			skipCheck := st.evm.Config.NoBaseFee && msg.BlobGasFeeCap().BitLen() == 0
			if !skipCheck {
				// This will panic if blobBaseFee is nil, but blobBaseFee presence
				// is verified as part of header validation.
				if msg.BlobGasFeeCap().Cmp(st.evm.Context.BlobBaseFee) < 0 {
					return fmt.Errorf("%w: address %v blobGasFeeCap: %v, blobBaseFee: %v", ErrBlobFeeCapTooLow,
						msg.From().Hex(), msg.BlobGasFeeCap(), st.evm.Context.BlobBaseFee)
				}
			}
		}
	}

	// Check that EIP-7702 authorization list signatures are well formed.
	if msg.AuthList() != nil {
		if msg.To() == nil {
			return fmt.Errorf("%w (sender %v)", ErrSetCodeTxCreate, msg.From())
		}
		if len(msg.AuthList()) == 0 {
			return fmt.Errorf("%w (sender %v)", ErrEmptyAuthList, msg.From())
		}
	}

	return st.buyGas()
}

// TransitionDb will transition the state by applying the current message and
// returning the evm execution result with following fields.
//
//   - used gas:
//     total gas used (including gas being refunded)
//   - returndata:
//     the returned data from evm
//   - concrete execution error:
//     various **EVM** error which aborts the execution,
//     e.g. ErrOutOfGas, ErrExecutionReverted
//
// However if any consensus issue encountered, return the error directly with
// nil evm execution result.
func (st *StateTransition) TransitionDb() (*ExecutionResult, error) {
	// First check this message satisfies all consensus rules before
	// applying the message. The rules include these clauses
	//
	// 1. the nonce of the message caller is correct
	// 2. payer has enough balance to cover transaction fee(gaslimit * gasprice)
	// payer signature is not expired
	// 3. the amount of gas required is available in the block
	// 4. the purchased gas is enough to cover intrinsic usage
	// 5. there is no overflow when calculating intrinsic gas
	// 6. caller has enough balance to cover asset transfer for **topmost** call

	// Check clauses 1-3, buy gas if everything is correct
	if err := st.preCheck(); err != nil {
		return nil, err
	}

	if tracer := st.evm.Config.Tracer; tracer != nil {
		var payer *common.Address
		if st.msg.From() != st.msg.Payer() {
			payerAddr := st.msg.Payer()
			payer = &payerAddr
		}
		tracer.CaptureTxStart(st.initialGas, payer)
		defer func() {
			tracer.CaptureTxEnd(st.gas)
		}()
	}

	msg := st.msg
	sender := vm.AccountRef(msg.From())
	rules := st.evm.ChainConfig().Rules(st.evm.Context.BlockNumber)
	contractCreation := msg.To() == nil

	// Check clauses 4-5, subtract intrinsic gas if everything is correct
	if !st.evm.Config.IsSystemTransaction {
		gas, err := IntrinsicGas(st.data, msg.AccessList(), msg.AuthList(), contractCreation, rules.IsHomestead, rules.IsIstanbul, rules.IsShanghai)
		if err != nil {
			return nil, err
		}
		if st.gas < gas {
			return nil, fmt.Errorf("%w: have %d, want %d", ErrIntrinsicGas, st.gas, gas)
		}
		st.gas -= gas
	}

	// Check clause 6
	if msg.Value().Sign() > 0 && !st.evm.Context.CanTransfer(st.state, msg.From(), msg.Value()) {
		return nil, fmt.Errorf("%w: address %v", ErrInsufficientFundsForTransfer, msg.From().Hex())
	}

	// Check whether the init code size has been exceeded.
	if rules.IsShanghai && contractCreation && len(st.data) > params.MaxInitCodeSize {
		return nil, fmt.Errorf("%w: code size %v limit %v", ErrMaxInitCodeSizeExceeded, len(st.data), params.MaxInitCodeSize)
	}

	// Execute the preparatory steps for state transition which includes:
	// - prepare accessList(post-berlin)
	// - reset transient storage(eip 1153)
	st.state.Prepare(rules, msg.From(), st.evm.Context.Coinbase, msg.To(), vm.ActivePrecompiles(rules), msg.AccessList())

	var (
		ret   []byte
		vmerr error // vm errors do not effect consensus and are therefore not assigned to err
	)
	if contractCreation {
		ret, _, st.gas, vmerr = st.evm.Create(sender, st.data, st.gas, st.value)
	} else {
		// Increment the nonce for the next transaction.
		st.state.SetNonce(msg.From(), st.state.GetNonce(msg.From())+1)

		// Apply EIP-7702 authorizations.
		if msg.AuthList() != nil {
			for _, auth := range msg.AuthList() {
				// Note errors are ignored, we simply skip invalid authorizations here.
				st.applyAuthorization(&auth)
			}
		}

		// Perform convenience warming of sender's delegation target. Although the
		// sender is already warmed in Prepare(..), it's possible a delegation to
		// the account was deployed during this transaction. To handle correctly,
		// simply wait until the final state of delegations is determined before
		// performing the resolution and warming.
		if addr, ok := types.ParseDelegation(st.state.GetCode(*msg.To())); ok {
			st.state.AddAddressToAccessList(addr)
		}

		// Execute the transaction's call.
		ret, st.gas, vmerr = st.evm.Call(sender, st.to(), st.data, st.gas, st.value)
	}

	var gasRefund uint64
	if !st.evm.Config.IsSystemTransaction {
		if !rules.IsLondon {
			// Before EIP-3529: refunds were capped to gasUsed / 2
			gasRefund = st.refundGas(params.RefundQuotient)
		} else {
			// After EIP-3529: refunds are capped to gasUsed / 5
			gasRefund = st.refundGas(params.RefundQuotientEIP3529)
		}

		effectiveTip := st.gasPrice
		if rules.IsLondon {
			effectiveTip = cmath.BigMin(st.gasTipCap, new(big.Int).Sub(st.gasFeeCap, st.evm.Context.BaseFee))
		}

		// if currentBlock is ConsortiumV2 then add balance to system address
		newEffectiveTip := new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), effectiveTip)
		if st.evm.ChainConfig().IsConsortiumV2(st.evm.Context.BlockNumber) {
			st.state.AddBalance(consensus.SystemAddress, newEffectiveTip)
		} else {
			st.state.AddBalance(st.evm.Context.Coinbase, newEffectiveTip)
		}

		// After Venoki the base fee is non-zero and the fee is transferred to treasury
		if rules.IsVenoki {
			treasuryAddress := st.evm.ChainConfig().RoninTreasuryAddress
			if treasuryAddress != nil {
				fee := new(big.Int).Mul(big.NewInt(int64(st.gasUsed())), st.evm.Context.BaseFee)
				st.state.AddBalance(*treasuryAddress, fee)
			}
		}
	}

	return &ExecutionResult{
		UsedGas:     st.gasUsed(),
		RefundedGas: gasRefund,
		Err:         vmerr,
		ReturnData:  ret,
	}, nil
}

// validateAuthorization validates an EIP-7702 authorization against the state.
func (st *StateTransition) validateAuthorization(auth *types.Authorization) (authority common.Address, err error) {
	// Verify chain ID is 0 or equal to current chain ID.
	if auth.ChainID != 0 && st.evm.ChainConfig().ChainID.Uint64() != auth.ChainID {
		return authority, ErrAuthorizationWrongChainID
	}
	// Limit nonce to 2^64-1 per EIP-2681.
	if auth.Nonce+1 < auth.Nonce {
		return authority, ErrAuthorizationNonceOverflow
	}
	// Validate signature values and recover authority.
	authority, err = auth.Authority()
	if err != nil {
		return authority, fmt.Errorf("%w: %v", ErrAuthorizationInvalidSignature, err)
	}
	// Check the authority account
	//  1) doesn't have code or has exisiting delegation
	//  2) matches the auth's nonce
	//
	// Note it is added to the access list even if the authorization is invalid.
	st.state.AddAddressToAccessList(authority)
	code := st.state.GetCode(authority)
	if _, ok := types.ParseDelegation(code); len(code) != 0 && !ok {
		return authority, ErrAuthorizationDestinationHasCode
	}
	if have := st.state.GetNonce(authority); have != auth.Nonce {
		return authority, ErrAuthorizationNonceMismatch
	}
	return authority, nil
}

// applyAuthorization applies an EIP-7702 code delegation to the state.
func (st *StateTransition) applyAuthorization(auth *types.Authorization) error {
	authority, err := st.validateAuthorization(auth)
	if err != nil {
		return err
	}

	// If the account already exists in state, refund the new account cost
	// charged in the intrinsic calculation.
	if st.state.Exist(authority) {
		st.state.AddRefund(params.CallNewAccountGas - params.TxAuthTupleGas)
	}

	// Update nonce and account code.
	st.state.SetNonce(authority, auth.Nonce+1)
	if auth.Address == (common.Address{}) {
		// Delegation to zero address means clear.
		st.state.SetCode(authority, nil)
		return nil
	}

	// Otherwise install delegation to auth.Address.
	st.state.SetCode(authority, types.AddressToDelegation(auth.Address))

	return nil
}

func (st *StateTransition) refundGas(refundQuotient uint64) uint64 {
	// Apply refund counter, capped to a refund quotient
	refund := st.gasUsed() / refundQuotient
	if refund > st.state.GetRefund() {
		refund = st.state.GetRefund()
	}
	st.gas += refund

	// Return ETH for remaining gas, exchanged at the original rate.
	remaining := new(big.Int).Mul(new(big.Int).SetUint64(st.gas), st.gasPrice)
	st.state.AddBalance(st.msg.Payer(), remaining)

	// Also return remaining gas to the block gas counter so it is
	// available for the next transaction.
	st.gp.AddGas(st.gas)

	return refund
}

// gasUsed returns the amount of gas used up by the state transition.
func (st *StateTransition) gasUsed() uint64 {
	return st.initialGas - st.gas
}

// blobGasUsed returns the amount of blob gas used by the message.
func (st *StateTransition) blobGasUsed() uint64 {
	return uint64(len(st.msg.BlobHashes()) * params.BlobTxBlobGasPerBlob)
}
