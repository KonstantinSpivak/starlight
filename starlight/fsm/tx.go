package fsm

import (
	"bytes"

	b "github.com/stellar/go/build"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/xdr"

	"github.com/interstellar/starlight/errors"
	"github.com/interstellar/starlight/math/checked"
	"github.com/interstellar/starlight/starlight/key"
	"github.com/interstellar/starlight/starlight/log"
	"github.com/interstellar/starlight/worizon"
	"github.com/interstellar/starlight/worizon/xlm"
)

var txHandlerFuncs = []func(*Updater, *worizon.Tx, bool) (bool, error){
	handleCoopCloseTx,
	handleSettleCleanupTx,
	handleFundingTx,
	handleRatchetTx,
	handleSettleWithGuestTx,
	handleSettleWithHostTx,
	handleSetupAccountTx,
	handleTopUpTx,
}

func handleSetupAccountTx(u *Updater, tx *worizon.Tx, success bool) (bool, error) {
	// ignore the first two SetupAccountTxs
	if txMatches(tx, u.C.HostAcct,
		createAccountOp(u.C.HostAcct, u.C.HostRatchetAcct, xlm.Lumen),
	) || txMatches(tx, u.C.HostAcct,
		createAccountOp(u.C.HostAcct, u.C.GuestRatchetAcct, xlm.Lumen),
	) {
		return true, nil
	}
	if !txMatches(tx, u.C.HostAcct,
		createAccountOp(u.C.HostAcct, u.C.EscrowAcct, xlm.Lumen),
	) {
		return false, nil
	}

	if !success {
		// unreserve the HostAccount's one lumen
		u.H.NativeBalance += xlm.Lumen
	}

	if u.C.Role == Guest && u.C.State == AwaitingFunding {
		return true, nil
	}

	if u.C.State != SettingUp {
		return true, errors.Wrapf(ErrUnexpectedState, "got %s, want %s", u.C.State, SettingUp)
	}

	// compute the initial sequence number of the account
	// it's the ledger number of the transaction that created it, shifted left 32 bits
	u.C.BaseSequenceNumber = xdr.SequenceNumber(uint64(tx.LedgerNum) << 32)
	u.C.FundingTime = tx.LedgerTime
	u.C.PaymentTime = tx.LedgerTime
	err := u.transitionTo(ChannelProposed)
	return true, err
}

var (
	zero xdr.Uint32
	two  xdr.Uint32 = 2
)

// MatchesFundingTx reports whether a transaction is the funding transaction for the channel.
func MatchesFundingTx(c *Channel, tx *worizon.Tx) bool {
	return txMatches(tx, c.HostAcct,
		paymentOp(c.HostAcct, c.EscrowAcct, c.HostAmount+500*xlm.Millilumen+8*c.ChannelFeerate),
		xdr.Operation{
			SourceAccount: c.EscrowAcct.XDR(),
			Body: xdr.OperationBody{
				Type: xdr.OperationTypeSetOptions,
				SetOptionsOp: &xdr.SetOptionsOp{
					LowThreshold:  &two,
					MedThreshold:  &two,
					HighThreshold: &two,
					Signer: &xdr.Signer{
						Key: xdr.SignerKey{
							Type:    xdr.SignerKeyTypeSignerKeyTypeEd25519,
							Ed25519: c.GuestAcct.Ed25519,
						},
						Weight: 1,
					},
				},
			},
		},
		paymentOp(c.HostAcct, c.GuestRatchetAcct, xlm.Lumen+c.ChannelFeerate),
		xdr.Operation{
			SourceAccount: c.GuestRatchetAcct.XDR(),
			Body: xdr.OperationBody{
				Type: xdr.OperationTypeSetOptions,
				SetOptionsOp: &xdr.SetOptionsOp{
					MasterWeight:  &zero,
					LowThreshold:  &two,
					MedThreshold:  &two,
					HighThreshold: &two,
					Signer: &xdr.Signer{
						Key: xdr.SignerKey{
							Type:    xdr.SignerKeyTypeSignerKeyTypeEd25519,
							Ed25519: c.GuestAcct.Ed25519,
						},
						Weight: 1,
					},
				},
			},
		},
		xdr.Operation{
			SourceAccount: c.GuestRatchetAcct.XDR(),
			Body: xdr.OperationBody{
				Type: xdr.OperationTypeSetOptions,
				SetOptionsOp: &xdr.SetOptionsOp{
					Signer: &xdr.Signer{
						Key: xdr.SignerKey{
							Type:    xdr.SignerKeyTypeSignerKeyTypeEd25519,
							Ed25519: c.EscrowAcct.Ed25519,
						},
						Weight: 1,
					},
				},
			},
		},
		paymentOp(c.HostAcct, c.HostRatchetAcct, 500*xlm.Millilumen+c.ChannelFeerate),
		xdr.Operation{
			SourceAccount: c.HostRatchetAcct.XDR(),
			Body: xdr.OperationBody{
				Type: xdr.OperationTypeSetOptions,
				SetOptionsOp: &xdr.SetOptionsOp{
					MasterWeight: &zero,
					Signer: &xdr.Signer{
						Key: xdr.SignerKey{
							Type:    xdr.SignerKeyTypeSignerKeyTypeEd25519,
							Ed25519: c.EscrowAcct.Ed25519,
						},
						Weight: 1,
					},
				},
			},
		},
	)
}

