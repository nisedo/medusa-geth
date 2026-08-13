package vm

import (
	"bytes"
	"errors"
	"math/big"
	"testing"

	"github.com/crytic/medusa-geth/common"
	"github.com/crytic/medusa-geth/core/state"
	"github.com/crytic/medusa-geth/core/tracing"
	"github.com/crytic/medusa-geth/core/types"
	"github.com/crytic/medusa-geth/crypto"
	"github.com/crytic/medusa-geth/params"
	"github.com/holiman/uint256"
)

const callFrameTestGas = uint64(1_000_000)

var (
	callFrameTestCaller = common.HexToAddress("0x100")
	callFrameTestTarget = common.HexToAddress("0x200")
)

func newCallFrameTestEVM(t *testing.T, targetCode []byte, override CallFrameResultOverrideFunc, hooks *tracing.Hooks) (*EVM, *state.StateDB) {
	t.Helper()

	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("create state database: %v", err)
	}
	stateDB.CreateAccount(callFrameTestCaller)
	stateDB.SetBalance(callFrameTestCaller, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
	if targetCode != nil {
		stateDB.CreateAccount(callFrameTestTarget)
		stateDB.SetCode(callFrameTestTarget, targetCode)
	}
	stateDB.Finalise(true)

	blockContext := BlockContext{
		CanTransfer: func(db StateDB, from common.Address, amount *uint256.Int) bool {
			return db.GetBalance(from).Cmp(amount) >= 0
		},
		Transfer: func(db StateDB, from common.Address, to common.Address, amount *uint256.Int) {
			if amount.IsZero() {
				return
			}
			db.SubBalance(from, amount, tracing.BalanceChangeTransfer)
			db.AddBalance(to, amount, tracing.BalanceChangeTransfer)
		},
		BlockNumber: big.NewInt(0),
	}
	config := Config{
		Tracer: hooks,
		ConfigExtensions: &ConfigExtensions{
			ContractAddressOverrides: make(map[common.Hash]common.Address),
			CallFrameResultOverride:  override,
		},
	}
	return NewEVM(blockContext, stateDB, params.AllEthashProtocolChanges, config), stateDB
}

