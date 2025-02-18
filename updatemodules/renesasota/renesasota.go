package renesasota

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"
	"github.com/aoscloud/aos_common/aostypes"
	"github.com/aoscloud/aos_common/partition"
	"github.com/aoscloud/aos_updatemanager/updatehandler"
	log "github.com/sirupsen/logrus"
	"github.com/syucream/posix_mq"
)

/***********************************************************************************************************************
 * Consts
 **********************************************************************************************************************/

const (
	otaCommandSyncCompose = 0
	otaCommandDownload    = 1
	otaCommandInstall     = 2
	otaCommandActivate    = 3
	otaCommandRevert      = 4
)

const (
	otaStatusSuccess = 0
	otaStatusFailed  = 1
)

const otaDefaultTimeout = 10 * time.Minute

const (
	idleState = iota
	preparedState
	updatedState
)

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

// RenesasUpdateModule update components using Renesas OTA master.
type RenesasUpdateModule struct {
	id             string
	config         moduleConfig
	storage        updatehandler.ModuleStorage
	State          updateState `json:"state"`
	VendorVersion  string      `json:"vendorVersion"`
	PendingVersion string      `json:"pendingVersion"`
}

type moduleConfig struct {
	SendQueueName    string            `json:"sendQueueName"`
	ReceiveQueueName string            `json:"receiveQueueName"`
	TargetFile       string            `json:"targetFile"`
	Timeout          aostypes.Duration `json:"timeout"`
}

type updateState int

/***********************************************************************************************************************
 * Public
 **********************************************************************************************************************/

// New creates fs update module instance.
func New(id string, config json.RawMessage, storage updatehandler.ModuleStorage) (updatehandler.UpdateModule, error) {
	log.WithField("module", id).Debug("Create renesasupdate module")

	module := &RenesasUpdateModule{
		id:      id,
		storage: storage,
		config: moduleConfig{
			Timeout: aostypes.Duration{Duration: otaDefaultTimeout},
		},
	}

	if err := json.Unmarshal(config, &module.config); err != nil {
		return nil, aoserrors.Wrap(err)
	}

	if module.config.ReceiveQueueName == "" || module.config.SendQueueName == "" {
		return nil, aoserrors.New("receive and send message queue should be configured")
	}

	if module.config.TargetFile == "" {
		return nil, aoserrors.New("target file name should be configured")
	}

	state, err := storage.GetModuleState(id)
	if err != nil {
		return nil, aoserrors.Wrap(err)
	}

	if len(state) > 0 {
		if err := json.Unmarshal(state, module); err != nil {
			return nil, aoserrors.Wrap(err)
		}
	}

	return module, nil
}

// Close closes DualPartModule.
func (module *RenesasUpdateModule) Close() error {
	log.WithFields(log.Fields{"id": module.id}).Debug("Close renesasupdate module")

	return nil
}

// GetID returns module ID.
func (module *RenesasUpdateModule) GetID() string {
	return module.id
}

// Init initializes module.
func (module *RenesasUpdateModule) Init() error {
	return nil
}

// GetVendorVersion returns vendor version.
func (module *RenesasUpdateModule) GetVendorVersion() (string, error) {
	return module.VendorVersion, nil
}

// Prepare preparing image.
func (module *RenesasUpdateModule) Prepare(imagePath string, vendorVersion string, annotations json.RawMessage) error {
	log.WithFields(log.Fields{
		"id":            module.id,
		"imagePath":     imagePath,
		"vendorVersion": vendorVersion,
	}).Debug("Prepare renesasupdate module")

	if module.State == preparedState {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(module.config.TargetFile), 0o700); err != nil {
		return aoserrors.Wrap(err)
	}

	file, err := os.Create(module.config.TargetFile)
	if err != nil {
		return aoserrors.Wrap(err)
	}
	file.Close()

	if _, err := partition.CopyFromGzipArchive(module.config.TargetFile, imagePath); err != nil {
		return aoserrors.Wrap(err)
	}

	if err := module.sendOTACommands(otaCommandSyncCompose, otaCommandDownload); err != nil {
		return err
	}

	module.PendingVersion = vendorVersion

	if err := module.setState(preparedState); err != nil {
		return err
	}

	return nil
}

