// Copyright 2018 The CortexTheseus Authors
// This file is part of the CortexFoundation library.
//
// The CortexFoundation library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The CortexFoundation library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the CortexFoundation library. If not, see <http://www.gnu.org/licenses/>.

// Package state provides a caching layer atop the Cortex state trie.
package state

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/CortexFoundation/CortexTheseus/common"
	"github.com/CortexFoundation/CortexTheseus/core/state/snapshot"
	"github.com/CortexFoundation/CortexTheseus/core/types"
	"github.com/CortexFoundation/CortexTheseus/crypto"
	"github.com/CortexFoundation/CortexTheseus/log"
	"github.com/CortexFoundation/CortexTheseus/rlp"
	"github.com/CortexFoundation/CortexTheseus/trie"
	"math/big"
	"sort"
)

type revision struct {
	id           int
	journalIndex int
}

var (
	// emptyState is the known hash of an empty state trie entry.
	//emptyState = crypto.Keccak256Hash(nil)
	emptyRoot = common.HexToHash("56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

	// emptyCode is the known hash of the empty CVM bytecode.
	emptyCode = crypto.Keccak256Hash(nil)
)

type proofList [][]byte

func (n *proofList) Put(key []byte, value []byte) error {
	*n = append(*n, value)
	return nil
}

func (n *proofList) Delete(key []byte) error {
	panic("not supported")
}

// StateDBs within the cortex protocol are used to store anything
// within the merkle trie. StateDBs take care of caching and storing
// nested states. It's the general query interface to retrieve:
// * Contracts
// * Accounts
type StateDB struct {
	db   Database
	trie Trie

	snaps         *snapshot.Tree
	snap          snapshot.Snapshot
	snapDestructs map[common.Hash]struct{}
	snapAccounts  map[common.Hash][]byte
	snapStorage   map[common.Hash]map[common.Hash][]byte

	// This map holds 'live' objects, which will get modified while processing a state transition.
	stateObjects        map[common.Address]*stateObject
	stateObjectsPending map[common.Address]struct{} // State objects finalized but not yet written to the trie
	stateObjectsDirty   map[common.Address]struct{}

	// DB error.
	// State objects are used by the consensus core and VM which are
	// unable to deal with database-level errors. Any error that occurs
	// during a database read is memoized here and will eventually be returned
	// by StateDB.Commit.
	dbErr error

	// The refund counter, also used by state transitioning.
	refund uint64

	thash, bhash common.Hash
	txIndex      int
	logs         map[common.Hash][]*types.Log
	logSize      uint

	preimages map[common.Hash][]byte

	// Journal of state modifications. This is the backbone of
	// Snapshot and RevertToSnapshot.
	journal        *journal
	validRevisions []revision
	nextRevisionId int

	//lock sync.Mutex
}

// Create a new state from a given trie.
func New(root common.Hash, db Database, snaps *snapshot.Tree) (*StateDB, error) {
	tr, err := db.OpenTrie(root)
	if err != nil {
		return nil, err
	}
	sdb := &StateDB{
		db:                  db,
		trie:                tr,
		snaps:               snaps,
		stateObjects:        make(map[common.Address]*stateObject),
		stateObjectsPending: make(map[common.Address]struct{}),
		stateObjectsDirty:   make(map[common.Address]struct{}),
		logs:                make(map[common.Hash][]*types.Log),
		preimages:           make(map[common.Hash][]byte),
		journal:             newJournal(),
	}
	if sdb.snaps != nil {
		if sdb.snap = sdb.snaps.Snapshot(root); sdb.snap != nil {
			sdb.snapDestructs = make(map[common.Hash]struct{})
			sdb.snapAccounts = make(map[common.Hash][]byte)
			sdb.snapStorage = make(map[common.Hash]map[common.Hash][]byte)
		}
	}
	return sdb, nil
}

// setError remembers the first non-nil error it is called with.
func (s *StateDB) setError(err error) {
	if s.dbErr == nil {
		s.dbErr = err
	}
}

func (s *StateDB) Error() error {
	return s.dbErr
}

// Reset clears out all ephemeral state objects from the state db, but keeps
// the underlying state trie to avoid reloading data for the next operations.
func (s *StateDB) Reset(root common.Hash) error {
	tr, err := s.db.OpenTrie(root)
	if err != nil {
		return err
	}
	s.trie = tr
	s.stateObjects = make(map[common.Address]*stateObject)
	s.stateObjectsPending = make(map[common.Address]struct{})
	s.stateObjectsDirty = make(map[common.Address]struct{})
	s.thash = common.Hash{}
	s.bhash = common.Hash{}
	s.txIndex = 0
	s.logs = make(map[common.Hash][]*types.Log)
	s.logSize = 0
	s.preimages = make(map[common.Hash][]byte)
	s.clearJournalAndRefund()

	if s.snaps != nil {
		s.snapAccounts, s.snapDestructs, s.snapStorage = nil, nil, nil
		if s.snap = s.snaps.Snapshot(root); s.snap != nil {
			s.snapDestructs = make(map[common.Hash]struct{})
			s.snapAccounts = make(map[common.Hash][]byte)
			s.snapStorage = make(map[common.Hash]map[common.Hash][]byte)
		}
	}
	return nil
}

func (s *StateDB) AddLog(log *types.Log) {
	s.journal.append(addLogChange{txhash: s.thash})

	log.TxHash = s.thash
	log.BlockHash = s.bhash
	log.TxIndex = uint(s.txIndex)
	log.Index = s.logSize
	s.logs[s.thash] = append(s.logs[s.thash], log)
	s.logSize++
}

func (s *StateDB) GetLogs(hash common.Hash) []*types.Log {
	return s.logs[hash]
}

func (s *StateDB) Logs() []*types.Log {
	var logs []*types.Log
	for _, lgs := range s.logs {
		logs = append(logs, lgs...)
	}
	return logs
}

// AddPreimage records a SHA3 preimage seen by the VM.
func (s *StateDB) AddPreimage(hash common.Hash, preimage []byte) {
	if _, ok := s.preimages[hash]; !ok {
		s.journal.append(addPreimageChange{hash: hash})
		pi := make([]byte, len(preimage))
		copy(pi, preimage)
		s.preimages[hash] = pi
	}
}

// Preimages returns a list of SHA3 preimages that have been submitted.
func (s *StateDB) Preimages() map[common.Hash][]byte {
	return s.preimages
}

// AddRefund adds gas to the refund counter
func (s *StateDB) AddRefund(gas uint64) {
	s.journal.append(refundChange{prev: s.refund})
	s.refund += gas
}

// SubRefund removes gas from the refund counter.
// This method will panic if the refund counter goes below zero
func (s *StateDB) SubRefund(gas uint64) {
	s.journal.append(refundChange{prev: s.refund})
	if gas > s.refund {
		panic(fmt.Sprintf("Refund counter below zero (gas: %d > refund: %d)", gas, s.refund))
	}
	s.refund -= gas
}

// Exist reports whether the given account address exists in the state.
// Notably this also returns true for suicided accounts.
func (s *StateDB) Exist(addr common.Address) bool {
	return s.getStateObject(addr) != nil
}

// Empty returns whether the state object is either non-existent
// or empty according to the EIP161 specification (balance = nonce = code = 0)
func (s *StateDB) Empty(addr common.Address) bool {
	so := s.getStateObject(addr)
	return so == nil || so.empty()
}

// Retrieve the balance from the given address or 0 if object not found
func (s *StateDB) GetBalance(addr common.Address) *big.Int {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Balance()
	}
	return common.Big0
}

