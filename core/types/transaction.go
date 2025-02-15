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

package types

import (
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"github.com/ethereum/go-ethereum/log"
	"io"
	"math/big"
	"sort"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/google/uuid"
)

var (
	ErrInvalidSig           = errors.New("invalid transaction v, r, s values")
	ErrUnexpectedProtection = errors.New("transaction type does not supported EIP-155 protected signatures")
	ErrInvalidTxType        = errors.New("transaction type not valid in this context")
	ErrTxTypeNotSupported   = errors.New("transaction type not supported")
	ErrGasFeeCapTooLow      = errors.New("fee cap less than base fee")
	errShortTypedTx         = errors.New("typed transaction too short")
)

// Transaction types.
const (
	LegacyTxType = iota
	AccessListTxType
	DynamicFeeTxType
)

// Transaction is an Ethereum transaction.
type Transaction struct {
	inner TxData    // Consensus contents of a transaction
	time  time.Time // Time first seen locally (spam avoidance)

	// caches
	hash atomic.Value
	size atomic.Value
	from atomic.Value
}

// NewTx creates a new transaction.
func NewTx(inner TxData) *Transaction {
	tx := new(Transaction)
	tx.setDecoded(inner.copy(), 0)
	return tx
}

// TxData is the underlying data of a transaction.
//
// This is implemented by DynamicFeeTx, LegacyTx and AccessListTx.
type TxData interface {
	txType() byte // returns the type ID
	copy() TxData // creates a deep copy and initializes all fields

	chainID() *big.Int
	accessList() AccessList
	data() []byte
	gas() uint64
	gasPrice() *big.Int
	gasTipCap() *big.Int
	gasFeeCap() *big.Int
	value() *big.Int
	nonce() uint64
	to() *common.Address

	rawSignatureValues() (v, r, s *big.Int)
	setSignatureValues(chainID, v, r, s *big.Int)

	// effectiveGasPrice computes the gas price paid by the transaction, given
	// the inclusion block baseFee.
	//
	// Unlike other TxData methods, the returned *big.Int should be an independent
	// copy of the computed value, i.e. callers are allowed to mutate the result.
	// Method implementations can use 'dst' to store the result.
	effectiveGasPrice(dst *big.Int, baseFee *big.Int) *big.Int
}

// EncodeRLP implements rlp.Encoder
func (tx *Transaction) EncodeRLP(w io.Writer) error {
	if tx.Type() == LegacyTxType {
		return rlp.Encode(w, tx.inner)
	}
	// It's an EIP-2718 typed TX envelope.
	buf := encodeBufferPool.Get().(*bytes.Buffer)
	defer encodeBufferPool.Put(buf)
	buf.Reset()
	if err := tx.encodeTyped(buf); err != nil {
		return err
	}
	return rlp.Encode(w, buf.Bytes())
}

// encodeTyped writes the canonical encoding of a typed transaction to w.
func (tx *Transaction) encodeTyped(w *bytes.Buffer) error {
	w.WriteByte(tx.Type())
	return rlp.Encode(w, tx.inner)
}

// MarshalBinary returns the canonical encoding of the transaction.
// For legacy transactions, it returns the RLP encoding. For EIP-2718 typed
// transactions, it returns the type and payload.
func (tx *Transaction) MarshalBinary() ([]byte, error) {
	if tx.Type() == LegacyTxType {
		return rlp.EncodeToBytes(tx.inner)
	}
	var buf bytes.Buffer
	err := tx.encodeTyped(&buf)
	return buf.Bytes(), err
}

// DecodeRLP implements rlp.Decoder
func (tx *Transaction) DecodeRLP(s *rlp.Stream) error {
	kind, size, err := s.Kind()
	switch {
	case err != nil:
		return err
	case kind == rlp.List:
		// It's a legacy transaction.
		var inner LegacyTx
		err := s.Decode(&inner)
		if err == nil {
			tx.setDecoded(&inner, rlp.ListSize(size))
		}
		return err
	default:
		// It's an EIP-2718 typed TX envelope.
		var b []byte
		if b, err = s.Bytes(); err != nil {
			return err
		}
		inner, err := tx.decodeTyped(b)
		if err == nil {
			tx.setDecoded(inner, uint64(len(b)))
		}
		return err
	}
}