func TestCallFrameResultOverrideCoversAllCallKinds(t *testing.T) {
	returnWordCode := common.FromHex("0x602a60005260206000f3")
	createCode := common.FromHex("0x6001600c60003960016000f300")
	callInput := []byte{0x01, 0x02, 0x03}
	overrideOutput := []byte{0xaa, 0xbb}
	overrideAddress := common.HexToAddress("0x1")
	salt := uint256.NewInt(7)

	tests := []struct {
		name  string
		op    OpCode
		input []byte
		to    func() common.Address
		run   func(*EVM) ([]byte, common.Address, uint64, error)
	}{
		{
			name:  "call",
			op:    CALL,
			input: callInput,
			to:    func() common.Address { return callFrameTestTarget },
			run: func(evm *EVM) ([]byte, common.Address, uint64, error) {
				output, gas, err := evm.Call(callFrameTestCaller, callFrameTestTarget, callInput, callFrameTestGas, new(uint256.Int))
				return output, common.Address{}, gas, err
			},
		},
		{
			name:  "callcode",
			op:    CALLCODE,
			input: callInput,
			to:    func() common.Address { return callFrameTestTarget },
			run: func(evm *EVM) ([]byte, common.Address, uint64, error) {
				output, gas, err := evm.CallCode(callFrameTestCaller, callFrameTestTarget, callInput, callFrameTestGas, new(uint256.Int))
				return output, common.Address{}, gas, err
			},
		},
		{
			name:  "delegatecall",
			op:    DELEGATECALL,
			input: callInput,
			to:    func() common.Address { return callFrameTestTarget },
			run: func(evm *EVM) ([]byte, common.Address, uint64, error) {
				output, gas, err := evm.DelegateCall(common.HexToAddress("0x300"), callFrameTestCaller, callFrameTestTarget, callInput, callFrameTestGas, new(uint256.Int))
				return output, common.Address{}, gas, err
			},
		},
		{
			name:  "staticcall",
			op:    STATICCALL,
			input: callInput,
			to:    func() common.Address { return callFrameTestTarget },
			run: func(evm *EVM) ([]byte, common.Address, uint64, error) {
				output, gas, err := evm.StaticCall(callFrameTestCaller, callFrameTestTarget, callInput, callFrameTestGas)
				return output, common.Address{}, gas, err
			},
		},
		{
			name:  "create",
			op:    CREATE,
			input: createCode,
			to: func() common.Address {
				return crypto.CreateAddress(callFrameTestCaller, 0)
			},
			run: func(evm *EVM) ([]byte, common.Address, uint64, error) {
				return evm.Create(callFrameTestCaller, createCode, callFrameTestGas, new(uint256.Int))
			},
		},
		{
			name:  "create2",
			op:    CREATE2,
			input: createCode,
			to: func() common.Address {
				return crypto.CreateAddress2(callFrameTestCaller, salt.Bytes32(), crypto.Keccak256(createCode))
			},
			run: func(evm *EVM) ([]byte, common.Address, uint64, error) {
				return evm.Create2(callFrameTestCaller, createCode, callFrameTestGas, new(uint256.Int), salt)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				callbackCount int
				rawGas        uint64
			)
			override := func(context CallFrameContext, result CallFrameResult) CallFrameResult {
				callbackCount++
				if context.Depth != 3 {
					t.Errorf("depth mismatch: have %d, want 3", context.Depth)
				}
				if context.OpCode != test.op {
					t.Errorf("opcode mismatch: have %s, want %s", context.OpCode, test.op)
				}
				if context.From != callFrameTestCaller {
					t.Errorf("from mismatch: have %s, want %s", context.From, callFrameTestCaller)
				}
				if context.To != test.to() {
					t.Errorf("to mismatch: have %s, want %s", context.To, test.to())
				}
				if !bytes.Equal(context.Input, test.input) {
					t.Errorf("input mismatch: have %x, want %x", context.Input, test.input)
				}
				if context.Gas != callFrameTestGas {
					t.Errorf("start gas mismatch: have %d, want %d", context.Gas, callFrameTestGas)
				}
				if result.Err != nil {
					t.Errorf("unexpected raw error: %v", result.Err)
				}
				if test.op == CREATE || test.op == CREATE2 {
					if result.CreateAddress != test.to() {
						t.Errorf("raw create address mismatch: have %s, want %s", result.CreateAddress, test.to())
					}
				} else if result.CreateAddress != (common.Address{}) {
					t.Errorf("unexpected create address for %s: %s", test.op, result.CreateAddress)
				}
				rawGas = context.GasRemaining
				result.Output = overrideOutput
				result.CreateAddress = overrideAddress
				return result
			}

			evm, _ := newCallFrameTestEVM(t, returnWordCode, override, nil)
			evm.depth = 3
			output, address, gas, err := test.run(evm)
			if err != nil {
				t.Fatalf("presented call failed: %v", err)
			}
			if callbackCount != 1 {
				t.Fatalf("callback count mismatch: have %d, want 1", callbackCount)
			}
			if !bytes.Equal(output, overrideOutput) {
				t.Errorf("presented output mismatch: have %x, want %x", output, overrideOutput)
			}
			if gas != rawGas {
				t.Errorf("presented gas mismatch: have %d, want %d", gas, rawGas)
			}
			if test.op == CREATE || test.op == CREATE2 {
				if address != test.to() {
					t.Errorf("presented create address mismatch: have %s, want %s", address, test.to())
				}
			} else if address != (common.Address{}) {
				t.Errorf("unexpected returned address for %s: %s", test.op, address)
			}
		})
	}
}

func TestNilCallFrameResultOverridePreservesResult(t *testing.T) {
	expectedOutput := common.FromHex("0x2a")
	returnCode := common.FromHex("0x602a6000526001601ff3")
	evm, _ := newCallFrameTestEVM(t, returnCode, nil, nil)

	output, gas, err := evm.Call(callFrameTestCaller, callFrameTestTarget, nil, callFrameTestGas, new(uint256.Int))
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if !bytes.Equal(output, expectedOutput) {
		t.Errorf("output mismatch: have %x, want %x", output, expectedOutput)
	}
	if gas >= callFrameTestGas {
		t.Errorf("call did not consume gas: have %d, start %d", gas, callFrameTestGas)
	}
}

func TestCallFrameResultOverrideRejectsExceptionalErrorForRawSuccess(t *testing.T) {
	expectedOutput := common.FromHex("0x2a")
	returnCode := common.FromHex("0x602a6000526001601ff3")
	override := func(_ CallFrameContext, result CallFrameResult) CallFrameResult {
		result.Output = []byte{0xff}
		result.Err = ErrOutOfGas
		return result
	}
	evm, _ := newCallFrameTestEVM(t, returnCode, override, nil)

	output, gas, err := evm.Call(callFrameTestCaller, callFrameTestTarget, nil, callFrameTestGas, new(uint256.Int))
	if err != nil {
		t.Fatalf("unsupported replacement error changed raw success: %v", err)
	}
	if !bytes.Equal(output, expectedOutput) {
		t.Errorf("output mismatch: have %x, want %x", output, expectedOutput)
	}
	if gas == 0 {
		t.Error("unsupported replacement error consumed all remaining gas")
	}
}