func (s *StateDB) GetUpload(addr common.Address) *big.Int {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Upload()
	}
	return common.Big0
}

func (s *StateDB) GetNum(addr common.Address) *big.Int {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Num()
	}
	return common.Big0
}

func (s *StateDB) GetNonce(addr common.Address) uint64 {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Nonce()
	}

	return 0
}

// TxIndex returns the current transaction index set by Prepare.
func (s *StateDB) TxIndex() int {
	return s.txIndex
}

// BlockHash returns the current block hash set by Prepare.
func (s *StateDB) BlockHash() common.Hash {
	return s.bhash
}

func (s *StateDB) GetCode(addr common.Address) []byte {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Code(s.db)
	}
	return nil
}

func (s *StateDB) GetCodeSize(addr common.Address) int {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return 0
	}
	if stateObject.code != nil {
		return len(stateObject.code)
	}
	size, err := s.db.ContractCodeSize(stateObject.addrHash, common.BytesToHash(stateObject.CodeHash()))
	if err != nil {
		s.setError(err)
	}
	return size
}

func (s *StateDB) GetCodeHash(addr common.Address) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return common.Hash{}
	}
	return common.BytesToHash(stateObject.CodeHash())
}

// GetState retrieves a value from the given account's storage trie
func (s *StateDB) GetState(addr common.Address, bhash common.Hash) common.Hash {
	stateObject := s.getStateObject(addr)
	log.Trace("GetState", "addr", addr, "bhash", bhash, "obj", stateObject)
	if stateObject != nil {
		return stateObject.GetState(s.db, bhash)
	}
	return common.Hash{}
}

