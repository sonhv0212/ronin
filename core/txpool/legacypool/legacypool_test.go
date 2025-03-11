// Copyright 2015 The go-ethereum Authors
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

package legacypool

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"math/rand"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
)

var (
	// testTxPoolConfig is a transaction pool configuration without stateful disk
	// sideeffects used during testing.
	testTxPoolConfig Config

	// eip1559Config is a chain config with EIP-1559 enabled at block 0.
	eip1559Config *params.ChainConfig
)

func init() {
	testTxPoolConfig = DefaultConfig
	testTxPoolConfig.Journal = ""

	cpy := *params.TestChainConfig
	eip1559Config = &cpy
	eip1559Config.MikoBlock = common.Big0
	eip1559Config.BerlinBlock = common.Big0
	eip1559Config.LondonBlock = common.Big0
}

type testBlockChain struct {
	gasLimit      uint64 // must be first field for 64 bit alignment (atomic access)
	statedb       *state.StateDB
	chainHeadFeed *event.Feed
	headerTime    uint64
}

func (bc *testBlockChain) CurrentBlock() *types.Block {
	return types.NewBlock(&types.Header{
		GasLimit: atomic.LoadUint64(&bc.gasLimit),
		Time:     bc.headerTime,
	}, nil, nil, nil, trie.NewStackTrie(nil))
}

func (bc *testBlockChain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return bc.CurrentBlock()
}

func (bc *testBlockChain) StateAt(common.Hash) (*state.StateDB, error) {
	return bc.statedb, nil
}

func (bc *testBlockChain) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return bc.chainHeadFeed.Subscribe(ch)
}

func transaction(nonce uint64, gaslimit uint64, key *ecdsa.PrivateKey) *types.Transaction {
	return pricedTransaction(nonce, gaslimit, big.NewInt(1), key)
}

func pricedTransaction(nonce uint64, gaslimit uint64, gasprice *big.Int, key *ecdsa.PrivateKey) *types.Transaction {
	tx, _ := types.SignTx(types.NewTransaction(nonce, common.Address{}, big.NewInt(100), gaslimit, gasprice, nil), types.HomesteadSigner{}, key)
	return tx
}

func pricedDataTransaction(nonce uint64, gaslimit uint64, gasprice *big.Int, key *ecdsa.PrivateKey, bytes uint64) *types.Transaction {
	data := make([]byte, bytes)
	rand.Read(data)

	tx, _ := types.SignTx(types.NewTransaction(nonce, common.Address{}, big.NewInt(0), gaslimit, gasprice, data), types.HomesteadSigner{}, key)
	return tx
}

func dynamicFeeTx(nonce uint64, gaslimit uint64, gasFee *big.Int, tip *big.Int, key *ecdsa.PrivateKey) *types.Transaction {
	tx, _ := types.SignNewTx(key, types.LatestSignerForChainID(params.TestChainConfig.ChainID), &types.DynamicFeeTx{
		ChainID:    params.TestChainConfig.ChainID,
		Nonce:      nonce,
		GasTipCap:  tip,
		GasFeeCap:  gasFee,
		Gas:        gaslimit,
		To:         &common.Address{},
		Value:      big.NewInt(100),
		Data:       nil,
		AccessList: nil,
	})
	return tx
}

func setupPool() (*LegacyPool, *ecdsa.PrivateKey) {
	return setupPoolWithConfig(params.TestChainConfig)
}

func setupPoolWithConfig(config *params.ChainConfig) (*LegacyPool, *ecdsa.PrivateKey) {
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{10000000, statedb, new(event.Feed), 0}

	key, _ := crypto.GenerateKey()
	pool := New(testTxPoolConfig, config, blockchain)
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// wait for the pool to initialize
	<-pool.initDoneCh
	return pool, key
}

// validatePoolInternals checks various consistency invariants within the pool.
func validatePoolInternals(pool *LegacyPool) error {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	// Ensure the total transaction set is consistent with pending + queued
	pending, queued := pool.stats()
	if total := pool.all.Count(); total != pending+queued {
		return fmt.Errorf("total transaction count %d != %d pending + %d queued", total, pending, queued)
	}
	pool.priced.Reheap()
	priced, remote := pool.priced.urgent.Len()+pool.priced.floating.Len(), pool.all.RemoteCount()
	if priced != remote {
		return fmt.Errorf("total priced transaction count %d != %d", priced, remote)
	}
	// Ensure the next nonce to assign is the correct one
	for addr, txs := range pool.pending {
		// Find the last transaction
		var last uint64
		for nonce := range txs.txs.items {
			if last < nonce {
				last = nonce
			}
		}
		if nonce := pool.pendingNonces.get(addr); nonce != last+1 {
			return fmt.Errorf("pending nonce mismatch: have %v, want %v", nonce, last+1)
		}
		if txs.totalcost.Cmp(common.Big0) < 0 {
			return fmt.Errorf("totalcost went negative: %v", txs.totalcost)
		}
	}
	// Ensure all auths in pool are tracked
	for _, tx := range pool.all.locals {
		for _, addr := range tx.SetCodeAuthorities() {
			list := pool.all.auths[addr]
			if i := slices.Index(list, tx.Hash()); i < 0 {
				return fmt.Errorf("authority not tracked: addr %s, tx %s", addr, tx.Hash())
			}
		}
	}
	for _, tx := range pool.all.remotes {
		for _, addr := range tx.SetCodeAuthorities() {
			list := pool.all.auths[addr]
			if i := slices.Index(list, tx.Hash()); i < 0 {
				return fmt.Errorf("authority not tracked: addr %s, tx %s", addr, tx.Hash())
			}
		}
	}
	// Ensure all auths in pool have an associated tx.
	for addr, hashes := range pool.all.auths {
		for _, hash := range hashes {
			_, foundLocal := pool.all.locals[hash]
			_, foundRemote := pool.all.remotes[hash]
			if !foundLocal && !foundRemote {
				return fmt.Errorf("dangling authority, missing originating tx: addr %s, hash %s", addr, hash.Hex())
			}
		}
	}
	return nil
}

// validateEvents checks that the correct number of transaction addition events
// were fired on the pool's event feed.
func validateEvents(events chan core.NewTxsEvent, count int) error {
	var received []*types.Transaction

	for len(received) < count {
		select {
		case ev := <-events:
			received = append(received, ev.Txs...)
		case <-time.After(time.Second):
			return fmt.Errorf("event #%d not fired", len(received))
		}
	}
	if len(received) > count {
		return fmt.Errorf("more than %d events fired: %v", count, received[count:])
	}
	select {
	case ev := <-events:
		return fmt.Errorf("more than %d events fired: %v", count, ev.Txs)

	case <-time.After(50 * time.Millisecond):
		// This branch should be "default", but it's a data race between goroutines,
		// reading the event channel and pushing into it, so better wait a bit ensuring
		// really nothing gets injected.
	}
	return nil
}

func deriveSender(tx *types.Transaction) (common.Address, error) {
	return types.Sender(types.HomesteadSigner{}, tx)
}

type unsignedAuth struct {
	nonce uint64
	key   *ecdsa.PrivateKey
}

func setCodeTx(nonce uint64, key *ecdsa.PrivateKey, unsigned []unsignedAuth) *types.Transaction {
	return pricedSetCodeTx(nonce, 250000, uint256.NewInt(1000), uint256.NewInt(1), key, unsigned)
}

func pricedSetCodeTx(nonce uint64, gaslimit uint64, gasFee, tip *uint256.Int, key *ecdsa.PrivateKey, unsigned []unsignedAuth) *types.Transaction {
	var authList []types.Authorization
	for _, u := range unsigned {
		auth, _ := types.SignAuth(types.Authorization{
			ChainID: params.TestChainConfig.ChainID.Uint64(),
			Address: common.Address{0x42},
			Nonce:   u.nonce,
		}, u.key)
		authList = append(authList, auth)
	}
	return pricedSetCodeTxWithAuth(nonce, gaslimit, gasFee, tip, key, authList)
}

func pricedSetCodeTxWithAuth(nonce uint64, gaslimit uint64, gasFee, tip *uint256.Int, key *ecdsa.PrivateKey, authList []types.Authorization) *types.Transaction {
	return types.MustSignNewTx(key, types.LatestSignerForChainID(params.TestChainConfig.ChainID), &types.SetCodeTx{
		ChainID:    params.TestChainConfig.ChainID.Uint64(),
		Nonce:      nonce,
		GasTipCap:  tip,
		GasFeeCap:  gasFee,
		Gas:        gaslimit,
		To:         common.Address{},
		Value:      uint256.NewInt(100),
		Data:       nil,
		AccessList: nil,
		AuthList:   authList,
	})
}

func makeAddressReserver() txpool.AddressReserver {
	var (
		reserved = make(map[common.Address]struct{})
		lock     sync.Mutex
	)
	return func(addr common.Address, reserve bool) error {
		lock.Lock()
		defer lock.Unlock()

		_, exists := reserved[addr]
		if reserve {
			if exists {
				panic("already reserved")
			}
			reserved[addr] = struct{}{}
			return nil
		}
		if !exists {
			panic("not reserved")
		}
		delete(reserved, addr)
		return nil
	}
}

type testChain struct {
	*testBlockChain
	address common.Address
	trigger *bool
}

// testChain.State() is used multiple times to reset the pending state.
// when simulate is true it will create a state that indicates
// that tx0 and tx1 are included in the chain.
func (c *testChain) State() (*state.StateDB, error) {
	// delay "state change" by one. The tx pool fetches the
	// state multiple times and by delaying it a bit we simulate
	// a state change between those fetches.
	stdb := c.statedb
	if *c.trigger {
		c.statedb, _ = state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
		// simulate that the new head block included tx0 and tx1
		c.statedb.SetNonce(c.address, 2)
		c.statedb.SetBalance(c.address, new(big.Int).SetUint64(params.Ether))
		*c.trigger = false
	}
	return stdb, nil
}

// This test simulates a scenario where a new block is imported during a
// state reset and tests whether the pending state is in sync with the
// block head event that initiated the resetState().
func TestStateChangeDuringReset(t *testing.T) {
	t.Parallel()

	var (
		key, _     = crypto.GenerateKey()
		address    = crypto.PubkeyToAddress(key.PublicKey)
		statedb, _ = state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
		trigger    = false
	)

	// setup pool with 2 transaction in it
	statedb.SetBalance(address, new(big.Int).SetUint64(params.Ether))
	blockchain := &testChain{&testBlockChain{1000000000, statedb, new(event.Feed), 0}, address, &trigger}

	tx0 := transaction(0, 100000, key)
	tx1 := transaction(1, 100000, key)

	pool := New(testTxPoolConfig, params.TestChainConfig, blockchain)
	defer pool.Close()
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	nonce := pool.Nonce(address)
	if nonce != 0 {
		t.Fatalf("Invalid nonce, want 0, got %d", nonce)
	}

	pool.AddRemotesSync([]*types.Transaction{tx0, tx1})

	nonce = pool.Nonce(address)
	if nonce != 2 {
		t.Fatalf("Invalid nonce, want 2, got %d", nonce)
	}

	// trigger state change in the background
	trigger = true
	<-pool.requestReset(nil, nil)

	nonce = pool.Nonce(address)
	if nonce != 2 {
		t.Fatalf("Invalid nonce, want 2, got %d", nonce)
	}
}

func testAddBalance(pool *LegacyPool, addr common.Address, amount *big.Int) {
	pool.mu.Lock()
	pool.currentState.AddBalance(addr, amount)
	pool.mu.Unlock()
}

func testSetNonce(pool *LegacyPool, addr common.Address, nonce uint64) {
	pool.mu.Lock()
	pool.currentState.SetNonce(addr, nonce)
	pool.mu.Unlock()
}

func TestInvalidTransactions(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx := transaction(0, 100, key)
	from, _ := deriveSender(tx)

	// Intrinsic gas too low
	testAddBalance(pool, from, big.NewInt(1))
	if err, want := pool.AddRemote(tx), core.ErrIntrinsicGas; !errors.Is(err, want) {
		t.Errorf("want %v have %v", want, err)
	}

	// Insufficient funds
	tx = transaction(0, 100000, key)
	if err, want := pool.AddRemote(tx), core.ErrInsufficientFunds; !errors.Is(err, want) {
		t.Errorf("want %v have %v", want, err)
	}

	testSetNonce(pool, from, 1)
	testAddBalance(pool, from, big.NewInt(0xffffffffffffff))
	tx = transaction(0, 100000, key)
	if err, want := pool.AddRemote(tx), core.ErrNonceTooLow; !errors.Is(err, want) {
		t.Errorf("want %v have %v", want, err)
	}

	tx = transaction(1, 100000, key)
	pool.gasTip.Store(big.NewInt(1000))
	if err, want := pool.AddRemote(tx), txpool.ErrUnderpriced; !errors.Is(err, want) {
		t.Errorf("want %v have %v", want, err)
	}
	if err := pool.AddLocal(tx); err != nil {
		t.Error("expected", nil, "got", err)
	}
}

func TestQueue(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx := transaction(0, 100, key)
	from, _ := deriveSender(tx)
	testAddBalance(pool, from, big.NewInt(1000))
	<-pool.requestReset(nil, nil)

	pool.enqueueTx(tx.Hash(), tx, false, true)
	<-pool.requestPromoteExecutables(newAccountSet(pool.signer, from))
	if len(pool.pending) != 1 {
		t.Error("expected valid txs to be 1 is", len(pool.pending))
	}

	tx = transaction(1, 100, key)
	from, _ = deriveSender(tx)
	testSetNonce(pool, from, 2)
	pool.enqueueTx(tx.Hash(), tx, false, true)

	<-pool.requestPromoteExecutables(newAccountSet(pool.signer, from))
	if _, ok := pool.pending[from].txs.items[tx.Nonce()]; ok {
		t.Error("expected transaction to be in tx pool")
	}
	if len(pool.queue) > 0 {
		t.Error("expected transaction queue to be empty. is", len(pool.queue))
	}
}

