// Copyright © 2021 Cisco Systems, Inc. and its affiliates.
// All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backend

import (
	"context"
	"fmt"
	"mime"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-openapi/spec"
	"github.com/go-openapi/strfmt"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/apiclarity/apiclarity/api/server/models"
	_config "github.com/apiclarity/apiclarity/backend/pkg/config"
	_database "github.com/apiclarity/apiclarity/backend/pkg/database"
	"github.com/apiclarity/apiclarity/backend/pkg/healthz"
	"github.com/apiclarity/apiclarity/backend/pkg/k8smonitor"
	"github.com/apiclarity/apiclarity/backend/pkg/modules"
	"github.com/apiclarity/apiclarity/backend/pkg/rest"
	"github.com/apiclarity/apiclarity/backend/pkg/traces"
	pluginsmodels "github.com/apiclarity/apiclarity/plugins/api/server/models"
	_spec "github.com/apiclarity/speculator/pkg/spec"
	_speculator "github.com/apiclarity/speculator/pkg/speculator"
	_mimeutils "github.com/apiclarity/speculator/pkg/utils"
)

type Backend struct {
	speculator          *_speculator.Speculator
	stateBackupInterval time.Duration
	stateBackupFileName string
	monitor             *k8smonitor.Monitor
	apiInventoryLock    sync.RWMutex
	dbHandler           _database.Database
	modules             modules.Module
}

func CreateBackend(config *_config.Config, monitor *k8smonitor.Monitor, speculator *_speculator.Speculator, dbHandler *_database.Handler, modules modules.Module) *Backend {
	return &Backend{
		speculator:          speculator,
		stateBackupInterval: time.Second * time.Duration(config.StateBackupIntervalSec),
		stateBackupFileName: config.StateBackupFileName,
		monitor:             monitor,
		dbHandler:           dbHandler,
		modules:             modules,
	}
}

func createDatabaseConfig(config *_config.Config) *_database.DBConfig {
	return &_database.DBConfig{
		DriverType:     config.DatabaseDriver,
		EnableInfoLogs: config.EnableDBInfoLogs,
		DBPassword:     config.DBPassword,
		DBUser:         config.DBUser,
		DBHost:         config.DBHost,
		DBPort:         config.DBPort,
		DBName:         config.DBName,
	}
}

const defaultChanSize = 100

