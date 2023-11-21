package builder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/attestantio/go-builder-client/api/bellatrix"
	"github.com/attestantio/go-builder-client/api/capella"
	"github.com/ethereum/go-ethereum/log"
	"github.com/flashbots/go-boost-utils/utils"
)

var ErrValidatorNotFound = errors.New("validator not found")

type RemoteRelay struct {
	client http.Client
	config RelayConfig

	localRelay *LocalRelay

	cancellationsEnabled bool

	validatorsLock       sync.RWMutex
	validatorSyncOngoing bool
	lastRequestedSlot    uint64
	validatorSlotMap     map[uint64]ValidatorData
}

func NewRemoteRelay(config RelayConfig, localRelay *LocalRelay, cancellationsEnabled bool) *RemoteRelay {
	r := &RemoteRelay{
		client:               http.Client{Timeout: time.Second},
		localRelay:           localRelay,
		cancellationsEnabled: cancellationsEnabled,
		validatorSyncOngoing: false,
		lastRequestedSlot:    0,
		validatorSlotMap:     make(map[uint64]ValidatorData),
		config:               config,
	}

	err := r.updateValidatorsMap(0, 3)
	if err != nil {
		log.Error("could not connect to remote relay, continuing anyway", "err", err)
	}
	return r
}

type GetValidatorRelayResponse []struct {
	Slot  uint64 `json:"slot,string"`
	Entry struct {
		Message struct {
			FeeRecipient string `json:"fee_recipient"`
			GasLimit     uint64 `json:"gas_limit,string"`
			Timestamp    uint64 `json:"timestamp,string"`
			Pubkey       string `json:"pubkey"`
		} `json:"message"`
		Signature string `json:"signature"`
	} `json:"entry"`
}

func (r *RemoteRelay) updateValidatorsMap(currentSlot uint64, retries int) error {
	fmt.Println("updateValidatorsMap() start")
	r.validatorsLock.Lock()
	if r.validatorSyncOngoing {
		r.validatorsLock.Unlock()
		return errors.New("sync is ongoing")
	}
	r.validatorSyncOngoing = true
	r.validatorsLock.Unlock()

	fmt.Println("updateValidatorsMap() loading 1")
	log.Info("requesting ", "currentSlot", currentSlot)
	newMap, err := r.getSlotValidatorMapFromRelay()
	for err != nil && retries > 0 {
		log.Error("111 getSlotValidatorMapFromRelay() error")
		log.Error("could not get validators map from relay, retrying", "err", err)
		time.Sleep(time.Second)
		newMap, err = r.getSlotValidatorMapFromRelay()
		retries -= 1
	}
	r.validatorsLock.Lock()
	r.validatorSyncOngoing = false
	fmt.Println("updateValidatorsMap() loading 2")
	if err != nil {
		r.validatorsLock.Unlock()
		log.Error("222 error")
		log.Error("could not get validators map from relay", "err", err)
		return err
	}
	fmt.Println("updateValidatorsMap() loading 3")
	fmt.Println("updateValidatorsMap() newMap:", newMap)
	r.validatorSlotMap = newMap
	r.lastRequestedSlot = currentSlot
	r.validatorsLock.Unlock()

	log.Info("Updated validators", "count", len(newMap), "slot", currentSlot)
	return nil
}

func (r *RemoteRelay) GetValidatorForSlot(nextSlot uint64) (ValidatorData, error) {
	// next slot is expected to be the actual chain's next slot, not something requested by the user!
	// if not sanitized it will force resync of validator data and possibly is a DoS vector

	// 手动添加一段代码
	fmt.Println("手动updateValidatorsMap")
	err := r.updateValidatorsMap(nextSlot, 1)
	if err != nil {
		fmt.Println("手动 updateValidatorsMap 失败")
		log.Error("could not update validators map", "err", err)
	} else {
		fmt.Println("手动 updateValidatorsMap 成功")
	}

	fmt.Println("GetValidatorForSlot start")
	r.validatorsLock.RLock()
	// TODO 主要是因为这个32被写死了
	if r.lastRequestedSlot == 0 || nextSlot/32 > r.lastRequestedSlot/32 {
		fmt.Println("GetValidatorForSlot loading 0.5")
		// Every epoch request validators map
		go func() {
			fmt.Println("r.updateValidatorsMap ", "nextSlot", nextSlot)
			err := r.updateValidatorsMap(nextSlot, 1)
			if err != nil {
				log.Error("could not update validators map", "err", err)
			}
		}()
	}
	fmt.Println("GetValidatorForSlot loading 1")
	fmt.Println("r.validatorSlotMap:", r.validatorSlotMap)
	vd, found := r.validatorSlotMap[nextSlot]
	r.validatorsLock.RUnlock()

	if r.localRelay != nil {
		fmt.Println("LOCAL RELAY is active")
		localValidator, err := r.localRelay.GetValidatorForSlot(nextSlot)
		fmt.Println("RemoteRelay qqq GetValidatorForSlot() r.localRelay != nil, localValidator:", localValidator)
		if err == nil {
			log.Info("Validator registration overwritten by local data", "slot", nextSlot, "validator", localValidator)
			return localValidator, nil
		}
	} else {
		fmt.Println("LOCAL RELAY is inactive")
	}
	fmt.Println("GetValidatorForSlot loading 2")

	if found {
		fmt.Println("RemoteRelay qqq GetValidatorForSlot() found:", found)
		fmt.Println("RemoteRelay qqq GetValidatorForSlot() vd:", vd)
		return vd, nil
	}

	fmt.Println("GetValidatorForSlot end: ", ValidatorData{}, ErrValidatorNotFound)

	return ValidatorData{}, ErrValidatorNotFound
}