// UnmarshalBinary decodes the canonical encoding of transactions.
// It supports legacy RLP transactions and EIP2718 typed transactions.
func (tx *Transaction) UnmarshalBinary(b []byte) error {
	if len(b) > 0 && b[0] > 0x7f {
		// It's a legacy transaction.
		var data LegacyTx
		err := rlp.DecodeBytes(b, &data)
		if err != nil {
			return err
		}
		tx.setDecoded(&data, uint64(len(b)))
		return nil
	}
	// It's an EIP2718 typed transaction envelope.
	inner, err := tx.decodeTyped(b)
	if err != nil {
		return err
	}
	tx.setDecoded(inner, uint64(len(b)))
	return nil
}

// decodeTyped decodes a typed transaction from the canonical format.
func (tx *Transaction) decodeTyped(b []byte) (TxData, error) {
	if len(b) <= 1 {
		return nil, errShortTypedTx
	}
	switch b[0] {
	case AccessListTxType:
		var inner AccessListTx
		err := rlp.DecodeBytes(b[1:], &inner)
		return &inner, err
	case DynamicFeeTxType:
		var inner DynamicFeeTx
		err := rlp.DecodeBytes(b[1:], &inner)
		return &inner, err
	default:
		return nil, ErrTxTypeNotSupported
	}
}

// setDecoded sets the inner transaction and size after decoding.
func (tx *Transaction) setDecoded(inner TxData, size uint64) {
	tx.inner = inner
	tx.time = time.Now()
	if size > 0 {
		tx.size.Store(size)
	}
}

func sanityCheckSignature(v *big.Int, r *big.Int, s *big.Int, maybeProtected bool) error {
	if isProtectedV(v) && !maybeProtected {
		return ErrUnexpectedProtection
	}

	var plainV byte
	if isProtectedV(v) {
		chainID := deriveChainId(v).Uint64()
		plainV = byte(v.Uint64() - 35 - 2*chainID)
	} else if maybeProtected {
		// Only EIP-155 signatures can be optionally protected. Since
		// we determined this v value is not protected, it must be a
		// raw 27 or 28.
		plainV = byte(v.Uint64() - 27)
	} else {
		// If the signature is not optionally protected, we assume it
		// must already be equal to the recovery id.
		plainV = byte(v.Uint64())
	}
	if !crypto.ValidateSignatureValues(plainV, r, s, false) {
		return ErrInvalidSig
	}

	return nil
}

func isProtectedV(V *big.Int) bool {
	if V.BitLen() <= 8 {
		v := V.Uint64()
		return v != 27 && v != 28 && v != 1 && v != 0
	}
	// anything not 27 or 28 is considered protected
	return true
}

// Protected says whether the transaction is replay-protected.
func (tx *Transaction) Protected() bool {
	switch tx := tx.inner.(type) {
	case *LegacyTx:
		return tx.V != nil && isProtectedV(tx.V)
	default:
		return true
	}
}

// Type returns the transaction type.
func (tx *Transaction) Type() uint8 {
	return tx.inner.txType()
}

// ChainId returns the EIP155 chain ID of the transaction. The return value will always be
// non-nil. For legacy transactions which are not replay-protected, the return value is
// zero.
func (tx *Transaction) ChainId() *big.Int {
	return tx.inner.chainID()
}

// Data returns the input data of the transaction.
func (tx *Transaction) Data() []byte { return tx.inner.data() }

// AccessList returns the access list of the transaction.
func (tx *Transaction) AccessList() AccessList { return tx.inner.accessList() }

// Gas returns the gas limit of the transaction.
func (tx *Transaction) Gas() uint64 { return tx.inner.gas() }