func Run() {
	config, err := _config.LoadConfig()
	if err != nil {
		log.Errorf("Failed to load config: %v", err)
		return
	}
	errChan := make(chan struct{}, defaultChanSize)

	healthServer := healthz.NewHealthServer(config.HealthCheckAddress)
	healthServer.Start(errChan)

	healthServer.SetIsReady(false)

	globalCtx, globalCancel := context.WithCancel(context.Background())
	defer globalCancel()

	log.Info("APIClarity backend is running")

	dbConfig := createDatabaseConfig(config)
	dbHandler := _database.Init(dbConfig)
	dbHandler.StartReviewTableCleaner(globalCtx, time.Duration(config.DatabaseCleanerIntervalSec)*time.Second)
	var clientset kubernetes.Interface
	if config.K8sLocal {
		clientset, err = k8smonitor.CreateLocalK8sClientset()
		if err != nil {
			log.Fatalf("failed to create K8s clientset: %v", err)
		}
	} else if !viper.GetBool(_database.FakeTracesEnvVar) && !viper.GetBool(_database.FakeDataEnvVar) {
		clientset, err = k8smonitor.CreateK8sClientset()
		if err != nil {
			log.Fatalf("failed to create K8s clientset: %v", err)
		}
	}

	var monitor *k8smonitor.Monitor
	if !viper.GetBool(_config.NoMonitorEnvVar) && !viper.GetBool(_database.FakeTracesEnvVar) && !viper.GetBool(_database.FakeDataEnvVar) {
		monitor, err = k8smonitor.CreateMonitor(clientset)
		if err != nil {
			log.Errorf("Failed to create a monitor: %v", err)
			return
		}
		monitor.Start()
		defer monitor.Stop()
	} else if viper.GetBool(_database.FakeDataEnvVar) {
		go dbHandler.CreateFakeData()
	}

	speculator, err := _speculator.DecodeState(config.StateBackupFileName, config.SpeculatorConfig)
	if err != nil {
		log.Infof("No speculator state to decode, creating new: %v", err)
		speculator = _speculator.CreateSpeculator(config.SpeculatorConfig)
	} else {
		log.Infof("Using encoded speculator state")
	}

	module := modules.New(globalCtx, dbHandler, clientset)
	backend := CreateBackend(config, monitor, speculator, dbHandler, module)

	restServer, err := rest.CreateRESTServer(config.BackendRestPort, speculator, dbHandler, module)
	if err != nil {
		log.Fatalf("Failed to create REST server: %v", err)
	}
	restServer.Start(errChan)
	defer restServer.Stop()

	tracesServer, err := traces.CreateHTTPTracesServer(config.HTTPTracesPort, backend.handleHTTPTrace)
	if err != nil {
		log.Fatalf("Failed to create trace server: %v", err)
	}
	tracesServer.Start(errChan)
	defer tracesServer.Stop()

	backend.startStateBackup(globalCtx)

	healthServer.SetIsReady(true)
	log.Info("APIClarity backend is ready")

	if viper.GetBool(_database.FakeTracesEnvVar) {
		go backend.startSendingFakeTraces()
	}

	// Wait for deactivation
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	select {
	case <-errChan:
		log.Errorf("Received an error - shutting down")
	case s := <-sig:
		log.Warningf("Received a termination signal: %v", s)
	}
}

type eventDiff struct {
	Path     string
	PathItem *spec.PathItem
}

func convertSpecDiffToEventDiff(diff *_spec.APIDiff) (originalRet, modifiedRet []byte, err error) {
	original := eventDiff{
		Path:     diff.Path,
		PathItem: diff.OriginalPathItem,
	}
	modified := eventDiff{
		Path:     diff.Path,
		PathItem: diff.ModifiedPathItem,
	}
	originalRet, err = yaml.Marshal(original)
	if err != nil {
		return nil, nil, fmt.Errorf("failed marshal original: %v", err)
	}
	modifiedRet, err = yaml.Marshal(modified)
	if err != nil {
		return nil, nil, fmt.Errorf("failed marshal modified: %v", err)
	}

	return originalRet, modifiedRet, nil
}