// Update updates module.
func (module *RenesasUpdateModule) Update() (rebootRequired bool, err error) {
	log.WithFields(log.Fields{"id": module.id}).Debug("Update renesasupdate module")

	if module.State == updatedState {
		return false, nil
	}

	if err := module.sendOTACommands(otaCommandInstall, otaCommandActivate); err != nil {
		return false, err
	}

	module.VendorVersion, module.PendingVersion = module.PendingVersion, module.VendorVersion

	if err := module.setState(updatedState); err != nil {
		return false, err
	}

	return false, nil
}

// Revert reverts update.
func (module *RenesasUpdateModule) Revert() (rebootRequired bool, err error) {
	log.WithFields(log.Fields{"id": module.id}).Debug("Revert renesasupdate module")

	if module.State == idleState {
		return false, nil
	}

	if err := module.sendOTACommands(otaCommandRevert); err != nil {
		return false, err
	}

	if module.State == updatedState {
		module.VendorVersion, module.PendingVersion = module.PendingVersion, module.VendorVersion
	}

	if err := module.setState(idleState); err != nil {
		return false, err
	}

	return rebootRequired, nil
}

// Apply applies update.
func (module *RenesasUpdateModule) Apply() (rebootRequired bool, err error) {
	log.WithFields(log.Fields{"id": module.id}).Debug("Apply renesasupdate module")

	if module.State == idleState {
		return false, nil
	}

	if err := module.setState(idleState); err != nil {
		return false, err
	}

	return false, nil
}

// Reboot performs module reboot.
func (module *RenesasUpdateModule) Reboot() error {
	log.WithFields(log.Fields{"id": module.id}).Debugf("Reboot renesasupdate module")

	return aoserrors.New("not supported")
}

func (state updateState) String() string {
	return []string{"idle", "prepared", "updated"}[state]
}

/***********************************************************************************************************************
 * Private
 **********************************************************************************************************************/

func (module *RenesasUpdateModule) setState(state updateState) error {
	log.WithFields(log.Fields{"id": module.id, "state": state}).Debugf("State changed")

	module.State = state

	data, err := json.Marshal(module)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if err = module.storage.SetModuleState(module.id, data); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (module *RenesasUpdateModule) sendOTACommands(commands ...int64) error {
	sendMQ, err := posix_mq.NewMessageQueue(
		module.config.SendQueueName, posix_mq.O_WRONLY, 0o600, nil)
	if err != nil {
		return aoserrors.Wrap(err)
	}
	defer sendMQ.Close()

	recvMQ, err := posix_mq.NewMessageQueue(
		module.config.ReceiveQueueName, posix_mq.O_RDONLY, 0o600, nil)
	if err != nil {
		return aoserrors.Wrap(err)
	}
	defer recvMQ.Close()

	for _, command := range commands {
		buffer := bytes.NewBuffer(nil)

		if err = binary.Write(buffer, binary.LittleEndian, command); err != nil {
			return aoserrors.Wrap(err)
		}

		if err = sendMQ.TimedSend(buffer.Bytes(), 0, time.Now().Add(module.config.Timeout.Duration)); err != nil {
			return aoserrors.Wrap(err)
		}

		recvData, _, err := recvMQ.TimedReceive(time.Now().Add(module.config.Timeout.Duration))
		if err != nil {
			return aoserrors.Wrap(err)
		}

		buffer = bytes.NewBuffer(recvData)

		var status int64

		if err = binary.Read(buffer, binary.LittleEndian, &status); err != nil {
			return aoserrors.Wrap(err)
		}

		if status != otaStatusSuccess {
			return aoserrors.Errorf("execute command %d failed", command)
		}
	}

	return nil
}