func TestQueue2(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx1 := transaction(0, 100, key)
	tx2 := transaction(10, 100, key)
	tx3 := transaction(11, 100, key)
	from, _ := deriveSender(tx1)
	testAddBalance(pool, from, big.NewInt(1000))
	pool.reset(nil, nil)

	pool.enqueueTx(tx1.Hash(), tx1, false, true)
	pool.enqueueTx(tx2.Hash(), tx2, false, true)
	pool.enqueueTx(tx3.Hash(), tx3, false, true)

	pool.promoteExecutables([]common.Address{from})
	if len(pool.pending) != 1 {
		t.Error("expected pending length to be 1, got", len(pool.pending))
	}
	if pool.queue[from].Len() != 2 {
		t.Error("expected len(queue) == 2, got", pool.queue[from].Len())
	}
}

func TestNegativeValue(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx, _ := types.SignTx(types.NewTransaction(0, common.Address{}, big.NewInt(-1), 100, big.NewInt(1), nil), types.HomesteadSigner{}, key)
	from, _ := deriveSender(tx)
	testAddBalance(pool, from, big.NewInt(1))
	if err := pool.AddRemote(tx); err != txpool.ErrNegativeValue {
		t.Error("expected", txpool.ErrNegativeValue, "got", err)
	}
}

func TestTipAboveFeeCap(t *testing.T) {
	t.Parallel()

	pool, key := setupPoolWithConfig(eip1559Config)
	defer pool.Close()

	tx := dynamicFeeTx(0, 100, big.NewInt(1), big.NewInt(2), key)

	if err := pool.AddRemote(tx); err != core.ErrTipAboveFeeCap {
		t.Error("expected", core.ErrTipAboveFeeCap, "got", err)
	}
}

func TestVeryHighValues(t *testing.T) {
	t.Parallel()

	pool, key := setupPoolWithConfig(eip1559Config)
	defer pool.Close()

	veryBigNumber := big.NewInt(1)
	veryBigNumber.Lsh(veryBigNumber, 300)

	tx := dynamicFeeTx(0, 100, big.NewInt(1), veryBigNumber, key)
	if err := pool.AddRemote(tx); err != core.ErrTipVeryHigh {
		t.Error("expected", core.ErrTipVeryHigh, "got", err)
	}

	tx2 := dynamicFeeTx(0, 100, veryBigNumber, big.NewInt(1), key)
	if err := pool.AddRemote(tx2); err != core.ErrFeeCapVeryHigh {
		t.Error("expected", core.ErrFeeCapVeryHigh, "got", err)
	}
}

func TestChainFork(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	addr := crypto.PubkeyToAddress(key.PublicKey)
	resetState := func() {
		statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
		statedb.AddBalance(addr, big.NewInt(100000000000000))

		pool.chain = &testBlockChain{1000000, statedb, new(event.Feed), 0}
		<-pool.requestReset(nil, nil)
	}
	resetState()

	tx := transaction(0, 100000, key)
	if _, err := pool.add(tx, false); err != nil {
		t.Error("didn't expect error", err)
	}
	pool.removeTx(tx.Hash(), true, false)

	// reset the pool's internal state
	resetState()
	if _, err := pool.add(tx, false); err != nil {
		t.Error("didn't expect error", err)
	}
}

func TestDoubleNonce(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	addr := crypto.PubkeyToAddress(key.PublicKey)
	resetState := func() {
		statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
		statedb.AddBalance(addr, big.NewInt(100000000000000))

		pool.chain = &testBlockChain{1000000, statedb, new(event.Feed), 0}
		<-pool.requestReset(nil, nil)
	}
	resetState()

	signer := types.HomesteadSigner{}
	tx1, _ := types.SignTx(types.NewTransaction(0, common.Address{}, big.NewInt(100), 100000, big.NewInt(1), nil), signer, key)
	tx2, _ := types.SignTx(types.NewTransaction(0, common.Address{}, big.NewInt(100), 1000000, big.NewInt(2), nil), signer, key)
	tx3, _ := types.SignTx(types.NewTransaction(0, common.Address{}, big.NewInt(100), 1000000, big.NewInt(1), nil), signer, key)

	// Add the first two transaction, ensure higher priced stays only
	if replace, err := pool.add(tx1, false); err != nil || replace {
		t.Errorf("first transaction insert failed (%v) or reported replacement (%v)", err, replace)
	}
	if replace, err := pool.add(tx2, false); err != nil || !replace {
		t.Errorf("second transaction insert failed (%v) or not reported replacement (%v)", err, replace)
	}
	<-pool.requestPromoteExecutables(newAccountSet(signer, addr))
	if pool.pending[addr].Len() != 1 {
		t.Error("expected 1 pending transactions, got", pool.pending[addr].Len())
	}
	if tx := pool.pending[addr].txs.items[0]; tx.Hash() != tx2.Hash() {
		t.Errorf("transaction mismatch: have %x, want %x", tx.Hash(), tx2.Hash())
	}

	// Add the third transaction and ensure it's not saved (smaller price)
	pool.add(tx3, false)
	<-pool.requestPromoteExecutables(newAccountSet(signer, addr))
	if pool.pending[addr].Len() != 1 {
		t.Error("expected 1 pending transactions, got", pool.pending[addr].Len())
	}
	if tx := pool.pending[addr].txs.items[0]; tx.Hash() != tx2.Hash() {
		t.Errorf("transaction mismatch: have %x, want %x", tx.Hash(), tx2.Hash())
	}
	// Ensure the total transaction count is correct
	if pool.all.Count() != 1 {
		t.Error("expected 1 total transactions, got", pool.all.Count())
	}
}

func TestMissingNonce(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	addr := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, addr, big.NewInt(100000000000000))
	tx := transaction(1, 100000, key)
	if _, err := pool.add(tx, false); err != nil {
		t.Error("didn't expect error", err)
	}
	if len(pool.pending) != 0 {
		t.Error("expected 0 pending transactions, got", len(pool.pending))
	}
	if pool.queue[addr].Len() != 1 {
		t.Error("expected 1 queued transaction, got", pool.queue[addr].Len())
	}
	if pool.all.Count() != 1 {
		t.Error("expected 1 total transactions, got", pool.all.Count())
	}
}

func TestNonceRecovery(t *testing.T) {
	t.Parallel()

	const n = 10
	pool, key := setupPool()
	defer pool.Close()

	addr := crypto.PubkeyToAddress(key.PublicKey)
	testSetNonce(pool, addr, n)
	testAddBalance(pool, addr, big.NewInt(100000000000000))
	<-pool.requestReset(nil, nil)

	tx := transaction(n, 100000, key)
	if err := pool.AddRemote(tx); err != nil {
		t.Error(err)
	}
	// simulate some weird re-order of transactions and missing nonce(s)
	testSetNonce(pool, addr, n-1)
	<-pool.requestReset(nil, nil)
	if fn := pool.Nonce(addr); fn != n-1 {
		t.Errorf("expected nonce to be %d, got %d", n-1, fn)
	}
}

// Tests that if an account runs out of funds, any pending and queued transactions
// are dropped.
func TestDropping(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000))

	// Add some pending and some queued transactions
	var (
		tx0  = transaction(0, 100, key)
		tx1  = transaction(1, 200, key)
		tx2  = transaction(2, 300, key)
		tx10 = transaction(10, 100, key)
		tx11 = transaction(11, 200, key)
		tx12 = transaction(12, 300, key)
	)
	pool.all.Add(tx0, false)
	pool.priced.Put(tx0, false)
	pool.promoteTx(account, tx0.Hash(), tx0)

	pool.all.Add(tx1, false)
	pool.priced.Put(tx1, false)
	pool.promoteTx(account, tx1.Hash(), tx1)

	pool.all.Add(tx2, false)
	pool.priced.Put(tx2, false)
	pool.promoteTx(account, tx2.Hash(), tx2)

	pool.enqueueTx(tx10.Hash(), tx10, false, true)
	pool.enqueueTx(tx11.Hash(), tx11, false, true)
	pool.enqueueTx(tx12.Hash(), tx12, false, true)

	// Check that pre and post validations leave the pool as is
	if pool.pending[account].Len() != 3 {
		t.Errorf("pending transaction mismatch: have %d, want %d", pool.pending[account].Len(), 3)
	}
	if pool.queue[account].Len() != 3 {
		t.Errorf("queued transaction mismatch: have %d, want %d", pool.queue[account].Len(), 3)
	}
	if pool.all.Count() != 6 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 6)
	}
	<-pool.requestReset(nil, nil)
	if pool.pending[account].Len() != 3 {
		t.Errorf("pending transaction mismatch: have %d, want %d", pool.pending[account].Len(), 3)
	}
	if pool.queue[account].Len() != 3 {
		t.Errorf("queued transaction mismatch: have %d, want %d", pool.queue[account].Len(), 3)
	}
	if pool.all.Count() != 6 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 6)
	}
	// Reduce the balance of the account, and check that invalidated transactions are dropped
	testAddBalance(pool, account, big.NewInt(-650))
	<-pool.requestReset(nil, nil)

	if _, ok := pool.pending[account].txs.items[tx0.Nonce()]; !ok {
		t.Errorf("funded pending transaction missing: %v", tx0)
	}
	if _, ok := pool.pending[account].txs.items[tx1.Nonce()]; !ok {
		t.Errorf("funded pending transaction missing: %v", tx0)
	}
	if _, ok := pool.pending[account].txs.items[tx2.Nonce()]; ok {
		t.Errorf("out-of-fund pending transaction present: %v", tx1)
	}
	if _, ok := pool.queue[account].txs.items[tx10.Nonce()]; !ok {
		t.Errorf("funded queued transaction missing: %v", tx10)
	}
	if _, ok := pool.queue[account].txs.items[tx11.Nonce()]; !ok {
		t.Errorf("funded queued transaction missing: %v", tx10)
	}
	if _, ok := pool.queue[account].txs.items[tx12.Nonce()]; ok {
		t.Errorf("out-of-fund queued transaction present: %v", tx11)
	}
	if pool.all.Count() != 4 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 4)
	}
	// Reduce the block gas limit, check that invalidated transactions are dropped
	atomic.StoreUint64(&pool.chain.(*testBlockChain).gasLimit, 100)
	<-pool.requestReset(nil, nil)

	if _, ok := pool.pending[account].txs.items[tx0.Nonce()]; !ok {
		t.Errorf("funded pending transaction missing: %v", tx0)
	}
	if _, ok := pool.pending[account].txs.items[tx1.Nonce()]; ok {
		t.Errorf("over-gased pending transaction present: %v", tx1)
	}
	if _, ok := pool.queue[account].txs.items[tx10.Nonce()]; !ok {
		t.Errorf("funded queued transaction missing: %v", tx10)
	}
	if _, ok := pool.queue[account].txs.items[tx11.Nonce()]; ok {
		t.Errorf("over-gased queued transaction present: %v", tx11)
	}
	if pool.all.Count() != 2 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 2)
	}
}

// Tests that if a transaction is dropped from the current pending pool (e.g. out
// of fund), all consecutive (still valid, but not executable) transactions are
// postponed back into the future queue to prevent broadcasting them.
func TestPostponing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the postponing with
	pool, _ := setupPool()
	defer pool.Close()

	// Create two test accounts to produce different gap profiles with
	keys := make([]*ecdsa.PrivateKey, 2)
	accs := make([]common.Address, len(keys))

	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		accs[i] = crypto.PubkeyToAddress(keys[i].PublicKey)

		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(50100))
	}
	// Add a batch consecutive pending transactions for validation
	txs := []*types.Transaction{}
	for i, key := range keys {

		for j := 0; j < 100; j++ {
			var tx *types.Transaction
			if (i+j)%2 == 0 {
				tx = transaction(uint64(j), 25000, key)
			} else {
				tx = transaction(uint64(j), 50000, key)
			}
			txs = append(txs, tx)
		}
	}
	for i, err := range pool.AddRemotesSync(txs) {
		if err != nil {
			t.Fatalf("tx %d: failed to add transactions: %v", i, err)
		}
	}
	// Check that pre and post validations leave the pool as is
	if pending := pool.pending[accs[0]].Len() + pool.pending[accs[1]].Len(); pending != len(txs) {
		t.Errorf("pending transaction mismatch: have %d, want %d", pending, len(txs))
	}
	if len(pool.queue) != 0 {
		t.Errorf("queued accounts mismatch: have %d, want %d", len(pool.queue), 0)
	}
	if pool.all.Count() != len(txs) {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), len(txs))
	}
	<-pool.requestReset(nil, nil)
	if pending := pool.pending[accs[0]].Len() + pool.pending[accs[1]].Len(); pending != len(txs) {
		t.Errorf("pending transaction mismatch: have %d, want %d", pending, len(txs))
	}
	if len(pool.queue) != 0 {
		t.Errorf("queued accounts mismatch: have %d, want %d", len(pool.queue), 0)
	}
	if pool.all.Count() != len(txs) {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), len(txs))
	}
	// Reduce the balance of the account, and check that transactions are reorganised
	for _, addr := range accs {
		testAddBalance(pool, addr, big.NewInt(-1))
	}
	<-pool.requestReset(nil, nil)

	// The first account's first transaction remains valid, check that subsequent
	// ones are either filtered out, or queued up for later.
	if _, ok := pool.pending[accs[0]].txs.items[txs[0].Nonce()]; !ok {
		t.Errorf("tx %d: valid and funded transaction missing from pending pool: %v", 0, txs[0])
	}
	if _, ok := pool.queue[accs[0]].txs.items[txs[0].Nonce()]; ok {
		t.Errorf("tx %d: valid and funded transaction present in future queue: %v", 0, txs[0])
	}
	for i, tx := range txs[1:100] {
		if i%2 == 1 {
			if _, ok := pool.pending[accs[0]].txs.items[tx.Nonce()]; ok {
				t.Errorf("tx %d: valid but future transaction present in pending pool: %v", i+1, tx)
			}
			if _, ok := pool.queue[accs[0]].txs.items[tx.Nonce()]; !ok {
				t.Errorf("tx %d: valid but future transaction missing from future queue: %v", i+1, tx)
			}
		} else {
			if _, ok := pool.pending[accs[0]].txs.items[tx.Nonce()]; ok {
				t.Errorf("tx %d: out-of-fund transaction present in pending pool: %v", i+1, tx)
			}
			if _, ok := pool.queue[accs[0]].txs.items[tx.Nonce()]; ok {
				t.Errorf("tx %d: out-of-fund transaction present in future queue: %v", i+1, tx)
			}
		}
	}
	// The second account's first transaction got invalid, check that all transactions
	// are either filtered out, or queued up for later.
	if pool.pending[accs[1]] != nil {
		t.Errorf("invalidated account still has pending transactions")
	}
	for i, tx := range txs[100:] {
		if i%2 == 1 {
			if _, ok := pool.queue[accs[1]].txs.items[tx.Nonce()]; !ok {
				t.Errorf("tx %d: valid but future transaction missing from future queue: %v", 100+i, tx)
			}
		} else {
			if _, ok := pool.queue[accs[1]].txs.items[tx.Nonce()]; ok {
				t.Errorf("tx %d: out-of-fund transaction present in future queue: %v", 100+i, tx)
			}
		}
	}
	if pool.all.Count() != len(txs)/2 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), len(txs)/2)
	}
}

