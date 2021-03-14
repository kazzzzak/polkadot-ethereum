// Copyright 2020 Snowfork
// SPDX-License-Identifier: LGPL-3.0-only

package parachain

// TODO: this is a copy of Ethereum writer and should be refactored

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/sirupsen/logrus"

	"github.com/snowfork/polkadot-ethereum/relayer/chain"
	"github.com/snowfork/polkadot-ethereum/relayer/chain/ethereum"
	"github.com/snowfork/polkadot-ethereum/relayer/contracts/lightclientbridge"
	"github.com/snowfork/polkadot-ethereum/relayer/parachain"
)

const LightClientBridgeContractID = "lightclientbridge"

type Writer struct {
	config    *Config
	conn      *ethereum.Connection
	contracts map[string]*lightclientbridge.Contract
	messages  <-chan []chain.Message
	beefy     chan parachain.BeefyCommitmentInfo
	log       *logrus.Entry
}

func NewWriter(config *Config, conn *ethereum.Connection, messages <-chan []chain.Message, beefy chan parachain.BeefyCommitmentInfo,
	contracts map[string]*lightclientbridge.Contract, log *logrus.Entry) (*Writer, error) {
	return &Writer{
		config:    config,
		conn:      conn,
		contracts: contracts,
		messages:  messages,
		beefy:     beefy,
		log:       log,
	}, nil
}

func (wr *Writer) Start(ctx context.Context, eg *errgroup.Group) error {

	contract, err := lightclientbridge.NewContract(common.HexToAddress(wr.config.Ethereum.Contracts.RelayBridgeLightClient), wr.conn.GetClient())
	if err != nil {
		return err
	}
	wr.contracts[LightClientBridgeContractID] = contract

	eg.Go(func() error {
		return wr.writeLoop(ctx)
	})

	return nil
}

func (wr *Writer) onDone(ctx context.Context) error {
	wr.log.Info("Shutting down writer...")
	// Avoid deadlock if a listener is still trying to send to a channel
	for range wr.messages {
		wr.log.Debug("Discarded message")
	}
	return ctx.Err()
}

func (wr *Writer) writeLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return wr.onDone(ctx)
		case msgs := <-wr.messages:
			for _, msg := range msgs {
				beefyInfo, ok := msg.(parachain.BeefyCommitmentInfo)
				if !ok {
					return fmt.Errorf("Invalid message")
				}

				switch beefyInfo.Status {
				case parachain.CommitmentWitnessed:
					err := wr.WriteNewSignatureCommitment(ctx, beefyInfo)
					if err != nil {
						wr.log.WithError(err).Error("Error submitting message to ethereum")
					}
				// TODO: will these cases even be hit? it's in a different channel.
				case parachain.InitialVerificationTxSent, parachain.InitialVerificationTxConfirmed:
					continue // Ethereum listener is responsible for checking tx confirmation
				case parachain.ReadyToComplete:
					err := wr.WriteCompleteSignatureCommitment(ctx, beefyInfo)
					if err != nil {
						wr.log.WithError(err).Error("Error submitting message to ethereum")
					}
				default:
					wr.log.Info("Invalid beefy commitment status")
				}
			}
		}
	}
}

func (wr *Writer) signerFn(_ common.Address, tx *types.Transaction) (*types.Transaction, error) {
	signedTx, err := types.SignTx(tx, types.HomesteadSigner{}, wr.conn.GetKeyPair().PrivateKey())
	if err != nil {
		return nil, err
	}
	return signedTx, nil
}

func (wr *Writer) WriteNewSignatureCommitment(ctx context.Context, beefyInfo parachain.BeefyCommitmentInfo) error {
	wr.log.Info("Parachain writer received msg")

	msg, err := beefyInfo.BuildNewSignatureCommitmentMessage()
	if err != nil {
		return err
	}

	contract := wr.contracts[LightClientBridgeContractID] // TODO: don't hardcode this
	if contract == nil {
		return fmt.Errorf("Unknown contract")
	}

	options := bind.TransactOpts{
		From:     wr.conn.GetKeyPair().CommonAddress(),
		Signer:   wr.signerFn,
		Context:  ctx,
		GasLimit: 5000000, // TODO: reasonable gas limit
	}

	tx, err := contract.NewSignatureCommitment(&options, msg.Payload,
		msg.ValidatorClaimsBitfield, msg.ValidatorSignatureCommitment,
		msg.ValidatorPublicKey, msg.ValidatorPublicKeyMerkleProof)
	if err != nil {
		wr.log.WithError(err).Error("Failed to submit transaction")
		return err
	}

	wr.log.WithFields(logrus.Fields{
		"txHash": tx.Hash().Hex(),
	}).Info("New Signature Commitment transaction submitted")

	beefyInfo.Status = parachain.InitialVerificationTxSent
	beefyInfo.InitialVerificationTxHash = tx.Hash()
	wr.beefy <- beefyInfo

	return nil
}

// WriteCompleteSignatureCommitment sends a CompleteSignatureCommitment tx to the LightClientBridge contract
func (wr *Writer) WriteCompleteSignatureCommitment(ctx context.Context, beefyInfo parachain.BeefyCommitmentInfo) error {
	wr.log.Info("Parachain writer received msg")

	msg, err := beefyInfo.BuildCompleteSignatureCommitmentMessage()
	if err != nil {
		return err
	}

	contract := wr.contracts[LightClientBridgeContractID] // TODO: don't hardcode this
	if contract == nil {
		return fmt.Errorf("Unknown contract")
	}

	options := bind.TransactOpts{
		From:     wr.conn.GetKeyPair().CommonAddress(),
		Signer:   wr.signerFn,
		Context:  ctx,
		GasLimit: 5000000, // TODO: reasonable gas limit
	}

	tx, err := contract.CompleteSignatureCommitment(&options, msg.ID, msg.Payload, msg.RandomSignatureCommitments,
		msg.RandomSignatureBitfieldPositions, msg.RandomValidatorAddresses, msg.RandomPublicKeyMerkleProofs)

	if err != nil {
		wr.log.WithError(err).Error("Failed to submit transaction")
		return err
	}

	wr.log.WithFields(logrus.Fields{
		"txHash": tx.Hash().Hex(),
	}).Info("Complete Signature Commitment transaction submitted")

	return nil
}