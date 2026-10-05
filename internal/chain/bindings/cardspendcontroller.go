// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package bindings

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// Reference imports to suppress errors if they are not otherwise used.
var (
	_ = errors.New
	_ = big.NewInt
	_ = strings.NewReader
	_ = ethereum.NotFound
	_ = bind.Bind
	_ = common.Big1
	_ = types.BloomLookup
	_ = event.NewSubscription
	_ = abi.ConvertType
	_ = time.Tick
	_ = context.Background
)

// CardSpendControllerMetaData contains all meta data concerning the CardSpendController contract.
var CardSpendControllerMetaData = &bind.MetaData{
	ABI: "[{\"type\":\"constructor\",\"inputs\":[{\"name\":\"token_\",\"type\":\"address\",\"internalType\":\"contractIERC20\"},{\"name\":\"treasury_\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"admin\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"operator\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"DEFAULT_ADMIN_ROLE\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"OPERATOR_ROLE\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"authorizations\",\"inputs\":[{\"name\":\"\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}],\"outputs\":[{\"name\":\"user\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"debited\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"refunded\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"debit\",\"inputs\":[{\"name\":\"user\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"amount\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"authId\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"validUntil\",\"type\":\"uint64\",\"internalType\":\"uint64\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"getRoleAdmin\",\"inputs\":[{\"name\":\"role\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}],\"outputs\":[{\"name\":\"\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"grantRole\",\"inputs\":[{\"name\":\"role\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"account\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"hasRole\",\"inputs\":[{\"name\":\"role\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"account\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[{\"name\":\"\",\"type\":\"bool\",\"internalType\":\"bool\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"pause\",\"inputs\":[],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"paused\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"bool\",\"internalType\":\"bool\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"refund\",\"inputs\":[{\"name\":\"authId\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"refundId\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"amount\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"refundUsed\",\"inputs\":[{\"name\":\"\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}],\"outputs\":[{\"name\":\"\",\"type\":\"bool\",\"internalType\":\"bool\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"remainingDailyLimit\",\"inputs\":[{\"name\":\"user\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[{\"name\":\"\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"renounceRole\",\"inputs\":[{\"name\":\"role\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"callerConfirmation\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"revokeRole\",\"inputs\":[{\"name\":\"role\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"account\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"setDailyLimit\",\"inputs\":[{\"name\":\"user\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"limit\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"supportsInterface\",\"inputs\":[{\"name\":\"interfaceId\",\"type\":\"bytes4\",\"internalType\":\"bytes4\"}],\"outputs\":[{\"name\":\"\",\"type\":\"bool\",\"internalType\":\"bool\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"token\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"contractIERC20\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"treasury\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"unpause\",\"inputs\":[],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"event\",\"name\":\"DailyLimitSet\",\"inputs\":[{\"name\":\"user\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"limit\",\"type\":\"uint256\",\"indexed\":false,\"internalType\":\"uint256\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"Debited\",\"inputs\":[{\"name\":\"authId\",\"type\":\"bytes32\",\"indexed\":true,\"internalType\":\"bytes32\"},{\"name\":\"user\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"amount\",\"type\":\"uint256\",\"indexed\":false,\"internalType\":\"uint256\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"Paused\",\"inputs\":[{\"name\":\"account\",\"type\":\"address\",\"indexed\":false,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"Refunded\",\"inputs\":[{\"name\":\"authId\",\"type\":\"bytes32\",\"indexed\":true,\"internalType\":\"bytes32\"},{\"name\":\"refundId\",\"type\":\"bytes32\",\"indexed\":true,\"internalType\":\"bytes32\"},{\"name\":\"user\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"amount\",\"type\":\"uint256\",\"indexed\":false,\"internalType\":\"uint256\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"RoleAdminChanged\",\"inputs\":[{\"name\":\"role\",\"type\":\"bytes32\",\"indexed\":true,\"internalType\":\"bytes32\"},{\"name\":\"previousAdminRole\",\"type\":\"bytes32\",\"indexed\":true,\"internalType\":\"bytes32\"},{\"name\":\"newAdminRole\",\"type\":\"bytes32\",\"indexed\":true,\"internalType\":\"bytes32\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"RoleGranted\",\"inputs\":[{\"name\":\"role\",\"type\":\"bytes32\",\"indexed\":true,\"internalType\":\"bytes32\"},{\"name\":\"account\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"sender\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"RoleRevoked\",\"inputs\":[{\"name\":\"role\",\"type\":\"bytes32\",\"indexed\":true,\"internalType\":\"bytes32\"},{\"name\":\"account\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"sender\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"Unpaused\",\"inputs\":[{\"name\":\"account\",\"type\":\"address\",\"indexed\":false,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"error\",\"name\":\"AccessControlBadConfirmation\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"AccessControlUnauthorizedAccount\",\"inputs\":[{\"name\":\"account\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"neededRole\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}]},{\"type\":\"error\",\"name\":\"AuthAlreadyUsed\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"AuthExpired\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"DailyLimitExceeded\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"EnforcedPause\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"ExpectedPause\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"RefundAlreadyUsed\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"RefundExceedsDebit\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"SafeERC20FailedOperation\",\"inputs\":[{\"name\":\"token\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"UnknownAuth\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"ZeroAddress\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"ZeroAmount\",\"inputs\":[]}]",
}

// CardSpendControllerABI is the input ABI used to generate the binding from.
// Deprecated: Use CardSpendControllerMetaData.ABI instead.
var CardSpendControllerABI = CardSpendControllerMetaData.ABI

// CardSpendController is an auto generated Go binding around an Ethereum contract.
type CardSpendController struct {
	CardSpendControllerCaller     // Read-only binding to the contract
	CardSpendControllerTransactor // Write-only binding to the contract
	CardSpendControllerFilterer   // Log filterer for contract events
}

// CardSpendControllerCaller is an auto generated read-only Go binding around an Ethereum contract.
type CardSpendControllerCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// CardSpendControllerTransactor is an auto generated write-only Go binding around an Ethereum contract.
type CardSpendControllerTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// CardSpendControllerFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type CardSpendControllerFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// CardSpendControllerSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type CardSpendControllerSession struct {
	Contract     *CardSpendController // Generic contract binding to set the session for
	CallOpts     bind.CallOpts        // Call options to use throughout this session
	TransactOpts bind.TransactOpts    // Transaction auth options to use throughout this session
}

// CardSpendControllerCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type CardSpendControllerCallerSession struct {
	Contract *CardSpendControllerCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts              // Call options to use throughout this session
}

// CardSpendControllerTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type CardSpendControllerTransactorSession struct {
	Contract     *CardSpendControllerTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts              // Transaction auth options to use throughout this session
}

// CardSpendControllerRaw is an auto generated low-level Go binding around an Ethereum contract.
type CardSpendControllerRaw struct {
	Contract *CardSpendController // Generic contract binding to access the raw methods on
}

// CardSpendControllerCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type CardSpendControllerCallerRaw struct {
	Contract *CardSpendControllerCaller // Generic read-only contract binding to access the raw methods on
}

// CardSpendControllerTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type CardSpendControllerTransactorRaw struct {
	Contract *CardSpendControllerTransactor // Generic write-only contract binding to access the raw methods on
}