// Tests that if the transaction pool has both executable and non-executable
// transactions from an origin account, filling the nonce gap moves all queued
// ones into the pending pool.
func TestGapFilling(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan core.NewTxsEvent, testTxPoolConfig.AccountQueue+5)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a pending and a queued transaction with a nonce-gap in between
	pool.AddRemotesSync([]*types.Transaction{
		transaction(0, 100000, key),
		transaction(2, 100000, key),
	})
	pending, queued := pool.Stats()
	if pending != 1 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 1)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Fill the nonce gap and ensure all transactions become pending
	if err := pool.addRemoteSync(transaction(1, 100000, key)); err != nil {
		t.Fatalf("failed to add gapped transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("gap-filling event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if the transaction count belonging to a single account goes above
// some threshold, the higher transactions are dropped to prevent DOS attacks.
func TestQueueAccountLimiting(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	// Keep queuing up transactions and make sure all above a limit are dropped
	for i := uint64(1); i <= testTxPoolConfig.AccountQueue+5; i++ {
		if err := pool.addRemoteSync(transaction(i, 100000, key)); err != nil {
			t.Fatalf("tx %d: failed to add transaction: %v", i, err)
		}
		if len(pool.pending) != 0 {
			t.Errorf("tx %d: pending pool size mismatch: have %d, want %d", i, len(pool.pending), 0)
		}
		if i <= testTxPoolConfig.AccountQueue {
			if pool.queue[account].Len() != int(i) {
				t.Errorf("tx %d: queue size mismatch: have %d, want %d", i, pool.queue[account].Len(), i)
			}
		} else {
			if pool.queue[account].Len() != int(testTxPoolConfig.AccountQueue) {
				t.Errorf("tx %d: queue limit mismatch: have %d, want %d", i, pool.queue[account].Len(), testTxPoolConfig.AccountQueue)
			}
		}
	}
	if pool.all.Count() != int(testTxPoolConfig.AccountQueue) {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), testTxPoolConfig.AccountQueue)
	}
}

// Tests that if the transaction count belonging to multiple accounts go above
// some threshold, the higher transactions are dropped to prevent DOS attacks.
//
// This logic should not hold for local transactions, unless the local tracking
// mechanism is disabled.
func TestQueueGlobalLimiting(t *testing.T) {
	testQueueGlobalLimiting(t, false)
}
func TestQueueGlobalLimitingNoLocals(t *testing.T) {
	testQueueGlobalLimiting(t, true)
}

func testQueueGlobalLimiting(t *testing.T, nolocals bool) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	config := testTxPoolConfig
	config.NoLocals = nolocals
	config.GlobalQueue = config.AccountQueue*3 - 1 // reduce the queue limits to shorten test time (-1 to make it non divisible)

	pool := New(config, params.TestChainConfig, blockchain)
	defer pool.Close()
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// Create a number of test accounts and fund them (last one will be the local)
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	local := keys[len(keys)-1]

	// Generate and queue a batch of transactions
	nonces := make(map[common.Address]uint64)

	txs := make(types.Transactions, 0, 3*config.GlobalQueue)
	for len(txs) < cap(txs) {
		key := keys[rand.Intn(len(keys)-1)] // skip adding transactions with the local account
		addr := crypto.PubkeyToAddress(key.PublicKey)

		txs = append(txs, transaction(nonces[addr]+1, 100000, key))
		nonces[addr]++
	}
	// Import the batch and verify that limits have been enforced
	pool.AddRemotesSync(txs)

	queued := 0
	for addr, list := range pool.queue {
		if list.Len() > int(config.AccountQueue) {
			t.Errorf("addr %x: queued accounts overflown allowance: %d > %d", addr, list.Len(), config.AccountQueue)
		}
		queued += list.Len()
	}
	if queued > int(config.GlobalQueue) {
		t.Fatalf("total transactions overflow allowance: %d > %d", queued, config.GlobalQueue)
	}
	// Generate a batch of transactions from the local account and import them
	txs = txs[:0]
	for i := uint64(0); i < 3*config.GlobalQueue; i++ {
		txs = append(txs, transaction(i+1, 100000, local))
	}
	pool.AddLocals(txs)

	// If locals are disabled, the previous eviction algorithm should apply here too
	if nolocals {
		queued := 0
		for addr, list := range pool.queue {
			if list.Len() > int(config.AccountQueue) {
				t.Errorf("addr %x: queued accounts overflown allowance: %d > %d", addr, list.Len(), config.AccountQueue)
			}
			queued += list.Len()
		}
		if queued > int(config.GlobalQueue) {
			t.Fatalf("total transactions overflow allowance: %d > %d", queued, config.GlobalQueue)
		}
	} else {
		// Local exemptions are enabled, make sure the local account owned the queue
		if len(pool.queue) != 1 {
			t.Errorf("multiple accounts in queue: have %v, want %v", len(pool.queue), 1)
		}
		// Also ensure no local transactions are ever dropped, even if above global limits
		if queued := pool.queue[crypto.PubkeyToAddress(local.PublicKey)].Len(); uint64(queued) != 3*config.GlobalQueue {
			t.Fatalf("local account queued transaction count mismatch: have %v, want %v", queued, 3*config.GlobalQueue)
		}
	}
}

// Tests that if an account remains idle for a prolonged amount of time, any
// non-executable transactions queued up are dropped to prevent wasting resources
// on shuffling them around.
//
// This logic should not hold for local transactions, unless the local tracking
// mechanism is disabled.
func TestQueueTimeLimiting(t *testing.T) {
	testQueueTimeLimiting(t, false)
}
func TestQueueTimeLimitingNoLocals(t *testing.T) {
	testQueueTimeLimiting(t, true)
}

func testQueueTimeLimiting(t *testing.T, nolocals bool) {
	// Reduce the eviction interval to a testable amount
	defer func(old time.Duration) { evictionInterval = old }(evictionInterval)
	evictionInterval = time.Millisecond * 100

	// Create the pool to test the non-expiration enforcement
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	config := testTxPoolConfig
	config.Lifetime = time.Second
	config.NoLocals = nolocals

	pool := New(config, params.TestChainConfig, blockchain)
	defer pool.Close()
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// Create two test accounts to ensure remotes expire but locals do not
	local, _ := crypto.GenerateKey()
	remote, _ := crypto.GenerateKey()

	testAddBalance(pool, crypto.PubkeyToAddress(local.PublicKey), big.NewInt(1000000000))
	testAddBalance(pool, crypto.PubkeyToAddress(remote.PublicKey), big.NewInt(1000000000))

	// Add the two transactions and ensure they both are queued up
	if err := pool.AddLocal(pricedTransaction(1, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(1, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	pending, queued := pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// Allow the eviction interval to run
	time.Sleep(2 * evictionInterval)

	// Transactions should not be evicted from the queue yet since lifetime duration has not passed
	pending, queued = pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// Wait a bit for eviction to run and clean up any leftovers, and ensure only the local remains
	time.Sleep(2 * config.Lifetime)

	pending, queued = pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if nolocals {
		if queued != 0 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
		}
	} else {
		if queued != 1 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// remove current transactions and increase nonce to prepare for a reset and cleanup
	statedb.SetNonce(crypto.PubkeyToAddress(remote.PublicKey), 2)
	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 2)
	<-pool.requestReset(nil, nil)

	// make sure queue, pending are cleared
	pending, queued = pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// Queue gapped transactions
	if err := pool.AddLocal(pricedTransaction(4, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	if err := pool.addRemoteSync(pricedTransaction(4, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	time.Sleep(5 * evictionInterval) // A half lifetime pass

	// Queue executable transactions, the life cycle should be restarted.
	if err := pool.AddLocal(pricedTransaction(2, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	if err := pool.addRemoteSync(pricedTransaction(2, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	time.Sleep(6 * evictionInterval)

	// All gapped transactions shouldn't be kicked out
	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 3)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// The whole life time pass after last promotion, kick out stale transactions
	time.Sleep(2 * config.Lifetime)
	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if nolocals {
		if queued != 0 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
		}
	} else {
		if queued != 1 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that even if the transaction count belonging to a single account goes
// above some threshold, as long as the transactions are executable, they are
// accepted.
func TestPendingLimiting(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000000000))

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan core.NewTxsEvent, testTxPoolConfig.AccountQueue+5)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Keep queuing up transactions and make sure all above a limit are dropped
	for i := uint64(0); i < testTxPoolConfig.AccountQueue+5; i++ {
		if err := pool.addRemoteSync(transaction(i, 100000, key)); err != nil {
			t.Fatalf("tx %d: failed to add transaction: %v", i, err)
		}
		if pool.pending[account].Len() != int(i)+1 {
			t.Errorf("tx %d: pending pool size mismatch: have %d, want %d", i, pool.pending[account].Len(), i+1)
		}
		if len(pool.queue) != 0 {
			t.Errorf("tx %d: queue size mismatch: have %d, want %d", i, pool.queue[account].Len(), 0)
		}
	}
	if pool.all.Count() != int(testTxPoolConfig.AccountQueue+5) {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), testTxPoolConfig.AccountQueue+5)
	}
	if err := validateEvents(events, int(testTxPoolConfig.AccountQueue+5)); err != nil {
		t.Fatalf("event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if the transaction count belonging to multiple accounts go above
// some hard threshold, the higher transactions are dropped to prevent DOS
// attacks.
func TestPendingGlobalLimiting(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	config := testTxPoolConfig
	config.GlobalSlots = config.AccountSlots * 10

	pool := New(config, params.TestChainConfig, blockchain)
	defer pool.Close()
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions
	nonces := make(map[common.Address]uint64)

	txs := types.Transactions{}
	for _, key := range keys {
		addr := crypto.PubkeyToAddress(key.PublicKey)
		for j := 0; j < int(config.GlobalSlots)/len(keys)*2; j++ {
			txs = append(txs, transaction(nonces[addr], 100000, key))
			nonces[addr]++
		}
	}
	// Import the batch and verify that limits have been enforced
	pool.AddRemotesSync(txs)

	pending := 0
	for _, list := range pool.pending {
		pending += list.Len()
	}
	if pending > int(config.GlobalSlots) {
		t.Fatalf("total pending transactions overflow allowance: %d > %d", pending, config.GlobalSlots)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Test the limit on transaction size is enforced correctly.
// This test verifies every transaction having allowed size
// is added to the pool, and longer transactions are rejected.
func TestAllowedTxSize(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000000))

	// Compute maximal data size for transactions (lower bound).
	//
	// It is assumed the fields in the transaction (except of the data) are:
	//   - nonce     <= 32 bytes
	//   - gasTip  <= 32 bytes
	//   - gasLimit  <= 32 bytes
	//   - recipient == 20 bytes
	//   - value     <= 32 bytes
	//   - signature == 65 bytes
	// All those fields are summed up to at most 213 bytes.
	baseSize := uint64(213)
	dataSize := txMaxSize - baseSize
	// Try adding a transaction with maximal allowed size
	tx := pricedDataTransaction(0, pool.currentHead.Load().GasLimit, big.NewInt(1), key, dataSize)
	if err := pool.addRemoteSync(tx); err != nil {
		t.Fatalf("failed to add transaction of size %d, close to maximal: %v", int(tx.Size()), err)
	}
	// Try adding a transaction with random allowed size
	if err := pool.addRemoteSync(pricedDataTransaction(1, pool.currentHead.Load().GasLimit, big.NewInt(1), key, uint64(rand.Intn(int(dataSize))))); err != nil {
		t.Fatalf("failed to add transaction of random allowed size: %v", err)
	}
	// Try adding a transaction of minimal not allowed size
	if err := pool.addRemoteSync(pricedDataTransaction(2, pool.currentHead.Load().GasLimit, big.NewInt(1), key, txMaxSize)); err == nil {
		t.Fatalf("expected rejection on slightly oversize transaction")
	}
	// Try adding a transaction of random not allowed size
	if err := pool.addRemoteSync(pricedDataTransaction(2, pool.currentHead.Load().GasLimit, big.NewInt(1), key, dataSize+1+uint64(rand.Intn(10*txMaxSize)))); err == nil {
		t.Fatalf("expected rejection on oversize transaction")
	}
	// Run some sanity checks on the pool internals
	pending, queued := pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if transactions start being capped, transactions are also removed from 'all'
func TestCapClearsFromAll(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	config := testTxPoolConfig
	config.AccountSlots = 2
	config.AccountQueue = 2
	config.GlobalSlots = 8

	pool := New(config, params.TestChainConfig, blockchain)
	defer pool.Close()
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// Create a number of test accounts and fund them
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, addr, big.NewInt(1000000))

	txs := types.Transactions{}
	for j := 0; j < int(config.GlobalSlots)*2; j++ {
		txs = append(txs, transaction(uint64(j), 100000, key))
	}
	// Import the batch and verify that limits have been enforced
	pool.AddRemotes(txs)
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if the transaction count belonging to multiple accounts go above
// some hard threshold, if they are under the minimum guaranteed slot count then
// the transactions are still kept.
func TestPendingMinimumAllowance(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	config := testTxPoolConfig
	config.GlobalSlots = 1

	pool := New(config, params.TestChainConfig, blockchain)
	defer pool.Close()
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions
	nonces := make(map[common.Address]uint64)

	txs := types.Transactions{}
	for _, key := range keys {
		addr := crypto.PubkeyToAddress(key.PublicKey)
		for j := 0; j < int(config.AccountSlots)*2; j++ {
			txs = append(txs, transaction(nonces[addr], 100000, key))
			nonces[addr]++
		}
	}
	// Import the batch and verify that limits have been enforced
	pool.AddRemotesSync(txs)

	for addr, list := range pool.pending {
		if list.Len() != int(config.AccountSlots) {
			t.Errorf("addr %x: total pending transactions mismatch: have %d, want %d", addr, list.Len(), config.AccountSlots)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that setting the transaction pool gas price to a higher value correctly
// discards everything cheaper than that and moves any gapped transactions back
// from the pending pool to the queue.
//
// Note, local transactions are never allowed to be dropped.
func TestRepricing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	pool, _ := setupPool()
	defer pool.Close()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan core.NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 4)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(2), keys[0]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(1), keys[0]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(2), keys[0]))

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[1]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[1]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(2), keys[1]))

	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[2]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[2]))
	txs = append(txs, pricedTransaction(3, 100000, big.NewInt(2), keys[2]))

	ltx := pricedTransaction(0, 100000, big.NewInt(1), keys[3])

	// Import the batch and that both pending and queued transactions match up
	pool.AddRemotesSync(txs)
	pool.AddLocal(ltx)

	pending, queued := pool.Stats()
	if pending != 7 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 7)
	}
	if queued != 3 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 3)
	}
	if err := validateEvents(events, 7); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Reprice the pool and check that underpriced transactions get dropped
	pool.SetGasTip(big.NewInt(2))

	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 5 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 5)
	}
	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("reprice event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Check that we can't add the old transactions back
	if err := pool.AddRemote(pricedTransaction(1, 100000, big.NewInt(1), keys[0])); !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, txpool.ErrUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(1), keys[1])); !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, txpool.ErrUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(1), keys[2])); !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("adding underpriced queued transaction error mismatch: have %v, want %v", err, txpool.ErrUnderpriced)
	}
	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("post-reprice event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// However we can add local underpriced transactions
	tx := pricedTransaction(1, 100000, big.NewInt(1), keys[3])
	if err := pool.AddLocal(tx); err != nil {
		t.Fatalf("failed to add underpriced local transaction: %v", err)
	}
	if pending, _ = pool.Stats(); pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("post-reprice local event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// And we can fill gaps with properly priced transactions
	if err := pool.AddRemote(pricedTransaction(1, 100000, big.NewInt(2), keys[0])); err != nil {
		t.Fatalf("failed to add pending transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(2), keys[1])); err != nil {
		t.Fatalf("failed to add pending transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(2), keys[2])); err != nil {
		t.Fatalf("failed to add queued transaction: %v", err)
	}
	if err := validateEvents(events, 5); err != nil {
		t.Fatalf("post-reprice event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

func TestMinGasPriceEnforced(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	// blockchain := newTestBlockChain(eip1559Config, 10000000, statedb, new(event.Feed))
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	txPoolConfig := DefaultConfig
	txPoolConfig.NoLocals = true
	pool := New(txPoolConfig, params.TestChainConfig, blockchain)
	pool.Init(txPoolConfig.PriceLimit, blockchain.CurrentBlock().Header(), func(addr common.Address, reserve bool) error { return nil })
	defer pool.Close()

	key, _ := crypto.GenerateKey()
	testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1000000))

	tx := pricedTransaction(0, 100000, big.NewInt(2), key)
	pool.SetGasTip(big.NewInt(tx.GasPrice().Int64() + 1))

	if err := pool.AddLocal(tx); !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("Min tip not enforced")
	}

	if err := pool.Add([]*types.Transaction{tx}, true, false)[0]; !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("Min tip not enforced")
	}

	tx = dynamicFeeTx(0, 100000, big.NewInt(3), big.NewInt(2), key)
	pool.SetGasTip(big.NewInt(tx.GasTipCap().Int64() + 1))

	if err := pool.AddLocal(tx); !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("Min tip not enforced")
	}

	if err := pool.Add([]*types.Transaction{tx}, true, false)[0]; !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("Min tip not enforced")
	}
	// Make sure the tx is accepted if locals are enabled
	pool.config.NoLocals = false
	if err := pool.Add([]*types.Transaction{tx}, true, false)[0]; err != nil {
		t.Fatalf("Min tip enforced with locals enabled, error: %v", err)
	}
}