// GasPrice returns the gas price of the transaction.
func (tx *Transaction) GasPrice() *big.Int { return new(big.Int).Set(tx.inner.gasPrice()) }

// GasTipCap returns the gasTipCap per gas of the transaction.
func (tx *Transaction) GasTipCap() *big.Int { return new(big.Int).Set(tx.inner.gasTipCap()) }

// GasFeeCap returns the fee cap per gas of the transaction.
func (tx *Transaction) GasFeeCap() *big.Int { return new(big.Int).Set(tx.inner.gasFeeCap()) }

// Value returns the ether amount of the transaction.
func (tx *Transaction) Value() *big.Int { return new(big.Int).Set(tx.inner.value()) }

// Nonce returns the sender account nonce of the transaction.
func (tx *Transaction) Nonce() uint64 { return tx.inner.nonce() }

// To returns the recipient address of the transaction.
// For contract-creation transactions, To returns nil.
func (tx *Transaction) To() *common.Address {
	return copyAddressPtr(tx.inner.to())
}

// Cost returns gas * gasPrice + value.
func (tx *Transaction) Cost() *big.Int {
	total := new(big.Int).Mul(tx.GasPrice(), new(big.Int).SetUint64(tx.Gas()))
	total.Add(total, tx.Value())
	return total
}

// RawSignatureValues returns the V, R, S signature values of the transaction.
// The return values should not be modified by the caller.
func (tx *Transaction) RawSignatureValues() (v, r, s *big.Int) {
	return tx.inner.rawSignatureValues()
}

// GasFeeCapCmp compares the fee cap of two transactions.
func (tx *Transaction) GasFeeCapCmp(other *Transaction) int {
	return tx.inner.gasFeeCap().Cmp(other.inner.gasFeeCap())
}

// GasFeeCapIntCmp compares the fee cap of the transaction against the given fee cap.
func (tx *Transaction) GasFeeCapIntCmp(other *big.Int) int {
	return tx.inner.gasFeeCap().Cmp(other)
}

// GasTipCapCmp compares the gasTipCap of two transactions.
func (tx *Transaction) GasTipCapCmp(other *Transaction) int {
	return tx.inner.gasTipCap().Cmp(other.inner.gasTipCap())
}

// GasTipCapIntCmp compares the gasTipCap of the transaction against the given gasTipCap.
func (tx *Transaction) GasTipCapIntCmp(other *big.Int) int {
	return tx.inner.gasTipCap().Cmp(other)
}

// EffectiveGasTip returns the effective miner gasTipCap for the given base fee.
// Note: if the effective gasTipCap is negative, this method returns both error
// the actual negative value, _and_ ErrGasFeeCapTooLow
func (tx *Transaction) EffectiveGasTip(baseFee *big.Int) (*big.Int, error) {
	if baseFee == nil {
		return tx.GasTipCap(), nil
	}
	var err error
	gasFeeCap := tx.GasFeeCap()
	if gasFeeCap.Cmp(baseFee) == -1 {
		err = ErrGasFeeCapTooLow
	}
	return math.BigMin(tx.GasTipCap(), gasFeeCap.Sub(gasFeeCap, baseFee)), err
}

// EffectiveGasTipValue is identical to EffectiveGasTip, but does not return an
// error in case the effective gasTipCap is negative
func (tx *Transaction) EffectiveGasTipValue(baseFee *big.Int) *big.Int {
	effectiveTip, _ := tx.EffectiveGasTip(baseFee)
	return effectiveTip
}

// EffectiveGasTipCmp compares the effective gasTipCap of two transactions assuming the given base fee.
func (tx *Transaction) EffectiveGasTipCmp(other *Transaction, baseFee *big.Int) int {
	if baseFee == nil {
		return tx.GasTipCapCmp(other)
	}
	return tx.EffectiveGasTipValue(baseFee).Cmp(other.EffectiveGasTipValue(baseFee))
}

