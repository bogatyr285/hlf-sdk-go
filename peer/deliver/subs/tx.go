package subs

import (
	"github.com/bogatyr285/hlf-sdk-go/util/txflags"
	"github.com/pkg/errors"

	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/protoutil"

	"github.com/bogatyr285/hlf-sdk-go/api"
)

func NewTxSubscription(txId api.ChaincodeTx) *TxSubscription {
	return &TxSubscription{
		txId:   txId,
		result: make(chan *result, 1),
	}
}

type result struct {
	code peer.TxValidationCode
	err  error
}

type TxSubscription struct {
	txId   api.ChaincodeTx
	result chan *result
	ErrorCloser
}

func (ts *TxSubscription) Serve(sub ErrorCloser, readyForHandling ReadyForHandling) *TxSubscription {
	ts.ErrorCloser = sub
	readyForHandling()
	return ts
}

func (ts *TxSubscription) Result() (peer.TxValidationCode, error) {
	select {
	case r, ok := <-ts.result:
		if !ok {
			return -1, errors.New(`code is closed`)
		}
		return r.code, r.err
	case err, ok := <-ts.Err():
		if !ok {
			// NOTE: sometime error can be closed early thet result
			select {
			case r, ok := <-ts.result:
				if !ok {
					return -1, errors.New(`code is closed`)
				}
				return r.code, r.err
			default:
				return -1, errors.New(`err is closed`)
			}
		}
		return -1, err
	}
}

func (ts *TxSubscription) Handler(block *common.Block) bool {
	if block == nil {
		close(ts.result)
		return false
	}
	txFilter := txflags.ValidationFlags(
		block.GetMetadata().GetMetadata()[common.BlockMetadataIndex_TRANSACTIONS_FILTER],
	)

	for i, r := range block.GetData().GetData() {
		env, err := protoutil.GetEnvelopeFromBlock(r)
		if err != nil {
			ts.result <- &result{code: 0, err: err}
			return true
		}

		p, err := protoutil.UnmarshalPayload(env.Payload)
		if err != nil {
			ts.result <- &result{code: 0, err: err}
			return true
		}

		chHeader, err := protoutil.UnmarshalChannelHeader(p.Header.ChannelHeader)
		if err != nil {
			ts.result <- &result{code: 0, err: err}
			return true
		}

		//println("TXID", chHeader.TxId, txFilter.IsValid(i))
		if api.ChaincodeTx(chHeader.TxId) == ts.txId {
			//defer ts.ErrorCloser.Close()
			if txFilter.IsValid(i) {
				ts.result <- &result{code: txFilter.Flag(i), err: nil}
				return true
			} else {
				err = errors.Errorf("TxId validation code failed: %s", peer.TxValidationCode_name[int32(txFilter.Flag(i))])
				ts.result <- &result{code: txFilter.Flag(i), err: err}
				return true
			}
		}
	}

	return false
}