func (b *Backend) handleHTTPTrace(ctx context.Context, trace *pluginsmodels.Telemetry) error {
	var reconstructedDiff *_spec.APIDiff
	var providedDiff *_spec.APIDiff
	var err error

	log.Debugf("Handling telemetry: %+v", trace)

	// we need to convert the trace to speculator trace format in order to call speculator methods on that trace.
	// from here on, we work only with speculator telemetry
	telemetry := ConvertModelsToSpeculatorTelemetry(trace)

	if telemetry.Request.Host == "" {
		headers := _spec.ConvertHeadersToMap(telemetry.Request.Common.Headers)
		if host, ok := headers["host"]; ok {
			telemetry.Request.Host = host
		}
	}

	telemetry.Request.Host, err = getHostname(telemetry.Request.Host)
	if err != nil {
		return fmt.Errorf("failed to get hostname from host: %v", err)
	}

	destInfo, err := _speculator.GetAddressInfoFromAddress(telemetry.DestinationAddress)
	if err != nil {
		return fmt.Errorf("failed to get destination info: %v", err)
	}
	destPort, err := strconv.Atoi(destInfo.Port)
	if err != nil {
		return fmt.Errorf("failed to convert destination port: %v", err)
	}
	srcInfo, err := _speculator.GetAddressInfoFromAddress(telemetry.SourceAddress)
	if err != nil {
		return fmt.Errorf("failed to get source info: %v", err)
	}

	// Initialize API info
	apiInfo := _database.APIInfo{
		Name:      telemetry.Request.Host,
		Port:      int64(destPort),
		Namespace: trace.DestinationNamespace,
	}

	// Set API Info type
	if b.monitor.IsInternalCIDR(destInfo.IP) {
		apiInfo.Type = models.APITypeINTERNAL
	} else {
		apiInfo.Type = models.APITypeEXTERNAL
	}

	isNonAPI := isNonAPI(telemetry)
	// Don't link non APIs to an API in the inventory
	if !isNonAPI {
		// lock the API inventory to avoid creating API entries twice on trace handling races
		b.apiInventoryLock.Lock()
		if err := b.dbHandler.APIInventoryTable().FirstOrCreate(&apiInfo); err != nil {
			b.apiInventoryLock.Unlock()
			return fmt.Errorf("failed to get or create API info: %v", err)
		}
		b.apiInventoryLock.Unlock()
		log.Infof("API Info in DB: %+v", apiInfo)

		// Handle trace telemetry by Speculator
		specKey := _speculator.GetSpecKey(telemetry.Request.Host, destInfo.Port)
		if b.speculator.HasProvidedSpec(specKey) {
			providedDiff, err = b.speculator.DiffTelemetry(telemetry, _spec.DiffSourceProvided)
			if err != nil {
				return fmt.Errorf("failed to diff telemetry against provided spec: %v", err)
			}
		}
		if b.speculator.HasApprovedSpec(specKey) {
			reconstructedDiff, err = b.speculator.DiffTelemetry(telemetry, _spec.DiffSourceReconstructed)
			if err != nil {
				return fmt.Errorf("failed to diff telemetry against approved spec: %v", err)
			}
		} else {
			err := b.speculator.LearnTelemetry(telemetry)
			if err != nil {
				return fmt.Errorf("failed to learn telemetry: %v", err)
			}
		}
	}

	// Update API event in DB
	statusCode, err := strconv.Atoi(telemetry.Response.StatusCode)
	if err != nil {
		return fmt.Errorf("failed to convert status code: %v", err)
	}

	path, query := _spec.GetPathAndQuery(telemetry.Request.Path)

	event := &_database.APIEvent{
		APIInfoID:       apiInfo.ID,
		Time:            strfmt.DateTime(time.Now().UTC()),
		Method:          models.HTTPMethod(telemetry.Request.Method),
		RequestTime:     strfmt.DateTime(time.UnixMilli(trace.Request.Common.Time).UTC()),
		Path:            path,
		Query:           query,
		StatusCode:      int64(statusCode),
		SourceIP:        srcInfo.IP,
		DestinationIP:   destInfo.IP,
		DestinationPort: int64(destPort),
		HostSpecName:    telemetry.Request.Host,
		IsNonAPI:        isNonAPI,
		EventType:       apiInfo.Type,
	}

	reconstructedDiffType := models.DiffTypeNODIFF
	if reconstructedDiff != nil {
		if reconstructedDiff.Type != _spec.DiffTypeNoDiff {
			original, modified, err := convertSpecDiffToEventDiff(reconstructedDiff)
			if err != nil {
				return fmt.Errorf("failed to convert spec diff to event diff: %v", err)
			}
			event.HasReconstructedSpecDiff = true
			event.HasSpecDiff = true
			event.OldReconstructedSpec = string(original)
			event.NewReconstructedSpec = string(modified)
		}
		event.ReconstructedPathID = reconstructedDiff.PathID
		reconstructedDiffType = convertAPIDiffType(reconstructedDiff.Type)
	}

	providedDiffType := models.DiffTypeNODIFF
	if providedDiff != nil {
		if providedDiff.Type != _spec.DiffTypeNoDiff {
			original, modified, err := convertSpecDiffToEventDiff(providedDiff)
			if err != nil {
				return fmt.Errorf("failed to convert spec diff to event diff: %v", err)
			}
			event.HasProvidedSpecDiff = true
			event.HasSpecDiff = true
			event.OldProvidedSpec = string(original)
			event.NewProvidedSpec = string(modified)
		}
		event.ProvidedPathID = providedDiff.PathID
		providedDiffType = convertAPIDiffType(providedDiff.Type)
	}

	event.SpecDiffType = getHighestPrioritySpecDiffType(providedDiffType, reconstructedDiffType)

	b.dbHandler.APIEventsTable().CreateAPIEvent(event)

	b.modules.EventNotify(ctx, &modules.Event{APIEvent: event, Telemetry: trace})

	return nil
}

