package chaincode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-protos-go/common"
	fabricPeer "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/msp"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"

	"github.com/bogatyr285/hlf-sdk-go/api"
	"github.com/bogatyr285/hlf-sdk-go/client/chaincode/txwaiter"
	"github.com/bogatyr285/hlf-sdk-go/peer"
)

type invokeBuilder struct {
	ccCore        *Core
	fn            string
	processor     api.PeerProcessor
	peerPool      api.PeerPool
	identity      msp.SigningIdentity
	txWaiter      api.TxWaiter
	args          [][]byte
	transientArgs api.TransArgs
	err           *errArgMap
}

// A string that might be shortened to a specified length.
type TruncatableString struct {
	// The shortened string. For example, if the original string was 500 bytes long and
	// the limit of the string was 128 bytes, then this value contains the first 128
	// bytes of the 500-byte string. Note that truncation always happens on a
	// character boundary, to ensure that a truncated string is still valid UTF-8.
	// Because it may contain multi-byte characters, the size of the truncated string
	// may be less than the truncation limit.
	Value string

	// The number of bytes removed from the original string. If this
	// value is 0, then the string was not shortened.
	TruncatedByteCount int
}

func (t TruncatableString) String() string {
	if t.TruncatedByteCount == 0 {
		return t.Value
	}

	return fmt.Sprintf("%s(%d)", t.Value, t.TruncatedByteCount)
}

func makeTruncatableString(str string, size int) TruncatableString {
	if len(str) <= size {
		return TruncatableString{
			Value:              str,
			TruncatedByteCount: 0,
		}
	} else {
		return TruncatableString{
			Value:              str[0:size] + `...`,
			TruncatedByteCount: len(str[size:]),
		}
	}
}

func newErrArgMap() *errArgMap {
	return &errArgMap{
		container: make(map[TruncatableString]error),
	}
}

type errArgMap struct {
	// slice of part of arg...
	container map[TruncatableString]error
}

func (e *errArgMap) Add(arg interface{}, err error) {
	e.container[makeTruncatableString(fmt.Sprintf("%#v", arg), 50)] = err
}

func (e *errArgMap) Err() error {
	if len(e.container) == 0 {
		return nil
	}

	buff := bytes.NewBuffer(nil)
	for key, err := range e.container {
		buff.WriteString(errors.Wrap(err, key.String()).Error() + "\n")
	}
	return errors.New(buff.String())
}

func (b *invokeBuilder) WithIdentity(identity msp.SigningIdentity) api.ChaincodeInvokeBuilder {
	b.identity = identity
	return b
}

func (b *invokeBuilder) ArgBytes(args [][]byte) api.ChaincodeInvokeBuilder {
	b.args = args
	return b
}

func (b *invokeBuilder) Transient(args api.TransArgs) api.ChaincodeInvokeBuilder {
	b.transientArgs = args
	return b
}

func (b *invokeBuilder) getTransaction(proposal *fabricPeer.SignedProposal, peerResponses []*fabricPeer.ProposalResponse) (*common.Envelope, error) {

	prop := new(fabricPeer.Proposal)

	if err := proto.Unmarshal(proposal.ProposalBytes, prop); err != nil {
		return nil, errors.Wrap(err, `failed to unmarshal `)
	}

	return protoutil.CreateSignedTx(prop, b.identity, peerResponses...)
}

func (b *invokeBuilder) ArgJSON(in ...interface{}) api.ChaincodeInvokeBuilder {
	argBytes := make([][]byte, 0)
	for _, arg := range in {
		if data, err := json.Marshal(arg); err != nil {
			b.err.Add(arg, err)
		} else {
			argBytes = append(argBytes, data)
		}
	}
	return b.ArgBytes(argBytes)
}

func (b *invokeBuilder) ArgString(args ...string) api.ChaincodeInvokeBuilder {
	return b.ArgBytes(argsToBytes(args...))
}

func (b *invokeBuilder) Do(ctx context.Context, options ...api.DoOption) (*fabricPeer.Response, api.ChaincodeTx, error) {
	err := b.err.Err()
	if err != nil {
		return nil, ``, err
	}

	ccd, err := b.ccCore.dp.Chaincode(ctx, b.ccCore.channelName, b.ccCore.name)
	if err != nil {
		return nil, ``, errors.Wrap(err, `failed to get chaincode definition`)
	}

	endorsingMspIDs := getEndorsingMSPs(ccd)

	doOpts := &api.DoOptions{
		Identity:        b.identity,
		Pool:            b.peerPool,
		EndorsingMspIDs: endorsingMspIDs,
	}
	// set default options
	if len(options) == 0 {
		options = append(options, WithTxWaiter(txwaiter.Self))
	}

	for _, applyOpt := range options {
		if err := applyOpt(doOpts); err != nil {
			return nil, ``, err
		}
	}
	b.txWaiter = doOpts.TxWaiter

	proposal, tx, err := b.processor.CreateProposal(ccd.ChaincodeName(), b.identity, b.fn, b.args, b.transientArgs)
	if err != nil {
		return nil, ``, errors.Wrap(err, `failed to get signed proposal`)
	}

	peerResponses, err := b.processor.Send(ctx, proposal, endorsingMspIDs, b.peerPool)
	if err != nil {
		return nil, tx, errors.Wrap(err, `failed to collect peer responses`)
	}

	envelope, err := b.getTransaction(proposal, peerResponses)
	if err != nil {
		return nil, tx, errors.Wrap(err, `failed to get envelope`)
	}

	_, err = b.ccCore.orderer.Broadcast(ctx, envelope)
	if err != nil {
		return nil, tx, errors.Wrap(err, `failed to get orderer response`)
	}

	if err = b.txWaiter.Wait(ctx, b.ccCore.channelName, tx); err != nil {
		return nil, tx, err
	}

	return peerResponses[0].Response, tx, nil
}

func getEndorsingMSPs(d api.ChaincodeDiscoverer) (endorsingMspIDs []string) {
	endorsers := d.Endorsers()
	for i := range endorsers {
		endorsingMspIDs = append(endorsingMspIDs, endorsers[i].MspID)
	}
	return endorsingMspIDs
}

func NewInvokeBuilder(ccCore *Core, fn string) api.ChaincodeInvokeBuilder {
	processor := peer.NewProcessor(ccCore.channelName)
	return &invokeBuilder{
		ccCore:    ccCore,
		peerPool:  ccCore.peerPool,
		fn:        fn,
		processor: processor,
		identity:  ccCore.identity,

		err: newErrArgMap(),
	}
}
