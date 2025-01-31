package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	builderApi "github.com/attestantio/go-builder-client/api"
	builderSpec "github.com/attestantio/go-builder-client/spec"
	eth2ApiV1Deneb "github.com/attestantio/go-eth2-client/api/v1/deneb"
	eth2ApiV1Electra "github.com/attestantio/go-eth2-client/api/v1/electra"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/flashbots/mev-boost/config"
	"github.com/flashbots/mev-boost/server/params"
	"github.com/flashbots/mev-boost/server/types"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

func (m *BoostService) getHeader(log *logrus.Entry, ua UserAgent, _slot uint64, pubkey, parentHashHex string) (bidResp, error) {
	if len(pubkey) != 98 {
		return bidResp{}, errInvalidPubkey
	}

	if len(parentHashHex) != 66 {
		return bidResp{}, errInvalidHash
	}

	// Make sure we have a uid for this slot
	m.slotUIDLock.Lock()
	if m.slotUID.slot < _slot {
		m.slotUID.slot = _slot
		m.slotUID.uid = uuid.New()
	}
	slotUID := m.slotUID.uid
	m.slotUIDLock.Unlock()
	log = log.WithField("slotUID", slotUID)

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + _slot*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("getHeader request start - %d milliseconds into slot %d", msIntoSlot, _slot)
	// Add request headers
	headers := map[string]string{
		HeaderKeySlotUID:      slotUID.String(),
		HeaderStartTimeUnixMS: fmt.Sprintf("%d", time.Now().UTC().UnixMilli()),
	}
	// Prepare relay responses
	var (
		result = bidResp{}                                 // the final response, containing the highest bid (if any)
		relays = make(map[BlockHashHex][]types.RelayEntry) // relays that sent the bid for a specific blockHash

		mu sync.Mutex
		wg sync.WaitGroup
	)

	// Call the relays
	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay types.RelayEntry) {
			defer wg.Done()
			path := fmt.Sprintf("/eth/v1/builder/header/%d/%s/%s", _slot, parentHashHex, pubkey)
			url := relay.GetURI(path)
			log := log.WithField("url", url)
			responsePayload := new(builderSpec.VersionedSignedBuilderBid)
			code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, url, ua, headers, nil, responsePayload)
			if err != nil {
				log.WithError(err).Warn("error making request to relay")
				return
			}

			if code == http.StatusNoContent {
				log.Debug("no-content response")
				return
			}

			// Skip if payload is empty
			if responsePayload.IsEmpty() {
				return
			}

			// Getting the bid info will check if there are missing fields in the response
			bidInfo, err := parseBidInfo(responsePayload)
			if err != nil {
				log.WithError(err).Warn("error parsing bid info")
				return
			}

			if bidInfo.blockHash == nilHash {
				log.Warn("relay responded with empty block hash")
				return
			}

			valueEth := weiBigIntToEthBigFloat(bidInfo.value.ToBig())
			log = log.WithFields(logrus.Fields{
				"blockNumber": bidInfo.blockNumber,
				"blockHash":   bidInfo.blockHash.String(),
				"txRoot":      bidInfo.txRoot.String(),
				"value":       valueEth.Text('f', 18),
			})

			if relay.PublicKey.String() != bidInfo.pubkey.String() {
				log.Errorf("bid pubkey mismatch. expected: %s - got: %s", relay.PublicKey.String(), bidInfo.pubkey.String())
				return
			}

			// Verify the relay signature in the relay response
			if !config.SkipRelaySignatureCheck {
				ok, err := checkRelaySignature(responsePayload, m.builderSigningDomain, relay.PublicKey)
				if err != nil {
					log.WithError(err).Error("error verifying relay signature")
					return
				}
				if !ok {
					log.Error("failed to verify relay signature")
					return
				}
			}

			// Verify response coherence with proposer's input data
			if bidInfo.parentHash.String() != parentHashHex {
				log.WithFields(logrus.Fields{
					"originalParentHash": parentHashHex,
					"responseParentHash": bidInfo.parentHash.String(),
				}).Error("proposer and relay parent hashes are not the same")
				return
			}

			isZeroValue := bidInfo.value.IsZero()
			isEmptyListTxRoot := bidInfo.txRoot.String() == "0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"
			if isZeroValue || isEmptyListTxRoot {
				log.Warn("ignoring bid with 0 value")
				return
			}
			log.Debug("bid received")

			// Skip if value (fee) is lower than the minimum bid
			if bidInfo.value.CmpBig(m.relayMinBid.BigInt()) == -1 {
				log.Debug("ignoring bid below min-bid value")
				return
			}

			mu.Lock()
			defer mu.Unlock()

			// Remember which relays delivered which bids (multiple relays might deliver the top bid)
			relays[BlockHashHex(bidInfo.blockHash.String())] = append(relays[BlockHashHex(bidInfo.blockHash.String())], relay)

			// Compare the bid with already known top bid (if any)
			if !result.response.IsEmpty() {
				valueDiff := bidInfo.value.Cmp(result.bidInfo.value)
				if valueDiff == -1 { // current bid is less profitable than already known one
					return
				} else if valueDiff == 0 { // current bid is equally profitable as already known one. Use hash as tiebreaker
					previousBidBlockHash := result.bidInfo.blockHash
					if bidInfo.blockHash.String() >= previousBidBlockHash.String() {
						return
					}
				}
			}

			// Use this relay's response as mev-boost response because it's most profitable
			log.Debug("new best bid")
			result.response = *responsePayload
			result.bidInfo = bidInfo
			result.t = time.Now()
		}(relay)
	}
	// Wait for all requests to complete...
	wg.Wait()

	// Set the winning relay before returning
	result.relays = relays[BlockHashHex(result.bidInfo.blockHash.String())]
	return result, nil
}