// GetState returns a value in account storage.
//func (s *StateDB) GetSolidityUint256(addr common.Address, slot common.Hash) ([]byte, error) {
func (s *StateDB) GetSolidityBytes(addr common.Address, slot common.Hash) ([]byte, error) {
	length := s.GetState(addr, slot).Big().Uint64()
	if length == uint64(0) {
		return nil, nil
	}
	hashBig := new(big.Int).SetBytes(crypto.Keccak256(slot.Bytes()))
	//log.Warn(fmt.Sprintf("Pos %v, %v => %v, %v", addr, slot, length, hash))
	//log.Trace("solid", "addr", addr, "slot", slot, "length", length, "hash", hash, "x", s.GetState(addr, slot), "y", common.Hash{})
	// fmt.Println(fmt.Sprintf("Pos %v, %v => %v, %v", addr, slot, length, hash))

	//buffSize := length * 32

	buff := new(bytes.Buffer) //make([]byte, length * 32)
	for i := int64(0); i < int64(length); i++ {
		slotAddr := common.BigToHash(big.NewInt(0).Add(hashBig, big.NewInt(i)))
		payload := s.GetState(addr, slotAddr).Bytes()
		//copy(buff[idx*32:], payload[:])
		binary.Write(buff, binary.LittleEndian, payload[:])
		//binary.LittleEndian.Put(buff[idx*32:], payload[:])
		// fmt.Println(fmt.Sprintf("load[%v]: %x, %x => %x, %x", idx, addr, slotAddr, payload, hash))
	}
	// fmt.Println(fmt.Sprintf("data: %v", buff))
	return buff.Bytes(), nil
}

// GetState returns a value in account storage.
//func (s *StateDB) GetSolidityBytes(addr common.Address, slot common.Hash) ([]byte, error) {
//	return s.GetSolidityUint256(addr, slot)
// pos := s.GetState(addr, slot).Big().Uint64()
// cont := pos % 2
// length := pos / 2
// hash := crypto.Keccak256(slot.Bytes())
// hashBig := new(big.Int).SetBytes(hash)
// log.Trace(fmt.Sprintf("Pos %v, %v => %v, %v", addr, slot, pos, hash))
// if length < 32 || cont == 0 {
// 	return []byte{}, errors.New("not implemented for data size less than 32!")
// }

// buffSize := uint(length/32) * 32
// if length%32 != 0 {
// 	buffSize += 32
// }