// EffectiveGasTipIntCmp compares the effective gasTipCap of a transaction to the given gasTipCap.
func (tx *Transaction) EffectiveGasTipIntCmp(other *big.Int, baseFee *big.Int) int {
	if baseFee == nil {
		return tx.GasTipCapIntCmp(other)
	}
	return tx.EffectiveGasTipValue(baseFee).Cmp(other)
}

// Hash returns the transaction hash.
func (tx *Transaction) Hash() common.Hash {
	if hash := tx.hash.Load(); hash != nil {
		return hash.(common.Hash)
	}

	var h common.Hash
	if tx.Type() == LegacyTxType {
		h = rlpHash(tx.inner)
	} else {
		h = prefixedRlpHash(tx.Type(), tx.inner)
	}
	tx.hash.Store(h)
	return h
}

// Size returns the true encoded storage size of the transaction, either by encoding
// and returning it, or returning a previously cached value.
func (tx *Transaction) Size() uint64 {
	if size := tx.size.Load(); size != nil {
		return size.(uint64)
	}
	c := writeCounter(0)
	rlp.Encode(&c, &tx.inner)

	size := uint64(c)
	if tx.Type() != LegacyTxType {
		size += 1 // type byte
	}
	tx.size.Store(size)
	return size
}

// WithSignature returns a new transaction with the given signature.
// This signature needs to be in the [R || S || V] format where V is 0 or 1.
func (tx *Transaction) WithSignature(signer Signer, sig []byte) (*Transaction, error) {
	r, s, v, err := signer.SignatureValues(tx, sig)
	if err != nil {
		return nil, err
	}
	cpy := tx.inner.copy()
	cpy.setSignatureValues(signer.ChainID(), v, r, s)
	return &Transaction{inner: cpy, time: tx.time}, nil
}

// Transactions implements DerivableList for transactions.
type Transactions []*Transaction

// Len returns the length of s.
func (s Transactions) Len() int { return len(s) }

// EncodeIndex encodes the i'th transaction to w. Note that this does not check for errors
// because we assume that *Transaction will only ever contain valid txs that were either
// constructed by decoding or via public API in this package.
func (s Transactions) EncodeIndex(i int, w *bytes.Buffer) {
	tx := s[i]
	if tx.Type() == LegacyTxType {
		rlp.Encode(w, tx.inner)
	} else {
		tx.encodeTyped(w)
	}
}

// TxDifference returns a new set which is the difference between a and b.
func TxDifference(a, b Transactions) Transactions {
	keep := make(Transactions, 0, len(a))

	remove := make(map[common.Hash]struct{})
	for _, tx := range b {
		remove[tx.Hash()] = struct{}{}
	}

	for _, tx := range a {
		if _, ok := remove[tx.Hash()]; !ok {
			keep = append(keep, tx)
		}
	}

	return keep
}

// HashDifference returns a new set which is the difference between a and b.
func HashDifference(a, b []common.Hash) []common.Hash {
	keep := make([]common.Hash, 0, len(a))

	remove := make(map[common.Hash]struct{})
	for _, hash := range b {
		remove[hash] = struct{}{}
	}

	for _, hash := range a {
		if _, ok := remove[hash]; !ok {
			keep = append(keep, hash)
		}
	}

	return keep
}

// TxByNonce implements the sort interface to allow sorting a list of transactions
// by their nonces. This is usually only useful for sorting transactions from a
// single account, otherwise a nonce comparison doesn't make much sense.
type TxByNonce Transactions

func (s TxByNonce) Len() int           { return len(s) }
func (s TxByNonce) Less(i, j int) bool { return s[i].Nonce() < s[j].Nonce() }
func (s TxByNonce) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

type _Order interface {
	AsTx() *Transaction
	AsBundle() *SimulatedBundle
	AsSBundle() *SimSBundle
}

type _TxOrder struct {
	tx *Transaction
}

func (o _TxOrder) AsTx() *Transaction         { return o.tx }
func (o _TxOrder) AsBundle() *SimulatedBundle { return nil }
func (o _TxOrder) AsSBundle() *SimSBundle     { return nil }

type _BundleOrder struct {
	bundle *SimulatedBundle
}

