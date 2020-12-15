package sources

import (
	"context"
	"errors"
	"fmt"
	"github.com/hashicorp/go-multierror"
	"github.com/jitsucom/eventnative/destinations"
	"github.com/jitsucom/eventnative/drivers"
	"github.com/jitsucom/eventnative/events"
	"github.com/jitsucom/eventnative/logging"
	"github.com/jitsucom/eventnative/meta"
	"github.com/jitsucom/eventnative/metrics"
	"github.com/jitsucom/eventnative/resources"
	"github.com/jitsucom/eventnative/safego"
	"github.com/jitsucom/eventnative/storages"
	"github.com/panjf2000/ants/v2"
	"github.com/spf13/viper"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	marshallingErrorMsg              = `Error initializing source (see documentation: https://docs.eventnative.dev/configuration ): `
	serviceName                      = "sources"
	unknownSourceConfigurationFormat = "unknown format of sources configuration. Expected map, " +
		"json string representation or string starting with file:// or http(s)://"
)

type Service struct {
	io.Closer
	sync.RWMutex

	ctx     context.Context
	sources map[string]*Unit
	pool    *ants.PoolWithFunc

	destinationsService *destinations.Service
	metaStorage         meta.Storage
	monitorKeeper       storages.MonitorKeeper

	closed bool
}

//only for tests
func NewTestService() *Service {
	return &Service{}
}

func NewService(ctx context.Context, sources *viper.Viper, sourcesProvider string, destinationsService *destinations.Service,
	metaStorage meta.Storage, monitorKeeper storages.MonitorKeeper, poolSize int) (*Service, error) {

	service := &Service{
		ctx:     ctx,
		sources: map[string]*Unit{},

		destinationsService: destinationsService,
		metaStorage:         metaStorage,
		monitorKeeper:       monitorKeeper,
	}

	if sources == nil && sourcesProvider == "" {
		logging.Warnf("Sources aren't configured")
		return service, nil
	}

	if metaStorage.Type() == meta.DummyType {
		return nil, errors.New("Meta storage is required")
	}

	pool, err := ants.NewPoolWithFunc(poolSize, service.syncCollection)
	if err != nil {
		return nil, fmt.Errorf("Error creating goroutines pool: %v", err)
	}
	service.pool = pool
	defer service.startMonitoring()
	if sources != nil {
		sourceConfigs := make(map[string]drivers.SourceConfig)
		if err := sources.Unmarshal(&sourceConfigs); err != nil {
			return nil, err
		}
		service.initDrivers(sourceConfigs)
	} else {
		if err := service.loadSources(sourcesProvider); err != nil {
			return nil, err
		}
	}

	if len(service.sources) == 0 {
		logging.Errorf("Sources are empty")
	}

	return service, nil
}

func (s *Service) loadSources(sourcesProvider string) error {
	// Parse config as string
	reloadSec := viper.GetInt("server.sources_reload_sec")
	//var sourcesProvider string
	//err = sources.Unmarshal(sourcesProvider)
	//if err != nil {
	//	return err
	//}
	if strings.HasPrefix(sourcesProvider, "http://") || strings.HasPrefix(sourcesProvider, "https://") {
		resources.Watch(serviceName, sourcesProvider, resources.LoadFromHttp, s.updateSources, time.Duration(reloadSec)*time.Second)
	} else if strings.HasPrefix(sourcesProvider, "file://") {
		resources.Watch(serviceName, strings.Replace(sourcesProvider, "file://", "", 1), resources.LoadFromFile, s.updateSources, time.Duration(reloadSec)*time.Second)
	} else if strings.HasPrefix(sourcesProvider, "{") && strings.HasSuffix(sourcesProvider, "}") {
		sourcesConfig, err := parseFromBytes([]byte(sourcesProvider))
		if err != nil {
			return err
		}
		s.initDrivers(sourcesConfig)
	} else {
		return fmt.Errorf(unknownSourceConfigurationFormat)
	}
	return nil
}

func (s *Service) updateSources(payload []byte) {
	sourceConfigs, err := parseFromBytes(payload)
	if err != nil {
		logging.Errorf("Error updating sources: %v", err)
	} else {
		s.initDrivers(sourceConfigs)
	}
}