// Tests that setting the transaction pool gas price to a higher value correctly
// discards everything cheaper (legacy & dynamic fee) than that and moves any
// gapped transactions back from the pending pool to the queue.
//
// Note, local transactions are never allowed to be dropped.
func TestRepricingDynamicFee(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	pool, _ := setupPoolWithConfig(eip1559Config)
	defer pool.Close()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan core.NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 4)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(2), keys[0]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(1), keys[0]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(2), keys[0]))

	txs = append(txs, dynamicFeeTx(0, 100000, big.NewInt(2), big.NewInt(1), keys[1]))
	txs = append(txs, dynamicFeeTx(1, 100000, big.NewInt(3), big.NewInt(2), keys[1]))
	txs = append(txs, dynamicFeeTx(2, 100000, big.NewInt(3), big.NewInt(2), keys[1]))

	txs = append(txs, dynamicFeeTx(1, 100000, big.NewInt(2), big.NewInt(2), keys[2]))
	txs = append(txs, dynamicFeeTx(2, 100000, big.NewInt(1), big.NewInt(1), keys[2]))
	txs = append(txs, dynamicFeeTx(3, 100000, big.NewInt(2), big.NewInt(2), keys[2]))

	ltx := dynamicFeeTx(0, 100000, big.NewInt(2), big.NewInt(1), keys[3])

	// Import the batch and that both pending and queued transactions match up
	pool.AddRemotesSync(txs)
	pool.AddLocal(ltx)

	pending, queued := pool.Stats()
	if pending != 7 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 7)
	}
	if queued != 3 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 3)
	}
	if err := validateEvents(events, 7); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Reprice the pool and check that underpriced transactions get dropped
	pool.SetGasTip(big.NewInt(2))

	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 5 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 5)
	}
	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("reprice event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Check that we can't add the old transactions back
	tx := pricedTransaction(1, 100000, big.NewInt(1), keys[0])
	if err := pool.AddRemote(tx); !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, txpool.ErrUnderpriced)
	}
	tx = dynamicFeeTx(0, 100000, big.NewInt(2), big.NewInt(1), keys[1])
	if err := pool.AddRemote(tx); !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, txpool.ErrUnderpriced)
	}
	tx = dynamicFeeTx(2, 100000, big.NewInt(1), big.NewInt(1), keys[2])
	if err := pool.AddRemote(tx); !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("adding underpriced queued transaction error mismatch: have %v, want %v", err, txpool.ErrUnderpriced)
	}
	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("post-reprice event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// However we can add local underpriced transactions
	tx = dynamicFeeTx(1, 100000, big.NewInt(1), big.NewInt(1), keys[3])
	if err := pool.AddLocal(tx); err != nil {
		t.Fatalf("failed to add underpriced local transaction: %v", err)
	}
	if pending, _ = pool.Stats(); pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("post-reprice local event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// And we can fill gaps with properly priced transactions
	tx = pricedTransaction(1, 100000, big.NewInt(2), keys[0])
	if err := pool.AddRemote(tx); err != nil {
		t.Fatalf("failed to add pending transaction: %v", err)
	}
	tx = dynamicFeeTx(0, 100000, big.NewInt(3), big.NewInt(2), keys[1])
	if err := pool.AddRemote(tx); err != nil {
		t.Fatalf("failed to add pending transaction: %v", err)
	}
	tx = dynamicFeeTx(2, 100000, big.NewInt(2), big.NewInt(2), keys[2])
	if err := pool.AddRemote(tx); err != nil {
		t.Fatalf("failed to add queued transaction: %v", err)
	}
	if err := validateEvents(events, 5); err != nil {
		t.Fatalf("post-reprice event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that setting the transaction pool gas price to a higher value does not
// remove local transactions (legacy & dynamic fee).
func TestRepricingKeepsLocals(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	pool, _ := setupPoolWithConfig(eip1559Config)
	defer pool.Close()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 3)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(100000*1000000))
	}
	// Create transaction (both pending and queued) with a linearly growing gasprice
	for i := uint64(0); i < 500; i++ {
		// Add pending transaction.
		pendingTx := pricedTransaction(i, 100000, big.NewInt(int64(i)), keys[2])
		if err := pool.AddLocal(pendingTx); err != nil {
			t.Fatal(err)
		}
		// Add queued transaction.
		queuedTx := pricedTransaction(i+501, 100000, big.NewInt(int64(i)), keys[2])
		if err := pool.AddLocal(queuedTx); err != nil {
			t.Fatal(err)
		}

		// Add pending dynamic fee transaction.
		pendingTx = dynamicFeeTx(i, 100000, big.NewInt(int64(i)+1), big.NewInt(int64(i)), keys[1])
		if err := pool.AddLocal(pendingTx); err != nil {
			t.Fatal(err)
		}
		// Add queued dynamic fee transaction.
		queuedTx = dynamicFeeTx(i+501, 100000, big.NewInt(int64(i)+1), big.NewInt(int64(i)), keys[1])
		if err := pool.AddLocal(queuedTx); err != nil {
			t.Fatal(err)
		}
	}
	pending, queued := pool.Stats()
	expPending, expQueued := 1000, 1000
	validate := func() {
		pending, queued = pool.Stats()
		if pending != expPending {
			t.Fatalf("pending transactions mismatched: have %d, want %d", pending, expPending)
		}
		if queued != expQueued {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, expQueued)
		}

		if err := validatePoolInternals(pool); err != nil {
			t.Fatalf("pool internal state corrupted: %v", err)
		}
	}
	validate()

	// Reprice the pool and check that nothing is dropped
	pool.SetGasTip(big.NewInt(2))
	validate()

	pool.SetGasTip(big.NewInt(2))
	pool.SetGasTip(big.NewInt(4))
	pool.SetGasTip(big.NewInt(8))
	pool.SetGasTip(big.NewInt(100))
	validate()
}

