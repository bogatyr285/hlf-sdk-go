package util

import (
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric-protos-go/orderer"
	"github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/msp"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"

	"github.com/bogatyr285/hlf-sdk-go/crypto"
)

func SeekEnvelope(channelName string, startPos *orderer.SeekPosition, stopPos *orderer.SeekPosition, identity msp.SigningIdentity) (*common.Envelope, error) {
	creator, err := identity.Serialize()
	if err != nil {
		return nil, errors.Wrap(err, `failed to get creator`)
	}

	channelHeader, err := proto.Marshal(&common.ChannelHeader{
		Type:      int32(common.HeaderType_DELIVER_SEEK_INFO),
		Version:   0,
		Timestamp: util.CreateUtcTimestamp(),
		ChannelId: channelName,
		Epoch:     0,
	})
	if err != nil {
		return nil, errors.Wrap(err, `failed to marshal channel header`)
	}

	nonce, err := crypto.RandomBytes(24)
	if err != nil {
		return nil, errors.Wrap(err, `failed to get nonce`)
	}

	signatureHeader, err := proto.Marshal(&common.SignatureHeader{
		Creator: creator,
		Nonce:   nonce,
	})
	if err != nil {
		return nil, errors.Wrap(err, `failed to marshal signature header`)
	}

	seekData, err := proto.Marshal(&orderer.SeekInfo{
		Start:    startPos,
		Stop:     stopPos,
		Behavior: orderer.SeekInfo_BLOCK_UNTIL_READY,
	})
	if err != nil {
		return nil, errors.Wrap(err, `failed to marshal seek info`)
	}

	payload, err := proto.Marshal(&common.Payload{
		Header: &common.Header{ChannelHeader: channelHeader, SignatureHeader: signatureHeader},
		Data:   seekData,
	})
	if err != nil {
		return nil, errors.Wrap(err, `failed to marshal payload`)
	}

	payloadSignature, err := identity.Sign(payload)
	if err != nil {
		return nil, errors.Wrap(err, `failed to sign payload`)
	}

	return &common.Envelope{Payload: payload, Signature: payloadSignature}, nil
}

type ErrUnsupportedTxType struct {
	Type string
}

func (e *ErrUnsupportedTxType) Error() string {
	return fmt.Sprintf("err unknown tx type: %s", e.Type)
}

func IsErrUnsupportedTxType(err error) bool {
	switch err.(type) {
	case *ErrUnsupportedTxType:
		return true
	default:
		return false
	}
}

func GetEventFromEnvelope(envelopeData []byte) (*peer.ChaincodeEvent, error) {
	if envelopeData == nil {
		return nil, errors.New(`no envelope data`)
	}
	if envelope, err := protoutil.GetEnvelopeFromBlock(envelopeData); err != nil {
		return nil, errors.Wrap(err, `failed to get envelope`)
	} else {
		if payload, err := protoutil.UnmarshalPayload(envelope.Payload); err != nil {
			return nil, errors.Wrap(err, `failed to get payload from envelope`)
		} else {
			if channelHeader, err := protoutil.UnmarshalChannelHeader(payload.Header.ChannelHeader); err != nil {
				return nil, errors.Wrap(err, `failed to unmarshal channel header`)
			} else {
				switch common.HeaderType(channelHeader.Type) {
				case common.HeaderType_ENDORSER_TRANSACTION:
					if tx, err := protoutil.UnmarshalTransaction(payload.Data); err != nil {
						return nil, errors.Wrap(err, `failed to get transaction`)
					} else {
						if ccActionPayload, err := protoutil.UnmarshalChaincodeActionPayload(tx.Actions[0].Payload); err != nil {
							return nil, errors.Wrap(err, `failed to get chaincode action payload`)
						} else {
							if propRespPayload, err := protoutil.UnmarshalProposalResponsePayload(ccActionPayload.Action.ProposalResponsePayload); err != nil {
								return nil, errors.Wrap(err, `failed to get proposal response payload`)
							} else {
								if caPayload, err := protoutil.UnmarshalChaincodeAction(propRespPayload.Extension); err != nil {
									return nil, errors.Wrap(err, `failed to get chaincode action`)
								} else {
									if ccEvent, err := protoutil.UnmarshalChaincodeEvents(caPayload.Events); err != nil {
										return nil, errors.Wrap(err, `failed to get events`)
									} else {
										return ccEvent, nil
									}
								}
							}
						}
					}
				default:
					return nil, &ErrUnsupportedTxType{
						Type: common.HeaderType_name[channelHeader.Type],
					}
				}
			}
		}
	}
}
