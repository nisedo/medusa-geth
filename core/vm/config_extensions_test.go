package vm

import (
	"math/big"
	"testing"

	"github.com/crytic/medusa-geth/params"
)

func TestNewEVMInitializesConfigExtensions(t *testing.T) {
	blockContext := BlockContext{BlockNumber: new(big.Int)}

	evm := NewEVM(blockContext, nil, params.AllEthashProtocolChanges, Config{})
	if evm.Config.ConfigExtensions == nil {
		t.Fatal("NewEVM left ConfigExtensions nil")
	}

	extensions := &ConfigExtensions{OverrideCodeSizeCheck: true}
	evm = NewEVM(blockContext, nil, params.AllEthashProtocolChanges, Config{
		ConfigExtensions: extensions,
	})
	if evm.Config.ConfigExtensions != extensions {
		t.Fatal("NewEVM replaced the supplied ConfigExtensions")
	}
}