// buff := make([]byte, buffSize)
// var idx int64
// for idx = 0; idx < int64(length)/32; idx++ {
// 	slotAddr := common.BigToHash(big.NewInt(0).Add(hashBig, big.NewInt(idx)))
// 	payload := s.GetState(addr, slotAddr).Bytes()
// 	copy(buff[idx*32:], payload[:])
// 	log.Trace2(fmt.Sprintf("load[%v]: %x, %x => %x, %x", idx, addr, slotAddr, payload, hash))
// }
// buff = buff[:length]
// log.Trace2(fmt.Sprintf("data: %v", buff))
// return buff, nil
//}

// GetProof returns the MerkleProof for a given Account
func (s *StateDB) GetProof(a common.Address) ([][]byte, error) {
	var proof proofList
	err := s.trie.Prove(crypto.Keccak256(a.Bytes()), 0, &proof)
	return [][]byte(proof), err
}

// GetProof returns the StorageProof for given key
func (s *StateDB) GetStorageProof(a common.Address, key common.Hash) ([][]byte, error) {
	var proof proofList
	trie := s.StorageTrie(a)
	if trie == nil {
		return proof, errors.New("storage trie for requested address does not exist")
	}
	err := trie.Prove(crypto.Keccak256(key.Bytes()), 0, &proof)
	return [][]byte(proof), err
}

// GetCommittedState retrieves a value from the given account's committed storage trie.
func (s *StateDB) GetCommittedState(addr common.Address, hash common.Hash) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetCommittedState(s.db, hash)
	}
	return common.Hash{}
}

// Database retrieves the low level database supporting the lower level trie ops.
func (s *StateDB) Database() Database {
	return s.db
}

// StorageTrie returns the storage trie of an account.
// The return value is a copy and is nil for non-existent accounts.
func (s *StateDB) StorageTrie(addr common.Address) Trie {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return nil
	}
	cpy := stateObject.deepCopy(s)
	cpy.updateTrie(s.db)
	return cpy.getTrie(s.db)
}

func (s *StateDB) HasSuicided(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.suicided
	}
	return false
}

/*
 * SETTERS
 */

// AddBalance adds amount to the account associated with addr.
func (s *StateDB) AddBalance(addr common.Address, amount *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.AddBalance(amount)
	}
}

func (s *StateDB) Upload(addr common.Address) *big.Int {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		return stateObject.Upload()
	}
	return nil
}

// SubBalance subtracts amount from the account associated with addr.
func (s *StateDB) SubBalance(addr common.Address, amount *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SubBalance(amount)
	}
}

func (s *StateDB) SetBalance(addr common.Address, amount *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetBalance(amount)
	}
}
func (s *StateDB) AddUpload(addr common.Address, amount *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.AddUpload(amount)
	}
}

func (s *StateDB) SubUpload(addr common.Address, amount *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SubUpload(amount)
		//log.Debug("Uploading progress", "address", addr.Hex(), "amount", amount, "pending", stateObject.Upload())
	} else {
		//log.Debug("Uploading failed", "address", addr.Hex(), "amount", amount, "pending", stateObject.Upload())
	}
}

func (s *StateDB) SetUpload(addr common.Address, amount *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetUpload(amount)
	}
}

func (s *StateDB) SetNum(addr common.Address, num *big.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetNum(num)
	}
}

func (s *StateDB) SetNonce(addr common.Address, nonce uint64) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetNonce(nonce)
	}
}

func (s *StateDB) SetCode(addr common.Address, code []byte) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetCode(crypto.Keccak256Hash(code), code)
	}
}
func (s *StateDB) Uploading(addr common.Address) bool {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		return stateObject.Upload().Sign() > 0
	}
	return false
}

func (s *StateDB) SetState(addr common.Address, key, value common.Hash) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetState(s.db, key, value)
	}
}

// Suicide marks the given account as suicided.
// This clears the account balance.
//
// The account's state object is still available until the state is committed,
// getStateObject will return a non-nil account after Suicide.
func (s *StateDB) Suicide(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return false
	}
	s.journal.append(suicideChange{
		account:     &addr,
		prev:        stateObject.suicided,
		prevbalance: new(big.Int).Set(stateObject.Balance()),
		prevupload:  new(big.Int).Set(stateObject.Upload()),
		prevnum:     new(big.Int).Set(stateObject.Num()),
	})
	stateObject.markSuicided()
	stateObject.data.Balance = new(big.Int)
	stateObject.data.Upload = new(big.Int)
	stateObject.data.Num = new(big.Int)

	return true
}