func handleFundingTx(u *Updater, tx *worizon.Tx, success bool) (bool, error) {
	if !MatchesFundingTx(u.C, tx) {
		return false, nil
	}
	if u.C.State != AwaitingFunding {
		return false, errors.Wrapf(ErrUnexpectedState, "got %s, want %s", u.C.State, AwaitingFunding)
	}
	if !success {
		if u.C.Role == Host {
			// Host gets back total funding tx-related amount.
			u.H.NativeBalance += u.C.totalFundingTxAmount()
			u.H.Seqnum++
			err := u.transitionTo(AwaitingCleanup)
			return true, err
		}
		err := u.transitionTo(Closed)
		return true, err
	}
	err := u.transitionTo(Open)
	return true, err
}

func handleCoopCloseTx(u *Updater, tx *worizon.Tx, success bool) (bool, error) {
	// if guest has 0 balance,
	// the coop close is matched by handleSettleWithHostTx
	if !txMatches(tx, u.C.EscrowAcct,
		paymentOp(u.C.EscrowAcct, u.C.GuestAcct, u.C.GuestAmount),
		mergeOp(u.C.EscrowAcct, u.C.HostAcct),
		mergeOp(u.C.GuestRatchetAcct, u.C.HostAcct),
		mergeOp(u.C.HostRatchetAcct, u.C.HostAcct),
	) {
		return false, nil
	}
	if u.C.State != AwaitingClose {
		return false, errors.Wrapf(ErrUnexpectedState, "got %s, want %s", u.C.State, AwaitingClose)
	}
	if !success {
		err := u.setForceCloseState()
		return true, err
	}
	err := u.transitionTo(Closed)
	return true, err
}

func handleRatchetTx(u *Updater, ptx *worizon.Tx, success bool) (bool, error) {
	tx := ptx.Env.Tx

	for _, role := range []Role{Host, Guest} {
		var ratchetAcct AccountID
		switch role {
		case Host:
			ratchetAcct = u.C.HostRatchetAcct
		case Guest:
			ratchetAcct = u.C.GuestRatchetAcct
		}

		if !xdrEqual(tx.SourceAccount, xdr.AccountId(ratchetAcct)) {
			continue
		}
		if len(tx.Operations) != 1 {
			continue
		}
		op := tx.Operations[0]
		if op.Body.Type != xdr.OperationTypeBumpSequence {
			continue
		}
		if !xdrEqual(*op.SourceAccount, xdr.AccountId(u.C.EscrowAcct)) {
			continue
		}

		// It's a ratchet tx.

		if !success {
			// It's my ratchet tx, since we can only detect tx failures for transactions that we submit.
			// TODO(vniu): add more detailed failure handling for different error cases, such as bump sequence target too low.
			u.transitionTo(Closed)
			log.Infof("unrecoverable failure on submitted ratchet tx, channel %s: closing channel immediately and abandoning balance", string(u.C.ID))
			return true, nil
		}

		if u.C.Role == role {
			err := u.transitionTo(AwaitingSettlementMintime)
			return true, err
		}

		bumpTo := op.Body.BumpSequenceOp.BumpTo
		switch {
		case bumpTo < u.C.roundSeqNum()+1:
			// Their ratchet tx is outdated.
			err := u.setForceCloseState()
			return true, err

		case bumpTo > u.C.roundSeqNum()+1:
			// Their ratchet tx is newer than expected.
			u.C.CurrentSettleWithGuestTx = u.C.CounterpartyLatestSettleWithGuestTx
			u.C.CurrentSettleWithHostTx = u.C.CounterpartyLatestSettleWithHostTx
			switch u.C.Role {
			case Guest:
				u.C.GuestAmount = u.C.GuestAmount + u.C.PendingAmountReceived - u.C.PendingAmountSent
				u.C.HostAmount = u.C.HostAmount - u.C.PendingAmountReceived + u.C.PendingAmountSent
			case Host:
				u.C.GuestAmount = u.C.GuestAmount - u.C.PendingAmountReceived + u.C.PendingAmountSent
				u.C.HostAmount = u.C.HostAmount + u.C.PendingAmountReceived - u.C.PendingAmountSent
			}
			u.C.RoundNumber++
			err := u.transitionTo(AwaitingSettlementMintime)
			return true, err

		default:
			if u.C.Role == Guest && u.C.GuestAmount == 0 {
				err := u.setForceCloseState()
				return true, err
			}
			// Their ratchet tx is juuuust right.
			err := u.transitionTo(AwaitingSettlementMintime)
			return true, err
		}
	}
	return false, nil
}