func (o _BundleOrder) AsTx() *Transaction         { return nil }
func (o _BundleOrder) AsBundle() *SimulatedBundle { return o.bundle }
func (o _BundleOrder) AsSBundle() *SimSBundle     { return nil }

type _SBundleOrder struct {
	sbundle *SimSBundle
}

func (o _SBundleOrder) AsTx() *Transaction         { return nil }
func (o _SBundleOrder) AsBundle() *SimulatedBundle { return nil }
func (o _SBundleOrder) AsSBundle() *SimSBundle     { return o.sbundle }

// TxWithMinerFee wraps a transaction with its gas price or effective miner gasTipCap
type TxWithMinerFee struct {
	order    _Order
	minerFee *big.Int
}

func (t *TxWithMinerFee) Tx() *Transaction {
	return t.order.AsTx()
}

func (t *TxWithMinerFee) Bundle() *SimulatedBundle {
	return t.order.AsBundle()
}

func (t *TxWithMinerFee) SBundle() *SimSBundle {
	return t.order.AsSBundle()
}

func (t *TxWithMinerFee) Price() *big.Int {
	return new(big.Int).Set(t.minerFee)
}

func (t *TxWithMinerFee) Profit(baseFee *big.Int, gasUsed uint64) *big.Int {
	if tx := t.Tx(); tx != nil {
		profit := new(big.Int).Sub(tx.GasPrice(), baseFee)
		if gasUsed != 0 {
			profit.Mul(profit, new(big.Int).SetUint64(gasUsed))
		} else {
			profit.Mul(profit, new(big.Int).SetUint64(tx.Gas()))
		}
		return profit
	} else if bundle := t.Bundle(); bundle != nil {
		return bundle.EthSentToCoinbase
	} else if sbundle := t.SBundle(); sbundle != nil {
		return sbundle.Profit
	} else {
		panic("profit called on unsupported order type")
	}
}

// SetPrice sets the miner fee of the wrapped transaction.
func (t *TxWithMinerFee) SetPrice(price *big.Int) {
	t.minerFee.Set(price)
}

// SetProfit sets the profit of the wrapped transaction.
func (t *TxWithMinerFee) SetProfit(profit *big.Int) {
	if bundle := t.Bundle(); bundle != nil {
		bundle.TotalEth.Set(profit)
	} else if sbundle := t.SBundle(); sbundle != nil {
		sbundle.Profit.Set(profit)
	} else {
		panic("SetProfit called on unsupported order type")
	}
}

// NewTxWithMinerFee creates a wrapped transaction, calculating the effective
// miner gasTipCap if a base fee is provided.
// Returns error in case of a negative effective miner gasTipCap.
func NewTxWithMinerFee(tx *Transaction, baseFee *big.Int) (*TxWithMinerFee, error) {
	minerFee, err := tx.EffectiveGasTip(baseFee)
	if err != nil {
		return nil, err
	}
	return &TxWithMinerFee{
		order:    _TxOrder{tx},
		minerFee: minerFee,
	}, nil
}

// NewBundleWithMinerFee creates a wrapped bundle.
func NewBundleWithMinerFee(bundle *SimulatedBundle, _ *big.Int) (*TxWithMinerFee, error) {
	minerFee := bundle.MevGasPrice
	return &TxWithMinerFee{
		order:    _BundleOrder{bundle},
		minerFee: minerFee,
	}, nil
}

// NewSBundleWithMinerFee creates a wrapped bundle.
func NewSBundleWithMinerFee(sbundle *SimSBundle, _ *big.Int) (*TxWithMinerFee, error) {
	minerFee := sbundle.MevGasPrice
	return &TxWithMinerFee{
		order:    _SBundleOrder{sbundle},
		minerFee: minerFee,
	}, nil
}

// TxByPriceAndTime implements both the sort and the heap interface, making it useful
// for all at once sorting as well as individually adding and removing elements.
type TxByPriceAndTime []*TxWithMinerFee