func TestCallFrameResultOverridePreservesRawRevertAndTrace(t *testing.T) {
	revertData := common.FromHex("0xdeadbeef")
	revertAfterStoreCode := common.FromHex("0x600160005563deadbeef6000526004601cfd")
	presentedOutput := []byte{0xaa}
	var (
		tracedOutput   []byte
		tracedErr      error
		tracedReverted bool
	)
	hooks := &tracing.Hooks{
		OnExit: func(_ int, output []byte, _ uint64, err error, reverted bool) {
			tracedOutput = bytes.Clone(output)
			tracedErr = err
			tracedReverted = reverted
		},
	}
	override := func(_ CallFrameContext, result CallFrameResult) CallFrameResult {
		if !errors.Is(result.Err, ErrExecutionReverted) {
			t.Errorf("raw error mismatch: have %v, want %v", result.Err, ErrExecutionReverted)
		}
		if !bytes.Equal(result.Output, revertData) {
			t.Errorf("raw output mismatch: have %x, want %x", result.Output, revertData)
		}
		result.Output = presentedOutput
		result.Err = nil
		return result
	}

	evm, stateDB := newCallFrameTestEVM(t, revertAfterStoreCode, override, hooks)
	output, _, err := evm.Call(callFrameTestCaller, callFrameTestTarget, nil, callFrameTestGas, new(uint256.Int))
	if err != nil {
		t.Fatalf("presented call failed: %v", err)
	}
	if !bytes.Equal(output, presentedOutput) {
		t.Errorf("presented output mismatch: have %x, want %x", output, presentedOutput)
	}
	if state := stateDB.GetState(callFrameTestTarget, common.Hash{}); state != (common.Hash{}) {
		t.Errorf("raw revert leaked storage: have %s, want zero", state)
	}
	if !errors.Is(tracedErr, ErrExecutionReverted) {
		t.Errorf("tracer error mismatch: have %v, want %v", tracedErr, ErrExecutionReverted)
	}
	if !bytes.Equal(tracedOutput, revertData) {
		t.Errorf("tracer output mismatch: have %x, want raw %x", tracedOutput, revertData)
	}
	if !tracedReverted {
		t.Error("tracer did not mark the raw revert as rolled back")
	}
}

func TestCallFrameResultOverrideRollsBackSyntheticFailure(t *testing.T) {
	successData := common.FromHex("0xcafebabe")
	storeThenReturnCode := common.FromHex("0x600160005563cafebabe6000526004601cf3")
	failureData := []byte("expected revert failure")
	var (
		tracedOutput   []byte
		tracedErr      error
		tracedReverted bool
	)
	hooks := &tracing.Hooks{
		OnExit: func(_ int, output []byte, _ uint64, err error, reverted bool) {
			tracedOutput = bytes.Clone(output)
			tracedErr = err
			tracedReverted = reverted
		},
	}
	var stateDB *state.StateDB
	override := func(_ CallFrameContext, result CallFrameResult) CallFrameResult {
		if result.Err != nil {
			t.Errorf("unexpected raw error: %v", result.Err)
		}
		if !bytes.Equal(result.Output, successData) {
			t.Errorf("raw output mismatch: have %x, want %x", result.Output, successData)
		}
		if stored := stateDB.GetState(callFrameTestTarget, common.Hash{}); stored != common.BigToHash(big.NewInt(1)) {
			t.Errorf("override ran after rollback: stored value is %s", stored)
		}
		result.Output = failureData
		result.Err = ErrExecutionReverted
		return result
	}

	evm, testStateDB := newCallFrameTestEVM(t, storeThenReturnCode, override, hooks)
	stateDB = testStateDB
	output, _, err := evm.Call(callFrameTestCaller, callFrameTestTarget, nil, callFrameTestGas, new(uint256.Int))
	if !errors.Is(err, ErrExecutionReverted) {
		t.Fatalf("presented error mismatch: have %v, want %v", err, ErrExecutionReverted)
	}
	if !bytes.Equal(output, failureData) {
		t.Errorf("presented output mismatch: have %x, want %x", output, failureData)
	}
	if stored := stateDB.GetState(callFrameTestTarget, common.Hash{}); stored != (common.Hash{}) {
		t.Errorf("synthetic failure leaked storage: have %s, want zero", stored)
	}
	if !errors.Is(tracedErr, ErrExecutionReverted) {
		t.Errorf("tracer error mismatch: have %v, want %v", tracedErr, ErrExecutionReverted)
	}
	if !bytes.Equal(tracedOutput, failureData) {
		t.Errorf("tracer output mismatch: have %x, want presented %x", tracedOutput, failureData)
	}
	if !tracedReverted {
		t.Error("tracer did not mark the synthetic failure as rolled back")
	}
}