func (m *BoostService) processDenebPayload(log *logrus.Entry, ua UserAgent, blindedBlock *eth2ApiV1Deneb.SignedBlindedBeaconBlock) (*builderApi.VersionedSubmitBlindedBlockResponse, bidResp) {
	// Get the currentSlotUID for this slot
	currentSlotUID := ""
	m.slotUIDLock.Lock()
	if m.slotUID.slot == uint64(blindedBlock.Message.Slot) {
		currentSlotUID = m.slotUID.uid.String()
	} else {
		log.Warnf("latest slotUID is for slot %d rather than payload slot %d", m.slotUID.slot, blindedBlock.Message.Slot)
	}
	m.slotUIDLock.Unlock()

	// Prepare logger
	log = log.WithFields(logrus.Fields{
		"ua":         ua,
		"slot":       blindedBlock.Message.Slot,
		"blockHash":  blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash.String(),
		"parentHash": blindedBlock.Message.Body.ExecutionPayloadHeader.ParentHash.String(),
		"slotUID":    currentSlotUID,
	})

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + uint64(blindedBlock.Message.Slot)*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("submitBlindedBlock request start - %d milliseconds into slot %d", msIntoSlot, blindedBlock.Message.Slot)

	// Get the bid!
	m.bidsLock.Lock()
	originalBid := m.bids[bidKey(uint64(blindedBlock.Message.Slot), blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash)]
	m.bidsLock.Unlock()
	if originalBid.response.IsEmpty() {
		log.Error("no bid for this getPayload payload found, was getHeader called before?")
	} else if len(originalBid.relays) == 0 {
		log.Warn("bid found but no associated relays")
	}

	// Add request headers
	headers := map[string]string{
		HeaderKeySlotUID:      currentSlotUID,
		HeaderStartTimeUnixMS: fmt.Sprintf("%d", time.Now().UTC().UnixMilli()),
	}

	// Prepare for requests
	resultCh := make(chan *builderApi.VersionedSubmitBlindedBlockResponse, len(m.relays))
	var received atomic.Bool
	go func() {
		// Make sure we receive a response within the timeout
		time.Sleep(m.httpClientGetPayload.Timeout)
		resultCh <- nil
	}()

	// Prepare the request context, which will be cancelled after the first successful response from a relay
	requestCtx, requestCtxCancel := context.WithCancel(context.Background())
	defer requestCtxCancel()

	for _, relay := range m.relays {
		go func(relay types.RelayEntry) {
			url := relay.GetURI(params.PathGetPayload)
			log := log.WithField("url", url)
			log.Debug("calling getPayload")

			responsePayload := new(builderApi.VersionedSubmitBlindedBlockResponse)
			_, err := SendHTTPRequestWithRetries(requestCtx, m.httpClientGetPayload, http.MethodPost, url, ua, headers, blindedBlock, responsePayload, m.requestMaxRetries, log)
			if err != nil {
				if errors.Is(requestCtx.Err(), context.Canceled) {
					log.Info("request was cancelled") // this is expected, if payload has already been received by another relay
				} else {
					log.WithError(err).Error("error making request to relay")
				}
				return
			}

			if responsePayload.Version != spec.DataVersionDeneb {
				log.WithFields(logrus.Fields{
					"version": responsePayload.Version,
				}).Error("response version was not deneb")
				return
			}
			if getPayloadResponseIsEmpty(responsePayload) {
				log.Error("response with empty data!")
				return
			}

			payload := responsePayload.Deneb.ExecutionPayload
			blobs := responsePayload.Deneb.BlobsBundle

			// Ensure the response blockhash matches the request
			if blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash != payload.BlockHash {
				log.WithFields(logrus.Fields{
					"responseBlockHash": payload.BlockHash.String(),
				}).Error("requestBlockHash does not equal responseBlockHash")
				return
			}

			commitments := blindedBlock.Message.Body.BlobKZGCommitments
			// Ensure that blobs are valid and matches the request
			if len(commitments) != len(blobs.Blobs) || len(commitments) != len(blobs.Commitments) || len(commitments) != len(blobs.Proofs) {
				log.WithFields(logrus.Fields{
					"requestBlobCommitments":  len(commitments),
					"responseBlobs":           len(blobs.Blobs),
					"responseBlobCommitments": len(blobs.Commitments),
					"responseBlobProofs":      len(blobs.Proofs),
				}).Error("block KZG commitment length does not equal responseBlobs length")
				return
			}

			for i, commitment := range commitments {
				if commitment != blobs.Commitments[i] {
					log.WithFields(logrus.Fields{
						"requestBlobCommitment":  commitment.String(),
						"responseBlobCommitment": blobs.Commitments[i].String(),
						"index":                  i,
					}).Error("requestBlobCommitment does not equal responseBlobCommitment")
					return
				}
			}

			requestCtxCancel()
			if received.CompareAndSwap(false, true) {
				resultCh <- responsePayload
				log.Info("received payload from relay")
			} else {
				log.Trace("Discarding response, already received a correct response")
			}
		}(relay)
	}

	// Wait for the first request to complete
	result := <-resultCh

	return result, originalBid
}