func convertAPIDiffType(diffType _spec.DiffType) models.DiffType {
	switch diffType {
	case _spec.DiffTypeNoDiff:
		return models.DiffTypeNODIFF
	case _spec.DiffTypeShadowDiff:
		return models.DiffTypeSHADOWDIFF
	case _spec.DiffTypeZombieDiff:
		return models.DiffTypeZOMBIEDIFF
	case _spec.DiffTypeGeneralDiff:
		return models.DiffTypeGENERALDIFF
	default:
		log.Warnf("Unknown diff type: %v", diffType)
	}

	return models.DiffTypeNODIFF
}

//nolint:gomnd
var diffTypePriority = map[models.DiffType]int{
	// starting from 1 since unknown type will return 0
	models.DiffTypeNODIFF:      1,
	models.DiffTypeGENERALDIFF: 2,
	models.DiffTypeSHADOWDIFF:  3,
	models.DiffTypeZOMBIEDIFF:  4,
}

// getHighestPrioritySpecDiffType will return the type with the highest priority.
func getHighestPrioritySpecDiffType(providedDiffType, reconstructedDiffType models.DiffType) models.DiffType {
	if diffTypePriority[providedDiffType] > diffTypePriority[reconstructedDiffType] {
		return providedDiffType
	}

	return reconstructedDiffType
}

// getHostname will return only hostname without scheme and port
// ex. https://example.org:8000 --> example.org.
func getHostname(host string) (string, error) {
	if !strings.Contains(host, "://") {
		// need to add scheme to host in order for url.Parse to parse properly
		host = "http://" + host
	}

	parsedHost, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("failed to parse host. host=%v: %v", host, err)
	}

	if parsedHost.Hostname() == "" {
		return "", fmt.Errorf("hostname is empty. host=%v", host)
	}

	return parsedHost.Hostname(), nil
}

const (
	contentTypeHeaderName      = "content-type"
	contentTypeApplicationJSON = "application/json"
)

func isNonAPI(telemetry *_spec.Telemetry) bool {
	respHeaders := _spec.ConvertHeadersToMap(telemetry.Response.Common.Headers)

	// If response content-type header is missing, we will classify it as API
	respContentType, ok := respHeaders[contentTypeHeaderName]
	if !ok {
		return false
	}

	mediaType, _, err := mime.ParseMediaType(respContentType)
	if err != nil {
		log.Errorf("Failed to parse response media type - classifying as non-API. Content-Type=%v: %v", respContentType, err)
		return true
	}

	// If response content-type is not application/json, need to classify the telemetry as non-api
	return !_mimeutils.IsApplicationJSONMediaType(mediaType)
}

func (b *Backend) startStateBackup(ctx context.Context) {
	go func() {
		stateBackupInterval := b.stateBackupInterval
		for {
			select {
			case <-ctx.Done():
				log.Debugf("Stopping state backup")
				return
			case <-time.After(stateBackupInterval):
				if err := b.speculator.EncodeState(b.stateBackupFileName); err != nil {
					log.Errorf("Failed to encode state: %v", err)
				}
			}
		}
	}()
}