func TestCallFrameResultOverrideIsPresentedToCallingOpcode(t *testing.T) {
	const dummyOutputSize = 8_192

	targetRevertCode := common.FromHex("0x60006000fd")
	dummyOutput := make([]byte, dummyOutputSize)
	override := func(context CallFrameContext, result CallFrameResult) CallFrameResult {
		if context.Depth == 1 && context.To == callFrameTestTarget {
			if !errors.Is(result.Err, ErrExecutionReverted) {
				t.Errorf("raw child error mismatch: have %v, want %v", result.Err, ErrExecutionReverted)
			}
			result.Output = dummyOutput
			result.Err = nil
		}
		return result
	}

	evm, stateDB := newCallFrameTestEVM(t, targetRevertCode, override, nil)
	outerAddress := common.HexToAddress("0x300")
	outerCode := []byte{
		byte(PUSH1), 0x20, // output size
		byte(PUSH1), 0x00, // output offset
		byte(PUSH1), 0x00, // input size
		byte(PUSH1), 0x00, // input offset
		byte(PUSH1), 0x00, // value
		byte(PUSH20),
	}
	outerCode = append(outerCode, callFrameTestTarget.Bytes()...)
	outerCode = append(outerCode,
		byte(PUSH3), 0x0f, 0x42, 0x40, // gas
		byte(CALL),
		byte(PUSH1), 0x00, byte(MSTORE), // store CALL success bit
		byte(RETURNDATASIZE), byte(PUSH1), 0x20, byte(MSTORE),
		byte(PUSH1), 0x40, byte(PUSH1), 0x00, byte(RETURN),
	)
	stateDB.CreateAccount(outerAddress)
	stateDB.SetCode(outerAddress, outerCode)
	stateDB.Finalise(true)

	output, _, err := evm.Call(callFrameTestCaller, outerAddress, nil, 3_000_000, new(uint256.Int))
	if err != nil {
		t.Fatalf("outer call failed: %v", err)
	}
	if len(output) != 64 {
		t.Fatalf("outer output length mismatch: have %d, want 64", len(output))
	}
	if success := new(big.Int).SetBytes(output[:32]); success.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("CALL success bit mismatch: have %s, want 1", success)
	}
	if returnDataSize := new(big.Int).SetBytes(output[32:]); returnDataSize.Cmp(big.NewInt(dummyOutputSize)) != 0 {
		t.Errorf("RETURNDATASIZE mismatch: have %s, want %d", returnDataSize, dummyOutputSize)
	}
}