//
// Setting, updating & deleting state object methods.
//

// updateStateObject writes the given object to the trie.
func (s *StateDB) updateStateObject(obj *stateObject) {

	addr := obj.Address()
	data, err := rlp.EncodeToBytes(obj)
	if err != nil {
		panic(fmt.Errorf("can't encode object at %x: %v", addr[:], err))
	}
	s.setError(s.trie.TryUpdate(addr[:], data))

	// If state snapshotting is active, cache the data til commit. Note, this
	// update mechanism is not symmetric to the deletion, because whereas it is
	// enough to track account updates at commit time, deletions need tracking
	// at transaction boundary level to ensure we capture state clearing.
	if s.snap != nil {
		s.snapAccounts[obj.addrHash] = snapshot.AccountRLP(obj.data.Nonce, obj.data.Balance, obj.data.Root, obj.data.CodeHash, obj.data.Upload, obj.data.Num)
	}
}

// deleteStateObject removes the given object from the state trie.
func (s *StateDB) deleteStateObject(stateObject *stateObject) {
	//stateObject.deleted = true
	addr := stateObject.Address()
	s.setError(s.trie.TryDelete(addr[:]))
}

// getStateObject retrieves a state object given by the address, returning nil if
// the object is not found or was deleted in this execution context. If you need
// to differentiate between non-existent/just-deleted, use getDeletedStateObject
func (s *StateDB) getStateObject(addr common.Address) *stateObject {
	if obj := s.getDeletedStateObject(addr); obj != nil && !obj.deleted {
		return obj
	}
	return nil
}

// getDeletedStateObject is similar to getStateObject, but instead of returning
// nil for a deleted state object, it returns the actual object with the deleted
// flag set. This is needed by the state journal to revert to the correct s-
// destructed object instead of wiping all knowledge about the state object.
func (s *StateDB) getDeletedStateObject(addr common.Address) (stateObject *stateObject) {
	// Prefer 'live' objects.
	if obj := s.stateObjects[addr]; obj != nil {
		return obj
	}
	// If no live objects are available, attempt to use snapshots
	var (
		data Account
		err  error
	)
	if s.snap != nil {
		var acc *snapshot.Account
		if acc, err = s.snap.Account(crypto.Keccak256Hash(addr[:])); err == nil {
			if acc == nil {
				log.Trace("acc is nil", "addr", addr)
				return nil
			}
			data.Nonce, data.Balance, data.CodeHash, data.Upload, data.Num = acc.Nonce, acc.Balance, acc.CodeHash, acc.Upload, acc.Num
			if len(data.CodeHash) == 0 {
				data.CodeHash = emptyCodeHash
			}
			data.Root = common.BytesToHash(acc.Root)
			if data.Root == (common.Hash{}) {
				data.Root = emptyRoot
			}
		}
		log.Trace("Snap account is not nil", "addr", addr, "nonce", data.Nonce, "balance", data.Balance, "upload", data.Upload, "num", data.Num, "err", err)
	}
	// Load the object from the database.
	// If snapshot unavailable or reading from it failed, load from the database
	if s.snap == nil || err != nil {
		enc, err := s.trie.TryGet(addr[:])
		if len(enc) == 0 {
			s.setError(err)
			return nil
		}
		if err := rlp.DecodeBytes(enc, &data); err != nil {
			log.Error("Failed to decode state object", "addr", addr, "err", err)
			return nil
		}
		log.Trace("snap show is nul", "addr", addr, "err", err)
	}
	// Insert into the live set.
	obj := newObject(s, addr, data)
	s.setStateObject(obj)
	return obj
}

func (s *StateDB) setStateObject(object *stateObject) {
	s.stateObjects[object.Address()] = object
}