func (s TxByPriceAndTime) Len() int { return len(s) }
func (s TxByPriceAndTime) Less(i, j int) bool {
	// If the prices are equal, use the time the transaction was first seen for
	// deterministic sorting
	cmp := s[i].minerFee.Cmp(s[j].minerFee)
	if cmp == 0 {
		if s[i].Tx() != nil && s[j].Tx() != nil {
			return s[i].Tx().time.Before(s[j].Tx().time)
		} else if s[i].Bundle() != nil && s[j].Bundle() != nil {
			return s[i].Bundle().TotalGasUsed <= s[j].Bundle().TotalGasUsed
		} else if s[i].Bundle() != nil {
			return false
		}

		return true
	}
	return cmp > 0
}
func (s TxByPriceAndTime) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s *TxByPriceAndTime) Push(x interface{}) {
	*s = append(*s, x.(*TxWithMinerFee))
}

func (s *TxByPriceAndTime) Pop() interface{} {
	old := *s
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	*s = old[0 : n-1]
	return x
}

// TransactionsByPriceAndNonce represents a set of transactions that can return
// transactions in a profit-maximizing sorted order, while supporting removing
// entire batches of transactions for non-executable accounts.
type TransactionsByPriceAndNonce struct {
	txs     map[common.Address]Transactions // Per account nonce-sorted list of transactions
	heads   TxByPriceAndTime                // Next transaction for each unique account (price heap)
	signer  Signer                          // Signer for the set of transactions
	baseFee *big.Int                        // Current base fee
}

// NewTransactionsByPriceAndNonce creates a transaction set that can retrieve
// price sorted transactions in a nonce-honouring way.
//
// Note, the input map is reowned so the caller should not interact any more with
// if after providing it to the constructor.
func NewTransactionsByPriceAndNonce(signer Signer, txs map[common.Address]Transactions, bundles []SimulatedBundle, sbundles []*SimSBundle, baseFee *big.Int) *TransactionsByPriceAndNonce {
	// Initialize a price and received time based heap with the head transactions
	heads := make(TxByPriceAndTime, 0, len(txs)+len(bundles)+len(sbundles))

	for i := range sbundles {
		wrapped, err := NewSBundleWithMinerFee(sbundles[i], baseFee)
		if err != nil {
			continue
		}
		heads = append(heads, wrapped)
	}

	for i := range bundles {
		wrapped, err := NewBundleWithMinerFee(&bundles[i], baseFee)
		if err != nil {
			continue
		}
		heads = append(heads, wrapped)
	}

	for from, accTxs := range txs {
		acc, _ := Sender(signer, accTxs[0])
		wrapped, err := NewTxWithMinerFee(accTxs[0], baseFee)
		// Remove transaction if sender doesn't match from, or if wrapping fails.
		if acc != from || err != nil {
			delete(txs, from)
			continue
		}
		heads = append(heads, wrapped)
		txs[from] = accTxs[1:]
	}
	heap.Init(&heads)
	for _, h := range heads {
		if h.Tx() != nil {
			log.Info(h.Tx().Hash().String())
			log.Info(h.minerFee.String())
			log.Info("==============")
		}
		if h.Bundle() != nil {
			for _, t := range h.Bundle().OriginalBundle.Txs {
				log.Info(t.Hash().String())
			}
			log.Info(h.minerFee.String())
			log.Info("==============")
		}

	}
	// Assemble and return the transaction set
	return &TransactionsByPriceAndNonce{
		txs:     txs,
		heads:   heads,
		signer:  signer,
		baseFee: baseFee,
	}
}

func (t *TransactionsByPriceAndNonce) DeepCopy() *TransactionsByPriceAndNonce {
	newT := &TransactionsByPriceAndNonce{
		txs:     make(map[common.Address]Transactions),
		heads:   append(TxByPriceAndTime{}, t.heads...),
		signer:  t.signer,
		baseFee: new(big.Int).Set(t.baseFee),
	}
	for k, v := range t.txs {
		newT.txs[k] = v
	}
	return newT
}