func (r *RemoteRelay) Start() error {
	return nil
}

func (r *RemoteRelay) Stop() {}

func (r *RemoteRelay) SubmitBlock(msg *bellatrix.SubmitBlockRequest, _ ValidatorData) error {
	log.Info("submitting block to remote relay", "endpoint", r.config.Endpoint)
	endpoint := r.config.Endpoint + "/relay/v1/builder/blocks"
	if r.cancellationsEnabled {
		endpoint = endpoint + "?cancellations=1"
	}
	code, err := SendHTTPRequest(context.TODO(), *http.DefaultClient, http.MethodPost, endpoint, msg, nil)
	if err != nil {
		return fmt.Errorf("error sending http request to relay %s. err: %w", r.config.Endpoint, err)
	}
	if code > 299 {
		return fmt.Errorf("non-ok response code %d from relay %s", code, r.config.Endpoint)
	}

	if r.localRelay != nil {
		r.localRelay.submitBlock(msg)
	}

	return nil
}

func (r *RemoteRelay) SubmitBlockCapella(msg *capella.SubmitBlockRequest, _ ValidatorData) error {
	log.Info("submitting block to remote relay", "endpoint", r.config.Endpoint)

	endpoint := r.config.Endpoint + "/relay/v1/builder/blocks"
	if r.cancellationsEnabled {
		endpoint = endpoint + "?cancellations=1"
	}

	if r.config.SszEnabled {
		bodyBytes, err := msg.MarshalSSZ()
		if err != nil {
			return fmt.Errorf("error marshaling ssz: %w", err)
		}
		log.Debug("submitting block to remote relay", "endpoint", r.config.Endpoint)
		code, err := SendSSZRequest(context.TODO(), *http.DefaultClient, http.MethodPost, endpoint, bodyBytes, r.config.GzipEnabled)
		if err != nil {
			return fmt.Errorf("error sending http request to relay %s. err: %w", r.config.Endpoint, err)
		}
		if code > 299 {
			return fmt.Errorf("non-ok response code %d from relay %s", code, r.config.Endpoint)
		}
	} else {
		code, err := SendHTTPRequest(context.TODO(), *http.DefaultClient, http.MethodPost, endpoint, msg, nil)
		if err != nil {
			return fmt.Errorf("error sending http request to relay %s. err: %w", r.config.Endpoint, err)
		}
		if code > 299 {
			return fmt.Errorf("non-ok response code %d from relay %s", code, r.config.Endpoint)
		}
	}

	if r.localRelay != nil {
		r.localRelay.submitBlockCapella(msg)
	}

	return nil
}

func (r *RemoteRelay) getSlotValidatorMapFromRelay() (map[uint64]ValidatorData, error) {
	var dst GetValidatorRelayResponse
	code, err := SendHTTPRequest(context.TODO(), *http.DefaultClient, http.MethodGet, r.config.Endpoint+"/relay/v1/builder/validators", nil, &dst)
	if err != nil {
		return nil, err
	}
	jsonDst, _ := json.Marshal(dst)
	// 这里的输出是正常的
	fmt.Println("000 getSlotValidatorMapFromRelay:", string(jsonDst))
	if code > 299 {
		return nil, fmt.Errorf("non-ok response code %d from relay", code)
	}

	res := make(map[uint64]ValidatorData)
	for _, data := range dst {
		fmt.Println("111 data:", data)
		fmt.Println("111 data.Entry.Message:", data.Entry.Message)
		fmt.Println("111 data.Entry.Message.FeeRecipient:", data.Entry.Message.FeeRecipient)
		feeRecipient, err := utils.HexToAddress(data.Entry.Message.FeeRecipient)
		//feeRecipient, err := utils.HexToAddress("0x123463a4b065722e99115d6c222f267d9cabb524")

		if err != nil {
			log.Error("Ill-formatted fee_recipient from relay", "data", data)
			continue
		}
		// 这里是正常的
		fmt.Println("这里是正常的 111 feeRecipient:", feeRecipient)

		pubkeyHex := PubkeyHex(strings.ToLower(data.Entry.Message.Pubkey))

		res[data.Slot] = ValidatorData{
			Pubkey:       pubkeyHex,
			FeeRecipient: feeRecipient,
			GasLimit:     data.Entry.Message.GasLimit,
		}
	}

	return res, nil
}

func (r *RemoteRelay) Config() RelayConfig {
	return r.config
}
