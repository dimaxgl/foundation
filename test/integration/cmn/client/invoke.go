package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"

	pbfound "github.com/anoideaopen/foundation/proto"
	"github.com/anoideaopen/foundation/test/integration/cmn"
	"github.com/anoideaopen/foundation/test/integration/cmn/client/types"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/golang/protobuf/proto"
	cb "github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	pb "github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric/integration/nwo"
	"github.com/hyperledger/fabric/integration/nwo/commands"
	"github.com/hyperledger/fabric/protoutil"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

func sendTransactionToPeer(
	network *nwo.Network,
	peer *nwo.Peer,
	orderer *nwo.Orderer,
	userOrg string,
	channel string,
	ccName string,
	args ...string,
) (*gexec.Session, error) {
	return network.PeerUserSession(peer, userOrg, commands.ChaincodeInvoke{
		ChannelID: channel,
		Orderer:   network.OrdererAddress(orderer, nwo.ListenPort),
		Name:      ccName,
		Ctor:      cmn.CtorFromSlice(args),
		PeerAddresses: []string{
			network.PeerAddress(network.Peer("Org1", peer.Name), nwo.ListenPort),
			network.PeerAddress(network.Peer("Org2", peer.Name), nwo.ListenPort),
		},
		WaitForEvent: true,
	})
}

func invokeNBTx(
	network *nwo.Network,
	peer *nwo.Peer,
	orderer *nwo.Orderer,
	userOrg string,
	channel string,
	ccName string,
	args ...string,
) *types.InvokeResult {
	result := &types.InvokeResult{}
	sess, err := sendTransactionToPeer(network, peer, orderer, userOrg, channel, ccName, args...)
	Expect(sess).NotTo(BeNil())
	Eventually(sess, network.EventuallyTimeout).Should(gexec.Exit())
	result.SetResponse(sess.Out.Contents())
	result.SetMessage(sess.Err.Contents())
	result.SetErrorCode(int32(sess.ExitCode()))

	if err == nil {
		Eventually(sess, network.EventuallyTimeout).Should(gexec.Exit(0))
		Expect(sess.Err).To(gbytes.Say("Chaincode invoke successful. result: status:200"))
	}

	return result
}

func batchResponseProcess(response *pbfound.TxResponse, txID string, isValid bool) *types.InvokeResult {
	if hex.EncodeToString(response.GetId()) == txID {
		Expect(isValid).To(BeTrue())
		result := &types.InvokeResult{}
		result.SetTxID(txID)
		if response.GetError() != nil {
			result.SetMessage([]byte(response.GetError().GetError()))
			result.SetErrorCode(response.GetError().GetCode())
		}
		return result
	}

	return nil
}