// Tests that when the pool reaches its global transaction limit, underpriced
// transactions are gradually shifted out for more expensive ones and any gapped
// pending transactions are moved into the queue.
//
// Note, local transactions are never allowed to be dropped.
func TestUnderpricing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	config := testTxPoolConfig
	config.GlobalSlots = 2
	config.GlobalQueue = 2

	pool := New(config, params.TestChainConfig, blockchain)
	defer pool.Close()
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan core.NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[0]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[0]))

	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(1), keys[1]))

	ltx := pricedTransaction(0, 100000, big.NewInt(1), keys[2])

	// Import the batch and that both pending and queued transactions match up
	pool.AddRemotes(txs)
	pool.AddLocal(ltx)

	pending, queued := pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 3); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding an underpriced transaction on block limit fails
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(1), keys[1])); !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, txpool.ErrUnderpriced)
	}
	// Replace a future transaction with a future transaction
	if err := pool.AddRemote(pricedTransaction(1, 100000, big.NewInt(2), keys[1])); err != nil { // +K1:1 => -K1:1 => Pend K0:0, K0:1, K2:0; Que K1:1
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	// Ensure that adding high priced transactions drops cheap ones, but not own
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(3), keys[1])); err != nil { // +K1:0 => -K1:1 => Pend K0:0, K0:1, K1:0, K2:0; Que -
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(4), keys[1])); err != nil { // +K1:2 => -K0:0 => Pend K1:0, K2:0; Que K0:1 K1:2
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(3, 100000, big.NewInt(5), keys[1])); err != nil { // +K1:3 => -K0:1 => Pend K1:0, K2:0; Que K1:2 K1:3
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	// Ensure that replacing a pending transaction with a future transaction fails
	if err := pool.AddRemote(pricedTransaction(5, 100000, big.NewInt(6), keys[1])); err != txpool.ErrFutureReplacePending {
		t.Fatalf("adding future replace transaction error mismatch: have %v, want %v", err, txpool.ErrFutureReplacePending)
	}
	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("additional event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding local transactions can push out even higher priced ones
	ltx = pricedTransaction(1, 100000, big.NewInt(0), keys[2])
	if err := pool.AddLocal(ltx); err != nil {
		t.Fatalf("failed to append underpriced local transaction: %v", err)
	}
	ltx = pricedTransaction(0, 100000, big.NewInt(0), keys[3])
	if err := pool.AddLocal(ltx); err != nil {
		t.Fatalf("failed to add new underpriced local transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("local event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that more expensive transactions push out cheap ones from the pool, but
// without producing instability by creating gaps that start jumping transactions
// back and forth between queued/pending.
func TestStableUnderpricing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	config := testTxPoolConfig
	config.GlobalSlots = 128
	config.GlobalQueue = 0

	pool := New(config, params.TestChainConfig, blockchain)
	defer pool.Close()
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan core.NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 2)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Fill up the entire queue with the same transaction price points
	txs := types.Transactions{}
	for i := uint64(0); i < config.GlobalSlots; i++ {
		txs = append(txs, pricedTransaction(i, 100000, big.NewInt(1), keys[0]))
	}
	pool.AddRemotesSync(txs)

	pending, queued := pool.Stats()
	if pending != int(config.GlobalSlots) {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, config.GlobalSlots)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateEvents(events, int(config.GlobalSlots)); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding high priced transactions drops a cheap, but doesn't produce a gap
	if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(3), keys[1])); err != nil {
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != int(config.GlobalSlots) {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, config.GlobalSlots)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("additional event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that when the pool reaches its global transaction limit, underpriced
// transactions (legacy & dynamic fee) are gradually shifted out for more
// expensive ones and any gapped pending transactions are moved into the queue.
//
// Note, local transactions are never allowed to be dropped.
func TestUnderpricingDynamicFee(t *testing.T) {
	t.Parallel()

	pool, _ := setupPoolWithConfig(eip1559Config)
	defer pool.Close()

	pool.config.GlobalSlots = 2
	pool.config.GlobalQueue = 2

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan core.NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 4)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}

	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, dynamicFeeTx(0, 100000, big.NewInt(3), big.NewInt(2), keys[0]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[0]))
	txs = append(txs, dynamicFeeTx(1, 100000, big.NewInt(2), big.NewInt(1), keys[1]))

	ltx := dynamicFeeTx(0, 100000, big.NewInt(2), big.NewInt(1), keys[2])

	// Import the batch and that both pending and queued transactions match up
	pool.AddRemotes(txs) // Pend K0:0, K0:1; Que K1:1
	pool.AddLocal(ltx)   // +K2:0 => Pend K0:0, K0:1, K2:0; Que K1:1

	pending, queued := pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 3); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// Ensure that adding an underpriced transaction fails
	tx := dynamicFeeTx(0, 100000, big.NewInt(2), big.NewInt(1), keys[1])
	if err := pool.AddRemote(tx); !errors.Is(err, txpool.ErrUnderpriced) { // Pend K0:0, K0:1, K2:0; Que K1:1
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, txpool.ErrUnderpriced)
	}

	// Ensure that adding high priced transactions drops cheap ones, but not own
	tx = pricedTransaction(0, 100000, big.NewInt(2), keys[1])
	if err := pool.AddRemote(tx); err != nil { // +K1:0, -K1:1 => Pend K0:0, K0:1, K1:0, K2:0; Que -
		t.Fatalf("failed to add well priced transaction: %v", err)
	}

	tx = pricedTransaction(1, 100000, big.NewInt(3), keys[1])
	if err := pool.AddRemote(tx); err != nil { // +K1:2, -K0:1 => Pend K0:0 K1:0, K2:0; Que K1:2
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	tx = dynamicFeeTx(2, 100000, big.NewInt(4), big.NewInt(1), keys[1])
	if err := pool.AddRemote(tx); err != nil { // +K1:3, -K1:0 => Pend K0:0 K2:0; Que K1:2 K1:3
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("additional event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding local transactions can push out even higher priced ones
	ltx = dynamicFeeTx(1, 100000, big.NewInt(0), big.NewInt(0), keys[2])
	if err := pool.AddLocal(ltx); err != nil {
		t.Fatalf("failed to append underpriced local transaction: %v", err)
	}
	ltx = dynamicFeeTx(0, 100000, big.NewInt(0), big.NewInt(0), keys[3])
	if err := pool.AddLocal(ltx); err != nil {
		t.Fatalf("failed to add new underpriced local transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("local event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests whether highest fee cap transaction is retained after a batch of high effective
// tip transactions are added and vice versa
func TestDualHeapEviction(t *testing.T) {
	t.Parallel()

	pool, _ := setupPoolWithConfig(eip1559Config)
	defer pool.Close()

	pool.config.GlobalSlots = 10
	pool.config.GlobalQueue = 10

	var (
		highTip, highCap *types.Transaction
		baseFee          int
	)

	check := func(tx *types.Transaction, name string) {
		if pool.all.GetRemote(tx.Hash()) == nil {
			t.Fatalf("highest %s transaction evicted from the pool", name)
		}
	}

	add := func(urgent bool) {
		for i := 0; i < 20; i++ {
			var tx *types.Transaction
			// Create a test accounts and fund it
			key, _ := crypto.GenerateKey()
			testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1000000000000))
			if urgent {
				tx = dynamicFeeTx(0, 100000, big.NewInt(int64(baseFee+1+i)), big.NewInt(int64(1+i)), key)
				highTip = tx
			} else {
				tx = dynamicFeeTx(0, 100000, big.NewInt(int64(baseFee+200+i)), big.NewInt(1), key)
				highCap = tx
			}
			pool.AddRemotesSync([]*types.Transaction{tx})
		}
		pending, queued := pool.Stats()
		if pending+queued != 20 {
			t.Fatalf("transaction count mismatch: have %d, want %d", pending+queued, 10)
		}
	}

	add(false)
	for baseFee = 0; baseFee <= 1000; baseFee += 100 {
		pool.priced.SetBaseFee(big.NewInt(int64(baseFee)))
		add(true)
		check(highCap, "fee cap")
		add(false)
		check(highTip, "effective tip")
	}

	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that the pool rejects duplicate transactions.
func TestDeduplication(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	pool, _ := setupPool()
	defer pool.Close()

	// Create a test account to add transactions with
	key, _ := crypto.GenerateKey()
	testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1000000000))

	// Create a batch of transactions and add a few of them
	txs := make([]*types.Transaction, 16)
	for i := 0; i < len(txs); i++ {
		txs[i] = pricedTransaction(uint64(i), 100000, big.NewInt(1), key)
	}
	var firsts []*types.Transaction
	for i := 0; i < len(txs); i += 2 {
		firsts = append(firsts, txs[i])
	}
	errs := pool.AddRemotesSync(firsts)
	if len(errs) != len(firsts) {
		t.Fatalf("first add mismatching result count: have %d, want %d", len(errs), len(firsts))
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("add %d failed: %v", i, err)
		}
	}
	pending, queued := pool.Stats()
	if pending != 1 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 1)
	}
	if queued != len(txs)/2-1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, len(txs)/2-1)
	}
	// Try to add all of them now and ensure previous ones error out as knowns
	errs = pool.AddRemotesSync(txs)
	if len(errs) != len(txs) {
		t.Fatalf("all add mismatching result count: have %d, want %d", len(errs), len(txs))
	}
	for i, err := range errs {
		if i%2 == 0 && err == nil {
			t.Errorf("add %d succeeded, should have failed as known", i)
		}
		if i%2 == 1 && err != nil {
			t.Errorf("add %d failed: %v", i, err)
		}
	}
	pending, queued = pool.Stats()
	if pending != len(txs) {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, len(txs))
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that the pool rejects replacement transactions that don't meet the minimum
// price bump required.
func TestReplacement(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	pool, _ := setupPool()
	defer pool.Close()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan core.NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a test account to add transactions with
	key, _ := crypto.GenerateKey()
	testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1000000000))

	// Add pending transactions, ensuring the minimum price bump is enforced for replacement (for ultra low prices too)
	price := int64(100)
	threshold := (price * (100 + int64(testTxPoolConfig.PriceBump))) / 100

	if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1), key)); err != nil {
		t.Fatalf("failed to add original cheap pending transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100001, big.NewInt(1), key)); err != txpool.ErrReplaceUnderpriced {
		t.Fatalf("original cheap pending transaction replacement error mismatch: have %v, want %v", err, txpool.ErrReplaceUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(2), key)); err != nil {
		t.Fatalf("failed to replace original cheap pending transaction: %v", err)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("cheap replacement event firing failed: %v", err)
	}

	if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(price), key)); err != nil {
		t.Fatalf("failed to add original proper pending transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100001, big.NewInt(threshold-1), key)); err != txpool.ErrReplaceUnderpriced {
		t.Fatalf("original proper pending transaction replacement error mismatch: have %v, want %v", err, txpool.ErrReplaceUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(threshold), key)); err != nil {
		t.Fatalf("failed to replace original proper pending transaction: %v", err)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("proper replacement event firing failed: %v", err)
	}

	// Add queued transactions, ensuring the minimum price bump is enforced for replacement (for ultra low prices too)
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(1), key)); err != nil {
		t.Fatalf("failed to add original cheap queued transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100001, big.NewInt(1), key)); err != txpool.ErrReplaceUnderpriced {
		t.Fatalf("original cheap queued transaction replacement error mismatch: have %v, want %v", err, txpool.ErrReplaceUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(2), key)); err != nil {
		t.Fatalf("failed to replace original cheap queued transaction: %v", err)
	}

	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(price), key)); err != nil {
		t.Fatalf("failed to add original proper queued transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100001, big.NewInt(threshold-1), key)); err != txpool.ErrReplaceUnderpriced {
		t.Fatalf("original proper queued transaction replacement error mismatch: have %v, want %v", err, txpool.ErrReplaceUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(threshold), key)); err != nil {
		t.Fatalf("failed to replace original proper queued transaction: %v", err)
	}

	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("queued replacement event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that the pool rejects replacement dynamic fee transactions that don't
// meet the minimum price bump required.
func TestReplacementDynamicFee(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	pool, key := setupPoolWithConfig(eip1559Config)
	defer pool.Close()
	testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1000000000))

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan core.NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Add pending transactions, ensuring the minimum price bump is enforced for replacement (for ultra low prices too)
	gasFeeCap := int64(100)
	feeCapThreshold := (gasFeeCap * (100 + int64(testTxPoolConfig.PriceBump))) / 100
	gasTipCap := int64(60)
	tipThreshold := (gasTipCap * (100 + int64(testTxPoolConfig.PriceBump))) / 100

	// Run the following identical checks for both the pending and queue pools:
	//	1.  Send initial tx => accept
	//	2.  Don't bump tip or fee cap => discard
	//	3.  Bump both more than min => accept
	//	4.  Check events match expected (2 new executable txs during pending, 0 during queue)
	//	5.  Send new tx with larger tip and gasFeeCap => accept
	//	6.  Bump tip max allowed so it's still underpriced => discard
	//	7.  Bump fee cap max allowed so it's still underpriced => discard
	//	8.  Bump tip min for acceptance => discard
	//	9.  Bump feecap min for acceptance => discard
	//	10. Bump feecap and tip min for acceptance => accept
	//	11. Check events match expected (2 new executable txs during pending, 0 during queue)
	stages := []string{"pending", "queued"}
	for _, stage := range stages {
		// Since state is empty, 0 nonce txs are "executable" and can go
		// into pending immediately. 2 nonce txs are "happed
		nonce := uint64(0)
		if stage == "queued" {
			nonce = 2
		}

		// 1.  Send initial tx => accept
		tx := dynamicFeeTx(nonce, 100000, big.NewInt(2), big.NewInt(1), key)
		if err := pool.addRemoteSync(tx); err != nil {
			t.Fatalf("failed to add original cheap %s transaction: %v", stage, err)
		}
		// 2.  Don't bump tip or feecap => discard
		tx = dynamicFeeTx(nonce, 100001, big.NewInt(2), big.NewInt(1), key)
		if err := pool.AddRemote(tx); err != txpool.ErrReplaceUnderpriced {
			t.Fatalf("original cheap %s transaction replacement error mismatch: have %v, want %v", stage, err, txpool.ErrReplaceUnderpriced)
		}
		// 3.  Bump both more than min => accept
		tx = dynamicFeeTx(nonce, 100000, big.NewInt(3), big.NewInt(2), key)
		if err := pool.AddRemote(tx); err != nil {
			t.Fatalf("failed to replace original cheap %s transaction: %v", stage, err)
		}
		// 4.  Check events match expected (2 new executable txs during pending, 0 during queue)
		count := 2
		if stage == "queued" {
			count = 0
		}
		if err := validateEvents(events, count); err != nil {
			t.Fatalf("cheap %s replacement event firing failed: %v", stage, err)
		}
		// 5.  Send new tx with larger tip and feeCap => accept
		tx = dynamicFeeTx(nonce, 100000, big.NewInt(gasFeeCap), big.NewInt(gasTipCap), key)
		if err := pool.addRemoteSync(tx); err != nil {
			t.Fatalf("failed to add original proper %s transaction: %v", stage, err)
		}
		// 6.  Bump tip max allowed so it's still underpriced => discard
		tx = dynamicFeeTx(nonce, 100000, big.NewInt(gasFeeCap), big.NewInt(tipThreshold-1), key)
		if err := pool.AddRemote(tx); err != txpool.ErrReplaceUnderpriced {
			t.Fatalf("original proper %s transaction replacement error mismatch: have %v, want %v", stage, err, txpool.ErrReplaceUnderpriced)
		}
		// 7.  Bump fee cap max allowed so it's still underpriced => discard
		tx = dynamicFeeTx(nonce, 100000, big.NewInt(feeCapThreshold-1), big.NewInt(gasTipCap), key)
		if err := pool.AddRemote(tx); err != txpool.ErrReplaceUnderpriced {
			t.Fatalf("original proper %s transaction replacement error mismatch: have %v, want %v", stage, err, txpool.ErrReplaceUnderpriced)
		}
		// 8.  Bump tip min for acceptance => accept
		tx = dynamicFeeTx(nonce, 100000, big.NewInt(gasFeeCap), big.NewInt(tipThreshold), key)
		if err := pool.AddRemote(tx); err != txpool.ErrReplaceUnderpriced {
			t.Fatalf("original proper %s transaction replacement error mismatch: have %v, want %v", stage, err, txpool.ErrReplaceUnderpriced)
		}
		// 9.  Bump fee cap min for acceptance => accept
		tx = dynamicFeeTx(nonce, 100000, big.NewInt(feeCapThreshold), big.NewInt(gasTipCap), key)
		if err := pool.AddRemote(tx); err != txpool.ErrReplaceUnderpriced {
			t.Fatalf("original proper %s transaction replacement error mismatch: have %v, want %v", stage, err, txpool.ErrReplaceUnderpriced)
		}
		// 10. Check events match expected (3 new executable txs during pending, 0 during queue)
		tx = dynamicFeeTx(nonce, 100000, big.NewInt(feeCapThreshold), big.NewInt(tipThreshold), key)
		if err := pool.AddRemote(tx); err != nil {
			t.Fatalf("failed to replace original cheap %s transaction: %v", stage, err)
		}
		// 11. Check events match expected (3 new executable txs during pending, 0 during queue)
		count = 2
		if stage == "queued" {
			count = 0
		}
		if err := validateEvents(events, count); err != nil {
			t.Fatalf("replacement %s event firing failed: %v", stage, err)
		}
	}

	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that local transactions are journaled to disk, but remote transactions
// get discarded between restarts.
func TestJournaling(t *testing.T)         { testJournaling(t, false) }
func TestJournalingNoLocals(t *testing.T) { testJournaling(t, true) }

func testJournaling(t *testing.T, nolocals bool) {
	t.Parallel()

	// Create a temporary file for the journal
	file, err := ioutil.TempFile("", "")
	if err != nil {
		t.Fatalf("failed to create temporary journal: %v", err)
	}
	journal := file.Name()
	defer os.Remove(journal)

	// Clean up the temporary file, we only need the path for now
	file.Close()
	os.Remove(journal)

	// Create the original pool to inject transaction into the journal
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	config := testTxPoolConfig
	config.NoLocals = nolocals
	config.Journal = journal
	config.Rejournal = time.Second

	pool := New(config, params.TestChainConfig, blockchain)
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// Create two test accounts to ensure remotes expire but locals do not
	local, _ := crypto.GenerateKey()
	remote, _ := crypto.GenerateKey()

	testAddBalance(pool, crypto.PubkeyToAddress(local.PublicKey), big.NewInt(1000000000))
	testAddBalance(pool, crypto.PubkeyToAddress(remote.PublicKey), big.NewInt(1000000000))

	// Add three local and a remote transactions and ensure they are queued up
	if err := pool.AddLocal(pricedTransaction(0, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.AddLocal(pricedTransaction(1, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.AddLocal(pricedTransaction(2, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	pending, queued := pool.Stats()
	if pending != 4 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 4)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Terminate the old pool, bump the local nonce, create a new pool and ensure relevant transaction survive
	pool.Close()
	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 1)
	blockchain = &testBlockChain{1000000, statedb, new(event.Feed), 0}

	pool = New(config, params.TestChainConfig, blockchain)
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	pending, queued = pool.Stats()
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if nolocals {
		if pending != 0 {
			t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
		}
	} else {
		if pending != 2 {
			t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Bump the nonce temporarily and ensure the newly invalidated transaction is removed
	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 2)
	<-pool.requestReset(nil, nil)
	time.Sleep(2 * config.Rejournal)
	pool.Close()

	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 1)
	blockchain = &testBlockChain{1000000, statedb, new(event.Feed), 0}
	pool = New(config, params.TestChainConfig, blockchain)
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	pending, queued = pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if nolocals {
		if queued != 0 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
		}
	} else {
		if queued != 1 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	pool.Close()
}

// TestStatusCheck tests that the pool can correctly retrieve the
// pending status of individual transactions.
func TestStatusCheck(t *testing.T) {
	t.Parallel()

	// Create the pool to test the status retrievals with
	pool, _ := setupPool()
	defer pool.Close()

	// Create the test accounts to check various transaction statuses with
	keys := make([]*ecdsa.PrivateKey, 3)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[0])) // Pending only
	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[1])) // Pending and queued
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[1]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[2])) // Queued only

	// Import the transaction and ensure they are correctly added
	pool.AddRemotesSync(txs)

	pending, queued := pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Retrieve the status of each transaction and validate them
	hashes := make([]common.Hash, len(txs))
	for i, tx := range txs {
		hashes[i] = tx.Hash()
	}
	hashes = append(hashes, common.Hash{})

	expect := []txpool.TxStatus{txpool.TxStatusPending, txpool.TxStatusPending, txpool.TxStatusQueued, txpool.TxStatusQueued, txpool.TxStatusUnknown}

	for i, hash := range hashes {
		status := pool.Status(hash)
		if status != expect[i] {
			t.Errorf("transaction %d: status mismatch: have %v, want %v", i, status, expect[i])
		}
	}
}