// Retrieve a state object or create a new state object if nil.
func (s *StateDB) GetOrNewStateObject(addr common.Address) *stateObject {
	stateObject := s.getStateObject(addr)
	if stateObject == nil { // || stateObject.deleted {
		stateObject, _ = s.createObject(addr)
	}
	return stateObject
}

// createObject creates a new state object. If there is an existing account with
// the given address, it is overwritten and returned as the second return value.
func (s *StateDB) createObject(addr common.Address) (newobj, prev *stateObject) {
	prev = s.getStateObject(addr)
	var prevdestruct bool
	if s.snap != nil && prev != nil {
		_, prevdestruct = s.snapDestructs[prev.addrHash]
		if !prevdestruct {
			s.snapDestructs[prev.addrHash] = struct{}{}
		}
	}
	newobj = newObject(s, addr, Account{})
	newobj.setNonce(0) // sets the object to dirty
	if prev == nil {
		s.journal.append(createObjectChange{account: &addr})
	} else {
		s.journal.append(resetObjectChange{prev: prev, prevdestruct: prevdestruct})
	}
	s.setStateObject(newobj)
	return newobj, prev
}

// CreateAccount explicitly creates a state object. If a state object with the address
// already exists the balance is carried over to the new account.
//
// CreateAccount is called during the CVM CREATE operation. The situation might arise that
// a contract does the following:
//
//   1. sends funds to sha(account ++ (nonce + 1))
//   2. tx_create(sha(account ++ nonce)) (note that this gets the address of 1)
//
// Carrying over the balance ensures that Cortex doesn't disappear.
func (s *StateDB) CreateAccount(addr common.Address) {
	newObj, prev := s.createObject(addr)
	if prev != nil {
		newObj.setBalance(prev.data.Balance)
		newObj.setUpload(prev.data.Upload)
		newObj.setNum(prev.data.Num)
	}
}

func (db *StateDB) ForEachStorage(addr common.Address, cb func(key, value common.Hash) bool) error {
	so := db.getStateObject(addr)
	if so == nil {
		return nil
	}
	it := trie.NewIterator(so.getTrie(db.db).NodeIterator(nil))

	for it.Next() {
		key := common.BytesToHash(db.trie.GetKey(it.Key))
		if value, dirty := so.dirtyStorage[key]; dirty {
			if !cb(key, value) {
				return nil
			}
			continue
		}
		//cb(key, common.BytesToHash(it.Value))
		if len(it.Value) > 0 {
			_, content, _, err := rlp.Split(it.Value)
			if err != nil {
				return err
			}
			if !cb(key, common.BytesToHash(content)) {
				return nil
			}
		}
	}
	return nil
}