func (s *Service) initDrivers(sourceConfigs map[string]drivers.SourceConfig) {
	for name, sourceConfig := range sourceConfigs {
		driverPerCollection, err := drivers.Create(s.ctx, name, &sourceConfig)
		if err != nil {
			logging.Errorf("[%s] Error initializing source of type %s: %v", name, sourceConfig.Type, err)
			continue
		}
		s.Lock()
		s.sources[name] = &Unit{
			DriverPerCollection: driverPerCollection,
			DestinationIds:      sourceConfig.Destinations,
		}
		s.Unlock()

		logging.Infof("[%s] source has been initialized!", name)
	}
}

//startMonitoring run goroutine for setting pool size metrics every 20 seconds
func (s *Service) startMonitoring() {
	safego.RunWithRestart(func() {
		for {
			if s.closed {
				break
			}

			metrics.RunningSourcesGoroutines(s.pool.Running())
			metrics.FreeSourcesGoroutines(s.pool.Free())

			time.Sleep(20 * time.Second)
		}
	})
}

func (s *Service) Sync(sourceId string) (multiErr error) {
	s.RLock()
	sourceUnit, ok := s.sources[sourceId]
	s.RUnlock()

	if !ok {
		return errors.New("Source doesn't exist")
	}

	var destinationStorages []events.Storage
	for _, destinationId := range sourceUnit.DestinationIds {
		storageProxy, ok := s.destinationsService.GetStorageById(destinationId)
		if ok {
			storage, ok := storageProxy.Get()
			if ok {
				destinationStorages = append(destinationStorages, storage)
			} else {
				logging.SystemErrorf("Unable to get destination [%s] in source [%s]: destination isn't initialized", destinationId, sourceId)
			}
		} else {
			logging.SystemErrorf("Unable to get destination [%s] in source [%s]: doesn't exist", destinationId, sourceId)
		}

	}

	if len(destinationStorages) == 0 {
		return errors.New("Empty destinations")
	}

	for collection, driver := range sourceUnit.DriverPerCollection {
		identifier := sourceId + "_" + collection

		collectionLock, err := s.monitorKeeper.Lock(sourceId, collection)
		if err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("Error locking [%s] source [%s] collection: %v", sourceId, collection, err))
			continue
		}

		err = s.pool.Invoke(SyncTask{
			sourceId:     sourceId,
			collection:   collection,
			identifier:   identifier,
			driver:       driver,
			metaStorage:  s.metaStorage,
			destinations: destinationStorages,
			lock:         collectionLock,
		})
		if err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("Error running sync task goroutine [%s] source [%s] collection: %v", sourceId, collection, err))
			continue
		}
	}

	return
}

//GetStatus return status per collection
func (s *Service) GetStatus(sourceId string) (map[string]string, error) {
	s.RLock()
	sourceUnit, ok := s.sources[sourceId]
	s.RUnlock()

	if !ok {
		return nil, errors.New("Source doesn't exist")
	}

	statuses := map[string]string{}
	for collection, _ := range sourceUnit.DriverPerCollection {
		status, err := s.metaStorage.GetCollectionStatus(sourceId, collection)
		if err != nil {
			return nil, fmt.Errorf("Error getting collection status: %v", err)
		}

		statuses[collection] = status
	}

	return statuses, nil
}

//GetStatus return logs per collection
func (s *Service) GetLogs(sourceId string) (map[string]string, error) {
	s.RLock()
	sourceUnit, ok := s.sources[sourceId]
	s.RUnlock()

	if !ok {
		return nil, errors.New("Source doesn't exist")
	}

	logsMap := map[string]string{}
	for collection, _ := range sourceUnit.DriverPerCollection {
		log, err := s.metaStorage.GetCollectionLog(sourceId, collection)
		if err != nil {
			return nil, fmt.Errorf("Error getting collection logs: %v", err)
		}

		logsMap[collection] = log
	}

	return logsMap, nil
}

func (s *Service) syncCollection(i interface{}) {
	synctTask, ok := i.(SyncTask)
	if !ok {
		logging.SystemErrorf("Sync task has unknown type: %T", i)
		return
	}

	defer s.monitorKeeper.Unlock(synctTask.lock)
	synctTask.Sync()
}

func (s *Service) Close() error {
	s.closed = true

	if s.pool != nil {
		s.pool.Release()
	}

	return nil
}