// Test the transaction slots consumption is computed correctly
func TestSlotCount(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()

	// Check that an empty transaction consumes a single slot
	smallTx := pricedDataTransaction(0, 0, big.NewInt(0), key, 0)
	if slots := numSlots(smallTx); slots != 1 {
		t.Fatalf("small transactions slot count mismatch: have %d want %d", slots, 1)
	}
	// Check that a large transaction consumes the correct number of slots
	bigTx := pricedDataTransaction(0, 0, big.NewInt(0), key, uint64(10*txSlotSize))
	if slots := numSlots(bigTx); slots != 11 {
		t.Fatalf("big transactions slot count mismatch: have %d want %d", slots, 11)
	}
}

// TestSetCodeTransactions tests a few scenarios regarding the EIP-7702
// SetCodeTx.
func TestSetCodeTransactions(t *testing.T) {
	t.Parallel()

	// Create the pool to test the status retrievals with
	// statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	// blockchain := newTestBlockChain(params.MergedTestChainConfig, 1000000, statedb, new(event.Feed))
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	pool := New(testTxPoolConfig, params.TestChainConfig, blockchain)
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		makeAddressReserver(),
	)

	// wait for the pool to initialize
	<-pool.initDoneCh

	defer pool.Close()

	// Create the test accounts
	var (
		keyA, _ = crypto.GenerateKey()
		keyB, _ = crypto.GenerateKey()
		keyC, _ = crypto.GenerateKey()
		addrA   = crypto.PubkeyToAddress(keyA.PublicKey)
		addrB   = crypto.PubkeyToAddress(keyB.PublicKey)
		addrC   = crypto.PubkeyToAddress(keyC.PublicKey)
	)
	testAddBalance(pool, addrA, big.NewInt(params.Ether))
	testAddBalance(pool, addrB, big.NewInt(params.Ether))
	testAddBalance(pool, addrC, big.NewInt(params.Ether))

	for _, tt := range []struct {
		name    string
		pending int
		queued  int
		run     func(string)
	}{
		{
			// Check that only one in-flight transaction is allowed for accounts
			// with delegation set. Also verify the accepted transaction can be
			// replaced by fee.
			name:    "only-one-in-flight",
			pending: 1,
			run: func(name string) {
				aa := common.Address{0xaa, 0xaa}
				statedb.SetCode(addrA, append(types.DelegationPrefix, aa.Bytes()...))
				statedb.SetCode(aa, []byte{byte(vm.ADDRESS), byte(vm.PUSH0), byte(vm.SSTORE)})
				// Send transactions. First is accepted, second is rejected.
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1), keyA)); err != nil {
					t.Fatalf("%s: failed to add remote transaction: %v", name, err)
				}
				if err := pool.addRemoteSync(pricedTransaction(1, 100000, big.NewInt(1), keyA)); !errors.Is(err, ErrInflightTxLimitReached) {
					t.Fatalf("%s: error mismatch: want %v, have %v", name, ErrInflightTxLimitReached, err)
				}
				// Also check gapped transaction.
				if err := pool.addRemoteSync(pricedTransaction(2, 100000, big.NewInt(1), keyA)); !errors.Is(err, ErrInflightTxLimitReached) {
					t.Fatalf("%s: error mismatch: want %v, have %v", name, ErrInflightTxLimitReached, err)
				}
				// Replace by fee.
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(10), keyA)); err != nil {
					t.Fatalf("%s: failed to replace with remote transaction: %v", name, err)
				}
			},
		},
		{
			name:    "allow-setcode-tx-with-pending-authority-tx",
			pending: 2,
			run: func(name string) {
				// Send two transactions where the first has no conflicting delegations and
				// the second should be allowed despite conflicting with the authorities in 1).
				if err := pool.addRemoteSync(setCodeTx(0, keyA, []unsignedAuth{{1, keyC}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				if err := pool.addRemoteSync(setCodeTx(0, keyB, []unsignedAuth{{1, keyC}})); err != nil {
					t.Fatalf("%s: failed to add conflicting delegation: %v", name, err)
				}
			},
		},
		{
			name:    "allow-one-tx-from-pooled-delegation",
			pending: 2,
			run: func(name string) {
				// Verify C cannot originate another transaction when it has a pooled delegation.
				if err := pool.addRemoteSync(setCodeTx(0, keyA, []unsignedAuth{{0, keyC}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1), keyC)); err != nil {
					t.Fatalf("%s: failed to add with pending delegatio: %v", name, err)
				}
				// Also check gapped transaction is rejected.
				if err := pool.addRemoteSync(pricedTransaction(1, 100000, big.NewInt(1), keyC)); !errors.Is(err, ErrInflightTxLimitReached) {
					t.Fatalf("%s: error mismatch: want %v, have %v", name, ErrInflightTxLimitReached, err)
				}
			},
		},
		{
			name:    "replace-by-fee-setcode-tx",
			pending: 1,
			run: func(name string) {
				// 4. Fee bump the setcode tx send.
				if err := pool.addRemoteSync(setCodeTx(0, keyB, []unsignedAuth{{1, keyC}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				if err := pool.addRemoteSync(pricedSetCodeTx(0, 250000, uint256.NewInt(2000), uint256.NewInt(2), keyB, []unsignedAuth{{0, keyC}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
			},
		},
		{
			name:    "allow-tx-from-replaced-authority",
			pending: 2,
			run: func(name string) {
				// Fee bump with a different auth list. Make sure that unlocks the authorities.
				if err := pool.addRemoteSync(pricedSetCodeTx(0, 250000, uint256.NewInt(10), uint256.NewInt(3), keyA, []unsignedAuth{{0, keyB}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				if err := pool.addRemoteSync(pricedSetCodeTx(0, 250000, uint256.NewInt(3000), uint256.NewInt(300), keyA, []unsignedAuth{{0, keyC}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				// Now send a regular tx from B.
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(10), keyB)); err != nil {
					t.Fatalf("%s: failed to replace with remote transaction: %v", name, err)
				}
			},
		},
		{
			name:    "allow-tx-from-replaced-self-sponsor-authority",
			pending: 2,
			run: func(name string) {
				//
				if err := pool.addRemoteSync(pricedSetCodeTx(0, 250000, uint256.NewInt(10), uint256.NewInt(3), keyA, []unsignedAuth{{0, keyA}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				if err := pool.addRemoteSync(pricedSetCodeTx(0, 250000, uint256.NewInt(30), uint256.NewInt(30), keyA, []unsignedAuth{{0, keyB}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				// Now send a regular tx from keyA.
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1000), keyA)); err != nil {
					t.Fatalf("%s: failed to replace with remote transaction: %v", name, err)
				}
				// Make sure we can still send from keyB.
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1000), keyB)); err != nil {
					t.Fatalf("%s: failed to replace with remote transaction: %v", name, err)
				}
			},
		},
		{
			name:    "track-multiple-conflicting-delegations",
			pending: 3,
			run: func(name string) {
				// Send two setcode txs both with C as an authority.
				if err := pool.addRemoteSync(pricedSetCodeTx(0, 250000, uint256.NewInt(10), uint256.NewInt(3), keyA, []unsignedAuth{{0, keyC}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				if err := pool.addRemoteSync(pricedSetCodeTx(0, 250000, uint256.NewInt(30), uint256.NewInt(30), keyB, []unsignedAuth{{0, keyC}})); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				// Replace the tx from A with a non-setcode tx.
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1000), keyA)); err != nil {
					t.Fatalf("%s: failed to replace with remote transaction: %v", name, err)
				}
				// Make sure we can only pool one tx from keyC since it is still a
				// pending authority.
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1000), keyC)); err != nil {
					t.Fatalf("%s: failed to added single pooled for account with pending delegation: %v", name, err)
				}
				if err, want := pool.addRemoteSync(pricedTransaction(1, 100000, big.NewInt(1000), keyC)), ErrInflightTxLimitReached; !errors.Is(err, want) {
					t.Fatalf("%s: error mismatch: want %v, have %v", name, want, err)
				}
			},
		},
		{
			name:    "reject-delegation-from-pending-account",
			pending: 1,
			run: func(name string) {
				// Attempt to submit a delegation from an account with a pending tx.
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1000), keyC)); err != nil {
					t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
				}
				if err, want := pool.addRemoteSync(setCodeTx(0, keyA, []unsignedAuth{{1, keyC}})), ErrAuthorityReserved; !errors.Is(err, want) {
					t.Fatalf("%s: error mismatch: want %v, have %v", name, want, err)
				}
			},
		},
		{
			name:    "remove-hash-from-authority-tracker",
			pending: 10,
			run: func(name string) {
				var keys []*ecdsa.PrivateKey
				for i := 0; i < 30; i++ {
					key, _ := crypto.GenerateKey()
					keys = append(keys, key)
					addr := crypto.PubkeyToAddress(key.PublicKey)
					testAddBalance(pool, addr, big.NewInt(params.Ether))
				}
				// Create a transactions with 3 unique auths so the lookup's auth map is
				// filled with addresses.
				for i := 0; i < 30; i += 3 {
					if err := pool.addRemoteSync(pricedSetCodeTx(0, 250000, uint256.NewInt(10), uint256.NewInt(3), keys[i], []unsignedAuth{{0, keys[i]}, {0, keys[i+1]}, {0, keys[i+2]}})); err != nil {
						t.Fatalf("%s: failed to add with remote setcode transaction: %v", name, err)
					}
				}
				// Replace one of the transactions with a normal transaction so that the
				// original hash is removed from the tracker. The hash should be
				// associated with 3 different authorities.
				if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1000), keys[0])); err != nil {
					t.Fatalf("%s: failed to replace with remote transaction: %v", name, err)
				}
			},
		},
	} {
		tt.run(tt.name)
		pending, queued := pool.Stats()
		if pending != tt.pending {
			t.Fatalf("%s: pending transactions mismatched: have %d, want %d", tt.name, pending, tt.pending)
		}
		if queued != tt.queued {
			t.Fatalf("%s: queued transactions mismatched: have %d, want %d", tt.name, queued, tt.queued)
		}
		if err := validatePoolInternals(pool); err != nil {
			t.Fatalf("%s: pool internal state corrupted: %v", tt.name, err)
		}
		pool.Clear()
	}
}

func TestSetCodeTransactionsReorg(t *testing.T) {
	t.Parallel()

	// Create the pool to test the status retrievals with
	// statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	// blockchain := newTestBlockChain(params.MergedTestChainConfig, 1000000, statedb, new(event.Feed))
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed), 0}

	pool := New(testTxPoolConfig, params.TestChainConfig, blockchain)
	pool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		makeAddressReserver(),
	)

	// wait for the pool to initialize
	<-pool.initDoneCh

	defer pool.Close()

	// Create the test accounts
	var (
		keyA, _ = crypto.GenerateKey()
		addrA   = crypto.PubkeyToAddress(keyA.PublicKey)
	)
	testAddBalance(pool, addrA, big.NewInt(params.Ether))
	// Send an authorization for 0x42
	var authList []types.Authorization
	auth, _ := types.SignAuth(types.Authorization{
		ChainID: params.TestChainConfig.ChainID.Uint64(),
		Address: common.Address{0x42},
		Nonce:   0,
	}, keyA)
	authList = append(authList, auth)
	if err := pool.addRemoteSync(pricedSetCodeTxWithAuth(0, 250000, uint256.NewInt(10), uint256.NewInt(3), keyA, authList)); err != nil {
		t.Fatalf("failed to add with remote setcode transaction: %v", err)
	}
	// Simulate the chain moving
	blockchain.statedb.SetNonce(addrA, 1)
	blockchain.statedb.SetCode(addrA, types.AddressToDelegation(auth.Address))
	<-pool.requestReset(nil, nil)
	// Set an authorization for 0x00
	auth, _ = types.SignAuth(types.Authorization{
		ChainID: params.TestChainConfig.ChainID.Uint64(),
		Address: common.Address{},
		Nonce:   0,
	}, keyA)
	authList = append(authList, auth)
	if err := pool.addRemoteSync(pricedSetCodeTxWithAuth(1, 250000, uint256.NewInt(10), uint256.NewInt(3), keyA, authList)); err != nil {
		t.Fatalf("failed to add with remote setcode transaction: %v", err)
	}
	// Try to add a transactions in
	if err := pool.addRemoteSync(pricedTransaction(2, 100000, big.NewInt(1000), keyA)); !errors.Is(err, ErrInflightTxLimitReached) {
		t.Fatalf("unexpected error %v, expecting %v", err, ErrInflightTxLimitReached)
	}
	// Simulate the chain moving
	blockchain.statedb.SetNonce(addrA, 2)
	blockchain.statedb.SetCode(addrA, nil)
	<-pool.requestReset(nil, nil)
	// Now send two transactions from addrA
	if err := pool.addRemoteSync(pricedTransaction(2, 100000, big.NewInt(1000), keyA)); err != nil {
		t.Fatalf("failed to added single transaction: %v", err)
	}
	if err := pool.addRemoteSync(pricedTransaction(3, 100000, big.NewInt(1000), keyA)); err != nil {
		t.Fatalf("failed to added single transaction: %v", err)
	}
}