// Copy creates a deep, independent copy of the state.
// Snapshots of the copied state cannot be applied to the copy.
func (s *StateDB) Copy() *StateDB {
	// Copy all the basic fields, initialize the memory ones
	state := &StateDB{
		db:                  s.db,
		trie:                s.db.CopyTrie(s.trie),
		stateObjects:        make(map[common.Address]*stateObject, len(s.journal.dirties)),
		stateObjectsPending: make(map[common.Address]struct{}, len(s.stateObjectsPending)),
		stateObjectsDirty:   make(map[common.Address]struct{}, len(s.journal.dirties)),
		refund:              s.refund,
		logs:                make(map[common.Hash][]*types.Log, len(s.logs)),
		logSize:             s.logSize,
		preimages:           make(map[common.Hash][]byte, len(s.preimages)),
		journal:             newJournal(),
	}
	// Copy the dirty states, logs, and preimages
	for addr := range s.journal.dirties {
		// As documented [here](https://github.com/CortexFoundation/CortexTheseus/pull/16485#issuecomment-380438527),
		// and in the Finalise-method, there is a case where an object is in the journal but not
		// in the stateObjects: OOG after touch on ripeMD prior to Byzantium. Thus, we need to check for
		// nil
		if object, exist := s.stateObjects[addr]; exist {
			// Even though the original object is dirty, we are not copying the journal,
			// so we need to make sure that anyside effect the journal would have caused
			// during a commit (or similar op) is already applied to the copy.
			state.stateObjects[addr] = object.deepCopy(state)

			state.stateObjectsDirty[addr] = struct{}{}
			state.stateObjectsPending[addr] = struct{}{} // Mark the copy pending to force external (account) commits
		}
	}
	// Above, we don't copy the actual journal. This means that if the copy is copied, the
	// loop above will be a no-op, since the copy's journal is empty.
	// Thus, here we iterate over stateObjects, to enable copies of copies
	for addr := range s.stateObjectsPending {
		if _, exist := state.stateObjects[addr]; !exist {
			state.stateObjects[addr] = s.stateObjects[addr].deepCopy(state)
		}
		state.stateObjectsPending[addr] = struct{}{}
	}
	for addr := range s.stateObjectsDirty {
		if _, exist := state.stateObjects[addr]; !exist {
			state.stateObjects[addr] = s.stateObjects[addr].deepCopy(state)
		}
		state.stateObjectsDirty[addr] = struct{}{}
	}
	for hash, logs := range s.logs {
		cpy := make([]*types.Log, len(logs))
		for i, l := range logs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		state.logs[hash] = cpy
	}
	for hash, preimage := range s.preimages {
		state.preimages[hash] = preimage
	}
	return state
}

// Snapshot returns an identifier for the current revision of the state.
func (s *StateDB) Snapshot() int {
	id := s.nextRevisionId
	s.nextRevisionId++
	s.validRevisions = append(s.validRevisions, revision{id, s.journal.length()})
	return id
}

// RevertToSnapshot reverts all state changes made since the given revision.
func (s *StateDB) RevertToSnapshot(revid int) {
	// Find the snapshot in the stack of valid snapshots.
	idx := sort.Search(len(s.validRevisions), func(i int) bool {
		return s.validRevisions[i].id >= revid
	})
	if idx == len(s.validRevisions) || s.validRevisions[idx].id != revid {
		panic(fmt.Errorf("revision id %v cannot be reverted, %v, %v", revid, idx, len(s.validRevisions)))
	}
	snapshot := s.validRevisions[idx].journalIndex

	// Replay the journal to undo changes and remove invalidated snapshots
	s.journal.revert(s, snapshot)
	s.validRevisions = s.validRevisions[:idx]
}

// GetRefund returns the current value of the refund counter.
func (s *StateDB) GetRefund() uint64 {
	return s.refund
}

// Finalise finalises the state by removing the s destructed objects and clears
// the journal as well as the refunds. Finalise, however, will not push any updates
// into the tries just yet. Only IntermediateRoot or Commit will do that.
func (s *StateDB) Finalise(deleteEmptyObjects bool) {
	for addr := range s.journal.dirties {
		obj, exist := s.stateObjects[addr]
		if !exist {
			// ripeMD is 'touched' at block 1714175, in tx 0x1237f737031e40bcde4a8b7e717b2d15e3ecadfe49bb1bbc71ee9deb09c6fcf2
			// That tx goes out of gas, and although the notion of 'touched' does not exist there, the
			// touch-event will still be recorded in the journal. Since ripeMD is a special snowflake,
			// it will persist in the journal even though the journal is reverted. In this special circumstance,
			// it may exist in `s.journal.dirties` but not in `s.stateObjects`.
			// Thus, we can safely ignore it here
			continue
		}
		if obj.suicided || (deleteEmptyObjects && obj.empty()) {
			obj.deleted = true

			// If state snapshotting is active, also mark the destruction there.
			// Note, we can't do this only at the end of a block because multiple
			// transactions within the same block might self destruct and then
			// ressurrect an account; but the snapshotter needs both events.
			if s.snap != nil {
				s.snapDestructs[obj.addrHash] = struct{}{} // We need to maintain account deletions explicitly (will remain set indefinitely)
				delete(s.snapAccounts, obj.addrHash)       // Clear out any previously updated account data (may be recreated via a ressurrect)
				delete(s.snapStorage, obj.addrHash)        // Clear out any previously updated storage data (may be recreated via a ressurrect)
			}
		} else {
			obj.finalise()
		}
		s.stateObjectsPending[addr] = struct{}{}
		s.stateObjectsDirty[addr] = struct{}{}
	}
	// Invalidate journal because reverting across transactions is not allowed.
	s.clearJournalAndRefund()
}