func deliverResponseProcess(resp *pb.DeliverResponse, txID string) *types.InvokeResult {
	b, ok := resp.GetType().(*pb.DeliverResponse_Block)
	Expect(ok).To(BeTrue())

	txFilter := b.Block.GetMetadata().GetMetadata()[cb.BlockMetadataIndex_TRANSACTIONS_FILTER]
	for txIndex, ebytes := range b.Block.GetData().GetData() {
		var env *cb.Envelope

		if ebytes == nil {
			continue
		}

		isValid := true
		if len(txFilter) != 0 &&
			pb.TxValidationCode(txFilter[txIndex]) != pb.TxValidationCode_VALID {
			isValid = false
		}

		env, err := protoutil.GetEnvelopeFromBlock(ebytes)
		if err != nil {
			continue
		}

		// get the payload from the envelope
		payload, err := protoutil.UnmarshalPayload(env.GetPayload())
		Expect(err).NotTo(HaveOccurred())

		if payload.GetHeader() == nil {
			continue
		}

		chdr, err := protoutil.UnmarshalChannelHeader(payload.GetHeader().GetChannelHeader())
		Expect(err).NotTo(HaveOccurred())

		if cb.HeaderType(chdr.GetType()) != cb.HeaderType_ENDORSER_TRANSACTION {
			continue
		}

		tx, err := protoutil.UnmarshalTransaction(payload.GetData())
		Expect(err).NotTo(HaveOccurred())

		for _, action := range tx.GetActions() {
			chaincodeActionPayload, err := protoutil.UnmarshalChaincodeActionPayload(action.GetPayload())
			Expect(err).NotTo(HaveOccurred())

			if chaincodeActionPayload.GetAction() == nil {
				continue
			}

			propRespPayload, err := protoutil.UnmarshalProposalResponsePayload(chaincodeActionPayload.GetAction().GetProposalResponsePayload())
			Expect(err).NotTo(HaveOccurred())

			caPayload, err := protoutil.UnmarshalChaincodeAction(propRespPayload.GetExtension())
			Expect(err).NotTo(HaveOccurred())

			ccEvent, err := protoutil.UnmarshalChaincodeEvents(caPayload.GetEvents())
			Expect(err).NotTo(HaveOccurred())

			if ccEvent.GetEventName() == "batchExecute" {
				batchResponse := &pbfound.BatchResponse{}
				err = proto.Unmarshal(caPayload.GetResponse().GetPayload(), batchResponse)
				Expect(err).NotTo(HaveOccurred())

				for _, r := range batchResponse.GetTxResponses() {
					invokeResult := batchResponseProcess(r, txID, isValid)
					if invokeResult != nil {
						return invokeResult
					}
				}
			}
		}
	}

	return nil
}

func invokeTx(
	network *nwo.Network,
	peer *nwo.Peer,
	orderer *nwo.Orderer,
	userOrg string,
	channel string,
	ccName string,
	args ...string,
) *types.InvokeResult {
	lh := nwo.GetLedgerHeight(network, peer, channel)

	sess, err := sendTransactionToPeer(network, peer, orderer, userOrg, channel, ccName, args...)
	Expect(err).NotTo(HaveOccurred())
	Eventually(sess, network.EventuallyTimeout).Should(gexec.Exit(0))
	Expect(sess.Err).To(gbytes.Say("Chaincode invoke successful. result: status:200"))

	l := sess.Err.Contents()
	txID := scanTxIDInLog(l)
	Expect(txID).NotTo(BeEmpty())

	By("getting the signer for user1 on peer " + peer.ID())
	signer := network.PeerUserSigner(peer, "User1")

	By("creating the deliver client to peer " + peer.ID())
	pcc := network.PeerClientConn(peer)
	defer func() {
		err := pcc.Close()
		Expect(err).NotTo(HaveOccurred())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), network.EventuallyTimeout)
	defer cancel()
	dc, err := pb.NewDeliverClient(pcc).Deliver(ctx)
	Expect(err).NotTo(HaveOccurred())
	defer func() {
		err := dc.CloseSend()
		Expect(err).NotTo(HaveOccurred())
	}()

	By("starting filtered delivery on peer " + peer.ID())
	deliverEnvelope, err := protoutil.CreateSignedEnvelope(
		cb.HeaderType_DELIVER_SEEK_INFO,
		channel,
		signer,
		&ab.SeekInfo{
			Behavior: ab.SeekInfo_BLOCK_UNTIL_READY,
			Start: &ab.SeekPosition{
				Type: &ab.SeekPosition_Specified{
					Specified: &ab.SeekSpecified{Number: uint64(lh)},
				},
			},
			Stop: &ab.SeekPosition{
				Type: &ab.SeekPosition_Specified{
					Specified: &ab.SeekSpecified{Number: math.MaxUint64},
				},
			},
		},
		0,
		0,
	)
	Expect(err).NotTo(HaveOccurred())
	err = dc.Send(deliverEnvelope)
	Expect(err).NotTo(HaveOccurred())

	By("waiting for deliver event on peer " + peer.ID())
	for {
		resp, err := dc.Recv()
		Expect(err).NotTo(HaveOccurred())

		invokeResult := deliverResponseProcess(resp, txID)
		if invokeResult != nil {
			return invokeResult
		}
	}
}