// Peek returns the next transaction by price.
func (t *TransactionsByPriceAndNonce) Peek() *TxWithMinerFee {
	if len(t.heads) == 0 {
		return nil
	}
	return t.heads[0]
}

// Shift replaces the current best head with the next one from the same account.
func (t *TransactionsByPriceAndNonce) Shift() {
	if tx := t.heads[0].Tx(); tx != nil {
		acc, _ := Sender(t.signer, tx)
		if txs, ok := t.txs[acc]; ok && len(txs) > 0 {
			if wrapped, err := NewTxWithMinerFee(txs[0], t.baseFee); err == nil {
				t.heads[0], t.txs[acc] = wrapped, txs[1:]
				heap.Fix(&t.heads, 0)
				return
			}
		}
	}
	heap.Pop(&t.heads)
}

// ShiftAndPushByAccountForTx attempts to update the transaction list associated with a given account address
// based on the input transaction account. If the associated account exists and has additional transactions,
// the top of the transaction list is popped and pushed to the heap.
// Note that this operation should only be performed when the head transaction on the heap is different from the
// input transaction. This operation is useful in scenarios where the current best head transaction for an account
// was already popped from the heap and we want to process the next one from the same account.
func (t *TransactionsByPriceAndNonce) ShiftAndPushByAccountForTx(tx *Transaction) {
	if tx == nil {
		return
	}

	acc, _ := Sender(t.signer, tx)
	if txs, exists := t.txs[acc]; exists && len(txs) > 0 {
		if wrapped, err := NewTxWithMinerFee(txs[0], t.baseFee); err == nil {
			t.txs[acc] = txs[1:]
			heap.Push(&t.heads, wrapped)
		}
	}
}

func (t *TransactionsByPriceAndNonce) Push(tx *TxWithMinerFee) {
	if tx == nil {
		return
	}

	heap.Push(&t.heads, tx)
}

// Pop removes the best transaction, *not* replacing it with the next one from
// the same account. This should be used when a transaction cannot be executed
// and hence all subsequent ones should be discarded from the same account.
func (t *TransactionsByPriceAndNonce) Pop() {
	heap.Pop(&t.heads)
}

// copyAddressPtr copies an address.
func copyAddressPtr(a *common.Address) *common.Address {
	if a == nil {
		return nil
	}
	cpy := *a
	return &cpy
}

var EmptyUUID uuid.UUID

type LatestUuidBundle struct {
	Uuid           uuid.UUID
	SigningAddress common.Address
	BundleHash     common.Hash
	BundleUUID     uuid.UUID
}

type MevBundle struct {
	Txs               Transactions
	BlockNumber       *big.Int
	Uuid              uuid.UUID
	SigningAddress    common.Address
	MinTimestamp      uint64
	MaxTimestamp      uint64
	RevertingTxHashes []common.Hash
	Hash              common.Hash
}

func (b *MevBundle) UniquePayload() []byte {
	var buf []byte
	buf = binary.AppendVarint(buf, b.BlockNumber.Int64())
	buf = append(buf, b.Hash[:]...)
	sort.Slice(b.RevertingTxHashes, func(i, j int) bool {
		return bytes.Compare(b.RevertingTxHashes[i][:], b.RevertingTxHashes[j][:]) <= 0
	})
	for _, txHash := range b.RevertingTxHashes {
		buf = append(buf, txHash[:]...)
	}
	return buf
}

func (b *MevBundle) ComputeUUID() uuid.UUID {
	return uuid.NewHash(sha256.New(), uuid.Nil, b.UniquePayload(), 5)
}

func (b *MevBundle) RevertingHash(hash common.Hash) bool {
	for _, revHash := range b.RevertingTxHashes {
		if revHash == hash {
			return true
		}
	}
	return false
}

type SimulatedBundle struct {
	MevGasPrice       *big.Int
	TotalEth          *big.Int
	EthSentToCoinbase *big.Int
	TotalGasUsed      uint64
	OriginalBundle    MevBundle
}