// Benchmarks the speed of validating the contents of the pending queue of the
// transaction pool.
func BenchmarkPendingDemotion100(b *testing.B)   { benchmarkPendingDemotion(b, 100) }
func BenchmarkPendingDemotion1000(b *testing.B)  { benchmarkPendingDemotion(b, 1000) }
func BenchmarkPendingDemotion10000(b *testing.B) { benchmarkPendingDemotion(b, 10000) }

func benchmarkPendingDemotion(b *testing.B, size int) {
	log.Root().SetHandler(log.DiscardHandler())
	// Add a batch of transactions to a pool one by one
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(100100))

	for i := 0; i < size; i++ {
		tx := transaction(uint64(i), 100000, key)
		pool.promoteTx(account, tx.Hash(), tx)
	}
	// Benchmark the speed of pool validation
	for i := 0; i < b.N; i++ {
		// Force the txList filter to loop through the whole list
		pool.pending[account].costcap = big.NewInt(200000)
		pool.demoteUnexecutables()
	}
}

func BenchmarkPendingSponsoredTxDemotion100(b *testing.B) {
	benchmarkPendingSponsoredTxDemotion(b, 100)
}
func BenchmarkPendingSponsoredTxDemotion1000(b *testing.B) {
	benchmarkPendingSponsoredTxDemotion(b, 1000)
}
func BenchmarkPendingSponsoredTxDemotion10000(b *testing.B) {
	benchmarkPendingSponsoredTxDemotion(b, 10000)
}

func benchmarkPendingSponsoredTxDemotion(b *testing.B, size int) {
	log.Root().SetHandler(log.DiscardHandler())
	// Add a batch of transactions to a pool one by one
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))
	mikoSigner := types.NewMikoSigner(common.Big1)

	for i := 0; i < size; i++ {
		// create different payer for every transaction, worst case scenario
		payerKey, _ := crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(payerKey.PublicKey), big.NewInt(1000000))

		innerTx := types.SponsoredTx{
			ChainID:     common.Big1,
			Nonce:       uint64(i),
			GasTipCap:   common.Big1,
			GasFeeCap:   common.Big1,
			Gas:         21000,
			To:          &account,
			ExpiredTime: 100,
		}
		var err error
		innerTx.PayerR, innerTx.PayerS, innerTx.PayerV, err = types.PayerSign(payerKey, mikoSigner, account, &innerTx)
		if err != nil {
			b.Fatal(err)
		}

		tx, err := types.SignNewTx(key, mikoSigner, &innerTx)
		if err != nil {
			b.Fatal(err)
		}
		pool.promoteTx(account, tx.Hash(), tx)
	}
	// Benchmark the speed of pool validation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.demoteUnexecutables()
	}
}

// Benchmarks the speed of scheduling the contents of the future queue of the
// transaction pool.
func BenchmarkFuturePromotion100(b *testing.B)   { benchmarkFuturePromotion(b, 100) }
func BenchmarkFuturePromotion1000(b *testing.B)  { benchmarkFuturePromotion(b, 1000) }
func BenchmarkFuturePromotion10000(b *testing.B) { benchmarkFuturePromotion(b, 10000) }

func benchmarkFuturePromotion(b *testing.B, size int) {
	// Add a batch of transactions to a pool one by one
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	for i := 0; i < size; i++ {
		tx := transaction(uint64(1+i), 100000, key)
		pool.enqueueTx(tx.Hash(), tx, false, true)
	}
	// Benchmark the speed of pool validation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.promoteExecutables(nil)
	}
}

// Benchmarks the speed of batched transaction insertion.
func BenchmarkBatchInsert100(b *testing.B)   { benchmarkBatchInsert(b, 100, false) }
func BenchmarkBatchInsert1000(b *testing.B)  { benchmarkBatchInsert(b, 1000, false) }
func BenchmarkBatchInsert10000(b *testing.B) { benchmarkBatchInsert(b, 10000, false) }

func BenchmarkBatchLocalInsert100(b *testing.B)   { benchmarkBatchInsert(b, 100, true) }
func BenchmarkBatchLocalInsert1000(b *testing.B)  { benchmarkBatchInsert(b, 1000, true) }
func BenchmarkBatchLocalInsert10000(b *testing.B) { benchmarkBatchInsert(b, 10000, true) }

func benchmarkBatchInsert(b *testing.B, size int, local bool) {
	// Generate a batch of transactions to enqueue into the pool
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000000000000000))

	batches := make([]types.Transactions, b.N)
	for i := 0; i < b.N; i++ {
		batches[i] = make(types.Transactions, size)
		for j := 0; j < size; j++ {
			batches[i][j] = transaction(uint64(size*i+j), 100000, key)
		}
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for _, batch := range batches {
		if local {
			pool.AddLocals(batch)
		} else {
			pool.AddRemotes(batch)
		}
	}
}

func BenchmarkBatchInsertSponsoredTx100(b *testing.B) {
	benchmarkBatchInsertSponsoredTx(b, 100)
}
func BenchmarkBatchInsertSponsoredTx1000(b *testing.B) {
	benchmarkBatchInsertSponsoredTx(b, 1000)
}
func BenchmarkBatchInsertSponsoredTx10000(b *testing.B) {
	benchmarkBatchInsertSponsoredTx(b, 10000)
}

func benchmarkBatchInsertSponsoredTx(b *testing.B, size int) {
	log.Root().SetHandler(log.DiscardHandler())
	// Add a batch of transactions to a pool one by one
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))
	mikoSigner := types.NewMikoSigner(common.Big1)

	batches := make([]types.Transactions, b.N)
	for i := 0; i < b.N; i++ {
		batches[i] = make(types.Transactions, size)
		for j := 0; j < size; j++ {
			// create different payer for every transaction, worst case scenario
			payerKey, _ := crypto.GenerateKey()
			testAddBalance(pool, crypto.PubkeyToAddress(payerKey.PublicKey), big.NewInt(1000000))

			innerTx := types.SponsoredTx{
				ChainID:     common.Big1,
				Nonce:       uint64(i),
				GasTipCap:   common.Big1,
				GasFeeCap:   common.Big1,
				Gas:         21000,
				To:          &account,
				ExpiredTime: 100,
			}
			var err error
			innerTx.PayerR, innerTx.PayerS, innerTx.PayerV, err = types.PayerSign(payerKey, mikoSigner, account, &innerTx)
			if err != nil {
				b.Fatal(err)
			}

			tx, err := types.SignNewTx(key, mikoSigner, &innerTx)
			if err != nil {
				b.Fatal(err)
			}
			batches[i][j] = tx
		}
	}

	// Benchmark the speed of pool validation
	b.ResetTimer()
	for _, batch := range batches {
		pool.AddRemotes(batch)
	}
}

func BenchmarkInsertRemoteWithAllLocals(b *testing.B) {
	// Allocate keys for testing
	key, _ := crypto.GenerateKey()
	account := crypto.PubkeyToAddress(key.PublicKey)

	remoteKey, _ := crypto.GenerateKey()
	remoteAddr := crypto.PubkeyToAddress(remoteKey.PublicKey)

	locals := make([]*types.Transaction, 4096+1024) // Occupy all slots
	for i := 0; i < len(locals); i++ {
		locals[i] = transaction(uint64(i), 100000, key)
	}
	remotes := make([]*types.Transaction, 1000)
	for i := 0; i < len(remotes); i++ {
		remotes[i] = pricedTransaction(uint64(i), 100000, big.NewInt(2), remoteKey) // Higher gasprice
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		pool, _ := setupPool()
		testAddBalance(pool, account, big.NewInt(100000000))
		for _, local := range locals {
			pool.AddLocal(local)
		}
		b.StartTimer()
		// Assign a high enough balance for testing
		testAddBalance(pool, remoteAddr, big.NewInt(100000000))
		for i := 0; i < len(remotes); i++ {
			pool.AddRemotes([]*types.Transaction{remotes[i]})
		}
		pool.Close()
	}
}

