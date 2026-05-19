package oracle

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
)

// CanonicalCREATE2FactoryAddress is the Arachnid deterministic-deployment
// proxy address, deployed via a pre-signed legacy transaction so it lives
// at the same address on every EVM chain.
var CanonicalCREATE2FactoryAddress = common.HexToAddress("0x4e59b44847b379578588920cA78FbF26c0B4956C")

// CanonicalCREATE2FactoryCode is the 69-byte runtime bytecode of the
// Arachnid deterministic-deployment proxy at
// CanonicalCREATE2FactoryAddress (per Etherscan source). The contract
// reads salt(32) ++ initcode from calldata, does CREATE2, and returns
// the deployed address.
var CanonicalCREATE2FactoryCode = common.FromHex(
	"0x7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe03601600081602082378035828234f58015156039578182fd5b8082525050506014600cf3",
)

// CREATE2DeploySpec describes one batch of CREATE2 deploys: a single
// initcode reused across `SaltCount` salts starting at `SaltStart`. Salts
// are encoded as 32-byte big-endian counters.
//
// If DeployedCode is non-nil, the EVM-execution step is skipped and the
// supplied bytes are written verbatim as the runtime code at every
// derived address. Use this for initcode whose deployed code is
// salt-independent (no ADDRESS opcode in the constructor's return
// path); skipping the EVM run is much faster for large salt ranges.
//
// When DeployedCode is nil, the EVM runs once per salt so that
// ADDRESS-dependent constructors (the deterministic-CREATE2
// benchmark initcode being the motivating case) produce the correct
// per-address runtime. Cost: ~one EVM frame of `Initcode` per salt.
type CREATE2DeploySpec struct {
	Initcode     []byte
	SaltStart    uint64
	SaltCount    uint64
	DeployedCode []byte
}

// AddCREATE2Factory drops the canonical Arachnid factory runtime at
// CanonicalCREATE2FactoryAddress with nonce=1, balance=0. Idempotent;
// safe to call multiple times.
func AddCREATE2Factory(cfg *generator.Config) error {
	if cfg == nil {
		return fmt.Errorf("AddCREATE2Factory: nil cfg")
	}
	if cfg.GenesisAccounts == nil {
		cfg.GenesisAccounts = map[common.Address]*types.StateAccount{}
	}
	if cfg.GenesisCode == nil {
		cfg.GenesisCode = map[common.Address][]byte{}
	}
	cfg.GenesisAccounts[CanonicalCREATE2FactoryAddress] = &types.StateAccount{
		Nonce:    1,
		Balance:  uint256.NewInt(0),
		Root:     types.EmptyRootHash,
		CodeHash: crypto.Keccak256Hash(CanonicalCREATE2FactoryCode).Bytes(),
	}
	cfg.GenesisCode[CanonicalCREATE2FactoryAddress] = CanonicalCREATE2FactoryCode
	return nil
}

// AddCREATE2Deploys writes one alloc entry per (spec, salt) pair into
// cfg.GenesisAccounts/Code, simulating deployment through the CREATE2
// factory at `factory`. The derived address is computed per CREATE2
// rules (keccak256(0xff ++ factory ++ salt ++ keccak256(initcode))[12:]).
//
// Per-salt runtime derivation: for each (initcode, salt) the EVM
// executes the factory call with that exact salt so ADDRESS-dependent
// constructors (which embed the CREATE2-derived address into the
// deployed code) produce the correct bytes at each address. Specs
// with DeployedCode != nil short-circuit the EVM entirely and write
// the supplied bytes verbatim.
//
// nonce=1, balance=0, no storage at each derived address. Errors if a
// derived address collides with an existing GenesisAccounts entry.
func AddCREATE2Deploys(cfg *generator.Config, factory common.Address, specs []CREATE2DeploySpec) error {
	if cfg == nil {
		return fmt.Errorf("AddCREATE2Deploys: nil cfg")
	}
	if len(specs) == 0 {
		return nil
	}
	if cfg.GenesisAccounts == nil {
		cfg.GenesisAccounts = map[common.Address]*types.StateAccount{}
	}
	if cfg.GenesisCode == nil {
		cfg.GenesisCode = map[common.Address][]byte{}
	}

	for i, spec := range specs {
		if len(spec.Initcode) == 0 {
			return fmt.Errorf("AddCREATE2Deploys: spec[%d] has empty initcode", i)
		}
		if spec.SaltCount == 0 {
			continue
		}
		initHash := crypto.Keccak256(spec.Initcode)

		for k := uint64(0); k < spec.SaltCount; k++ {
			var salt [32]byte
			binary.BigEndian.PutUint64(salt[24:], spec.SaltStart+k)
			derived := crypto.CreateAddress2(factory, salt, initHash)

			if _, dup := cfg.GenesisAccounts[derived]; dup {
				return fmt.Errorf("AddCREATE2Deploys: spec[%d] salt=%d: derived addr %s collides with existing GenesisAccounts entry",
					i, spec.SaltStart+k, derived.Hex())
			}

			var runtimeCode []byte
			if spec.DeployedCode != nil {
				runtimeCode = spec.DeployedCode
			} else {
				var err error
				runtimeCode, err = SimulateCREATE2(factory, spec.Initcode, salt)
				if err != nil {
					return fmt.Errorf("AddCREATE2Deploys: spec[%d] salt=%d: %w", i, spec.SaltStart+k, err)
				}
			}
			if len(runtimeCode) == 0 {
				return fmt.Errorf("AddCREATE2Deploys: spec[%d] salt=%d: empty deployed code", i, spec.SaltStart+k)
			}

			cfg.GenesisAccounts[derived] = &types.StateAccount{
				Nonce:    1,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: crypto.Keccak256Hash(runtimeCode).Bytes(),
			}
			cfg.GenesisCode[derived] = runtimeCode
		}
	}
	return nil
}

// SimulateCREATE2 runs `initcode` through the canonical factory at
// `factory` with the supplied salt and returns the deployed runtime
// code. msg.sender during construction is `factory`; ADDRESS resolves
// to the CREATE2-derived address — matching real-world deployment via
// the deterministic-deployment proxy.
func SimulateCREATE2(factory common.Address, initcode []byte, salt [32]byte) ([]byte, error) {
	db, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		return nil, fmt.Errorf("state.New: %w", err)
	}

	db.CreateAccount(factory)
	db.SetCode(factory, CanonicalCREATE2FactoryCode, tracing.CodeChangeUnspecified)

	origin := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	db.CreateAccount(origin)
	db.SetBalance(origin, uint256.NewInt(1_000_000_000_000_000_000), tracing.BalanceChangeUnspecified)

	calldata := make([]byte, 0, 32+len(initcode))
	calldata = append(calldata, salt[:]...)
	calldata = append(calldata, initcode...)

	cfg := &runtime.Config{
		State:    db,
		Origin:   origin,
		GasLimit: 100_000_000,
	}
	if _, _, err := runtime.Call(factory, calldata, cfg); err != nil {
		return nil, fmt.Errorf("runtime.Call(factory): %w", err)
	}

	initHash := crypto.Keccak256(initcode)
	derived := crypto.CreateAddress2(factory, salt, initHash)
	code := db.GetCode(derived)
	if len(code) == 0 {
		return nil, fmt.Errorf("CREATE2 deployment via factory %s with salt %x produced no code at derived address %s (factory likely reverted; check initcode and gas)",
			factory.Hex(), salt, derived.Hex())
	}
	return code, nil
}