func handleSettleWithGuestTx(u *Updater, ptx *worizon.Tx, _ bool) (bool, error) {
	tx := ptx.Env.Tx

	if !xdrEqual(tx.SourceAccount, xdr.AccountId(u.C.EscrowAcct)) {
		return false, nil
	}
	if len(tx.Operations) != 1 {
		return false, nil
	}
	op := tx.Operations[0]
	if op.Body.Type != xdr.OperationTypePayment {
		return false, nil
	}
	if !txOpHasSrc(tx, op, xdr.AccountId(u.C.EscrowAcct)) {
		return false, nil
	}
	if !xdrEqual(op.Body.PaymentOp.Destination, xdr.AccountId(u.C.GuestAcct)) {
		return false, nil
	}
	if op.Body.PaymentOp.Asset.Type != xdr.AssetTypeAssetTypeNative {
		return false, nil
	}
	// skip checking the amount
	if u.C.State != AwaitingSettlement {
		return false, errors.Wrapf(ErrUnexpectedState, "got %s, want %s", u.C.State, AwaitingSettlement)
	}
	// stay in AwaitingSettlement state
	return true, nil
}

// also handles SettleRound1Tx
func handleSettleWithHostTx(u *Updater, tx *worizon.Tx, _ bool) (bool, error) {
	if !txMatches(tx, u.C.EscrowAcct,
		mergeOp(u.C.EscrowAcct, u.C.HostAcct),
		mergeOp(u.C.GuestRatchetAcct, u.C.HostAcct),
		mergeOp(u.C.HostRatchetAcct, u.C.HostAcct),
	) {
		return false, nil
	}
	err := u.transitionTo(Closed)
	return true, err
}

func handleSettleCleanupTx(u *Updater, tx *worizon.Tx, _ bool) (bool, error) {
	if !txMatches(tx, u.C.HostAcct,
		mergeOp(u.C.EscrowAcct, u.C.HostAcct),
		mergeOp(u.C.HostRatchetAcct, u.C.HostAcct),
		mergeOp(u.C.GuestRatchetAcct, u.C.HostAcct),
	) {
		return false, nil
	}
	err := u.transitionTo(Closed)
	return true, err
}

// this one's different: checks for any and all payment ops in the tx to the escrow acct
func handleTopUpTx(u *Updater, ptx *worizon.Tx, success bool) (bool, error) {
	tx := ptx.Env.Tx
	var amt int64
	for index, op := range tx.Operations {
		switch op.Body.Type {
		case xdr.OperationTypePayment:
			payOp := op.Body.PaymentOp
			if !xdrEqual(payOp.Destination, xdr.AccountId(u.C.EscrowAcct)) {
				continue
			}
			var ok bool
			amt, ok = checked.AddInt64(amt, int64(payOp.Amount))
			if !ok {
				return false, checked.ErrOverflow
			}
			if payOp.Asset.Type != xdr.AssetTypeAssetTypeNative {
				continue
			}
		case xdr.OperationTypeAccountMerge:
			if !xdrEqual(op.Body.Destination, xdr.AccountId(u.C.EscrowAcct)) {
				continue
			}
			var ok bool
			mergeAmount := *(*ptx.Result.Result.Results)[index].Tr.AccountMergeResult.SourceAccountBalance
			amt, ok = checked.AddInt64(amt, int64(mergeAmount))
			if !ok {
				return false, checked.ErrOverflow
			}
		default:
			continue
		}
	}
	if amt > 0 {
		// TODO(bobg): what if the expected top-up amount is split across multiple txs?
		if !success {
			return true, nil
		}
		if newAmt, ok := checked.AddInt64(int64(u.C.HostAmount), amt); ok {
			u.C.HostAmount = xlm.Amount(newAmt)
		} else {
			return false, checked.ErrOverflow
		}
		u.C.TopUpAmount = 0
		return true, nil
	}
	return false, nil
}