// NewCardSpendController creates a new instance of CardSpendController, bound to a specific deployed contract.
func NewCardSpendController(address common.Address, backend bind.ContractBackend) (*CardSpendController, error) {
	contract, err := bindCardSpendController(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &CardSpendController{CardSpendControllerCaller: CardSpendControllerCaller{contract: contract}, CardSpendControllerTransactor: CardSpendControllerTransactor{contract: contract}, CardSpendControllerFilterer: CardSpendControllerFilterer{contract: contract}}, nil
}

// NewCardSpendControllerCaller creates a new read-only instance of CardSpendController, bound to a specific deployed contract.
func NewCardSpendControllerCaller(address common.Address, caller bind.ContractCaller) (*CardSpendControllerCaller, error) {
	contract, err := bindCardSpendController(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerCaller{contract: contract}, nil
}

// NewCardSpendControllerTransactor creates a new write-only instance of CardSpendController, bound to a specific deployed contract.
func NewCardSpendControllerTransactor(address common.Address, transactor bind.ContractTransactor) (*CardSpendControllerTransactor, error) {
	contract, err := bindCardSpendController(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerTransactor{contract: contract}, nil
}

// NewCardSpendControllerFilterer creates a new log filterer instance of CardSpendController, bound to a specific deployed contract.
func NewCardSpendControllerFilterer(address common.Address, filterer bind.ContractFilterer) (*CardSpendControllerFilterer, error) {
	contract, err := bindCardSpendController(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerFilterer{contract: contract}, nil
}

// bindCardSpendController binds a generic wrapper to an already deployed contract.
func bindCardSpendController(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := CardSpendControllerMetaData.GetAbi()
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, *parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_CardSpendController *CardSpendControllerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _CardSpendController.Contract.CardSpendControllerCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_CardSpendController *CardSpendControllerRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _CardSpendController.Contract.CardSpendControllerTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_CardSpendController *CardSpendControllerRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _CardSpendController.Contract.CardSpendControllerTransactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_CardSpendController *CardSpendControllerCallerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _CardSpendController.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_CardSpendController *CardSpendControllerTransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _CardSpendController.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_CardSpendController *CardSpendControllerTransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _CardSpendController.Contract.contract.Transact(opts, method, params...)
}

// DEFAULTADMINROLE is a free data retrieval call binding the contract method 0xa217fddf.
//
// Solidity: function DEFAULT_ADMIN_ROLE() view returns(bytes32)
func (_CardSpendController *CardSpendControllerCaller) DEFAULTADMINROLE(opts *bind.CallOpts) ([32]byte, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "DEFAULT_ADMIN_ROLE")

	if err != nil {
		return *new([32]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)

	return out0, err

}

// DEFAULTADMINROLE is a free data retrieval call binding the contract method 0xa217fddf.
//
// Solidity: function DEFAULT_ADMIN_ROLE() view returns(bytes32)
func (_CardSpendController *CardSpendControllerSession) DEFAULTADMINROLE() ([32]byte, error) {
	return _CardSpendController.Contract.DEFAULTADMINROLE(&_CardSpendController.CallOpts)
}

// DEFAULTADMINROLE is a free data retrieval call binding the contract method 0xa217fddf.
//
// Solidity: function DEFAULT_ADMIN_ROLE() view returns(bytes32)
func (_CardSpendController *CardSpendControllerCallerSession) DEFAULTADMINROLE() ([32]byte, error) {
	return _CardSpendController.Contract.DEFAULTADMINROLE(&_CardSpendController.CallOpts)
}

// OPERATORROLE is a free data retrieval call binding the contract method 0xf5b541a6.
//
// Solidity: function OPERATOR_ROLE() view returns(bytes32)
func (_CardSpendController *CardSpendControllerCaller) OPERATORROLE(opts *bind.CallOpts) ([32]byte, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "OPERATOR_ROLE")

	if err != nil {
		return *new([32]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)

	return out0, err

}

// OPERATORROLE is a free data retrieval call binding the contract method 0xf5b541a6.
//
// Solidity: function OPERATOR_ROLE() view returns(bytes32)
func (_CardSpendController *CardSpendControllerSession) OPERATORROLE() ([32]byte, error) {
	return _CardSpendController.Contract.OPERATORROLE(&_CardSpendController.CallOpts)
}

// OPERATORROLE is a free data retrieval call binding the contract method 0xf5b541a6.
//
// Solidity: function OPERATOR_ROLE() view returns(bytes32)
func (_CardSpendController *CardSpendControllerCallerSession) OPERATORROLE() ([32]byte, error) {
	return _CardSpendController.Contract.OPERATORROLE(&_CardSpendController.CallOpts)
}

// Authorizations is a free data retrieval call binding the contract method 0xe8d7d5eb.
//
// Solidity: function authorizations(bytes32 ) view returns(address user, uint256 debited, uint256 refunded)
func (_CardSpendController *CardSpendControllerCaller) Authorizations(opts *bind.CallOpts, arg0 [32]byte) (struct {
	User     common.Address
	Debited  *big.Int
	Refunded *big.Int
}, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "authorizations", arg0)

	outstruct := new(struct {
		User     common.Address
		Debited  *big.Int
		Refunded *big.Int
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.User = *abi.ConvertType(out[0], new(common.Address)).(*common.Address)
	outstruct.Debited = *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	outstruct.Refunded = *abi.ConvertType(out[2], new(*big.Int)).(**big.Int)

	return *outstruct, err

}

// Authorizations is a free data retrieval call binding the contract method 0xe8d7d5eb.
//
// Solidity: function authorizations(bytes32 ) view returns(address user, uint256 debited, uint256 refunded)
func (_CardSpendController *CardSpendControllerSession) Authorizations(arg0 [32]byte) (struct {
	User     common.Address
	Debited  *big.Int
	Refunded *big.Int
}, error) {
	return _CardSpendController.Contract.Authorizations(&_CardSpendController.CallOpts, arg0)
}

// Authorizations is a free data retrieval call binding the contract method 0xe8d7d5eb.
//
// Solidity: function authorizations(bytes32 ) view returns(address user, uint256 debited, uint256 refunded)
func (_CardSpendController *CardSpendControllerCallerSession) Authorizations(arg0 [32]byte) (struct {
	User     common.Address
	Debited  *big.Int
	Refunded *big.Int
}, error) {
	return _CardSpendController.Contract.Authorizations(&_CardSpendController.CallOpts, arg0)
}

// GetRoleAdmin is a free data retrieval call binding the contract method 0x248a9ca3.
//
// Solidity: function getRoleAdmin(bytes32 role) view returns(bytes32)
func (_CardSpendController *CardSpendControllerCaller) GetRoleAdmin(opts *bind.CallOpts, role [32]byte) ([32]byte, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "getRoleAdmin", role)

	if err != nil {
		return *new([32]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)

	return out0, err

}

// GetRoleAdmin is a free data retrieval call binding the contract method 0x248a9ca3.
//
// Solidity: function getRoleAdmin(bytes32 role) view returns(bytes32)
func (_CardSpendController *CardSpendControllerSession) GetRoleAdmin(role [32]byte) ([32]byte, error) {
	return _CardSpendController.Contract.GetRoleAdmin(&_CardSpendController.CallOpts, role)
}

// GetRoleAdmin is a free data retrieval call binding the contract method 0x248a9ca3.
//
// Solidity: function getRoleAdmin(bytes32 role) view returns(bytes32)
func (_CardSpendController *CardSpendControllerCallerSession) GetRoleAdmin(role [32]byte) ([32]byte, error) {
	return _CardSpendController.Contract.GetRoleAdmin(&_CardSpendController.CallOpts, role)
}

// HasRole is a free data retrieval call binding the contract method 0x91d14854.
//
// Solidity: function hasRole(bytes32 role, address account) view returns(bool)
func (_CardSpendController *CardSpendControllerCaller) HasRole(opts *bind.CallOpts, role [32]byte, account common.Address) (bool, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "hasRole", role, account)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// HasRole is a free data retrieval call binding the contract method 0x91d14854.
//
// Solidity: function hasRole(bytes32 role, address account) view returns(bool)
func (_CardSpendController *CardSpendControllerSession) HasRole(role [32]byte, account common.Address) (bool, error) {
	return _CardSpendController.Contract.HasRole(&_CardSpendController.CallOpts, role, account)
}

// HasRole is a free data retrieval call binding the contract method 0x91d14854.
//
// Solidity: function hasRole(bytes32 role, address account) view returns(bool)
func (_CardSpendController *CardSpendControllerCallerSession) HasRole(role [32]byte, account common.Address) (bool, error) {
	return _CardSpendController.Contract.HasRole(&_CardSpendController.CallOpts, role, account)
}

// Paused is a free data retrieval call binding the contract method 0x5c975abb.
//
// Solidity: function paused() view returns(bool)
func (_CardSpendController *CardSpendControllerCaller) Paused(opts *bind.CallOpts) (bool, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "paused")

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// Paused is a free data retrieval call binding the contract method 0x5c975abb.
//
// Solidity: function paused() view returns(bool)
func (_CardSpendController *CardSpendControllerSession) Paused() (bool, error) {
	return _CardSpendController.Contract.Paused(&_CardSpendController.CallOpts)
}

// Paused is a free data retrieval call binding the contract method 0x5c975abb.
//
// Solidity: function paused() view returns(bool)
func (_CardSpendController *CardSpendControllerCallerSession) Paused() (bool, error) {
	return _CardSpendController.Contract.Paused(&_CardSpendController.CallOpts)
}

// RefundUsed is a free data retrieval call binding the contract method 0x6cd6e698.
//
// Solidity: function refundUsed(bytes32 ) view returns(bool)
func (_CardSpendController *CardSpendControllerCaller) RefundUsed(opts *bind.CallOpts, arg0 [32]byte) (bool, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "refundUsed", arg0)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// RefundUsed is a free data retrieval call binding the contract method 0x6cd6e698.
//
// Solidity: function refundUsed(bytes32 ) view returns(bool)
func (_CardSpendController *CardSpendControllerSession) RefundUsed(arg0 [32]byte) (bool, error) {
	return _CardSpendController.Contract.RefundUsed(&_CardSpendController.CallOpts, arg0)
}

// RefundUsed is a free data retrieval call binding the contract method 0x6cd6e698.
//
// Solidity: function refundUsed(bytes32 ) view returns(bool)
func (_CardSpendController *CardSpendControllerCallerSession) RefundUsed(arg0 [32]byte) (bool, error) {
	return _CardSpendController.Contract.RefundUsed(&_CardSpendController.CallOpts, arg0)
}

// RemainingDailyLimit is a free data retrieval call binding the contract method 0xf497e873.
//
// Solidity: function remainingDailyLimit(address user) view returns(uint256)
func (_CardSpendController *CardSpendControllerCaller) RemainingDailyLimit(opts *bind.CallOpts, user common.Address) (*big.Int, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "remainingDailyLimit", user)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// RemainingDailyLimit is a free data retrieval call binding the contract method 0xf497e873.
//
// Solidity: function remainingDailyLimit(address user) view returns(uint256)
func (_CardSpendController *CardSpendControllerSession) RemainingDailyLimit(user common.Address) (*big.Int, error) {
	return _CardSpendController.Contract.RemainingDailyLimit(&_CardSpendController.CallOpts, user)
}

// RemainingDailyLimit is a free data retrieval call binding the contract method 0xf497e873.
//
// Solidity: function remainingDailyLimit(address user) view returns(uint256)
func (_CardSpendController *CardSpendControllerCallerSession) RemainingDailyLimit(user common.Address) (*big.Int, error) {
	return _CardSpendController.Contract.RemainingDailyLimit(&_CardSpendController.CallOpts, user)
}

// SupportsInterface is a free data retrieval call binding the contract method 0x01ffc9a7.
//
// Solidity: function supportsInterface(bytes4 interfaceId) view returns(bool)
func (_CardSpendController *CardSpendControllerCaller) SupportsInterface(opts *bind.CallOpts, interfaceId [4]byte) (bool, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "supportsInterface", interfaceId)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// SupportsInterface is a free data retrieval call binding the contract method 0x01ffc9a7.
//
// Solidity: function supportsInterface(bytes4 interfaceId) view returns(bool)
func (_CardSpendController *CardSpendControllerSession) SupportsInterface(interfaceId [4]byte) (bool, error) {
	return _CardSpendController.Contract.SupportsInterface(&_CardSpendController.CallOpts, interfaceId)
}

// SupportsInterface is a free data retrieval call binding the contract method 0x01ffc9a7.
//
// Solidity: function supportsInterface(bytes4 interfaceId) view returns(bool)
func (_CardSpendController *CardSpendControllerCallerSession) SupportsInterface(interfaceId [4]byte) (bool, error) {
	return _CardSpendController.Contract.SupportsInterface(&_CardSpendController.CallOpts, interfaceId)
}

// Token is a free data retrieval call binding the contract method 0xfc0c546a.
//
// Solidity: function token() view returns(address)
func (_CardSpendController *CardSpendControllerCaller) Token(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "token")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// Token is a free data retrieval call binding the contract method 0xfc0c546a.
//
// Solidity: function token() view returns(address)
func (_CardSpendController *CardSpendControllerSession) Token() (common.Address, error) {
	return _CardSpendController.Contract.Token(&_CardSpendController.CallOpts)
}

// Token is a free data retrieval call binding the contract method 0xfc0c546a.
//
// Solidity: function token() view returns(address)
func (_CardSpendController *CardSpendControllerCallerSession) Token() (common.Address, error) {
	return _CardSpendController.Contract.Token(&_CardSpendController.CallOpts)
}

// Treasury is a free data retrieval call binding the contract method 0x61d027b3.
//
// Solidity: function treasury() view returns(address)
func (_CardSpendController *CardSpendControllerCaller) Treasury(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _CardSpendController.contract.Call(opts, &out, "treasury")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// Treasury is a free data retrieval call binding the contract method 0x61d027b3.
//
// Solidity: function treasury() view returns(address)
func (_CardSpendController *CardSpendControllerSession) Treasury() (common.Address, error) {
	return _CardSpendController.Contract.Treasury(&_CardSpendController.CallOpts)
}

// Treasury is a free data retrieval call binding the contract method 0x61d027b3.
//
// Solidity: function treasury() view returns(address)
func (_CardSpendController *CardSpendControllerCallerSession) Treasury() (common.Address, error) {
	return _CardSpendController.Contract.Treasury(&_CardSpendController.CallOpts)
}

// Debit is a paid mutator transaction binding the contract method 0xc4c8d569.
//
// Solidity: function debit(address user, uint256 amount, bytes32 authId, uint64 validUntil) returns()
func (_CardSpendController *CardSpendControllerTransactor) Debit(opts *bind.TransactOpts, user common.Address, amount *big.Int, authId [32]byte, validUntil uint64) (*types.Transaction, error) {
	return _CardSpendController.contract.Transact(opts, "debit", user, amount, authId, validUntil)
}

// Debit is a paid mutator transaction binding the contract method 0xc4c8d569.
//
// Solidity: function debit(address user, uint256 amount, bytes32 authId, uint64 validUntil) returns()
func (_CardSpendController *CardSpendControllerSession) Debit(user common.Address, amount *big.Int, authId [32]byte, validUntil uint64) (*types.Transaction, error) {
	return _CardSpendController.Contract.Debit(&_CardSpendController.TransactOpts, user, amount, authId, validUntil)
}

// Debit is a paid mutator transaction binding the contract method 0xc4c8d569.
//
// Solidity: function debit(address user, uint256 amount, bytes32 authId, uint64 validUntil) returns()
func (_CardSpendController *CardSpendControllerTransactorSession) Debit(user common.Address, amount *big.Int, authId [32]byte, validUntil uint64) (*types.Transaction, error) {
	return _CardSpendController.Contract.Debit(&_CardSpendController.TransactOpts, user, amount, authId, validUntil)
}

// GrantRole is a paid mutator transaction binding the contract method 0x2f2ff15d.
//
// Solidity: function grantRole(bytes32 role, address account) returns()
func (_CardSpendController *CardSpendControllerTransactor) GrantRole(opts *bind.TransactOpts, role [32]byte, account common.Address) (*types.Transaction, error) {
	return _CardSpendController.contract.Transact(opts, "grantRole", role, account)
}

// GrantRole is a paid mutator transaction binding the contract method 0x2f2ff15d.
//
// Solidity: function grantRole(bytes32 role, address account) returns()
func (_CardSpendController *CardSpendControllerSession) GrantRole(role [32]byte, account common.Address) (*types.Transaction, error) {
	return _CardSpendController.Contract.GrantRole(&_CardSpendController.TransactOpts, role, account)
}

// GrantRole is a paid mutator transaction binding the contract method 0x2f2ff15d.
//
// Solidity: function grantRole(bytes32 role, address account) returns()
func (_CardSpendController *CardSpendControllerTransactorSession) GrantRole(role [32]byte, account common.Address) (*types.Transaction, error) {
	return _CardSpendController.Contract.GrantRole(&_CardSpendController.TransactOpts, role, account)
}

// Pause is a paid mutator transaction binding the contract method 0x8456cb59.
//
// Solidity: function pause() returns()
func (_CardSpendController *CardSpendControllerTransactor) Pause(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _CardSpendController.contract.Transact(opts, "pause")
}

// Pause is a paid mutator transaction binding the contract method 0x8456cb59.
//
// Solidity: function pause() returns()
func (_CardSpendController *CardSpendControllerSession) Pause() (*types.Transaction, error) {
	return _CardSpendController.Contract.Pause(&_CardSpendController.TransactOpts)
}

// Pause is a paid mutator transaction binding the contract method 0x8456cb59.
//
// Solidity: function pause() returns()
func (_CardSpendController *CardSpendControllerTransactorSession) Pause() (*types.Transaction, error) {
	return _CardSpendController.Contract.Pause(&_CardSpendController.TransactOpts)
}

// Refund is a paid mutator transaction binding the contract method 0x1c9d535f.
//
// Solidity: function refund(bytes32 authId, bytes32 refundId, uint256 amount) returns()
func (_CardSpendController *CardSpendControllerTransactor) Refund(opts *bind.TransactOpts, authId [32]byte, refundId [32]byte, amount *big.Int) (*types.Transaction, error) {
	return _CardSpendController.contract.Transact(opts, "refund", authId, refundId, amount)
}

// Refund is a paid mutator transaction binding the contract method 0x1c9d535f.
//
// Solidity: function refund(bytes32 authId, bytes32 refundId, uint256 amount) returns()
func (_CardSpendController *CardSpendControllerSession) Refund(authId [32]byte, refundId [32]byte, amount *big.Int) (*types.Transaction, error) {
	return _CardSpendController.Contract.Refund(&_CardSpendController.TransactOpts, authId, refundId, amount)
}

// Refund is a paid mutator transaction binding the contract method 0x1c9d535f.
//
// Solidity: function refund(bytes32 authId, bytes32 refundId, uint256 amount) returns()
func (_CardSpendController *CardSpendControllerTransactorSession) Refund(authId [32]byte, refundId [32]byte, amount *big.Int) (*types.Transaction, error) {
	return _CardSpendController.Contract.Refund(&_CardSpendController.TransactOpts, authId, refundId, amount)
}

// RenounceRole is a paid mutator transaction binding the contract method 0x36568abe.
//
// Solidity: function renounceRole(bytes32 role, address callerConfirmation) returns()
func (_CardSpendController *CardSpendControllerTransactor) RenounceRole(opts *bind.TransactOpts, role [32]byte, callerConfirmation common.Address) (*types.Transaction, error) {
	return _CardSpendController.contract.Transact(opts, "renounceRole", role, callerConfirmation)
}

// RenounceRole is a paid mutator transaction binding the contract method 0x36568abe.
//
// Solidity: function renounceRole(bytes32 role, address callerConfirmation) returns()
func (_CardSpendController *CardSpendControllerSession) RenounceRole(role [32]byte, callerConfirmation common.Address) (*types.Transaction, error) {
	return _CardSpendController.Contract.RenounceRole(&_CardSpendController.TransactOpts, role, callerConfirmation)
}

// RenounceRole is a paid mutator transaction binding the contract method 0x36568abe.
//
// Solidity: function renounceRole(bytes32 role, address callerConfirmation) returns()
func (_CardSpendController *CardSpendControllerTransactorSession) RenounceRole(role [32]byte, callerConfirmation common.Address) (*types.Transaction, error) {
	return _CardSpendController.Contract.RenounceRole(&_CardSpendController.TransactOpts, role, callerConfirmation)
}

// RevokeRole is a paid mutator transaction binding the contract method 0xd547741f.
//
// Solidity: function revokeRole(bytes32 role, address account) returns()
func (_CardSpendController *CardSpendControllerTransactor) RevokeRole(opts *bind.TransactOpts, role [32]byte, account common.Address) (*types.Transaction, error) {
	return _CardSpendController.contract.Transact(opts, "revokeRole", role, account)
}

// RevokeRole is a paid mutator transaction binding the contract method 0xd547741f.
//
// Solidity: function revokeRole(bytes32 role, address account) returns()
func (_CardSpendController *CardSpendControllerSession) RevokeRole(role [32]byte, account common.Address) (*types.Transaction, error) {
	return _CardSpendController.Contract.RevokeRole(&_CardSpendController.TransactOpts, role, account)
}

// RevokeRole is a paid mutator transaction binding the contract method 0xd547741f.
//
// Solidity: function revokeRole(bytes32 role, address account) returns()
func (_CardSpendController *CardSpendControllerTransactorSession) RevokeRole(role [32]byte, account common.Address) (*types.Transaction, error) {
	return _CardSpendController.Contract.RevokeRole(&_CardSpendController.TransactOpts, role, account)
}

// SetDailyLimit is a paid mutator transaction binding the contract method 0x2803212f.
//
// Solidity: function setDailyLimit(address user, uint256 limit) returns()
func (_CardSpendController *CardSpendControllerTransactor) SetDailyLimit(opts *bind.TransactOpts, user common.Address, limit *big.Int) (*types.Transaction, error) {
	return _CardSpendController.contract.Transact(opts, "setDailyLimit", user, limit)
}

// SetDailyLimit is a paid mutator transaction binding the contract method 0x2803212f.
//
// Solidity: function setDailyLimit(address user, uint256 limit) returns()
func (_CardSpendController *CardSpendControllerSession) SetDailyLimit(user common.Address, limit *big.Int) (*types.Transaction, error) {
	return _CardSpendController.Contract.SetDailyLimit(&_CardSpendController.TransactOpts, user, limit)
}

// SetDailyLimit is a paid mutator transaction binding the contract method 0x2803212f.
//
// Solidity: function setDailyLimit(address user, uint256 limit) returns()
func (_CardSpendController *CardSpendControllerTransactorSession) SetDailyLimit(user common.Address, limit *big.Int) (*types.Transaction, error) {
	return _CardSpendController.Contract.SetDailyLimit(&_CardSpendController.TransactOpts, user, limit)
}

// Unpause is a paid mutator transaction binding the contract method 0x3f4ba83a.
//
// Solidity: function unpause() returns()
func (_CardSpendController *CardSpendControllerTransactor) Unpause(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _CardSpendController.contract.Transact(opts, "unpause")
}

// Unpause is a paid mutator transaction binding the contract method 0x3f4ba83a.
//
// Solidity: function unpause() returns()
func (_CardSpendController *CardSpendControllerSession) Unpause() (*types.Transaction, error) {
	return _CardSpendController.Contract.Unpause(&_CardSpendController.TransactOpts)
}

// Unpause is a paid mutator transaction binding the contract method 0x3f4ba83a.
//
// Solidity: function unpause() returns()
func (_CardSpendController *CardSpendControllerTransactorSession) Unpause() (*types.Transaction, error) {
	return _CardSpendController.Contract.Unpause(&_CardSpendController.TransactOpts)
}

// CardSpendControllerDailyLimitSetIterator is returned from FilterDailyLimitSet and is used to iterate over the raw logs and unpacked data for DailyLimitSet events raised by the CardSpendController contract.
type CardSpendControllerDailyLimitSetIterator struct {
	Event *CardSpendControllerDailyLimitSet // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *CardSpendControllerDailyLimitSetIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(CardSpendControllerDailyLimitSet)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(CardSpendControllerDailyLimitSet)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *CardSpendControllerDailyLimitSetIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *CardSpendControllerDailyLimitSetIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// CardSpendControllerDailyLimitSet represents a DailyLimitSet event raised by the CardSpendController contract.
type CardSpendControllerDailyLimitSet struct {
	User  common.Address
	Limit *big.Int
	Raw   types.Log // Blockchain specific contextual infos
}

// FilterDailyLimitSet is a free log retrieval operation binding the contract event 0xd3d22ffd28b02735cf411bd7f925bd8da01212c7028153e0d632e2953ac3088e.
//
// Solidity: event DailyLimitSet(address indexed user, uint256 limit)
func (_CardSpendController *CardSpendControllerFilterer) FilterDailyLimitSet(opts *bind.FilterOpts, user []common.Address) (*CardSpendControllerDailyLimitSetIterator, error) {

	var userRule []interface{}
	for _, userItem := range user {
		userRule = append(userRule, userItem)
	}

	logs, sub, err := _CardSpendController.contract.FilterLogs(opts, "DailyLimitSet", userRule)
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerDailyLimitSetIterator{contract: _CardSpendController.contract, event: "DailyLimitSet", logs: logs, sub: sub}, nil
}

// WatchDailyLimitSet is a free log subscription operation binding the contract event 0xd3d22ffd28b02735cf411bd7f925bd8da01212c7028153e0d632e2953ac3088e.
//
// Solidity: event DailyLimitSet(address indexed user, uint256 limit)
func (_CardSpendController *CardSpendControllerFilterer) WatchDailyLimitSet(opts *bind.WatchOpts, sink chan<- *CardSpendControllerDailyLimitSet, user []common.Address) (event.Subscription, error) {

	var userRule []interface{}
	for _, userItem := range user {
		userRule = append(userRule, userItem)
	}

	logs, sub, err := _CardSpendController.contract.WatchLogs(opts, "DailyLimitSet", userRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(CardSpendControllerDailyLimitSet)
				if err := _CardSpendController.contract.UnpackLog(event, "DailyLimitSet", log); err != nil {
					// If the signature doesn't match, skip this log.
					if errors.Is(err, bind.ErrEventSignatureMismatch) {
						continue
					}
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseDailyLimitSet is a log parse operation binding the contract event 0xd3d22ffd28b02735cf411bd7f925bd8da01212c7028153e0d632e2953ac3088e.
//
// Solidity: event DailyLimitSet(address indexed user, uint256 limit)
func (_CardSpendController *CardSpendControllerFilterer) ParseDailyLimitSet(log types.Log) (*CardSpendControllerDailyLimitSet, error) {
	event := new(CardSpendControllerDailyLimitSet)
	if err := _CardSpendController.contract.UnpackLog(event, "DailyLimitSet", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// CardSpendControllerDebitedIterator is returned from FilterDebited and is used to iterate over the raw logs and unpacked data for Debited events raised by the CardSpendController contract.
type CardSpendControllerDebitedIterator struct {
	Event *CardSpendControllerDebited // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *CardSpendControllerDebitedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(CardSpendControllerDebited)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(CardSpendControllerDebited)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *CardSpendControllerDebitedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *CardSpendControllerDebitedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// CardSpendControllerDebited represents a Debited event raised by the CardSpendController contract.
type CardSpendControllerDebited struct {
	AuthId [32]byte
	User   common.Address
	Amount *big.Int
	Raw    types.Log // Blockchain specific contextual infos
}

// FilterDebited is a free log retrieval operation binding the contract event 0xcc263056a0c3ff63165efb08fb26b874e1bf3201d043a612abfb1b21e6ba0d8d.
//
// Solidity: event Debited(bytes32 indexed authId, address indexed user, uint256 amount)
func (_CardSpendController *CardSpendControllerFilterer) FilterDebited(opts *bind.FilterOpts, authId [][32]byte, user []common.Address) (*CardSpendControllerDebitedIterator, error) {

	var authIdRule []interface{}
	for _, authIdItem := range authId {
		authIdRule = append(authIdRule, authIdItem)
	}
	var userRule []interface{}
	for _, userItem := range user {
		userRule = append(userRule, userItem)
	}

	logs, sub, err := _CardSpendController.contract.FilterLogs(opts, "Debited", authIdRule, userRule)
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerDebitedIterator{contract: _CardSpendController.contract, event: "Debited", logs: logs, sub: sub}, nil
}

// WatchDebited is a free log subscription operation binding the contract event 0xcc263056a0c3ff63165efb08fb26b874e1bf3201d043a612abfb1b21e6ba0d8d.
//
// Solidity: event Debited(bytes32 indexed authId, address indexed user, uint256 amount)
func (_CardSpendController *CardSpendControllerFilterer) WatchDebited(opts *bind.WatchOpts, sink chan<- *CardSpendControllerDebited, authId [][32]byte, user []common.Address) (event.Subscription, error) {

	var authIdRule []interface{}
	for _, authIdItem := range authId {
		authIdRule = append(authIdRule, authIdItem)
	}
	var userRule []interface{}
	for _, userItem := range user {
		userRule = append(userRule, userItem)
	}

	logs, sub, err := _CardSpendController.contract.WatchLogs(opts, "Debited", authIdRule, userRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(CardSpendControllerDebited)
				if err := _CardSpendController.contract.UnpackLog(event, "Debited", log); err != nil {
					// If the signature doesn't match, skip this log.
					if errors.Is(err, bind.ErrEventSignatureMismatch) {
						continue
					}
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseDebited is a log parse operation binding the contract event 0xcc263056a0c3ff63165efb08fb26b874e1bf3201d043a612abfb1b21e6ba0d8d.
//
// Solidity: event Debited(bytes32 indexed authId, address indexed user, uint256 amount)
func (_CardSpendController *CardSpendControllerFilterer) ParseDebited(log types.Log) (*CardSpendControllerDebited, error) {
	event := new(CardSpendControllerDebited)
	if err := _CardSpendController.contract.UnpackLog(event, "Debited", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// CardSpendControllerPausedIterator is returned from FilterPaused and is used to iterate over the raw logs and unpacked data for Paused events raised by the CardSpendController contract.
type CardSpendControllerPausedIterator struct {
	Event *CardSpendControllerPaused // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *CardSpendControllerPausedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(CardSpendControllerPaused)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(CardSpendControllerPaused)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *CardSpendControllerPausedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *CardSpendControllerPausedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// CardSpendControllerPaused represents a Paused event raised by the CardSpendController contract.
type CardSpendControllerPaused struct {
	Account common.Address
	Raw     types.Log // Blockchain specific contextual infos
}

// FilterPaused is a free log retrieval operation binding the contract event 0x62e78cea01bee320cd4e420270b5ea74000d11b0c9f74754ebdbfc544b05a258.
//
// Solidity: event Paused(address account)
func (_CardSpendController *CardSpendControllerFilterer) FilterPaused(opts *bind.FilterOpts) (*CardSpendControllerPausedIterator, error) {

	logs, sub, err := _CardSpendController.contract.FilterLogs(opts, "Paused")
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerPausedIterator{contract: _CardSpendController.contract, event: "Paused", logs: logs, sub: sub}, nil
}

// WatchPaused is a free log subscription operation binding the contract event 0x62e78cea01bee320cd4e420270b5ea74000d11b0c9f74754ebdbfc544b05a258.
//
// Solidity: event Paused(address account)
func (_CardSpendController *CardSpendControllerFilterer) WatchPaused(opts *bind.WatchOpts, sink chan<- *CardSpendControllerPaused) (event.Subscription, error) {

	logs, sub, err := _CardSpendController.contract.WatchLogs(opts, "Paused")
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(CardSpendControllerPaused)
				if err := _CardSpendController.contract.UnpackLog(event, "Paused", log); err != nil {
					// If the signature doesn't match, skip this log.
					if errors.Is(err, bind.ErrEventSignatureMismatch) {
						continue
					}
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParsePaused is a log parse operation binding the contract event 0x62e78cea01bee320cd4e420270b5ea74000d11b0c9f74754ebdbfc544b05a258.
//
// Solidity: event Paused(address account)
func (_CardSpendController *CardSpendControllerFilterer) ParsePaused(log types.Log) (*CardSpendControllerPaused, error) {
	event := new(CardSpendControllerPaused)
	if err := _CardSpendController.contract.UnpackLog(event, "Paused", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// CardSpendControllerRefundedIterator is returned from FilterRefunded and is used to iterate over the raw logs and unpacked data for Refunded events raised by the CardSpendController contract.
type CardSpendControllerRefundedIterator struct {
	Event *CardSpendControllerRefunded // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *CardSpendControllerRefundedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(CardSpendControllerRefunded)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(CardSpendControllerRefunded)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *CardSpendControllerRefundedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *CardSpendControllerRefundedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// CardSpendControllerRefunded represents a Refunded event raised by the CardSpendController contract.
type CardSpendControllerRefunded struct {
	AuthId   [32]byte
	RefundId [32]byte
	User     common.Address
	Amount   *big.Int
	Raw      types.Log // Blockchain specific contextual infos
}

// FilterRefunded is a free log retrieval operation binding the contract event 0x6c5895acb60b66e78106939eaaa3976db6325f801ff434fe24ff7cb0a6795a5f.
//
// Solidity: event Refunded(bytes32 indexed authId, bytes32 indexed refundId, address indexed user, uint256 amount)
func (_CardSpendController *CardSpendControllerFilterer) FilterRefunded(opts *bind.FilterOpts, authId [][32]byte, refundId [][32]byte, user []common.Address) (*CardSpendControllerRefundedIterator, error) {

	var authIdRule []interface{}
	for _, authIdItem := range authId {
		authIdRule = append(authIdRule, authIdItem)
	}
	var refundIdRule []interface{}
	for _, refundIdItem := range refundId {
		refundIdRule = append(refundIdRule, refundIdItem)
	}
	var userRule []interface{}
	for _, userItem := range user {
		userRule = append(userRule, userItem)
	}

	logs, sub, err := _CardSpendController.contract.FilterLogs(opts, "Refunded", authIdRule, refundIdRule, userRule)
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerRefundedIterator{contract: _CardSpendController.contract, event: "Refunded", logs: logs, sub: sub}, nil
}

// WatchRefunded is a free log subscription operation binding the contract event 0x6c5895acb60b66e78106939eaaa3976db6325f801ff434fe24ff7cb0a6795a5f.
//
// Solidity: event Refunded(bytes32 indexed authId, bytes32 indexed refundId, address indexed user, uint256 amount)
func (_CardSpendController *CardSpendControllerFilterer) WatchRefunded(opts *bind.WatchOpts, sink chan<- *CardSpendControllerRefunded, authId [][32]byte, refundId [][32]byte, user []common.Address) (event.Subscription, error) {

	var authIdRule []interface{}
	for _, authIdItem := range authId {
		authIdRule = append(authIdRule, authIdItem)
	}
	var refundIdRule []interface{}
	for _, refundIdItem := range refundId {
		refundIdRule = append(refundIdRule, refundIdItem)
	}
	var userRule []interface{}
	for _, userItem := range user {
		userRule = append(userRule, userItem)
	}

	logs, sub, err := _CardSpendController.contract.WatchLogs(opts, "Refunded", authIdRule, refundIdRule, userRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(CardSpendControllerRefunded)
				if err := _CardSpendController.contract.UnpackLog(event, "Refunded", log); err != nil {
					// If the signature doesn't match, skip this log.
					if errors.Is(err, bind.ErrEventSignatureMismatch) {
						continue
					}
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseRefunded is a log parse operation binding the contract event 0x6c5895acb60b66e78106939eaaa3976db6325f801ff434fe24ff7cb0a6795a5f.
//
// Solidity: event Refunded(bytes32 indexed authId, bytes32 indexed refundId, address indexed user, uint256 amount)
func (_CardSpendController *CardSpendControllerFilterer) ParseRefunded(log types.Log) (*CardSpendControllerRefunded, error) {
	event := new(CardSpendControllerRefunded)
	if err := _CardSpendController.contract.UnpackLog(event, "Refunded", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// CardSpendControllerRoleAdminChangedIterator is returned from FilterRoleAdminChanged and is used to iterate over the raw logs and unpacked data for RoleAdminChanged events raised by the CardSpendController contract.
type CardSpendControllerRoleAdminChangedIterator struct {
	Event *CardSpendControllerRoleAdminChanged // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *CardSpendControllerRoleAdminChangedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(CardSpendControllerRoleAdminChanged)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(CardSpendControllerRoleAdminChanged)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *CardSpendControllerRoleAdminChangedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *CardSpendControllerRoleAdminChangedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// CardSpendControllerRoleAdminChanged represents a RoleAdminChanged event raised by the CardSpendController contract.
type CardSpendControllerRoleAdminChanged struct {
	Role              [32]byte
	PreviousAdminRole [32]byte
	NewAdminRole      [32]byte
	Raw               types.Log // Blockchain specific contextual infos
}

// FilterRoleAdminChanged is a free log retrieval operation binding the contract event 0xbd79b86ffe0ab8e8776151514217cd7cacd52c909f66475c3af44e129f0b00ff.
//
// Solidity: event RoleAdminChanged(bytes32 indexed role, bytes32 indexed previousAdminRole, bytes32 indexed newAdminRole)
func (_CardSpendController *CardSpendControllerFilterer) FilterRoleAdminChanged(opts *bind.FilterOpts, role [][32]byte, previousAdminRole [][32]byte, newAdminRole [][32]byte) (*CardSpendControllerRoleAdminChangedIterator, error) {

	var roleRule []interface{}
	for _, roleItem := range role {
		roleRule = append(roleRule, roleItem)
	}
	var previousAdminRoleRule []interface{}
	for _, previousAdminRoleItem := range previousAdminRole {
		previousAdminRoleRule = append(previousAdminRoleRule, previousAdminRoleItem)
	}
	var newAdminRoleRule []interface{}
	for _, newAdminRoleItem := range newAdminRole {
		newAdminRoleRule = append(newAdminRoleRule, newAdminRoleItem)
	}

	logs, sub, err := _CardSpendController.contract.FilterLogs(opts, "RoleAdminChanged", roleRule, previousAdminRoleRule, newAdminRoleRule)
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerRoleAdminChangedIterator{contract: _CardSpendController.contract, event: "RoleAdminChanged", logs: logs, sub: sub}, nil
}

// WatchRoleAdminChanged is a free log subscription operation binding the contract event 0xbd79b86ffe0ab8e8776151514217cd7cacd52c909f66475c3af44e129f0b00ff.
//
// Solidity: event RoleAdminChanged(bytes32 indexed role, bytes32 indexed previousAdminRole, bytes32 indexed newAdminRole)
func (_CardSpendController *CardSpendControllerFilterer) WatchRoleAdminChanged(opts *bind.WatchOpts, sink chan<- *CardSpendControllerRoleAdminChanged, role [][32]byte, previousAdminRole [][32]byte, newAdminRole [][32]byte) (event.Subscription, error) {

	var roleRule []interface{}
	for _, roleItem := range role {
		roleRule = append(roleRule, roleItem)
	}
	var previousAdminRoleRule []interface{}
	for _, previousAdminRoleItem := range previousAdminRole {
		previousAdminRoleRule = append(previousAdminRoleRule, previousAdminRoleItem)
	}
	var newAdminRoleRule []interface{}
	for _, newAdminRoleItem := range newAdminRole {
		newAdminRoleRule = append(newAdminRoleRule, newAdminRoleItem)
	}

	logs, sub, err := _CardSpendController.contract.WatchLogs(opts, "RoleAdminChanged", roleRule, previousAdminRoleRule, newAdminRoleRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(CardSpendControllerRoleAdminChanged)
				if err := _CardSpendController.contract.UnpackLog(event, "RoleAdminChanged", log); err != nil {
					// If the signature doesn't match, skip this log.
					if errors.Is(err, bind.ErrEventSignatureMismatch) {
						continue
					}
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseRoleAdminChanged is a log parse operation binding the contract event 0xbd79b86ffe0ab8e8776151514217cd7cacd52c909f66475c3af44e129f0b00ff.
//
// Solidity: event RoleAdminChanged(bytes32 indexed role, bytes32 indexed previousAdminRole, bytes32 indexed newAdminRole)
func (_CardSpendController *CardSpendControllerFilterer) ParseRoleAdminChanged(log types.Log) (*CardSpendControllerRoleAdminChanged, error) {
	event := new(CardSpendControllerRoleAdminChanged)
	if err := _CardSpendController.contract.UnpackLog(event, "RoleAdminChanged", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// CardSpendControllerRoleGrantedIterator is returned from FilterRoleGranted and is used to iterate over the raw logs and unpacked data for RoleGranted events raised by the CardSpendController contract.
type CardSpendControllerRoleGrantedIterator struct {
	Event *CardSpendControllerRoleGranted // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *CardSpendControllerRoleGrantedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(CardSpendControllerRoleGranted)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(CardSpendControllerRoleGranted)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *CardSpendControllerRoleGrantedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *CardSpendControllerRoleGrantedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// CardSpendControllerRoleGranted represents a RoleGranted event raised by the CardSpendController contract.
type CardSpendControllerRoleGranted struct {
	Role    [32]byte
	Account common.Address
	Sender  common.Address
	Raw     types.Log // Blockchain specific contextual infos
}

// FilterRoleGranted is a free log retrieval operation binding the contract event 0x2f8788117e7eff1d82e926ec794901d17c78024a50270940304540a733656f0d.
//
// Solidity: event RoleGranted(bytes32 indexed role, address indexed account, address indexed sender)
func (_CardSpendController *CardSpendControllerFilterer) FilterRoleGranted(opts *bind.FilterOpts, role [][32]byte, account []common.Address, sender []common.Address) (*CardSpendControllerRoleGrantedIterator, error) {

	var roleRule []interface{}
	for _, roleItem := range role {
		roleRule = append(roleRule, roleItem)
	}
	var accountRule []interface{}
	for _, accountItem := range account {
		accountRule = append(accountRule, accountItem)
	}
	var senderRule []interface{}
	for _, senderItem := range sender {
		senderRule = append(senderRule, senderItem)
	}

	logs, sub, err := _CardSpendController.contract.FilterLogs(opts, "RoleGranted", roleRule, accountRule, senderRule)
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerRoleGrantedIterator{contract: _CardSpendController.contract, event: "RoleGranted", logs: logs, sub: sub}, nil
}

// WatchRoleGranted is a free log subscription operation binding the contract event 0x2f8788117e7eff1d82e926ec794901d17c78024a50270940304540a733656f0d.
//
// Solidity: event RoleGranted(bytes32 indexed role, address indexed account, address indexed sender)
func (_CardSpendController *CardSpendControllerFilterer) WatchRoleGranted(opts *bind.WatchOpts, sink chan<- *CardSpendControllerRoleGranted, role [][32]byte, account []common.Address, sender []common.Address) (event.Subscription, error) {

	var roleRule []interface{}
	for _, roleItem := range role {
		roleRule = append(roleRule, roleItem)
	}
	var accountRule []interface{}
	for _, accountItem := range account {
		accountRule = append(accountRule, accountItem)
	}
	var senderRule []interface{}
	for _, senderItem := range sender {
		senderRule = append(senderRule, senderItem)
	}

	logs, sub, err := _CardSpendController.contract.WatchLogs(opts, "RoleGranted", roleRule, accountRule, senderRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(CardSpendControllerRoleGranted)
				if err := _CardSpendController.contract.UnpackLog(event, "RoleGranted", log); err != nil {
					// If the signature doesn't match, skip this log.
					if errors.Is(err, bind.ErrEventSignatureMismatch) {
						continue
					}
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseRoleGranted is a log parse operation binding the contract event 0x2f8788117e7eff1d82e926ec794901d17c78024a50270940304540a733656f0d.
//
// Solidity: event RoleGranted(bytes32 indexed role, address indexed account, address indexed sender)
func (_CardSpendController *CardSpendControllerFilterer) ParseRoleGranted(log types.Log) (*CardSpendControllerRoleGranted, error) {
	event := new(CardSpendControllerRoleGranted)
	if err := _CardSpendController.contract.UnpackLog(event, "RoleGranted", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// CardSpendControllerRoleRevokedIterator is returned from FilterRoleRevoked and is used to iterate over the raw logs and unpacked data for RoleRevoked events raised by the CardSpendController contract.
type CardSpendControllerRoleRevokedIterator struct {
	Event *CardSpendControllerRoleRevoked // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *CardSpendControllerRoleRevokedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(CardSpendControllerRoleRevoked)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(CardSpendControllerRoleRevoked)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *CardSpendControllerRoleRevokedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *CardSpendControllerRoleRevokedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// CardSpendControllerRoleRevoked represents a RoleRevoked event raised by the CardSpendController contract.
type CardSpendControllerRoleRevoked struct {
	Role    [32]byte
	Account common.Address
	Sender  common.Address
	Raw     types.Log // Blockchain specific contextual infos
}

// FilterRoleRevoked is a free log retrieval operation binding the contract event 0xf6391f5c32d9c69d2a47ea670b442974b53935d1edc7fd64eb21e047a839171b.
//
// Solidity: event RoleRevoked(bytes32 indexed role, address indexed account, address indexed sender)
func (_CardSpendController *CardSpendControllerFilterer) FilterRoleRevoked(opts *bind.FilterOpts, role [][32]byte, account []common.Address, sender []common.Address) (*CardSpendControllerRoleRevokedIterator, error) {

	var roleRule []interface{}
	for _, roleItem := range role {
		roleRule = append(roleRule, roleItem)
	}
	var accountRule []interface{}
	for _, accountItem := range account {
		accountRule = append(accountRule, accountItem)
	}
	var senderRule []interface{}
	for _, senderItem := range sender {
		senderRule = append(senderRule, senderItem)
	}

	logs, sub, err := _CardSpendController.contract.FilterLogs(opts, "RoleRevoked", roleRule, accountRule, senderRule)
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerRoleRevokedIterator{contract: _CardSpendController.contract, event: "RoleRevoked", logs: logs, sub: sub}, nil
}

// WatchRoleRevoked is a free log subscription operation binding the contract event 0xf6391f5c32d9c69d2a47ea670b442974b53935d1edc7fd64eb21e047a839171b.
//
// Solidity: event RoleRevoked(bytes32 indexed role, address indexed account, address indexed sender)
func (_CardSpendController *CardSpendControllerFilterer) WatchRoleRevoked(opts *bind.WatchOpts, sink chan<- *CardSpendControllerRoleRevoked, role [][32]byte, account []common.Address, sender []common.Address) (event.Subscription, error) {

	var roleRule []interface{}
	for _, roleItem := range role {
		roleRule = append(roleRule, roleItem)
	}
	var accountRule []interface{}
	for _, accountItem := range account {
		accountRule = append(accountRule, accountItem)
	}
	var senderRule []interface{}
	for _, senderItem := range sender {
		senderRule = append(senderRule, senderItem)
	}

	logs, sub, err := _CardSpendController.contract.WatchLogs(opts, "RoleRevoked", roleRule, accountRule, senderRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(CardSpendControllerRoleRevoked)
				if err := _CardSpendController.contract.UnpackLog(event, "RoleRevoked", log); err != nil {
					// If the signature doesn't match, skip this log.
					if errors.Is(err, bind.ErrEventSignatureMismatch) {
						continue
					}
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseRoleRevoked is a log parse operation binding the contract event 0xf6391f5c32d9c69d2a47ea670b442974b53935d1edc7fd64eb21e047a839171b.
//
// Solidity: event RoleRevoked(bytes32 indexed role, address indexed account, address indexed sender)
func (_CardSpendController *CardSpendControllerFilterer) ParseRoleRevoked(log types.Log) (*CardSpendControllerRoleRevoked, error) {
	event := new(CardSpendControllerRoleRevoked)
	if err := _CardSpendController.contract.UnpackLog(event, "RoleRevoked", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// CardSpendControllerUnpausedIterator is returned from FilterUnpaused and is used to iterate over the raw logs and unpacked data for Unpaused events raised by the CardSpendController contract.
type CardSpendControllerUnpausedIterator struct {
	Event *CardSpendControllerUnpaused // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *CardSpendControllerUnpausedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(CardSpendControllerUnpaused)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(CardSpendControllerUnpaused)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *CardSpendControllerUnpausedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *CardSpendControllerUnpausedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// CardSpendControllerUnpaused represents a Unpaused event raised by the CardSpendController contract.
type CardSpendControllerUnpaused struct {
	Account common.Address
	Raw     types.Log // Blockchain specific contextual infos
}

// FilterUnpaused is a free log retrieval operation binding the contract event 0x5db9ee0a495bf2e6ff9c91a7834c1ba4fdd244a5e8aa4e537bd38aeae4b073aa.
//
// Solidity: event Unpaused(address account)
func (_CardSpendController *CardSpendControllerFilterer) FilterUnpaused(opts *bind.FilterOpts) (*CardSpendControllerUnpausedIterator, error) {

	logs, sub, err := _CardSpendController.contract.FilterLogs(opts, "Unpaused")
	if err != nil {
		return nil, err
	}
	return &CardSpendControllerUnpausedIterator{contract: _CardSpendController.contract, event: "Unpaused", logs: logs, sub: sub}, nil
}

// WatchUnpaused is a free log subscription operation binding the contract event 0x5db9ee0a495bf2e6ff9c91a7834c1ba4fdd244a5e8aa4e537bd38aeae4b073aa.
//
// Solidity: event Unpaused(address account)
func (_CardSpendController *CardSpendControllerFilterer) WatchUnpaused(opts *bind.WatchOpts, sink chan<- *CardSpendControllerUnpaused) (event.Subscription, error) {

	logs, sub, err := _CardSpendController.contract.WatchLogs(opts, "Unpaused")
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(CardSpendControllerUnpaused)
				if err := _CardSpendController.contract.UnpackLog(event, "Unpaused", log); err != nil {
					// If the signature doesn't match, skip this log.
					if errors.Is(err, bind.ErrEventSignatureMismatch) {
						continue
					}
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseUnpaused is a log parse operation binding the contract event 0x5db9ee0a495bf2e6ff9c91a7834c1ba4fdd244a5e8aa4e537bd38aeae4b073aa.
//
// Solidity: event Unpaused(address account)
func (_CardSpendController *CardSpendControllerFilterer) ParseUnpaused(log types.Log) (*CardSpendControllerUnpaused, error) {
	event := new(CardSpendControllerUnpaused)
	if err := _CardSpendController.contract.UnpackLog(event, "Unpaused", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}