func scanTxIDInLog(data []byte) string {
	// find: txid [......] committed with status
	re := regexp.MustCompile(fmt.Sprintf("txid \\[.*\\] committed with status"))
	loc := re.FindIndex(data)
	Expect(len(loc)).To(BeNumerically(">", 0))

	start := loc[0]
	_, data, ok := bytes.Cut(data[start:], []byte("["))
	Expect(ok).To(BeTrue())

	data, _, ok = bytes.Cut(data, []byte("]"))
	Expect(ok).To(BeTrue())

	return string(data)
}

func (ts *FoundationTestSuite) TxInvoke(channelName, chaincodeName string, args ...string) *types.InvokeResult {
	return invokeTx(ts.Network, ts.Peer, ts.Orderer, ts.MainUserName, channelName, chaincodeName, args...)
}

func (ts *FoundationTestSuite) TxInvokeByRobot(channelName, chaincodeName string, args ...string) *types.InvokeResult {
	return invokeTx(ts.Network, ts.Peer, ts.Orderer, ts.RobotUserName, channelName, chaincodeName, args...)
}

func (ts *FoundationTestSuite) TxInvokeWithSign(
	channelName string,
	chaincodeName string,
	user *UserFoundation,
	fn string,
	requestID string,
	nonce string,
	args ...string,
) *types.InvokeResult {
	ctorArgs := append(append([]string{fn, requestID, channelName, chaincodeName}, args...), nonce)
	pubKey, sMsg, err := user.Sign(ctorArgs...)
	Expect(err).NotTo(HaveOccurred())

	ctorArgs = append(ctorArgs, pubKey, base58.Encode(sMsg))
	return ts.TxInvoke(channelName, chaincodeName, ctorArgs...)
}

func (ts *FoundationTestSuite) TxInvokeWithMultisign(
	channelName string,
	chaincodeName string,
	user *UserFoundationMultisigned,
	fn string,
	requestID string,
	nonce string,
	args ...string,
) *types.InvokeResult {
	ctorArgs := append(append([]string{fn, requestID, channelName, chaincodeName}, args...), nonce)
	pubKey, sMsgsByte, err := user.Sign(ctorArgs...)
	Expect(err).NotTo(HaveOccurred())

	var sMsgsStr []string
	for _, sMsgByte := range sMsgsByte {
		sMsgsStr = append(sMsgsStr, base58.Encode(sMsgByte))
	}

	ctorArgs = append(append(ctorArgs, pubKey...), sMsgsStr...)
	return ts.TxInvoke(channelName, chaincodeName, ctorArgs...)
}

func (ts *FoundationTestSuite) NBTxInvoke(channelName, chaincodeName string, args ...string) *types.InvokeResult {
	return invokeNBTx(ts.Network, ts.Peer, ts.Orderer, ts.MainUserName, channelName, chaincodeName, args...)
}

func (ts *FoundationTestSuite) NBTxInvokeByRobot(channelName, chaincodeName string, args ...string) *types.InvokeResult {
	return invokeNBTx(ts.Network, ts.Peer, ts.Orderer, ts.RobotUserName, channelName, chaincodeName, args...)
}

func (ts *FoundationTestSuite) NBTxInvokeWithSign(
	channelName string,
	chaincodeName string,
	user *UserFoundation,
	fn string,
	requestID string,
	nonce string,
	args ...string,
) *types.InvokeResult {
	ctorArgs := append(append([]string{fn, requestID, channelName, chaincodeName}, args...), nonce)
	pubKey, sMsg, err := user.Sign(ctorArgs...)
	Expect(err).NotTo(HaveOccurred())

	ctorArgs = append(ctorArgs, pubKey, base58.Encode(sMsg))
	return ts.NBTxInvoke(channelName, chaincodeName, ctorArgs...)
}