func (m *BoostService) processElectraPayload(log *logrus.Entry, ua UserAgent, blindedBlock *eth2ApiV1Electra.SignedBlindedBeaconBlock) (*builderApi.VersionedSubmitBlindedBlockResponse, bidResp) {
	// Get the currentSlotUID for this slot
	currentSlotUID := ""
	m.slotUIDLock.Lock()
	if m.slotUID.slot == uint64(blindedBlock.Message.Slot) {
		currentSlotUID = m.slotUID.uid.String()
	} else {
		log.Warnf("latest slotUID is for slot %d rather than payload slot %d", m.slotUID.slot, blindedBlock.Message.Slot)
	}
	m.slotUIDLock.Unlock()

	// Prepare logger
	log = log.WithFields(logrus.Fields{
		"ua":         ua,
		"slot":       blindedBlock.Message.Slot,
		"blockHash":  blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash.String(),
		"parentHash": blindedBlock.Message.Body.ExecutionPayloadHeader.ParentHash.String(),
		"slotUID":    currentSlotUID,
	})

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + uint64(blindedBlock.Message.Slot)*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("submitBlindedBlock request start - %d milliseconds into slot %d", msIntoSlot, blindedBlock.Message.Slot)

	// Get the bid!
	m.bidsLock.Lock()
	originalBid := m.bids[bidKey(uint64(blindedBlock.Message.Slot), blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash)]
	m.bidsLock.Unlock()
	if originalBid.response.IsEmpty() {
		log.Error("no bid for this getPayload payload found, was getHeader called before?")
	} else if len(originalBid.relays) == 0 {
		log.Warn("bid found but no associated relays")
	}

	// Add request headers
	headers := map[string]string{
		HeaderKeySlotUID:      currentSlotUID,
		HeaderStartTimeUnixMS: fmt.Sprintf("%d", time.Now().UTC().UnixMilli()),
	}

	// Prepare for requests
	resultCh := make(chan *builderApi.VersionedSubmitBlindedBlockResponse, len(m.relays))
	var received atomic.Bool
	go func() {
		// Make sure we receive a response within the timeout
		time.Sleep(m.httpClientGetPayload.Timeout)
		resultCh <- nil
	}()

	// Prepare the request context, which will be cancelled after the first successful response from a relay
	requestCtx, requestCtxCancel := context.WithCancel(context.Background())
	defer requestCtxCancel()

	for _, relay := range m.relays {
		go func(relay types.RelayEntry) {
			url := relay.GetURI(params.PathGetPayload)
			log := log.WithField("url", url)
			log.Debug("calling getPayload")

			responsePayload := new(builderApi.VersionedSubmitBlindedBlockResponse)
			_, err := SendHTTPRequestWithRetries(requestCtx, m.httpClientGetPayload, http.MethodPost, url, ua, headers, blindedBlock, responsePayload, m.requestMaxRetries, log)
			if err != nil {
				if errors.Is(requestCtx.Err(), context.Canceled) {
					log.Info("request was cancelled") // this is expected, if payload has already been received by another relay
				} else {
					log.WithError(err).Error("error making request to relay")
				}
				return
			}

			if responsePayload.Version != spec.DataVersionElectra {
				log.WithFields(logrus.Fields{
					"version": responsePayload.Version,
				}).Error("response version was not electra")
				return
			}
			if getPayloadResponseIsEmpty(responsePayload) {
				log.Error("response with empty data!")
				return
			}

			payload := responsePayload.Electra.ExecutionPayload
			blobs := responsePayload.Electra.BlobsBundle

			// Ensure the response blockhash matches the request
			if blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash != payload.BlockHash {
				log.WithFields(logrus.Fields{
					"responseBlockHash": payload.BlockHash.String(),
				}).Error("requestBlockHash does not equal responseBlockHash")
				return
			}

			commitments := blindedBlock.Message.Body.BlobKZGCommitments
			// Ensure that blobs are valid and matches the request
			if len(commitments) != len(blobs.Blobs) || len(commitments) != len(blobs.Commitments) || len(commitments) != len(blobs.Proofs) {
				log.WithFields(logrus.Fields{
					"requestBlobCommitments":  len(commitments),
					"responseBlobs":           len(blobs.Blobs),
					"responseBlobCommitments": len(blobs.Commitments),
					"responseBlobProofs":      len(blobs.Proofs),
				}).Error("block KZG commitment length does not equal responseBlobs length")
				return
			}

			for i, commitment := range commitments {
				if commitment != blobs.Commitments[i] {
					log.WithFields(logrus.Fields{
						"requestBlobCommitment":  commitment.String(),
						"responseBlobCommitment": blobs.Commitments[i].String(),
						"index":                  i,
					}).Error("requestBlobCommitment does not equal responseBlobCommitment")
					return
				}
			}

			requestCtxCancel()
			if received.CompareAndSwap(false, true) {
				resultCh <- responsePayload
				log.Info("received payload from relay")
			} else {
				log.Trace("Discarding response, already received a correct response")
			}
		}(relay)
	}

	// Wait for the first request to complete
	result := <-resultCh

	return result, originalBid
}