func txMatches(ptx *worizon.Tx, src AccountID, ops ...xdr.Operation) bool {
	tx := ptx.Env.Tx
	if len(tx.Operations) != len(ops) {
		return false
	}
	if !xdrEqual(tx.SourceAccount, xdr.AccountId(src)) {
		return false
	}
	for i, gotOp := range tx.Operations {
		wantOp := ops[i]
		if wantSrc := wantOp.SourceAccount; wantSrc != nil {
			if !txOpHasSrc(tx, gotOp, *wantSrc) {
				return false
			}
		}
		if !xdrEqual(gotOp.Body, wantOp.Body) {
			return false
		}
	}
	return true
}

func txOpHasSrc(tx xdr.Transaction, op xdr.Operation, wantSrc xdr.AccountId) bool {
	gotSrc := op.SourceAccount
	if gotSrc == nil {
		gotSrc = &tx.SourceAccount
	}
	return xdrEqual(*gotSrc, wantSrc)
}

func xdrEqual(a, b interface{}) bool {
	var abytes, bbytes bytes.Buffer

	_, err := xdr.Marshal(&abytes, a)
	if err != nil {
		return false
	}
	_, err = xdr.Marshal(&bbytes, b)
	if err != nil {
		return false
	}
	return bytes.Equal(abytes.Bytes(), bbytes.Bytes())
}

func createAccountOp(src, dest AccountID, bal xlm.Amount) xdr.Operation {
	return xdr.Operation{
		SourceAccount: src.XDR(),
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeCreateAccount,
			CreateAccountOp: &xdr.CreateAccountOp{
				Destination:     *dest.XDR(),
				StartingBalance: xdr.Int64(bal),
			},
		},
	}
}

func paymentOp(src, dest AccountID, amt xlm.Amount) xdr.Operation {
	return xdr.Operation{
		SourceAccount: src.XDR(),
		Body: xdr.OperationBody{
			Type: xdr.OperationTypePayment,
			PaymentOp: &xdr.PaymentOp{
				Destination: *dest.XDR(),
				Asset: xdr.Asset{
					Type: xdr.AssetTypeAssetTypeNative,
				},
				Amount: xdr.Int64(amt),
			},
		},
	}
}

func mergeOp(src, dest AccountID) xdr.Operation {
	return xdr.Operation{
		SourceAccount: src.XDR(),
		Body: xdr.OperationBody{
			Type:        xdr.OperationTypeAccountMerge,
			Destination: dest.XDR(),
		},
	}
}

func bumpSequenceOp(acct AccountID, bumpTo int64) xdr.Operation {
	return xdr.Operation{
		SourceAccount: acct.XDR(),
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeBumpSequence,
			BumpSequenceOp: &xdr.BumpSequenceOp{
				BumpTo: xdr.SequenceNumber(bumpTo),
			},
		},
	}
}

func verifySig(tx *b.TransactionBuilder, pubkey keypair.KP, signature xdr.DecoratedSignature) error {
	hash, err := tx.Hash()
	if err != nil {
		return err
	}
	return pubkey.Verify(hash[:], signature.Signature)
}

func txSig(tx *b.TransactionBuilder, seed []byte, indices ...uint32) (b.TransactionEnvelopeBuilder, error) {
	if seed == nil {
		return b.TransactionEnvelopeBuilder{}, errNoSeed
	}
	var secrets []string
	for _, index := range indices {
		secrets = append(secrets, key.DeriveAccount(seed, index).Seed())
	}
	return tx.Sign(secrets...)
}

func detachedSig(tx *xdr.Transaction, seed []byte, passphrase string, i uint32) (xdr.DecoratedSignature, error) {
	if seed == nil {
		return xdr.DecoratedSignature{}, errNoSeed
	}
	txhash, err := network.HashTransaction(tx, passphrase)
	if err != nil {
		return xdr.DecoratedSignature{}, err
	}
	kp := key.DeriveAccount(seed, i)
	return kp.SignDecorated(txhash[:])
}