func TestCallCallerOverrideUsesCallerForValueSemantics(t *testing.T) {
	const (
		callValue          = uint64(7)
		fundedBalance      = uint64(11)
		initialSinkBalance = uint64(3)
	)
	outerAddress := common.HexToAddress("0x300")
	actorAddress := common.HexToAddress("0xbeef")

	tests := []struct {
		name                string
		callerOverride      bool
		revertSink          bool
		outerBalance        uint64
		actorBalance        uint64
		wantSuccess         uint64
		wantOuterBalance    uint64
		wantActorBalance    uint64
		wantSinkBalance     uint64
		wantStoredCaller    common.Address
		wantStoredValue     uint64
		wantTracedCaller    common.Address
		wantOverrideMatches int
	}{
		{
			name:             "nil override preserves ordinary caller and payer",
			outerBalance:     fundedBalance,
			wantSuccess:      1,
			wantOuterBalance: fundedBalance - callValue,
			wantSinkBalance:  initialSinkBalance + callValue,
			wantStoredCaller: outerAddress,
			wantStoredValue:  callValue,
			wantTracedCaller: outerAddress,
		},
		{
			name:                "empty actor fails despite funded outer contract",
			callerOverride:      true,
			outerBalance:        fundedBalance,
			wantOuterBalance:    fundedBalance,
			wantSinkBalance:     initialSinkBalance,
			wantTracedCaller:    actorAddress,
			wantOverrideMatches: 1,
		},
		{
			name:                "funded actor pays despite empty outer contract",
			callerOverride:      true,
			actorBalance:        fundedBalance,
			wantSuccess:         1,
			wantActorBalance:    fundedBalance - callValue,
			wantSinkBalance:     initialSinkBalance + callValue,
			wantStoredCaller:    actorAddress,
			wantStoredValue:     callValue,
			wantTracedCaller:    actorAddress,
			wantOverrideMatches: 1,
		},
		{
			name:                "revert restores actor transfer and sink storage",
			callerOverride:      true,
			revertSink:          true,
			actorBalance:        fundedBalance,
			wantActorBalance:    fundedBalance,
			wantSinkBalance:     initialSinkBalance,
			wantTracedCaller:    actorAddress,
			wantOverrideMatches: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var tracedCaller common.Address
			var tracedValue *big.Int
			hooks := &tracing.Hooks{
				OnEnter: func(depth int, _ byte, from common.Address, to common.Address, _ []byte, _ uint64, value *big.Int) {
					if depth == 1 && to == callFrameTestTarget {
						tracedCaller = from
						tracedValue = new(big.Int).Set(value)
					}
				},
			}
			sinkCode := []byte{
				byte(CALLER), byte(PUSH1), 0x00, byte(SSTORE),
				byte(CALLVALUE), byte(PUSH1), 0x01, byte(SSTORE),
			}
			if test.revertSink {
				sinkCode = append(sinkCode, byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(REVERT))
			} else {
				sinkCode = append(sinkCode, byte(STOP))
			}

			evm, stateDB := newCallFrameTestEVM(t, sinkCode, nil, hooks)
			stateDB.CreateAccount(outerAddress)
			stateDB.SetCode(outerAddress, callCallerOverrideOuterCode(callFrameTestTarget, byte(callValue)))
			stateDB.SetBalance(outerAddress, uint256.NewInt(test.outerBalance), tracing.BalanceChangeUnspecified)
			stateDB.CreateAccount(actorAddress)
			stateDB.SetBalance(actorAddress, uint256.NewInt(test.actorBalance), tracing.BalanceChangeUnspecified)
			stateDB.SetBalance(callFrameTestTarget, uint256.NewInt(initialSinkBalance), tracing.BalanceChangeUnspecified)
			stateDB.Finalise(true)

			overrideMatches := 0
			if test.callerOverride {
				evm.Config.CallCallerOverride = func(context CallCallerOverrideContext) common.Address {
					if context.Depth == 1 && context.From == outerAddress && context.To == callFrameTestTarget {
						overrideMatches++
						return actorAddress
					}
					return context.From
				}
			}

			output, _, err := evm.Call(callFrameTestCaller, outerAddress, nil, 3_000_000, new(uint256.Int))
			if err != nil {
				t.Fatalf("outer call failed: %v", err)
			}
			if len(output) != 32 {
				t.Fatalf("outer output length mismatch: have %d, want 32", len(output))
			}
			if success := new(big.Int).SetBytes(output).Uint64(); success != test.wantSuccess {
				t.Errorf("CALL success bit mismatch: have %d, want %d", success, test.wantSuccess)
			}
			if have := stateDB.GetBalance(outerAddress).Uint64(); have != test.wantOuterBalance {
				t.Errorf("outer balance mismatch: have %d, want %d", have, test.wantOuterBalance)
			}
			if have := stateDB.GetBalance(actorAddress).Uint64(); have != test.wantActorBalance {
				t.Errorf("actor balance mismatch: have %d, want %d", have, test.wantActorBalance)
			}
			if have := stateDB.GetBalance(callFrameTestTarget).Uint64(); have != test.wantSinkBalance {
				t.Errorf("sink balance mismatch: have %d, want %d", have, test.wantSinkBalance)
			}
			if have := common.BytesToAddress(stateDB.GetState(callFrameTestTarget, common.Hash{}).Bytes()); have != test.wantStoredCaller {
				t.Errorf("stored caller mismatch: have %s, want %s", have, test.wantStoredCaller)
			}
			if have := new(big.Int).SetBytes(stateDB.GetState(callFrameTestTarget, common.BigToHash(big.NewInt(1))).Bytes()).Uint64(); have != test.wantStoredValue {
				t.Errorf("stored value mismatch: have %d, want %d", have, test.wantStoredValue)
			}
			if tracedCaller != test.wantTracedCaller {
				t.Errorf("traced caller mismatch: have %s, want %s", tracedCaller, test.wantTracedCaller)
			}
			if tracedValue == nil || tracedValue.Uint64() != callValue {
				t.Errorf("traced value mismatch: have %v, want %d", tracedValue, callValue)
			}
			if overrideMatches != test.wantOverrideMatches {
				t.Errorf("override match count mismatch: have %d, want %d", overrideMatches, test.wantOverrideMatches)
			}
		})
	}
}