// Benchmarks the speed of batch transaction insertion in case of multiple accounts.
func BenchmarkMultiAccountBatchInsert(b *testing.B) {
	// Generate a batch of transactions to enqueue into the pool
	pool, _ := setupPool()
	defer pool.Close()
	b.ReportAllocs()
	batches := make(types.Transactions, b.N)
	for i := 0; i < b.N; i++ {
		key, _ := crypto.GenerateKey()
		account := crypto.PubkeyToAddress(key.PublicKey)
		pool.currentState.AddBalance(account, big.NewInt(1000000))
		tx := transaction(uint64(0), 100000, key)
		batches[i] = tx
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for _, tx := range batches {
		pool.AddRemotesSync([]*types.Transaction{tx})
	}
}

func TestSponsoredTxBeforeMiko(t *testing.T) {
	var chainConfig params.ChainConfig

	chainConfig.EIP155Block = common.Big0
	chainConfig.ChainID = big.NewInt(2020)

	recipient := common.HexToAddress("1000000000000000000000000000000000000001")
	txpool, senderKey := setupPoolWithConfig(&chainConfig)
	defer txpool.Close()

	payerKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	innerTx := types.SponsoredTx{
		ChainID:     big.NewInt(2020),
		Nonce:       1,
		GasTipCap:   big.NewInt(100000),
		GasFeeCap:   big.NewInt(100000),
		Gas:         1000,
		To:          &recipient,
		Value:       big.NewInt(10),
		Data:        []byte("abcd"),
		ExpiredTime: 100000,
	}

	mikoSigner := types.NewMikoSigner(big.NewInt(2020))
	innerTx.PayerR, innerTx.PayerS, innerTx.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&innerTx,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx, err := types.SignNewTx(senderKey, mikoSigner, &innerTx)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	err = txpool.AddRemote(tx)
	if err == nil || !errors.Is(err, core.ErrTxTypeNotSupported) {
		t.Fatalf("Expect error %s, get %s", core.ErrTxTypeNotSupported, err)
	}
}

func TestExpiredTimeAndGasCheckSponsoredTx(t *testing.T) {
	var chainConfig params.ChainConfig

	chainConfig.EIP155Block = common.Big0
	chainConfig.MikoBlock = common.Big0
	chainConfig.VenokiBlock = common.Big1
	chainConfig.ChainID = big.NewInt(2020)

	recipient := common.HexToAddress("1000000000000000000000000000000000000001")
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{10000000, statedb, new(event.Feed), 0}

	txpool := New(testTxPoolConfig, &chainConfig, blockchain)
	defer txpool.Close()
	txpool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	senderKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	payerKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	innerTx := types.SponsoredTx{
		ChainID:     big.NewInt(2020),
		Nonce:       1,
		GasTipCap:   big.NewInt(100000),
		GasFeeCap:   big.NewInt(200000),
		Gas:         22000,
		To:          &recipient,
		Value:       big.NewInt(10),
		Data:        []byte("abcd"),
		ExpiredTime: 100,
	}

	mikoSigner := types.NewMikoSigner(big.NewInt(2020))
	innerTx.PayerR, innerTx.PayerS, innerTx.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&innerTx,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx, err := types.SignNewTx(senderKey, mikoSigner, &innerTx)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	// 1. Failed when gas fee cap and gas tip cap are different
	err = txpool.addRemoteSync(tx)
	if err == nil || !errors.Is(err, core.ErrDifferentFeeCapTipCap) {
		t.Fatalf("Expect error %s, get %s", core.ErrDifferentFeeCapTipCap, err)
	}

	// 2. Failed when tx is expired
	blockchain.headerTime = 2000
	<-txpool.requestReset(nil, nil)
	innerTx.GasFeeCap = innerTx.GasTipCap
	innerTx.PayerR, innerTx.PayerS, innerTx.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&innerTx,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx, err = types.SignNewTx(senderKey, mikoSigner, &innerTx)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	err = txpool.addRemoteSync(tx)
	if err == nil || !errors.Is(err, core.ErrExpiredSponsoredTx) {
		t.Fatalf("Expect error %s, get %s", core.ErrExpiredSponsoredTx, err)
	}

	// 3. Failed when sponsored tx has the same payer and sender
	innerTx.ExpiredTime = 3000
	innerTx.PayerR, innerTx.PayerS, innerTx.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(payerKey.PublicKey),
		&innerTx,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx, err = types.SignNewTx(payerKey, mikoSigner, &innerTx)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	err = txpool.addRemoteSync(tx)
	if err == nil || !errors.Is(err, types.ErrSamePayerSenderSponsoredTx) {
		t.Fatalf("Expect error %s, get %s", types.ErrSamePayerSenderSponsoredTx, err)
	}

	// 4. Failed when payer does not have sufficient fund for gas fee
	innerTx.PayerR, innerTx.PayerS, innerTx.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&innerTx,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx, err = types.SignNewTx(senderKey, mikoSigner, &innerTx)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	err = txpool.addRemoteSync(tx)
	if err == nil || !errors.Is(err, core.ErrInsufficientPayerFunds) {
		t.Fatalf("Expect error %s, get %s", core.ErrInsufficientPayerFunds, err)
	}

	// 5. Failed when sender does not have sufficient fund for msg.value
	statedb.SetBalance(crypto.PubkeyToAddress(payerKey.PublicKey), new(big.Int).Mul(big.NewInt(100000), big.NewInt(22000)))
	err = txpool.addRemoteSync(tx)
	if err == nil || !errors.Is(err, core.ErrInsufficientSenderFunds) {
		t.Fatalf("Expect error %s, get %s", core.ErrInsufficientSenderFunds, err)
	}

	// 6. Successfully add tx
	statedb.SetBalance(crypto.PubkeyToAddress(senderKey.PublicKey), big.NewInt(10))
	err = txpool.addRemoteSync(tx)
	if err != nil {
		t.Fatalf("Expect successfully add tx, get %s", err)
	}

	// 7. Sponsored tx with expired time == 0 is accepted
	innerTx.ExpiredTime = 0
	innerTx.Nonce = 2
	innerTx.PayerR, innerTx.PayerS, innerTx.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&innerTx,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx, err = types.SignNewTx(senderKey, mikoSigner, &innerTx)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	err = txpool.addRemoteSync(tx)
	if err != nil {
		t.Fatalf("Expect successfully add tx, get %s", err)
	}

	// 8. After Venoki, gas fee cap and gas tip cap can be different
	txpool.currentHead.Store(&types.Header{Number: common.Big1, GasLimit: 10000000})
	innerTx.Nonce = 3
	innerTx.GasFeeCap = big.NewInt(params.MinimumBaseFee + 1)
	innerTx.Value = common.Big0
	statedb.SetBalance(crypto.PubkeyToAddress(payerKey.PublicKey), new(big.Int).Mul(innerTx.GasFeeCap, big.NewInt(22000)))
	innerTx.PayerR, innerTx.PayerS, innerTx.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&innerTx,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx, err = types.SignNewTx(senderKey, mikoSigner, &innerTx)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	err = txpool.addRemoteSync(tx)
	if err != nil {
		t.Fatalf("Expect successfully add tx, get %s", err)
	}
}

// TestSponsoredTxInTxPoolQueue tests that sponsored tx is removed from
// txpool's queue when balance of payer/sender is insufficient or tx
// is expired
func TestSponsoredTxInTxPoolQueue(t *testing.T) {
	var chainConfig params.ChainConfig

	chainConfig.EIP155Block = common.Big0
	chainConfig.MikoBlock = common.Big0
	chainConfig.ChainID = big.NewInt(2020)

	recipient := common.HexToAddress("1000000000000000000000000000000000000001")
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{10000000, statedb, new(event.Feed), 0}

	txpool := New(testTxPoolConfig, &chainConfig, blockchain)
	defer txpool.Close()
	txpool.Init(
		testTxPoolConfig.PriceLimit,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	senderKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	senderAddr := crypto.PubkeyToAddress(senderKey.PublicKey)

	payerKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payerAddr := crypto.PubkeyToAddress(payerKey.PublicKey)

	sponsoredTx1 := types.SponsoredTx{
		ChainID:     big.NewInt(2020),
		Nonce:       0,
		GasTipCap:   big.NewInt(100000),
		GasFeeCap:   big.NewInt(100000),
		Gas:         30000,
		To:          &recipient,
		Value:       big.NewInt(10),
		ExpiredTime: 100,
	}
	sponsoredTx2 := types.SponsoredTx{
		ChainID:     big.NewInt(2020),
		Nonce:       2,
		GasTipCap:   big.NewInt(100000),
		GasFeeCap:   big.NewInt(100000),
		Gas:         30000,
		To:          &recipient,
		Value:       big.NewInt(10),
		ExpiredTime: 100,
	}
	gasFee := new(big.Int).Mul(sponsoredTx1.GasFeeCap, new(big.Int).SetUint64(sponsoredTx1.Gas))
	statedb.SetBalance(payerAddr, gasFee)
	statedb.SetBalance(senderAddr, sponsoredTx1.Value)

	mikoSigner := types.NewMikoSigner(big.NewInt(2020))
	sponsoredTx1.PayerR, sponsoredTx1.PayerS, sponsoredTx1.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&sponsoredTx1,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx1, err := types.SignNewTx(senderKey, mikoSigner, &sponsoredTx1)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	sponsoredTx2.PayerR, sponsoredTx2.PayerS, sponsoredTx2.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&sponsoredTx2,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx2, err := types.SignNewTx(senderKey, mikoSigner, &sponsoredTx2)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	errs := txpool.AddRemotesSync([]*types.Transaction{tx1, tx2})
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Fail to add tx to pool, err %s", err)
		}
	}

	pending, queued := txpool.Stats()
	if pending != 1 {
		t.Fatalf("Pending txpool, expect %d get %d", 1, pending)
	}
	if queued != 1 {
		t.Fatalf("Queued txpool, expect %d get %d", 1, queued)
	}

	// 1. Payer fund is insufficient, 2 txs are removed from pending and queued
	statedb.SubBalance(payerAddr, common.Big1)
	<-txpool.requestReset(nil, nil)
	pending, queued = txpool.Stats()
	if pending != 0 {
		t.Fatalf("Pending txpool, expect %d get %d", 0, pending)
	}
	if queued != 0 {
		t.Fatalf("Queued txpool, expect %d get %d", 0, queued)
	}

	// 2. Sender fund is insufficient, 2 txs are removed from pending and queued
	statedb.AddBalance(payerAddr, common.Big1)
	errs = txpool.AddRemotesSync([]*types.Transaction{tx1, tx2})
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Fail to add tx to pool, err %s", err)
		}
	}

	pending, queued = txpool.Stats()
	if pending != 1 {
		t.Fatalf("Pending txpool, expect %d get %d", 1, pending)
	}
	if queued != 1 {
		t.Fatalf("Queued txpool, expect %d get %d", 1, queued)
	}

	statedb.SubBalance(senderAddr, common.Big1)
	<-txpool.requestReset(nil, nil)
	pending, queued = txpool.Stats()
	if pending != 0 {
		t.Fatalf("Pending txpool, expect %d get %d", 0, pending)
	}
	if queued != 0 {
		t.Fatalf("Queued txpool, expect %d get %d", 0, queued)
	}

	// 3. Payer fund is insufficient, 2 txs with the same payer in a queue
	statedb.AddBalance(senderAddr, common.Big1)
	sponsoredTx3 := types.SponsoredTx{
		ChainID:     big.NewInt(2020),
		Nonce:       3,
		GasTipCap:   big.NewInt(100000),
		GasFeeCap:   big.NewInt(100000),
		Gas:         21000,
		To:          &recipient,
		Value:       big.NewInt(10),
		ExpiredTime: 100,
	}

	sponsoredTx3.PayerR, sponsoredTx3.PayerS, sponsoredTx3.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&sponsoredTx3,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx3, err := types.SignNewTx(senderKey, mikoSigner, &sponsoredTx3)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	errs = txpool.AddRemotesSync([]*types.Transaction{tx2, tx3})
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Fail to add tx to pool, err %s", err)
		}
	}

	_, queued = txpool.Stats()
	if queued != 2 {
		t.Fatalf("Queued txpool, expect %d get %d", 2, queued)
	}

	gasFee = new(big.Int).Mul(sponsoredTx3.GasFeeCap, new(big.Int).SetUint64(sponsoredTx3.Gas))
	statedb.SetBalance(payerAddr, gasFee)
	<-txpool.requestReset(nil, nil)

	// tx2 must be removed from queue but not tx3
	_, queued = txpool.Stats()
	if queued != 1 {
		t.Fatalf("Queued txpool, expect %d get %d", 1, queued)
	}

	statedb.SubBalance(payerAddr, common.Big1)
	<-txpool.requestReset(nil, nil)
	// tx3 must be removed now
	_, queued = txpool.Stats()
	if queued != 0 {
		t.Fatalf("Queued txpool, expect %d get %d", 0, queued)
	}

	// 4. Expired txs are removed from pending and queued
	gasFee = new(big.Int).Mul(sponsoredTx1.GasFeeCap, new(big.Int).SetUint64(sponsoredTx1.Gas))
	statedb.SetBalance(payerAddr, gasFee)
	errs = txpool.AddRemotesSync([]*types.Transaction{tx1, tx2})
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Fail to add tx to pool, err %s", err)
		}
	}

	pending, queued = txpool.Stats()
	if pending != 1 {
		t.Fatalf("Pending txpool, expect %d get %d", 1, pending)
	}
	if queued != 1 {
		t.Fatalf("Queued txpool, expect %d get %d", 1, queued)
	}

	<-txpool.requestReset(nil, types.CopyHeader(&types.Header{Time: 200}))
	pending, queued = txpool.Stats()
	if pending != 0 {
		t.Fatalf("Pending txpool, expect %d get %d", 0, pending)
	}
	if queued != 0 {
		t.Fatalf("Queued txpool, expect %d get %d", 0, queued)
	}

	// 5. Sponsored tx with expired time == 0 is not removed
	<-txpool.requestReset(nil, types.CopyHeader(&types.Header{Time: 200, GasLimit: blockchain.gasLimit}))

	sponsoredTx1.ExpiredTime = 0
	sponsoredTx1.PayerR, sponsoredTx1.PayerS, sponsoredTx1.PayerV, err = types.PayerSign(
		payerKey,
		mikoSigner,
		crypto.PubkeyToAddress(senderKey.PublicKey),
		&sponsoredTx1,
	)
	if err != nil {
		t.Fatalf("Payer fails to sign transaction, err %s", err)
	}

	tx1, err = types.SignNewTx(senderKey, mikoSigner, &sponsoredTx1)
	if err != nil {
		t.Fatalf("Fail to sign transaction, err %s", err)
	}

	errs = txpool.AddRemotesSync([]*types.Transaction{tx1})
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Fail to add tx to pool, err %s", err)
		}
	}

	pending, _ = txpool.Stats()
	if pending != 1 {
		t.Fatalf("Pending txpool, expect %d get %d", 1, pending)
	}

	<-txpool.requestReset(nil, types.CopyHeader(&types.Header{Time: 200, GasLimit: blockchain.gasLimit}))

	pending, _ = txpool.Stats()
	if pending != 1 {
		t.Fatalf("Pending txpool, expect %d get %d", 1, pending)
	}
}

func TestFeeCapCheckVenoki(t *testing.T) {
	var chainConfig params.ChainConfig

	chainConfig.EIP155Block = common.Big0
	chainConfig.LondonBlock = common.Big0
	chainConfig.VenokiBlock = common.Big0
	chainConfig.ChainID = params.TestChainConfig.ChainID

	senderKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	senderAddr := crypto.PubkeyToAddress(senderKey.PublicKey)

	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	statedb.AddBalance(senderAddr, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))

	blockchain := &testBlockChain{10000000, statedb, new(event.Feed), 0}

	pool := New(testTxPoolConfig, &chainConfig, blockchain)
	defer pool.Close()
	pool.Init(
		1,
		blockchain.CurrentBlock().Header(),
		func(addr common.Address, reserve bool) error { return nil },
	)

	// 1. fee cap restrict the max tip to 0, lower than pool's required tip
	tx := dynamicFeeTx(0, 21000, big.NewInt(params.MinimumBaseFee), common.Big2, senderKey)
	err = pool.addRemoteSync(tx)
	if !errors.Is(err, txpool.ErrUnderpriced) {
		t.Fatalf("Expect error %v, have %v", txpool.ErrUnderpriced, err)
	}

	// 2. Successfully add transaction
	tx = dynamicFeeTx(0, 21000, big.NewInt(params.MinimumBaseFee+1), common.Big2, senderKey)
	err = pool.addRemoteSync(tx)
	if err != nil {
		t.Fatalf("Expect successful add transaction have %v", err)
	}

	pending, _ := pool.Stats()
	if pending != 1 {
		t.Fatalf("Pending txpool, expect %d get %d", 1, pending)
	}

	// 3. Pool's required tip is increased, underpriced transaction is removed
	pool.SetGasTip(common.Big2)
	pending, _ = pool.Stats()
	if pending != 0 {
		t.Fatalf("Pending txpool, expect %d get %d", 0, pending)
	}
}