// IntermediateRoot computes the current root hash of the state trie.
// It is called in between transactions to get the root hash that
// goes into transaction receipts.
func (s *StateDB) IntermediateRoot(deleteEmptyObjects bool) common.Hash {

	s.Finalise(deleteEmptyObjects)

	for addr := range s.stateObjectsPending {
		obj := s.stateObjects[addr]
		if obj.deleted {
			s.deleteStateObject(obj)
		} else {
			obj.updateRoot(s.db)
			s.updateStateObject(obj)
		}
	}
	if len(s.stateObjectsPending) > 0 {
		s.stateObjectsPending = make(map[common.Address]struct{})
	}
	return s.trie.Hash()
}

// Prepare sets the current transaction hash and index and block hash which is
// used when the CVM emits new state logs.
func (s *StateDB) Prepare(thash, bhash common.Hash, ti int) {
	s.thash = thash
	s.bhash = bhash
	s.txIndex = ti
}

func (s *StateDB) clearJournalAndRefund() {
	if len(s.journal.entries) > 0 {
		s.journal = newJournal()
		s.refund = 0
	}
	s.validRevisions = s.validRevisions[:0]
}

// Commit writes the state to the underlying in-memory trie database.
func (s *StateDB) Commit(deleteEmptyObjects bool) (common.Hash, error) {
	// Finalize any pending changes and merge everything into the tries
	s.IntermediateRoot(deleteEmptyObjects)

	// Commit objects to the trie, measuring the elapsed time
	for addr := range s.stateObjectsDirty {
		if obj := s.stateObjects[addr]; !obj.deleted {
			// Write any contract code associated with the state object
			if obj.code != nil && obj.dirtyCode {
				s.db.TrieDB().InsertBlob(common.BytesToHash(obj.CodeHash()), obj.code)
				obj.dirtyCode = false
			}
			// Write any storage changes in the state object to its storage trie
			if err := obj.CommitTrie(s.db); err != nil {
				return common.Hash{}, err
			}
		}
	}
	if len(s.stateObjectsDirty) > 0 {
		s.stateObjectsDirty = make(map[common.Address]struct{})
	}
	// The onleaf func is called _serially_, so we can reuse the same account
	// for unmarshalling every time.
	var account Account
	root, err := s.trie.Commit(func(leaf []byte, parent common.Hash) error {
		if err := rlp.DecodeBytes(leaf, &account); err != nil {
			return nil
		}
		if account.Root != emptyRoot {
			s.db.TrieDB().Reference(account.Root, parent)
		}
		code := common.BytesToHash(account.CodeHash)
		if code != emptyCode {
			s.db.TrieDB().Reference(code, parent)
		}
		return nil
	})
	// If snapshotting is enabled, update the snapshot tree with this new version
	if s.snap != nil {
		// Only update if there's a state transition (skip empty Clique blocks)
		if parent := s.snap.Root(); parent != root {
			if err := s.snaps.Update(root, parent, s.snapDestructs, s.snapAccounts, s.snapStorage); err != nil {
				log.Warn("Failed to update snapshot tree", "from", parent, "to", root, "err", err)
			}
			if err := s.snaps.Cap(root, 127); err != nil { // Persistent layer is 128th, the last available trie
				log.Warn("Failed to cap snapshot tree", "root", root, "layers", 127, "err", err)
			}
		}
		s.snap, s.snapDestructs, s.snapAccounts, s.snapStorage = nil, nil, nil, nil
	}
	return root, err
}