func callCallerOverrideOuterCode(target common.Address, value byte) []byte {
	code := []byte{
		byte(PUSH1), 0x00, // output size
		byte(PUSH1), 0x00, // output offset
		byte(PUSH1), 0x00, // input size
		byte(PUSH1), 0x00, // input offset
		byte(PUSH1), value,
		byte(PUSH20),
	}
	code = append(code, target.Bytes()...)
	return append(code,
		byte(PUSH3), 0x0f, 0x42, 0x40, // gas
		byte(CALL),
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	)
}

func TestCreateResultOverrideRollback(t *testing.T) {
	t.Run("swallowed revert keeps raw rollback", func(t *testing.T) {
		revertingInitCode := common.FromHex("0x600160005560006000fd")
		dummyAddress := common.HexToAddress("0x1")
		predictedAddress := crypto.CreateAddress(callFrameTestCaller, 0)
		override := func(context CallFrameContext, result CallFrameResult) CallFrameResult {
			if context.To != predictedAddress {
				t.Errorf("predicted address mismatch: have %s, want %s", context.To, predictedAddress)
			}
			if !errors.Is(result.Err, ErrExecutionReverted) {
				t.Errorf("raw error mismatch: have %v, want %v", result.Err, ErrExecutionReverted)
			}
			if result.CreateAddress != predictedAddress {
				t.Errorf("raw create address mismatch: have %s, want %s", result.CreateAddress, predictedAddress)
			}
			result.Output = nil
			result.CreateAddress = dummyAddress
			result.Err = nil
			return result
		}

		evm, stateDB := newCallFrameTestEVM(t, nil, override, nil)
		_, address, _, err := evm.Create(callFrameTestCaller, revertingInitCode, callFrameTestGas, new(uint256.Int))
		if err != nil {
			t.Fatalf("presented create failed: %v", err)
		}
		if address != dummyAddress {
			t.Errorf("presented create address mismatch: have %s, want %s", address, dummyAddress)
		}
		if stateDB.Exist(predictedAddress) {
			t.Errorf("raw revert left a contract account at %s", predictedAddress)
		}
		if nonce := stateDB.GetNonce(callFrameTestCaller); nonce != 1 {
			t.Errorf("creator nonce mismatch: have %d, want 1", nonce)
		}
	})

	t.Run("synthetic failure removes successful deployment", func(t *testing.T) {
		createCode := common.FromHex("0x6001600c60003960016000f300")
		failureData := []byte("expected revert failure")
		predictedAddress := crypto.CreateAddress(callFrameTestCaller, 0)
		var stateDB *state.StateDB
		override := func(context CallFrameContext, result CallFrameResult) CallFrameResult {
			if result.Err != nil {
				t.Errorf("unexpected raw error: %v", result.Err)
			}
			if code := stateDB.GetCode(context.To); !bytes.Equal(code, []byte{0x00}) {
				t.Errorf("override ran before successful deployment: code is %x", code)
			}
			result.Output = failureData
			result.Err = ErrExecutionReverted
			return result
		}

		evm, testStateDB := newCallFrameTestEVM(t, nil, override, nil)
		stateDB = testStateDB
		output, _, _, err := evm.Create(callFrameTestCaller, createCode, callFrameTestGas, new(uint256.Int))
		if !errors.Is(err, ErrExecutionReverted) {
			t.Fatalf("presented error mismatch: have %v, want %v", err, ErrExecutionReverted)
		}
		if !bytes.Equal(output, failureData) {
			t.Errorf("presented output mismatch: have %x, want %x", output, failureData)
		}
		if stateDB.Exist(predictedAddress) {
			t.Errorf("synthetic failure left a contract account at %s", predictedAddress)
		}
		if nonce := stateDB.GetNonce(callFrameTestCaller); nonce != 1 {
			t.Errorf("creator nonce mismatch: have %d, want 1", nonce)
		}
	})
}

