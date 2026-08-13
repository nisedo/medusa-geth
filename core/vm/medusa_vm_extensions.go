package vm

import "github.com/crytic/medusa-geth/common"

// MEDUSA: This entire file defines extensions for this package, used to power medusa features.

// CallFrameContext describes an EVM call or contract-creation frame whose result
// may be overridden by a Medusa extension.
//
// Input is only valid for the duration of the override callback. The callback
// must not modify or retain it.
type CallFrameContext struct {
	Depth  int
	OpCode OpCode
	From   common.Address
	To     common.Address
	Input  []byte
	Gas    uint64
	// GasRemaining is the raw frame's remaining gas. Result overrides cannot
	// change it.
	GasRemaining uint64
}

// CallFrameResult describes the result of an EVM call or contract-creation
// frame. CreateAddress is only meaningful for CREATE and CREATE2.
//
// Output is owned by the EVM. An override callback must not modify it in place.
type CallFrameResult struct {
	Output        []byte
	CreateAddress common.Address
	Err           error
}

// CallFrameResultOverrideFunc can replace the result presented to the calling
// opcode without changing the frame's actual execution result.
//
// The EVM always preserves rollback for an actual failure. If the callback
// changes an actual success into a presented failure, the EVM rolls the frame's
// state back before returning the replacement result. The only replacement
// error the EVM accepts is ErrExecutionReverted because other errors require
// different gas handling. A successful CREATE or CREATE2 also keeps its actual
// deployment address; callbacks may replace the address only when presenting an
// actual creation failure as a success.
type CallFrameResultOverrideFunc func(CallFrameContext, CallFrameResult) CallFrameResult

// CallCallerOverrideContext identifies a CALL before its caller is used for
// tracing, balance checks, value transfer, and child-frame construction.
type CallCallerOverrideContext struct {
	Depth int
	From  common.Address
	To    common.Address
}

// CallCallerOverrideFunc can replace the caller used by a CALL. The returned
// address becomes both the child frame's caller and the source of any value
// transfer.
type CallCallerOverrideFunc func(CallCallerOverrideContext) common.Address

// ConfigExtensions defines extended properties to be inherited by the Config.
// Note: Ensure any values which are added here and were not set do not change default EVM behaviour.
type ConfigExtensions struct {
	// OverrideCodeSizeCheck indicates whether code size checks should be disabled.
	OverrideCodeSizeCheck bool

	// AdditionalPrecompiles defines additional precompile contracts to be used by the VM.
	AdditionalPrecompiles map[common.Address]PrecompiledContract

	// ContractAddressOverrides maps the hash of a contract's init bytecode to the hardcoded address to where it should be
	// deployed. This allows for deterministic deployments of contracts.
	ContractAddressOverrides map[common.Hash]common.Address

	// CallFrameResultOverride can replace the result presented to a calling
	// opcode after a CALL, CALLCODE, DELEGATECALL, STATICCALL, CREATE, or CREATE2
	// frame finishes. A nil callback preserves standard EVM behaviour.
	//
	// ConfigExtensions is shared by EVMs constructed from the same Config.
	// Callbacks that capture mutable state must therefore be scoped to one chain
	// or made safe for concurrent use.
	CallFrameResultOverride CallFrameResultOverrideFunc

	// CallCallerOverride can replace the caller used by CALL before tracing,
	// account checks, value transfer, and child-frame creation. A nil callback
	// preserves standard EVM behaviour. CALLCODE, DELEGATECALL, STATICCALL, and
	// contract creation are not affected.
	//
	// ConfigExtensions is shared by EVMs constructed from the same Config.
	// Callbacks that capture mutable state must therefore be scoped to one chain
	// or made safe for concurrent use.
	CallCallerOverride CallCallerOverrideFunc
}

func (evm *EVM) hasCallFrameResultOverride() bool {
	return evm.Config.ConfigExtensions != nil && evm.Config.CallFrameResultOverride != nil
}

// finalizeCallFrame applies Medusa's optional result override, preserves the
// state rollback required by both the actual and presented outcomes, and emits
// the tracer exit event.
func (evm *EVM) finalizeCallFrame(context CallFrameContext, rawResult CallFrameResult, gasRemaining uint64, snapshot int, snapshotTaken bool) CallFrameResult {
	context.GasRemaining = gasRemaining
	presentedResult := rawResult
	if evm.Config.ConfigExtensions != nil && evm.Config.CallFrameResultOverride != nil {
		presentedResult = evm.Config.CallFrameResultOverride(context, rawResult)
	}

	// Only REVERT preserves remaining gas. Ignore unsupported replacement
	// errors so the callback cannot violate the EVM gas rules.
	if rawResult.Err == nil && presentedResult.Err != nil && presentedResult.Err != ErrExecutionReverted {
		presentedResult = rawResult
	} else if rawResult.Err != nil && presentedResult.Err != nil && presentedResult.Err != ErrExecutionReverted {
		presentedResult.Err = rawResult.Err
	}

	// A successful creation has already installed code at the raw address.
	// Returning another address would create a phantom deployment.
	if rawResult.Err == nil && rawResult.CreateAddress != (common.Address{}) {
		presentedResult.CreateAddress = rawResult.CreateAddress
	}

	// The normal call path has already reverted an actual failure. If an
	// override turns an actual success into a presented failure, perform the
	// equivalent rollback here.
	if rawResult.Err == nil && presentedResult.Err != nil && snapshotTaken {
		evm.StateDB.RevertToSnapshot(snapshot)
	}

	// Tracers must observe an error whenever state was rolled back. In
	// particular, a swallowed actual revert still needs to run Medusa's
	// non-StateDB restore hooks. For a synthetic failure, report the presented
	// error and output so the failure remains diagnosable.
	if evm.Config.Tracer != nil {
		tracedResult := presentedResult
		if rawResult.Err != nil {
			tracedResult = rawResult
		}
		evm.captureEnd(context.Depth, context.Gas, gasRemaining, tracedResult.Output, tracedResult.Err)
	}

	return presentedResult
}