func TestCreateResultOverrideIsPresentedToCreatingOpcode(t *testing.T) {
	revertingInitCode := common.FromHex("0x60006000fd")
	dummyAddress := common.HexToAddress("0x1")
	override := func(context CallFrameContext, result CallFrameResult) CallFrameResult {
		if context.Depth == 1 && context.OpCode == CREATE {
			if !errors.Is(result.Err, ErrExecutionReverted) {
				t.Errorf("raw CREATE error mismatch: have %v, want %v", result.Err, ErrExecutionReverted)
			}
			result.Output = nil
			result.CreateAddress = dummyAddress
			result.Err = nil
		}
		return result
	}

	evm, stateDB := newCallFrameTestEVM(t, nil, override, nil)
	outerAddress := common.HexToAddress("0x300")
	outerCode := []byte{
		byte(PUSH1), byte(len(revertingInitCode)),
		byte(PUSH1), 0x00, // init-code offset, filled below
		byte(PUSH1), 0x00,
		byte(CODECOPY),
		byte(PUSH1), byte(len(revertingInitCode)),
		byte(PUSH1), 0x00,
		byte(PUSH1), 0x00,
		byte(CREATE),
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(RETURNDATASIZE), byte(PUSH1), 0x20, byte(MSTORE),
		byte(PUSH1), 0x40, byte(PUSH1), 0x00, byte(RETURN),
	}
	outerCode[3] = byte(len(outerCode))
	outerCode = append(outerCode, revertingInitCode...)
	stateDB.CreateAccount(outerAddress)
	stateDB.SetCode(outerAddress, outerCode)
	stateDB.Finalise(true)

	output, _, err := evm.Call(callFrameTestCaller, outerAddress, nil, 3_000_000, new(uint256.Int))
	if err != nil {
		t.Fatalf("outer call failed: %v", err)
	}
	if len(output) != 64 {
		t.Fatalf("outer output length mismatch: have %d, want 64", len(output))
	}
	if address := common.BytesToAddress(output[:32]); address != dummyAddress {
		t.Errorf("CREATE address mismatch: have %s, want %s", address, dummyAddress)
	}
	if returnDataSize := new(big.Int).SetBytes(output[32:]); returnDataSize.Sign() != 0 {
		t.Errorf("RETURNDATASIZE mismatch: have %s, want 0", returnDataSize)
	}
	predictedAddress := crypto.CreateAddress(outerAddress, 0)
	if stateDB.Exist(predictedAddress) {
		t.Errorf("raw reverted CREATE left a contract account at %s", predictedAddress)
	}
}

func TestCallFrameResultOverrideRunsForNonInterpreterAndEarlyResults(t *testing.T) {
	t.Run("empty code", func(t *testing.T) {
		callbackCount := 0
		override := func(_ CallFrameContext, result CallFrameResult) CallFrameResult {
			callbackCount++
			if result.Err != nil {
				t.Errorf("unexpected raw error: %v", result.Err)
			}
			return result
		}
		evm, _ := newCallFrameTestEVM(t, nil, override, nil)
		if _, _, err := evm.Call(callFrameTestCaller, callFrameTestTarget, nil, callFrameTestGas, new(uint256.Int)); err != nil {
			t.Fatalf("empty-code call failed: %v", err)
		}
		if callbackCount != 1 {
			t.Errorf("callback count mismatch: have %d, want 1", callbackCount)
		}
	})

	t.Run("precompile", func(t *testing.T) {
		input := []byte{0x01, 0x02, 0x03}
		callbackCount := 0
		override := func(context CallFrameContext, result CallFrameResult) CallFrameResult {
			callbackCount++
			if context.To != common.BytesToAddress([]byte{4}) {
				t.Errorf("precompile address mismatch: %s", context.To)
			}
			if !bytes.Equal(result.Output, input) {
				t.Errorf("identity output mismatch: have %x, want %x", result.Output, input)
			}
			return result
		}
		evm, _ := newCallFrameTestEVM(t, nil, override, nil)
		if _, _, err := evm.Call(callFrameTestCaller, common.BytesToAddress([]byte{4}), input, callFrameTestGas, new(uint256.Int)); err != nil {
			t.Fatalf("precompile call failed: %v", err)
		}
		if callbackCount != 1 {
			t.Errorf("callback count mismatch: have %d, want 1", callbackCount)
		}
	})

	t.Run("depth error", func(t *testing.T) {
		callbackCount := 0
		override := func(_ CallFrameContext, result CallFrameResult) CallFrameResult {
			callbackCount++
			if !errors.Is(result.Err, ErrDepth) {
				t.Errorf("raw error mismatch: have %v, want %v", result.Err, ErrDepth)
			}
			return result
		}
		evm, _ := newCallFrameTestEVM(t, nil, override, nil)
		evm.depth = int(params.CallCreateDepth) + 1
		if _, _, err := evm.Call(callFrameTestCaller, callFrameTestTarget, nil, callFrameTestGas, new(uint256.Int)); !errors.Is(err, ErrDepth) {
			t.Fatalf("presented error mismatch: have %v, want %v", err, ErrDepth)
		}
		if callbackCount != 1 {
			t.Errorf("callback count mismatch: have %d, want 1", callbackCount)
		}
	})

	t.Run("insufficient balance", func(t *testing.T) {
		callbackCount := 0
		override := func(_ CallFrameContext, result CallFrameResult) CallFrameResult {
			callbackCount++
			if !errors.Is(result.Err, ErrInsufficientBalance) {
				t.Errorf("raw error mismatch: have %v, want %v", result.Err, ErrInsufficientBalance)
			}
			return result
		}
		evm, _ := newCallFrameTestEVM(t, nil, override, nil)
		evm.Context.CanTransfer = func(StateDB, common.Address, *uint256.Int) bool { return false }
		if _, _, err := evm.Call(callFrameTestCaller, callFrameTestTarget, nil, callFrameTestGas, uint256.NewInt(1)); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("presented error mismatch: have %v, want %v", err, ErrInsufficientBalance)
		}
		if callbackCount != 1 {
			t.Errorf("callback count mismatch: have %d, want 1", callbackCount)
		}
	})

	t.Run("create collision", func(t *testing.T) {
		callbackCount := 0
		override := func(context CallFrameContext, result CallFrameResult) CallFrameResult {
			callbackCount++
			if !errors.Is(result.Err, ErrContractAddressCollision) {
				t.Errorf("raw error mismatch: have %v, want %v", result.Err, ErrContractAddressCollision)
			}
			if context.To == (common.Address{}) {
				t.Error("missing predicted create address")
			}
			return result
		}
		evm, stateDB := newCallFrameTestEVM(t, nil, override, nil)
		predictedAddress := crypto.CreateAddress(callFrameTestCaller, stateDB.GetNonce(callFrameTestCaller))
		stateDB.CreateAccount(predictedAddress)
		stateDB.SetNonce(predictedAddress, 1, tracing.NonceChangeUnspecified)
		if _, _, _, err := evm.Create(callFrameTestCaller, []byte{0x00}, callFrameTestGas, new(uint256.Int)); !errors.Is(err, ErrContractAddressCollision) {
			t.Fatalf("presented error mismatch: have %v, want %v", err, ErrContractAddressCollision)
		}
		if callbackCount != 1 {
			t.Errorf("callback count mismatch: have %d, want 1", callbackCount)
		}
	})
}

func TestCallFrameResultOverridePreservesVerkleCreateRollback(t *testing.T) {
	dummyAddress := common.HexToAddress("0x1")
	override := func(_ CallFrameContext, result CallFrameResult) CallFrameResult {
		if !errors.Is(result.Err, ErrOutOfGas) {
			t.Errorf("raw error mismatch: have %v, want %v", result.Err, ErrOutOfGas)
		}
		result.CreateAddress = dummyAddress
		result.Err = nil
		return result
	}
	evm, stateDB := newCallFrameTestEVM(t, nil, override, nil)

	verkleTime := uint64(0)
	chainConfig := *params.AllEthashProtocolChanges
	chainConfig.VerkleTime = &verkleTime
	random := common.Hash{}
	evm.Context.Random = &random
	evm.chainConfig = &chainConfig
	evm.chainRules = chainConfig.Rules(evm.Context.BlockNumber, true, evm.Context.Time)
	evm.precompiles = activePrecompiledContracts(evm.chainRules)
	evm.SetTxContext(TxContext{})

	predictedAddress := crypto.CreateAddress(callFrameTestCaller, stateDB.GetNonce(callFrameTestCaller))
	gasProbe := state.NewAccessEvents(stateDB.PointCache())
	precheckGas := gasProbe.ContractCreatePreCheckGas(predictedAddress)
	if initGas := gasProbe.ContractCreateInitGas(predictedAddress); initGas == 0 {
		t.Fatal("test setup did not produce a Verkle create-init gas charge")
	}

	_, address, _, err := evm.Create(callFrameTestCaller, []byte{0x00}, precheckGas, new(uint256.Int))
	if err != nil {
		t.Fatalf("presented create failed: %v", err)
	}
	if address != dummyAddress {
		t.Errorf("presented create address mismatch: have %s, want %s", address, dummyAddress)
	}
	if stateDB.Exist(predictedAddress) {
		t.Errorf("raw failed CREATE left a partial account at %s", predictedAddress)
	}
	if nonce := stateDB.GetNonce(callFrameTestCaller); nonce != 1 {
		t.Errorf("creator nonce mismatch: have %d, want 1", nonce)
	}
}
