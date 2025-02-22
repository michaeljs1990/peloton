// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hostmgr

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mesos "github.com/uber/peloton/.gen/mesos/v1"
	mesos_maintenance "github.com/uber/peloton/.gen/mesos/v1/maintenance"
	mesos_master "github.com/uber/peloton/.gen/mesos/v1/master"
	sched "github.com/uber/peloton/.gen/mesos/v1/scheduler"
	hpb "github.com/uber/peloton/.gen/peloton/api/v0/host"
	"github.com/uber/peloton/.gen/peloton/api/v0/peloton"
	"github.com/uber/peloton/.gen/peloton/api/v0/task"
	"github.com/uber/peloton/.gen/peloton/api/v0/volume"
	pb_eventstream "github.com/uber/peloton/.gen/peloton/private/eventstream"
	"github.com/uber/peloton/.gen/peloton/private/hostmgr/hostsvc"

	hostsvcmocks "github.com/uber/peloton/.gen/peloton/private/hostmgr/hostsvc/mocks"
	"github.com/uber/peloton/pkg/common/queue"
	"github.com/uber/peloton/pkg/common/reservation"
	"github.com/uber/peloton/pkg/common/util"
	bin_packing "github.com/uber/peloton/pkg/hostmgr/binpacking"
	"github.com/uber/peloton/pkg/hostmgr/config"
	"github.com/uber/peloton/pkg/hostmgr/host"
	hm "github.com/uber/peloton/pkg/hostmgr/host/mocks"
	hostmgr_mesos_mocks "github.com/uber/peloton/pkg/hostmgr/mesos/mocks"
	mpb_mocks "github.com/uber/peloton/pkg/hostmgr/mesos/yarpc/encoding/mpb/mocks"
	"github.com/uber/peloton/pkg/hostmgr/metrics"
	"github.com/uber/peloton/pkg/hostmgr/offer/offerpool"
	qm "github.com/uber/peloton/pkg/hostmgr/queue/mocks"
	"github.com/uber/peloton/pkg/hostmgr/reserver"
	reserver_mocks "github.com/uber/peloton/pkg/hostmgr/reserver/mocks"
	"github.com/uber/peloton/pkg/hostmgr/summary"
	task_state_mocks "github.com/uber/peloton/pkg/hostmgr/task/mocks"
	"github.com/uber/peloton/pkg/hostmgr/watchevent"
	watchmocks "github.com/uber/peloton/pkg/hostmgr/watchevent/mocks"
	storage_mocks "github.com/uber/peloton/pkg/storage/mocks"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc/yarpcerrors"
	"golang.org/x/net/context"
)

const (
	_offerHoldTime = time.Minute * 5
	_streamID      = "streamID"
	_frameworkID   = "frameworkID"

	_perHostCPU  = 10.0
	_perHostMem  = 20.0
	_perHostDisk = 30.0

	_delta = 0.00001

	_testJobID  = "bca875f5-322a-4439-b0c9-63e3cf9f982e"
	_taskIDFmt  = _testJobID + "-%d-abcdef12-abcd-1234-5678-1234567890ab"
	_defaultCmd = "/bin/sh"

	_cpuName  = "cpus"
	_memName  = "mem"
	_diskName = "disk"
	_gpuName  = "gpus"

	_defaultResourceValue = 1
)

var (
	rootCtx      = context.Background()
	_testKey     = "testKey"
	_pelotonRole = "peloton"
)

func generateOffers(numOffers int) []*mesos.Offer {
	var offers []*mesos.Offer
	for i := 0; i < numOffers; i++ {
		oid := fmt.Sprintf("offer-%d", i)
		aid := fmt.Sprintf("agent-%d", i)
		hostname := fmt.Sprintf("hostname-%d", i)
		offers = append(offers, generateOfferWithResource(
			oid, aid, hostname, _perHostCPU, _perHostMem, _perHostDisk, 0.0))
	}
	return offers
}

func generateOfferWithResource(
	offerID string,
	agentID string,
	hostname string,
	cpu float64,
	mem float64,
	disk float64,
	gpu float64) *mesos.Offer {
	var resources []*mesos.Resource
	if cpu > 0 {
		resources = append(resources,
			util.NewMesosResourceBuilder().
				WithName("cpus").
				WithValue(cpu).
				Build(),
		)
	}
	if mem > 0 {
		resources = append(resources,
			util.NewMesosResourceBuilder().
				WithName("mem").
				WithValue(mem).
				Build(),
		)
	}
	if disk > 0 {
		resources = append(resources,
			util.NewMesosResourceBuilder().
				WithName("disk").
				WithValue(disk).
				Build(),
		)
	}
	if gpu > 0 {
		resources = append(resources,
			util.NewMesosResourceBuilder().
				WithName("gpus").
				WithValue(gpu).
				Build(),
		)
	}
	hostnameAttr := "name"
	textType := mesos.Value_TEXT
	return &mesos.Offer{
		Id:        &mesos.OfferID{Value: &offerID},
		AgentId:   &mesos.AgentID{Value: &agentID},
		Hostname:  &hostname,
		Resources: resources,
		Attributes: []*mesos.Attribute{
			{
				Name: &hostnameAttr,
				Text: &mesos.Value_Text{
					Value: &hostname,
				},
				Type: &textType,
			},
		},
	}
}

func generateLaunchableTasks(numTasks int) []*hostsvc.LaunchableTask {
	var tasks []*hostsvc.LaunchableTask
	for i := 0; i < numTasks; i++ {
		tid := fmt.Sprintf(_taskIDFmt, i)
		tmpCmd := _defaultCmd
		tasks = append(tasks, &hostsvc.LaunchableTask{
			TaskId: &mesos.TaskID{Value: &tid},
			Config: &task.TaskConfig{
				Name: fmt.Sprintf("name-%d", i),
				Resource: &task.ResourceConfig{
					CpuLimit:    _perHostCPU,
					MemLimitMb:  _perHostMem,
					DiskLimitMb: _perHostDisk,
				},
				Command: &mesos.CommandInfo{
					Value: &tmpCmd,
				},
			},
			Volume: &hostsvc.Volume{
				Resource: util.NewMesosResourceBuilder().
					WithName("disk").
					WithValue(1.0).
					Build(),
				ContainerPath: "test",
				Id: &peloton.VolumeID{
					Value: "volumeid",
				},
			},
		})
	}
	return tasks
}

type HostMgrHandlerTestSuite struct {
	suite.Suite

	ctx                    context.Context
	ctrl                   *gomock.Controller
	testScope              tally.TestScope
	schedulerClient        *mpb_mocks.MockSchedulerClient
	masterOperatorClient   *mpb_mocks.MockMasterOperatorClient
	provider               *hostmgr_mesos_mocks.MockFrameworkInfoProvider
	volumeStore            *storage_mocks.MockPersistentVolumeStore
	pool                   offerpool.Pool
	handler                *ServiceHandler
	frameworkID            *mesos.FrameworkID
	mesosDetector          *hostmgr_mesos_mocks.MockMasterDetector
	maintenanceQueue       *qm.MockMaintenanceQueue
	drainingMachines       []*mesos.MachineID
	downMachines           []*mesos.MachineID
	maintenanceHostInfoMap *hm.MockMaintenanceHostInfoMap
	taskStateManager       *task_state_mocks.MockStateManager
	watchProcessor         *watchmocks.MockWatchProcessor
	watchEventServer       *hostsvcmocks.MockInternalHostServiceServiceWatchEventYARPCServer
}

func (suite *HostMgrHandlerTestSuite) SetupSuite() {
	drainingHostname := "draininghost"
	drainingIP := "172.17.0.6"
	drainingMachine := &mesos.MachineID{
		Hostname: &drainingHostname,
		Ip:       &drainingIP,
	}
	suite.drainingMachines = append(suite.drainingMachines, drainingMachine)

	downHostname := "downhost"
	downIP := "172.17.0.7"
	downMachine := &mesos.MachineID{
		Hostname: &downHostname,
		Ip:       &downIP,
	}

	suite.downMachines = append(suite.downMachines, downMachine)
	bin_packing.Init()
}

func (suite *HostMgrHandlerTestSuite) SetupTest() {
	suite.ctx = context.Background()
	suite.ctrl = gomock.NewController(suite.T())
	suite.testScope = tally.NewTestScope("", map[string]string{})
	suite.schedulerClient = mpb_mocks.NewMockSchedulerClient(suite.ctrl)
	suite.masterOperatorClient = mpb_mocks.NewMockMasterOperatorClient(suite.ctrl)
	suite.provider = hostmgr_mesos_mocks.NewMockFrameworkInfoProvider(suite.ctrl)
	suite.volumeStore = storage_mocks.NewMockPersistentVolumeStore(suite.ctrl)
	suite.mesosDetector = hostmgr_mesos_mocks.NewMockMasterDetector(suite.ctrl)
	suite.taskStateManager = task_state_mocks.NewMockStateManager(suite.ctrl)

	mockValidValue := new(string)
	*mockValidValue = _frameworkID
	mockValidFrameWorkID := &mesos.FrameworkID{
		Value: mockValidValue,
	}

	suite.frameworkID = mockValidFrameWorkID
	suite.pool = offerpool.NewOfferPool(
		_offerHoldTime,
		suite.schedulerClient,
		offerpool.NewMetrics(suite.testScope.SubScope("offer")),
		nil,               /* frameworkInfoProvider */
		suite.volumeStore, /* volumeStore */
		[]string{},        /*scarce_resource_types*/
		[]string{},        /*slack_resource_types*/
		bin_packing.GetRankerByName("FIRST_FIT"),
		time.Duration(30*time.Second),
	)

	suite.maintenanceQueue = qm.NewMockMaintenanceQueue(suite.ctrl)
	suite.maintenanceHostInfoMap = hm.NewMockMaintenanceHostInfoMap(suite.ctrl)
	suite.watchProcessor = watchmocks.NewMockWatchProcessor(suite.ctrl)
	suite.watchEventServer = hostsvcmocks.NewMockInternalHostServiceServiceWatchEventYARPCServer(suite.ctrl)
	suite.handler = &ServiceHandler{
		schedulerClient:        suite.schedulerClient,
		operatorMasterClient:   suite.masterOperatorClient,
		metrics:                metrics.NewMetrics(suite.testScope),
		offerPool:              suite.pool,
		frameworkInfoProvider:  suite.provider,
		volumeStore:            suite.volumeStore,
		mesosDetector:          suite.mesosDetector,
		maintenanceQueue:       suite.maintenanceQueue,
		maintenanceHostInfoMap: suite.maintenanceHostInfoMap,
		taskStateManager:       suite.taskStateManager,
		watchProcessor:         suite.watchProcessor,
	}
	suite.handler.reserver = reserver.NewReserver(
		metrics.NewMetrics(suite.testScope),
		&config.Config{},
		suite.pool)
}

func (suite *HostMgrHandlerTestSuite) TearDownTest() {
	log.Debug("tearing down")
}

func (suite *HostMgrHandlerTestSuite) assertGaugeValues(
	values map[string]float64) {

	suite.pool.RefreshGaugeMaps()
	gauges := suite.testScope.Snapshot().Gauges()

	for key, value := range values {
		g, ok := gauges[key]
		suite.True(ok, "Snapshot %v does not have key %s", gauges, key)
		suite.InDelta(
			value,
			g.Value(),
			_delta,
			"Expected value %f does not match on key %s, full snapshot: %v",
			value,
			key,
			gauges,
		)
	}
}

// helper function to check particular set of resource quantity gauges
// matches number of hosts with default amount of resources.
func (suite *HostMgrHandlerTestSuite) checkResourcesGauges(
	numHosts int,
	status string,
) {
	values := map[string]float64{
		fmt.Sprintf("offer.pool.%s.cpu+", status):  float64(numHosts * _perHostCPU),
		fmt.Sprintf("offer.pool.%s.mem+", status):  float64(numHosts * _perHostMem),
		fmt.Sprintf("offer.pool.%s.disk+", status): float64(numHosts * _perHostDisk),
	}
	suite.assertGaugeValues(values)
}

func (suite *HostMgrHandlerTestSuite) acquireHostOffers(numOffers int) (
	*hostsvc.AcquireHostOffersResponse,
	error) {
	suite.pool.AddOffers(context.Background(), generateOffers(numOffers))

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(numOffers, "ready")
	suite.checkResourcesGauges(0, "placing")

	// Matching constraint.
	acquireReq := &hostsvc.AcquireHostOffersRequest{
		Filter: &hostsvc.HostFilter{
			Quantity: &hostsvc.QuantityControl{
				MaxHosts: uint32(1),
			},
			ResourceConstraint: &hostsvc.ResourceConstraint{
				Minimum: &task.ResourceConfig{
					CpuLimit:    _perHostCPU,
					MemLimitMb:  _perHostMem,
					DiskLimitMb: _perHostDisk,
				},
			},
		},
	}
	return suite.handler.AcquireHostOffers(
		rootCtx,
		acquireReq,
	)
}

func (suite *HostMgrHandlerTestSuite) TestGetOutstandingOffers() {
	defer suite.ctrl.Finish()

	resp, _ := suite.handler.GetOutstandingOffers(rootCtx, &hostsvc.GetOutstandingOffersRequest{})
	suite.Equal(resp.GetError().GetNoOffers().Message, "no offers present in offer pool")

	numHosts := 5
	suite.pool.AddOffers(context.Background(), generateOffers(numHosts))
	resp, _ = suite.handler.GetOutstandingOffers(rootCtx, &hostsvc.GetOutstandingOffersRequest{})
	suite.Equal(len(resp.Offers), numHosts)
}

func (suite *HostMgrHandlerTestSuite) TestGetHostsByQueryNoOffers() {
	defer suite.ctrl.Finish()

	req := &hostsvc.GetHostsByQueryRequest{
		Resource: &task.ResourceConfig{
			CpuLimit: 1.0,
			GpuLimit: 2.0,
		},
		Hostnames: []string{"host1", "host2"},
	}
	resp, _ := suite.handler.GetHostsByQuery(rootCtx, req)
	suite.Equal(0, len(resp.Hosts))
}

func (suite *HostMgrHandlerTestSuite) TestGetHostsByQueryAllHosts() {
	defer suite.ctrl.Finish()

	req := &hostsvc.GetHostsByQueryRequest{
		Resource: &task.ResourceConfig{
			CpuLimit: 0.0,
			GpuLimit: 0.0,
		},
	}
	numHosts := 5
	suite.pool.AddOffers(context.Background(), generateOffers(numHosts))
	resp, _ := suite.handler.GetHostsByQuery(rootCtx, req)
	suite.Equal(numHosts, len(resp.Hosts))
}

func (suite *HostMgrHandlerTestSuite) TestGetHostsByQueryFilterHostnames() {
	defer suite.ctrl.Finish()

	req := &hostsvc.GetHostsByQueryRequest{
		Hostnames: []string{"hostname-0", "hostname-2"},
	}
	var offers []*mesos.Offer
	offers = append(offers, generateOfferWithResource(
		"offer-0", "agent-0", "hostname-0", 1.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-1", "agent-1", "hostname-1", 3.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-2", "agent-2", "hostname-2", 1.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-3", "agent-3", "hostname-3", 3.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-4", "agent-4", "hostname-4", 4.0, _perHostMem, _perHostDisk, 3.0))
	offers = append(offers, generateOfferWithResource(
		"offer-5", "agent-5", "hostname-5", 4.0, _perHostMem, _perHostDisk, 0.0))

	suite.pool.AddOffers(context.Background(), offers)
	resp, _ := suite.handler.GetHostsByQuery(rootCtx, req)
	suite.Equal(2, len(resp.Hosts))
	sort.Slice(resp.Hosts, func(i, j int) bool {
		return strings.Compare(resp.Hosts[i].Hostname, resp.Hosts[j].Hostname) < 0
	})

	suite.Equal("hostname-0", resp.Hosts[0].Hostname)
	suite.Equal("hostname-2", resp.Hosts[1].Hostname)
}

func (suite *HostMgrHandlerTestSuite) TestGetHostsByQueryGreaterThanEqualTo() {
	defer suite.ctrl.Finish()

	req := &hostsvc.GetHostsByQueryRequest{
		Resource: &task.ResourceConfig{
			CpuLimit: 3.0,
			GpuLimit: 2.0,
		},
		CmpLess: false,
	}
	var offers []*mesos.Offer
	offers = append(offers, generateOfferWithResource(
		"offer-0", "agent-0", "hostname-0", 1.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-1", "agent-1", "hostname-1", 3.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-2", "agent-2", "hostname-2", 1.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-3", "agent-3", "hostname-3", 3.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-4", "agent-4", "hostname-4", 4.0, _perHostMem, _perHostDisk, 3.0))
	offers = append(offers, generateOfferWithResource(
		"offer-5", "agent-5", "hostname-5", 4.0, _perHostMem, _perHostDisk, 0.0))

	suite.pool.AddOffers(context.Background(), offers)
	resp, _ := suite.handler.GetHostsByQuery(rootCtx, req)
	suite.Equal(2, len(resp.Hosts))
	sort.Slice(resp.Hosts, func(i, j int) bool {
		return strings.Compare(resp.Hosts[i].Hostname, resp.Hosts[j].Hostname) < 0
	})

	suite.Equal("hostname-3", resp.Hosts[0].Hostname)
	suite.Equal("hostname-4", resp.Hosts[1].Hostname)
}

func (suite *HostMgrHandlerTestSuite) TestGetHostsByQueryGreaterThanEqualToCpuOnly() {
	defer suite.ctrl.Finish()

	req := &hostsvc.GetHostsByQueryRequest{
		Resource: &task.ResourceConfig{
			CpuLimit: 3.0,
			GpuLimit: 0.0,
		},
		CmpLess: false,
	}
	var offers []*mesos.Offer
	offers = append(offers, generateOfferWithResource(
		"offer-0", "agent-0", "hostname-0", 1.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-1", "agent-1", "hostname-1", 3.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-2", "agent-2", "hostname-2", 1.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-3", "agent-3", "hostname-3", 3.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-4", "agent-4", "hostname-4", 4.0, _perHostMem, _perHostDisk, 3.0))
	offers = append(offers, generateOfferWithResource(
		"offer-5", "agent-5", "hostname-5", 4.0, _perHostMem, _perHostDisk, 0.0))

	suite.pool.AddOffers(context.Background(), offers)
	resp, _ := suite.handler.GetHostsByQuery(rootCtx, req)
	suite.Equal(4, len(resp.Hosts))
	sort.Slice(resp.Hosts, func(i, j int) bool {
		return strings.Compare(resp.Hosts[i].Hostname, resp.Hosts[j].Hostname) < 0
	})

	suite.Equal("hostname-1", resp.Hosts[0].Hostname)
	suite.Equal("hostname-3", resp.Hosts[1].Hostname)
	suite.Equal("hostname-4", resp.Hosts[2].Hostname)
	suite.Equal("hostname-5", resp.Hosts[3].Hostname)
}

func (suite *HostMgrHandlerTestSuite) TestGetHostsByQueryLessThan() {
	defer suite.ctrl.Finish()

	req := &hostsvc.GetHostsByQueryRequest{
		Resource: &task.ResourceConfig{
			CpuLimit: 3.0,
			GpuLimit: 2.0,
		},
		CmpLess: true,
	}
	var offers []*mesos.Offer
	offers = append(offers, generateOfferWithResource(
		"offer-0", "agent-0", "hostname-0", 1.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-1", "agent-1", "hostname-1", 3.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-2", "agent-2", "hostname-2", 1.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-3", "agent-3", "hostname-3", 3.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-4", "agent-4", "hostname-4", 4.0, _perHostMem, _perHostDisk, 3.0))
	offers = append(offers, generateOfferWithResource(
		"offer-5", "agent-5", "hostname-5", 4.0, _perHostMem, _perHostDisk, 0.0))

	suite.pool.AddOffers(context.Background(), offers)
	resp, _ := suite.handler.GetHostsByQuery(rootCtx, req)

	for _, host := range resp.Hosts {
		fmt.Println("BLAH", host.Hostname)
	}

	suite.Equal(1, len(resp.Hosts))
	sort.Slice(resp.Hosts, func(i, j int) bool {
		return strings.Compare(resp.Hosts[i].Hostname, resp.Hosts[j].Hostname) < 0
	})

	suite.Equal("hostname-0", resp.Hosts[0].Hostname)
}

func (suite *HostMgrHandlerTestSuite) TestGetHostsByQueryNoHosts() {
	defer suite.ctrl.Finish()

	req := &hostsvc.GetHostsByQueryRequest{
		Resource: &task.ResourceConfig{
			CpuLimit: 10.0,
			GpuLimit: 10.0,
		},
		CmpLess: false,
	}
	var offers []*mesos.Offer
	offers = append(offers, generateOfferWithResource(
		"offer-0", "agent-0", "hostname-0", 1.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-1", "agent-1", "hostname-1", 3.0, _perHostMem, _perHostDisk, 1.0))
	offers = append(offers, generateOfferWithResource(
		"offer-2", "agent-2", "hostname-2", 1.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-3", "agent-3", "hostname-3", 3.0, _perHostMem, _perHostDisk, 2.0))
	offers = append(offers, generateOfferWithResource(
		"offer-4", "agent-4", "hostname-4", 4.0, _perHostMem, _perHostDisk, 3.0))
	offers = append(offers, generateOfferWithResource(
		"offer-5", "agent-5", "hostname-5", 4.0, _perHostMem, _perHostDisk, 0.0))

	suite.pool.AddOffers(context.Background(), offers)
	resp, _ := suite.handler.GetHostsByQuery(rootCtx, req)
	suite.Equal(0, len(resp.Hosts))
}

// This checks the happy case of acquire -> release -> acquire
// sequence and verifies that released resources can be used
// again by next acquire call.
func (suite *HostMgrHandlerTestSuite) TestAcquireReleaseHostOffers() {
	defer suite.ctrl.Finish()

	numHosts := 5
	suite.pool.AddOffers(context.Background(), generateOffers(numHosts))

	suite.checkResourcesGauges(numHosts, "ready")
	suite.checkResourcesGauges(0, "placing")

	// Empty constraint.
	acquiredResp, err := suite.handler.AcquireHostOffers(
		rootCtx,
		&hostsvc.AcquireHostOffersRequest{},
	)

	suite.Error(err)
	suite.NotNil(acquiredResp.GetError().GetInvalidHostFilter())

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["acquire_host_offers_invalid+"].Value())

	// Matching constraint.
	acquireReq := &hostsvc.AcquireHostOffersRequest{
		Filter: &hostsvc.HostFilter{
			Quantity: &hostsvc.QuantityControl{
				MaxHosts: uint32(numHosts * 2),
			},
			ResourceConstraint: &hostsvc.ResourceConstraint{
				Minimum: &task.ResourceConfig{
					CpuLimit:    _perHostCPU,
					MemLimitMb:  _perHostMem,
					DiskLimitMb: _perHostDisk,
				},
			},
		},
	}

	acquiredResp, err = suite.handler.AcquireHostOffers(
		rootCtx,
		acquireReq,
	)

	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	acquiredHostOffers := acquiredResp.GetHostOffers()
	suite.Equal(numHosts, len(acquiredHostOffers))

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["acquire_host_offers+"].Value())

	suite.Equal(
		int64(numHosts),
		suite.testScope.Snapshot().Counters()["acquire_host_offers_count+"].Value())

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(numHosts, "placing")

	// Call AcquireHostOffers again should not give us anything.
	acquiredResp, err = suite.handler.AcquireHostOffers(
		rootCtx,
		acquireReq,
	)

	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	suite.Equal(0, len(acquiredResp.GetHostOffers()))

	suite.Equal(
		int64(2),
		suite.testScope.Snapshot().Counters()["acquire_host_offers+"].Value())

	// Release previously acquired host offers.

	releaseReq := &hostsvc.ReleaseHostOffersRequest{
		HostOffers: acquiredHostOffers,
	}

	releaseResp, err := suite.handler.ReleaseHostOffers(
		rootCtx,
		releaseReq,
	)

	suite.NoError(err)
	suite.Nil(releaseResp.GetError())

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(numHosts, "ready")
	suite.checkResourcesGauges(0, "placing")

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["release_host_offers+"].Value())

	suite.Equal(
		int64(numHosts),
		suite.testScope.Snapshot().Counters()["release_hosts_count+"].Value())

	// Acquire again should return non empty result.
	acquiredResp, err = suite.handler.AcquireHostOffers(
		rootCtx,
		acquireReq,
	)

	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	suite.Equal(numHosts, len(acquiredResp.GetHostOffers()))
}

// This checks the happy case of acquire -> launch
// sequence.
func (suite *HostMgrHandlerTestSuite) TestAcquireAndLaunch() {
	defer suite.ctrl.Finish()

	// only create one host offer in this test.
	numHosts := 1
	acquiredResp, err := suite.acquireHostOffers(numHosts)
	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	acquiredHostOffers := acquiredResp.GetHostOffers()
	suite.Equal(1, len(acquiredHostOffers))
	suite.Equal(1, len(acquiredHostOffers[0].GetAttributes()))

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["acquire_host_offers+"].Value())

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(numHosts, "placing")

	// An empty launch request will trigger an error.
	launchReq := &hostsvc.LaunchTasksRequest{
		Hostname: acquiredHostOffers[0].GetHostname(),
		AgentId:  acquiredHostOffers[0].GetAgentId(),
		Tasks:    []*hostsvc.LaunchableTask{},
		Id:       acquiredHostOffers[0].GetId(),
	}

	launchResp, err := suite.handler.LaunchTasks(
		rootCtx,
		launchReq,
	)

	suite.Error(err)
	suite.NotNil(launchResp.GetError().GetInvalidArgument())

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["launch_tasks_invalid+"].Value())

	// Generate some launchable tasks.
	launchReq.Tasks = generateLaunchableTasks(1)

	gomock.InOrder(
		// Set expectations on provider
		suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
			suite.frameworkID),
		// Set expectations on provider
		suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(_streamID),
		// Set expectations on scheduler schedulerClient
		suite.schedulerClient.EXPECT().
			Call(
				gomock.Eq(_streamID),
				gomock.Any(),
			).
			Do(func(_ string, msg proto.Message) {
				// Verify clientCall message.
				call := msg.(*sched.Call)
				suite.Equal(sched.Call_ACCEPT, call.GetType())
				suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

				accept := call.GetAccept()
				suite.NotNil(accept)
				suite.Equal(1, len(accept.GetOfferIds()))
				suite.Equal("offer-0", accept.GetOfferIds()[0].GetValue())
				suite.Equal(1, len(accept.GetOperations()))
				operation := accept.GetOperations()[0]
				suite.Equal(
					mesos.Offer_Operation_LAUNCH,
					operation.GetType())
				launch := operation.GetLaunch()
				suite.NotNil(launch)
				suite.Equal(1, len(launch.GetTaskInfos()))
				suite.Equal(
					fmt.Sprintf(_taskIDFmt, 0),
					launch.GetTaskInfos()[0].GetTaskId().GetValue())
			}).
			Return(nil),
	)

	launchResp, err = suite.handler.LaunchTasks(
		rootCtx,
		launchReq,
	)

	suite.NoError(err)
	suite.Nil(launchResp.GetError())
	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["launch_tasks+"].Value())

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(0, "placing")
}

// This checks the case of acquire -> launch
// sequence when the target host is not the host held.
func (suite *HostMgrHandlerTestSuite) TestAcquireAndLaunchOnNonHeldTask() {
	defer suite.ctrl.Finish()

	numHosts := 1
	acquiredResp, err := suite.acquireHostOffers(numHosts)
	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	acquiredHostOffers := acquiredResp.GetHostOffers()
	suite.Equal(1, len(acquiredHostOffers))

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(numHosts, "placing")

	// An empty launch request will trigger an error.
	launchabelTask := generateLaunchableTasks(1)[0]
	launchReq := &hostsvc.LaunchTasksRequest{
		Hostname: acquiredHostOffers[0].GetHostname(),
		AgentId:  acquiredHostOffers[0].GetAgentId(),
		Tasks:    []*hostsvc.LaunchableTask{launchabelTask},
		Id:       acquiredHostOffers[0].GetId(),
	}

	// launch on host0 but host1 is held for the task
	suite.pool.AddOffers(context.Background(), generateOffers(2))
	hs1, err := suite.pool.GetHostSummary("hostname-1")
	suite.NoError(err)
	suite.NoError(
		suite.pool.HoldForTasks(hs1.GetHostname(),
			[]*peloton.TaskID{launchabelTask.GetId()}),
	)
	suite.Equal(hs1.GetHostStatus(), summary.HeldHost)

	gomock.InOrder(
		// Set expectations on provider
		suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
			suite.frameworkID),
		// Set expectations on provider
		suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(_streamID),
		// Set expectations on scheduler schedulerClient
		suite.schedulerClient.EXPECT().
			Call(
				gomock.Eq(_streamID),
				gomock.Any(),
			).
			Do(func(_ string, msg proto.Message) {
				// Verify clientCall message.
				call := msg.(*sched.Call)
				suite.Equal(sched.Call_ACCEPT, call.GetType())
				suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

				accept := call.GetAccept()
				suite.NotNil(accept)
				suite.Equal(1, len(accept.GetOfferIds()))
				suite.Equal("offer-0", accept.GetOfferIds()[0].GetValue())
				suite.Equal(1, len(accept.GetOperations()))
				operation := accept.GetOperations()[0]
				suite.Equal(
					mesos.Offer_Operation_LAUNCH,
					operation.GetType())
				launch := operation.GetLaunch()
				suite.NotNil(launch)
				suite.Equal(1, len(launch.GetTaskInfos()))
				suite.Equal(
					fmt.Sprintf(_taskIDFmt, 0),
					launch.GetTaskInfos()[0].GetTaskId().GetValue())
			}).
			Return(nil),
	)

	launchResp, err := suite.handler.LaunchTasks(
		rootCtx,
		launchReq,
	)

	suite.NoError(err)
	suite.Nil(launchResp.GetError().GetInvalidArgument())
	// the host should go back to ready state
	suite.Equal(hs1.GetHostStatus(), summary.ReadyHost)
}

// This checks the happy case of acquire -> launch sequence using offer operations.
func (suite *HostMgrHandlerTestSuite) TestAcquireAndLaunchOperation() {
	defer suite.ctrl.Finish()

	// only create one host offer in this test.
	numHosts := 1
	acquiredResp, err := suite.acquireHostOffers(numHosts)

	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	acquiredHostOffers := acquiredResp.GetHostOffers()
	suite.Equal(1, len(acquiredHostOffers))

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["acquire_host_offers+"].Value())

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(numHosts, "placing")

	launchOperation := &hostsvc.OfferOperation{
		Type: hostsvc.OfferOperation_LAUNCH,
		Launch: &hostsvc.OfferOperation_Launch{
			Tasks: []*hostsvc.LaunchableTask{},
		},
	}
	// An empty launch request will trigger an error.
	operationReq := &hostsvc.OfferOperationsRequest{
		Hostname: acquiredHostOffers[0].GetHostname(),
		Operations: []*hostsvc.OfferOperation{
			launchOperation,
		},
		Id: acquiredHostOffers[0].GetId(),
	}

	operationResp, err := suite.handler.OfferOperations(
		rootCtx,
		operationReq,
	)

	suite.NoError(err)
	suite.NotNil(operationResp.GetError().GetInvalidArgument())

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["offer_operations_invalid+"].Value())

	// Generate some launchable tasks.
	operationReq.Operations[0].Launch.Tasks = generateLaunchableTasks(1)

	gomock.InOrder(
		// Set expectations on provider
		suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
			suite.frameworkID),
		// Set expectations on provider
		suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(_streamID),
		// Set expectations on scheduler schedulerClient
		suite.schedulerClient.EXPECT().
			Call(
				gomock.Eq(_streamID),
				gomock.Any(),
			).
			Do(func(_ string, msg proto.Message) {
				// Verify clientCall message.
				call := msg.(*sched.Call)
				suite.Equal(sched.Call_ACCEPT, call.GetType())
				suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

				accept := call.GetAccept()
				suite.NotNil(accept)
				suite.Equal(1, len(accept.GetOfferIds()))
				suite.Equal("offer-0", accept.GetOfferIds()[0].GetValue())
				suite.Equal(1, len(accept.GetOperations()))
				operation := accept.GetOperations()[0]
				suite.Equal(
					mesos.Offer_Operation_LAUNCH,
					operation.GetType())
				launch := operation.GetLaunch()
				suite.NotNil(launch)
				suite.Equal(1, len(launch.GetTaskInfos()))
				suite.Equal(
					fmt.Sprintf(_taskIDFmt, 0),
					launch.GetTaskInfos()[0].GetTaskId().GetValue())
			}).
			Return(nil),
	)

	operationResp, err = suite.handler.OfferOperations(
		rootCtx,
		operationReq,
	)

	suite.NoError(err)
	suite.Nil(operationResp.GetError())
	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["offer_operations+"].Value())

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(0, "placing")
}

// Test happy case of shutdown executor
func (suite *HostMgrHandlerTestSuite) TestShutdownExecutors() {
	defer suite.ctrl.Finish()

	e1 := "e1"
	a1 := "a1"
	e2 := "e2"
	a2 := "a2"

	shutdownExecutors := []*hostsvc.ExecutorOnAgent{
		{
			ExecutorId: &mesos.ExecutorID{Value: &e1},
			AgentId:    &mesos.AgentID{Value: &a1},
		},
		{
			ExecutorId: &mesos.ExecutorID{Value: &e2},
			AgentId:    &mesos.AgentID{Value: &a2},
		},
	}
	shutdownReq := &hostsvc.ShutdownExecutorsRequest{
		Executors: shutdownExecutors,
	}

	shutdownedExecutors := make(map[string]string)
	mockMutex := &sync.Mutex{}

	// Set expectations on provider
	suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
		suite.frameworkID,
	).Times(2)
	suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(
		_streamID,
	).Times(2)

	suite.schedulerClient.EXPECT().
		Call(
			gomock.Eq(_streamID),
			gomock.Any(),
		).
		Do(func(_ string, msg proto.Message) {
			// Verify clientCall message.
			call := msg.(*sched.Call)
			suite.Equal(sched.Call_SHUTDOWN, call.GetType())
			suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

			aid := call.GetShutdown().GetAgentId()
			eid := call.GetShutdown().GetExecutorId()

			suite.NotNil(aid)
			suite.NotNil(eid)

			mockMutex.Lock()
			defer mockMutex.Unlock()
			shutdownedExecutors[eid.GetValue()] = aid.GetValue()
		}).
		Return(nil).
		Times(2)

	resp, err := suite.handler.ShutdownExecutors(rootCtx, shutdownReq)
	suite.NoError(err)
	suite.Nil(resp.GetError())
	suite.Equal(
		map[string]string{"e1": "a1", "e2": "a2"},
		shutdownedExecutors)

	suite.Equal(
		int64(2),
		suite.testScope.Snapshot().Counters()["shutdown_executors+"].Value())
}

// Test some failure cases of shutdown executor
func (suite *HostMgrHandlerTestSuite) TestShutdownExecutorsFailure() {
	defer suite.ctrl.Finish()

	e1 := "e1"
	a1 := "a1"

	testTable := []struct {
		executors    []*hostsvc.ExecutorOnAgent
		numExecutors int
		shutdownCall bool
		errMsg       string
		msg          string
	}{
		{
			executors:    nil,
			numExecutors: 0,
			shutdownCall: false,
			errMsg:       errEmptyExecutorList.Error(),
			msg:          "Test invalid shutdown executor request with no executors present",
		},
		{
			executors: []*hostsvc.ExecutorOnAgent{
				{
					ExecutorId: &mesos.ExecutorID{Value: &e1},
					AgentId:    nil,
				},
			},
			numExecutors: 1,
			shutdownCall: false,
			errMsg:       errEmptyAgentID.Error(),
			msg:          "Test empty Agent ID",
		},
		{
			executors: []*hostsvc.ExecutorOnAgent{
				{
					ExecutorId: nil,
					AgentId:    &mesos.AgentID{Value: &a1},
				},
			},
			numExecutors: 1,
			shutdownCall: false,
			errMsg:       errEmptyExecutorID.Error(),
			msg:          "Test empty Executor ID",
		},
		{
			executors: []*hostsvc.ExecutorOnAgent{
				{
					ExecutorId: &mesos.ExecutorID{Value: &e1},
					AgentId:    &mesos.AgentID{Value: &a1},
				},
			},
			numExecutors: 1,
			shutdownCall: true,
			errMsg:       "some error",
			msg:          "Failed Shutdown Executor Call",
		},
		{
			executors: []*hostsvc.ExecutorOnAgent{
				{
					ExecutorId: &mesos.ExecutorID{Value: &e1},
					AgentId:    &mesos.AgentID{Value: &a1},
				},
			},
			shutdownCall: true,
			numExecutors: 1,
			errMsg:       "",
			msg:          "Successfull Shutdown Executor Call",
		},
	}

	for _, tt := range testTable {
		shutdownReq := &hostsvc.ShutdownExecutorsRequest{
			Executors: tt.executors,
		}

		if tt.shutdownCall {
			suite.provider.EXPECT().GetFrameworkID(rootCtx).Return(suite.frameworkID)
			suite.provider.EXPECT().GetMesosStreamID(rootCtx).Return(_streamID)

			var err error
			err = nil
			if len(tt.errMsg) > 0 {
				err = errors.New(tt.errMsg)
			}

			callType := sched.Call_SHUTDOWN
			msg := &sched.Call{
				FrameworkId: suite.frameworkID,
				Type:        &callType,
				Shutdown: &sched.Call_Shutdown{
					ExecutorId: &mesos.ExecutorID{Value: &e1},
					AgentId:    &mesos.AgentID{Value: &a1},
				},
			}
			suite.schedulerClient.EXPECT().Call(_streamID, msg).Return(err)
		}

		resp, err := suite.handler.ShutdownExecutors(rootCtx, shutdownReq)
		suite.Equal(len(shutdownReq.GetExecutors()), tt.numExecutors)

		if !tt.shutdownCall {
			suite.Contains(resp.GetError().GetInvalidExecutors().Message, tt.errMsg)
		} else if tt.shutdownCall && len(tt.errMsg) > 0 {
			suite.NotNil(resp.GetError().GetShutdownFailure())
			suite.Equal(
				int64(1),
				suite.testScope.
					Snapshot().Counters()["shutdown_executors_fail+"].Value())
		} else {
			suite.NoError(err)
			suite.Equal(
				int64(1),
				suite.testScope.
					Snapshot().Counters()["shutdown_executors+"].Value())
		}
	}
}

// TestKillAndReserveTaskWithoutHostSummary test the case that failing
// to hold the host on kill should not return an error to user
func (suite *HostMgrHandlerTestSuite) TestKillAndReserveTaskWithoutHostSummary() {
	defer suite.ctrl.Finish()

	t1 := "t1"
	t2 := "t2"
	entries := []*hostsvc.KillAndReserveTasksRequest_Entry{
		{TaskId: &mesos.TaskID{Value: &t1}, HostToReserve: "hostname-0"},
		{TaskId: &mesos.TaskID{Value: &t2}, HostToReserve: "hostname-1"},
	}
	killAndReserveReq := &hostsvc.KillAndReserveTasksRequest{
		Entries: entries,
	}
	killedTaskIds := make(map[string]bool)
	mockMutex := &sync.Mutex{}

	// Set expectations on provider
	suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
		suite.frameworkID,
	).Times(2)
	suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(
		_streamID,
	).Times(2)

	// Set expectations on scheduler schedulerClient
	suite.schedulerClient.EXPECT().
		Call(
			gomock.Eq(_streamID),
			gomock.Any(),
		).
		Do(func(_ string, msg proto.Message) {
			// Verify clientCall message.
			call := msg.(*sched.Call)
			suite.Equal(sched.Call_KILL, call.GetType())
			suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

			tid := call.GetKill().GetTaskId()
			suite.NotNil(tid)
			mockMutex.Lock()
			defer mockMutex.Unlock()
			killedTaskIds[tid.GetValue()] = true
		}).
		Return(nil).
		Times(2)

	resp, err := suite.handler.KillAndReserveTasks(rootCtx, killAndReserveReq)
	suite.NoError(err)
	suite.Nil(resp.GetError())
	suite.Equal(
		map[string]bool{"t1": true, "t2": true},
		killedTaskIds)

	suite.Equal(
		int64(2),
		suite.testScope.Snapshot().Counters()["kill_tasks+"].Value())
}

// TestKillAndReserveTaskKillFailure test the case of fail to kill
// tasks after the host is placed on hold
func (suite *HostMgrHandlerTestSuite) TestKillAndReserveTaskKillFailure() {
	defer suite.ctrl.Finish()

	numHosts := 2
	suite.pool.AddOffers(context.Background(), generateOffers(numHosts))

	t1 := "t1"
	t2 := "t2"
	entries := []*hostsvc.KillAndReserveTasksRequest_Entry{
		{TaskId: &mesos.TaskID{Value: &t1}, HostToReserve: "hostname-0"},
		{TaskId: &mesos.TaskID{Value: &t2}, HostToReserve: "hostname-1"},
	}
	killAndReserveReq := &hostsvc.KillAndReserveTasksRequest{
		Entries: entries,
	}
	killedTaskIds := make(map[string]bool)
	mockMutex := &sync.Mutex{}

	// Set expectations on provider
	suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
		suite.frameworkID,
	).Times(2)
	suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(
		_streamID,
	).Times(2)

	// Set expectations on scheduler schedulerClient
	suite.schedulerClient.EXPECT().
		Call(
			gomock.Eq(_streamID),
			gomock.Any(),
		).
		Do(func(_ string, msg proto.Message) {
			// Verify clientCall message.
			call := msg.(*sched.Call)
			suite.Equal(sched.Call_KILL, call.GetType())
			suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

			tid := call.GetKill().GetTaskId()
			suite.NotNil(tid)
			mockMutex.Lock()
			defer mockMutex.Unlock()
			killedTaskIds[tid.GetValue()] = true
		}).
		Return(fmt.Errorf("test error")).
		Times(2)

	resp, err := suite.handler.KillAndReserveTasks(rootCtx, killAndReserveReq)
	suite.NoError(err)
	suite.NotNil(resp.GetError())
	suite.Equal(
		map[string]bool{"t1": true, "t2": true},
		killedTaskIds)

	suite.Equal(
		int64(0),
		suite.testScope.Snapshot().Counters()["kill_tasks+"].Value())

	hs0, err := suite.pool.GetHostSummary("hostname-0")
	suite.NoError(err)
	suite.Equal(hs0.GetHostStatus(), summary.ReadyHost)

	hs1, err := suite.pool.GetHostSummary("hostname-1")
	suite.NoError(err)
	suite.Equal(hs1.GetHostStatus(), summary.ReadyHost)
}

// TestKillAndReserveTask test the happy path
func (suite *HostMgrHandlerTestSuite) TestKillAndReserveTask() {
	defer suite.ctrl.Finish()

	numHosts := 2
	suite.pool.AddOffers(context.Background(), generateOffers(numHosts))

	t1 := "t1"
	t2 := "t2"
	entries := []*hostsvc.KillAndReserveTasksRequest_Entry{
		{TaskId: &mesos.TaskID{Value: &t1}, HostToReserve: "hostname-0"},
		{TaskId: &mesos.TaskID{Value: &t2}, HostToReserve: "hostname-1"},
	}
	killAndReserveReq := &hostsvc.KillAndReserveTasksRequest{
		Entries: entries,
	}
	killedTaskIds := make(map[string]bool)
	mockMutex := &sync.Mutex{}

	// Set expectations on provider
	suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
		suite.frameworkID,
	).Times(2)
	suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(
		_streamID,
	).Times(2)

	// Set expectations on scheduler schedulerClient
	suite.schedulerClient.EXPECT().
		Call(
			gomock.Eq(_streamID),
			gomock.Any(),
		).
		Do(func(_ string, msg proto.Message) {
			// Verify clientCall message.
			call := msg.(*sched.Call)
			suite.Equal(sched.Call_KILL, call.GetType())
			suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

			tid := call.GetKill().GetTaskId()
			suite.NotNil(tid)
			mockMutex.Lock()
			defer mockMutex.Unlock()
			killedTaskIds[tid.GetValue()] = true
		}).
		Return(nil).
		Times(2)

	resp, err := suite.handler.KillAndReserveTasks(rootCtx, killAndReserveReq)
	suite.NoError(err)
	suite.Nil(resp.GetError())
	suite.Equal(
		map[string]bool{"t1": true, "t2": true},
		killedTaskIds)

	suite.Equal(
		int64(2),
		suite.testScope.Snapshot().Counters()["kill_tasks+"].Value())

	hs0, err := suite.pool.GetHostSummary("hostname-0")
	suite.NoError(err)
	suite.Equal(hs0.GetHostStatus(), summary.HeldHost)

	hs1, err := suite.pool.GetHostSummary("hostname-1")
	suite.NoError(err)
	suite.Equal(hs1.GetHostStatus(), summary.HeldHost)
}

// Test happy case of killing task
func (suite *HostMgrHandlerTestSuite) TestKillTask() {
	defer suite.ctrl.Finish()

	t1 := "t1"
	t2 := "t2"
	taskIDs := []*mesos.TaskID{
		{Value: &t1},
		{Value: &t2},
	}
	killReq := &hostsvc.KillTasksRequest{
		TaskIds: taskIDs,
	}
	killedTaskIds := make(map[string]bool)
	mockMutex := &sync.Mutex{}

	// Set expectations on provider
	suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
		suite.frameworkID,
	).Times(2)
	suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(
		_streamID,
	).Times(2)

	// Set expectations on scheduler schedulerClient
	suite.schedulerClient.EXPECT().
		Call(
			gomock.Eq(_streamID),
			gomock.Any(),
		).
		Do(func(_ string, msg proto.Message) {
			// Verify clientCall message.
			call := msg.(*sched.Call)
			suite.Equal(sched.Call_KILL, call.GetType())
			suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

			tid := call.GetKill().GetTaskId()
			suite.NotNil(tid)
			mockMutex.Lock()
			defer mockMutex.Unlock()
			killedTaskIds[tid.GetValue()] = true
		}).
		Return(nil).
		Times(2)

	resp, err := suite.handler.KillTasks(rootCtx, killReq)
	suite.NoError(err)
	suite.Nil(resp.GetError())
	suite.Equal(
		map[string]bool{"t1": true, "t2": true},
		killedTaskIds)

	suite.Equal(
		int64(2),
		suite.testScope.Snapshot().Counters()["kill_tasks+"].Value())
}

// Test some failure cases of killing task
func (suite *HostMgrHandlerTestSuite) TestKillTaskFailure() {
	defer suite.ctrl.Finish()

	t1 := "t1"
	t2 := "t2"

	testTable := []struct {
		taskIDs     []*mesos.TaskID
		killRequest bool
		errMsg      string
		msg         string
	}{
		{
			taskIDs:     []*mesos.TaskID{},
			killRequest: false,
			errMsg:      "",
			msg:         "Test Kill Task Request w/o any TaskIDs",
		},
		{
			taskIDs: []*mesos.TaskID{
				{Value: &t1},
				{Value: &t2},
			},
			killRequest: true,
			errMsg:      "some error",
			msg:         "Failed Test Kill Task Request with TaskIDs",
		},
		{
			taskIDs: []*mesos.TaskID{
				{Value: &t1},
				{Value: &t2},
			},
			killRequest: true,
			errMsg:      "",
			msg:         "Success Test Kill Task Request with TaskIDs",
		},
	}

	for _, tt := range testTable {
		killReq := &hostsvc.KillTasksRequest{
			TaskIds: tt.taskIDs,
		}

		if tt.killRequest {
			var err error
			err = nil
			if len(tt.errMsg) > 0 {
				err = errors.New(tt.errMsg)
			}

			suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(suite.frameworkID).Times(2)
			suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(_streamID).Times(2)

			suite.schedulerClient.EXPECT().
				Call(
					gomock.Eq(_streamID),
					gomock.Any()).
				Do(func(_ string, msg proto.Message) {
					// Verify call message while process the kill task id into `killedTaskIds`
					call := msg.(*sched.Call)
					suite.Equal(sched.Call_KILL, call.GetType())
					suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

					tid := call.GetKill().GetTaskId()
					suite.NotNil(tid)
				}).
				Return(err).
				Times(2)
		}

		resp, _ := suite.handler.KillTasks(rootCtx, killReq)

		if !tt.killRequest {
			suite.NotNil(resp.GetError().GetInvalidTaskIDs())
		} else if tt.killRequest && len(tt.errMsg) > 0 {
			suite.NotNil(resp.GetError().GetKillFailure())
			suite.Equal(
				int64(2),
				suite.testScope.Snapshot().Counters()["kill_tasks_fail+"].Value())
		} else {
			suite.Equal(resp, &hostsvc.KillTasksResponse{})
			suite.Equal(
				int64(2),
				suite.testScope.Snapshot().Counters()["kill_tasks+"].Value())
		}
	}
}

func (suite *HostMgrHandlerTestSuite) TestServiceHandlerClusterCapacity() {
	scalerType := mesos.Value_SCALAR
	scalerVal := 200.0
	name := "cpus"

	loader := &host.Loader{
		OperatorClient:         suite.masterOperatorClient,
		Scope:                  suite.testScope,
		MaintenanceHostInfoMap: suite.maintenanceHostInfoMap,
	}
	numAgents := 2
	response := makeAgentsResponse(numAgents)
	suite.masterOperatorClient.EXPECT().Agents().Return(response, nil)
	suite.maintenanceHostInfoMap.EXPECT().
		GetDrainingHostInfos(gomock.Any()).
		Return([]*hpb.HostInfo{}).
		Times(len(response.GetAgents()))

	loader.Load(nil)

	tests := []struct {
		err         error
		response    []*mesos.Resource
		clientCall  bool
		frameworkID *mesos.FrameworkID
	}{
		{
			err:         errors.New("no resources configured"),
			response:    nil,
			clientCall:  true,
			frameworkID: suite.frameworkID,
		},
		{
			err: nil,
			response: []*mesos.Resource{
				{
					Name: &name,
					Scalar: &mesos.Value_Scalar{
						Value: &scalerVal,
					},
					Type: &scalerType,
				},
			},
			clientCall:  true,
			frameworkID: suite.frameworkID,
		},
		{
			err:         errors.New("unable to fetch framework ID"),
			clientCall:  true,
			frameworkID: nil,
		},
	}

	clusterCapacityReq := &hostsvc.ClusterCapacityRequest{}
	for _, tt := range tests {
		// Set expectations on provider interface
		suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(tt.frameworkID)

		if tt.clientCall {
			// Set expectations on the mesos operator client
			suite.masterOperatorClient.EXPECT().GetTasksAllocation(
				gomock.Any(),
			).Return(tt.response, tt.response, tt.err)
			suite.masterOperatorClient.EXPECT().GetQuota(gomock.Any()).Return(nil, nil)
		}

		// Make the cluster capacity API request
		resp, _ := suite.handler.ClusterCapacity(
			rootCtx,
			clusterCapacityReq,
		)

		if tt.err != nil {
			suite.NotNil(resp.Error)
			suite.Equal(
				tt.err.Error(),
				resp.Error.ClusterUnavailable.Message,
			)
			suite.Nil(resp.Resources)
			suite.Nil(resp.PhysicalResources)
		} else {
			suite.Nil(resp.Error)
			suite.NotNil(resp.Resources)
			suite.Equal(4, len(resp.PhysicalResources))
			for _, v := range resp.PhysicalResources {
				suite.Equal(v.Capacity, float64(numAgents))
			}
		}
	}
}

func (suite *HostMgrHandlerTestSuite) TestServiceHandlerClusterCapacityWithoutAgentMap() {
	defer suite.ctrl.Finish()

	clusterCapacityReq := &hostsvc.ClusterCapacityRequest{}
	response := &mesos_master.Response_GetAgents{
		Agents: []*mesos_master.Response_GetAgents_Agent{},
	}
	loader := &host.Loader{
		OperatorClient:         suite.masterOperatorClient,
		Scope:                  suite.testScope,
		MaintenanceHostInfoMap: hm.NewMockMaintenanceHostInfoMap(suite.ctrl),
	}
	suite.masterOperatorClient.EXPECT().Agents().Return(response, nil)
	loader.Load(nil)

	suite.provider.EXPECT().GetFrameworkID(rootCtx).Return(suite.frameworkID)

	scalerType := mesos.Value_SCALAR
	scalerVal := 200.0
	name := "cpus"
	suite.masterOperatorClient.EXPECT().GetTasksAllocation(gomock.Any()).Return([]*mesos.Resource{
		{
			Name: &name,
			Scalar: &mesos.Value_Scalar{
				Value: &scalerVal,
			},
			Type: &scalerType,
		},
	},
		[]*mesos.Resource{
			{
				Name: &name,
				Scalar: &mesos.Value_Scalar{
					Value: &scalerVal,
				},
				Type: &scalerType,
			},
		}, nil)

	// Make the cluster capacity API request
	resp, _ := suite.handler.ClusterCapacity(
		rootCtx,
		clusterCapacityReq,
	)

	suite.Equal(resp.Error.ClusterUnavailable.Message, "error getting host agentmap")
}

func (suite *HostMgrHandlerTestSuite) TestServiceHandlerClusterCapacityWithQuota() {
	scalerType := mesos.Value_SCALAR
	scalerVal := 200.0
	name := "cpus"
	quotaVal := 100.0

	loader := &host.Loader{
		OperatorClient:         suite.masterOperatorClient,
		Scope:                  suite.testScope,
		MaintenanceHostInfoMap: suite.maintenanceHostInfoMap,
	}
	numAgents := 2
	response := makeAgentsResponse(numAgents)
	suite.masterOperatorClient.EXPECT().Agents().Return(response, nil)
	suite.maintenanceHostInfoMap.EXPECT().
		GetDrainingHostInfos(gomock.Any()).
		Return([]*hpb.HostInfo{}).
		Times(len(response.GetAgents()))

	loader.Load(nil)

	clusterCapacityReq := &hostsvc.ClusterCapacityRequest{}

	responseAllocated := []*mesos.Resource{
		{
			Name: &name,
			Scalar: &mesos.Value_Scalar{
				Value: &scalerVal,
			},
			Type: &scalerType,
		},
	}

	responseQuota := []*mesos.Resource{
		{
			Name: &name,
			Scalar: &mesos.Value_Scalar{
				Value: &quotaVal,
			},
			Type: &scalerType,
		},
	}

	// Test GetQuota failure scenario
	suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(suite.frameworkID)
	suite.masterOperatorClient.EXPECT().GetTasksAllocation(gomock.Any()).Return(responseAllocated, responseAllocated, nil)
	suite.masterOperatorClient.EXPECT().GetQuota(gomock.Any()).Return(nil, errors.New("error getting quota"))
	resp, _ := suite.handler.ClusterCapacity(
		rootCtx,
		clusterCapacityReq,
	)
	suite.Equal(resp.Error.ClusterUnavailable.Message, "error getting quota")

	// Test GetQuota success scenario
	suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(suite.frameworkID)
	suite.masterOperatorClient.EXPECT().GetTasksAllocation(gomock.Any()).Return(responseAllocated, responseAllocated, nil)
	suite.masterOperatorClient.EXPECT().GetQuota(gomock.Any()).Return(responseQuota, nil)
	resp, _ = suite.handler.ClusterCapacity(
		rootCtx,
		clusterCapacityReq,
	)

	suite.Nil(resp.Error)
	suite.NotNil(resp.Resources)
	suite.Equal(4, len(resp.PhysicalResources))
	for _, v := range resp.PhysicalResources {
		if v.GetKind() == "cpu" {
			suite.Equal(v.Capacity, float64(quotaVal))
		}
	}
}

func (suite *HostMgrHandlerTestSuite) TestLaunchOperationWithReservedOffers() {
	defer suite.ctrl.Finish()

	volumeInfo := &volume.PersistentVolumeInfo{}

	suite.volumeStore.EXPECT().
		GetPersistentVolume(context.Background(), gomock.Any()).
		AnyTimes().
		Return(volumeInfo, nil)
	suite.volumeStore.EXPECT().
		UpdatePersistentVolume(context.Background(), volumeInfo).
		AnyTimes().
		Return(nil)

	gomock.InOrder(
		// Set expectations on provider
		suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
			suite.frameworkID),
		// Set expectations on provider
		suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(_streamID),
		// Set expectations on scheduler schedulerClient
		suite.schedulerClient.EXPECT().
			Call(
				gomock.Eq(_streamID),
				gomock.Any(),
			).
			Do(func(_ string, msg proto.Message) {
				// Verify clientCall message.
				call := msg.(*sched.Call)
				suite.Equal(sched.Call_ACCEPT, call.GetType())
				suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

				accept := call.GetAccept()
				suite.NotNil(accept)
				suite.Equal(1, len(accept.GetOfferIds()))
				suite.Equal("offer-0", accept.GetOfferIds()[0].GetValue())
				suite.Equal(1, len(accept.GetOperations()))
				launchOp := accept.GetOperations()[0]
				suite.Equal(
					mesos.Offer_Operation_LAUNCH,
					launchOp.GetType())
				launch := launchOp.GetLaunch()
				suite.NotNil(launch)
				suite.Equal(1, len(launch.GetTaskInfos()))
				suite.Equal(
					fmt.Sprintf(_taskIDFmt, 0),
					launch.GetTaskInfos()[0].GetTaskId().GetValue())
			}).
			Return(nil),
	)

	launchOperation := createHostLaunchOperation()

	operationReq := &hostsvc.OfferOperationsRequest{
		Hostname: "hostname-0",
		Operations: []*hostsvc.OfferOperation{
			launchOperation,
		},
	}

	reservedOffers := generateOffers(1)
	reservedOffer := reservedOffers[0]
	reservation := &mesos.Resource_ReservationInfo{
		Labels: createReservationLabels(),
	}
	diskInfo := &mesos.Resource_DiskInfo{
		Persistence: &mesos.Resource_DiskInfo_Persistence{
			Id: &_testKey,
		},
	}
	reservedResources := []*mesos.Resource{
		util.NewMesosResourceBuilder().
			WithName("cpus").
			WithValue(_perHostCPU).
			WithRole(_pelotonRole).
			WithReservation(reservation).
			Build(),
		util.NewMesosResourceBuilder().
			WithName("mem").
			WithValue(_perHostMem).
			WithReservation(reservation).
			WithRole(_pelotonRole).
			Build(),
		util.NewMesosResourceBuilder().
			WithName("disk").
			WithValue(_perHostDisk).
			WithRole(_pelotonRole).
			WithReservation(reservation).
			WithDisk(diskInfo).
			Build(),
	}
	reservedOffer.Resources = append(reservedOffer.Resources, reservedResources...)
	suite.pool.AddOffers(context.Background(), reservedOffers)

	operationResp, err := suite.handler.OfferOperations(
		rootCtx,
		operationReq,
	)

	suite.NoError(err)
	suite.Nil(operationResp.GetError())
	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["offer_operations+"].Value())
}

func (suite *HostMgrHandlerTestSuite) TestReserveCreateLaunchOperation() {
	defer suite.ctrl.Finish()

	// only create one host offer in this test.
	numHosts := 1
	suite.pool.AddOffers(context.Background(), generateOffers(numHosts))

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(numHosts, "ready")
	suite.checkResourcesGauges(0, "placing")

	// Matching constraint.
	acquireReq := getAcquireHostOffersRequest()
	acquiredResp, err := suite.handler.AcquireHostOffers(
		rootCtx,
		acquireReq,
	)

	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	acquiredHostOffers := acquiredResp.GetHostOffers()
	suite.Equal(1, len(acquiredHostOffers))

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["acquire_host_offers+"].Value())

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(numHosts, "placing")

	reserveOperation := createHostReserveOperation()
	createOperation := createHostCreateOperation()
	launchOperation := createHostLaunchOperation()

	// launch operation before reserve/create will trigger an error.
	operationReq := &hostsvc.OfferOperationsRequest{
		Hostname: acquiredHostOffers[0].GetHostname(),
		Operations: []*hostsvc.OfferOperation{
			reserveOperation,
			launchOperation,
			createOperation,
		},
		Id: acquiredHostOffers[0].GetId(),
	}

	operationResp, err := suite.handler.OfferOperations(
		rootCtx,
		operationReq,
	)

	suite.NoError(err)
	suite.NotNil(operationResp.GetError().GetInvalidArgument())

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["offer_operations_invalid+"].Value())

	gomock.InOrder(
		suite.volumeStore.EXPECT().
			GetPersistentVolume(gomock.Any(), gomock.Any()).
			Return(nil, nil),

		suite.volumeStore.EXPECT().
			CreatePersistentVolume(gomock.Any(), gomock.Any()).
			Return(nil),

		// Set expectations on provider
		suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
			suite.frameworkID),
		// Set expectations on provider
		suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(_streamID),
		// Set expectations on scheduler schedulerClient
		suite.schedulerClient.EXPECT().
			Call(
				gomock.Eq(_streamID),
				gomock.Any(),
			).
			Do(func(_ string, msg proto.Message) {
				// Verify clientCall message.
				call := msg.(*sched.Call)
				suite.Equal(sched.Call_ACCEPT, call.GetType())
				suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

				accept := call.GetAccept()
				suite.NotNil(accept)
				suite.Equal(1, len(accept.GetOfferIds()))
				suite.Equal("offer-0", accept.GetOfferIds()[0].GetValue())
				suite.Equal(3, len(accept.GetOperations()))
				reserveOp := accept.GetOperations()[0]
				createOp := accept.GetOperations()[1]
				launchOp := accept.GetOperations()[2]
				suite.Equal(
					mesos.Offer_Operation_RESERVE,
					reserveOp.GetType())
				suite.Equal(
					mesos.Offer_Operation_CREATE,
					createOp.GetType())
				suite.Equal(
					mesos.Offer_Operation_LAUNCH,
					launchOp.GetType())
				launch := launchOp.GetLaunch()
				suite.NotNil(launch)
				suite.Equal(1, len(launch.GetTaskInfos()))
				suite.Equal(
					fmt.Sprintf(_taskIDFmt, 0),
					launch.GetTaskInfos()[0].GetTaskId().GetValue())
			}).
			Return(nil),
	)

	operationReq = &hostsvc.OfferOperationsRequest{
		Hostname: acquiredHostOffers[0].GetHostname(),
		Operations: []*hostsvc.OfferOperation{
			reserveOperation,
			createOperation,
			launchOperation,
		},
		Id: acquiredHostOffers[0].GetId(),
	}

	operationResp, err = suite.handler.OfferOperations(
		rootCtx,
		operationReq,
	)

	suite.NoError(err)
	suite.Nil(operationResp.GetError())
	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["offer_operations+"].Value())

	// TODO: Add check for number of HostOffers in placing state.
	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(0, "placing")
}

func (suite *HostMgrHandlerTestSuite) TestReserveCreateLaunchOperationWithCreatedVolume() {
	defer suite.ctrl.Finish()

	// only create one host offer in this test.
	numHosts := 1
	suite.pool.AddOffers(context.Background(), generateOffers(numHosts))

	// Matching constraint.
	acquireReq := getAcquireHostOffersRequest()
	acquiredResp, err := suite.handler.AcquireHostOffers(
		rootCtx,
		acquireReq,
	)

	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	acquiredHostOffers := acquiredResp.GetHostOffers()

	reserveOperation := createHostReserveOperation()
	createOperation := createHostCreateOperation()
	launchOperation := createHostLaunchOperation()

	volumeInfo := &volume.PersistentVolumeInfo{}

	gomock.InOrder(
		suite.volumeStore.EXPECT().
			GetPersistentVolume(gomock.Any(), gomock.Any()).
			Return(volumeInfo, nil),

		// Set expectations on provider
		suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
			suite.frameworkID),
		// Set expectations on provider
		suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(_streamID),
		// Set expectations on scheduler schedulerClient
		suite.schedulerClient.EXPECT().
			Call(
				gomock.Eq(_streamID),
				gomock.Any(),
			).
			Do(func(_ string, msg proto.Message) {
				// Verify clientCall message.
				call := msg.(*sched.Call)
				suite.Equal(sched.Call_ACCEPT, call.GetType())
				suite.Equal(_frameworkID, call.GetFrameworkId().GetValue())

				accept := call.GetAccept()
				suite.NotNil(accept)
				suite.Equal(1, len(accept.GetOfferIds()))
				suite.Equal("offer-0", accept.GetOfferIds()[0].GetValue())
				suite.Equal(3, len(accept.GetOperations()))
				reserveOp := accept.GetOperations()[0]
				createOp := accept.GetOperations()[1]
				launchOp := accept.GetOperations()[2]
				suite.Equal(
					mesos.Offer_Operation_RESERVE,
					reserveOp.GetType())
				suite.Equal(
					mesos.Offer_Operation_CREATE,
					createOp.GetType())
				suite.Equal(
					mesos.Offer_Operation_LAUNCH,
					launchOp.GetType())
				launch := launchOp.GetLaunch()
				suite.NotNil(launch)
				suite.Equal(1, len(launch.GetTaskInfos()))
				suite.Equal(
					fmt.Sprintf(_taskIDFmt, 0),
					launch.GetTaskInfos()[0].GetTaskId().GetValue())
			}).
			Return(nil),
	)

	operationReq := &hostsvc.OfferOperationsRequest{
		Hostname: acquiredHostOffers[0].GetHostname(),
		Operations: []*hostsvc.OfferOperation{
			reserveOperation,
			createOperation,
			launchOperation,
		},
		Id: acquiredHostOffers[0].GetId(),
	}

	operationResp, err := suite.handler.OfferOperations(
		rootCtx,
		operationReq,
	)

	suite.NoError(err)
	suite.Nil(operationResp.GetError())
	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["offer_operations+"].Value())
}

func (suite *HostMgrHandlerTestSuite) TestGetMesosMasterHostPort() {
	defer suite.ctrl.Finish()

	suite.mesosDetector.EXPECT().HostPort().Return("")
	mesosMasterHostPortResponse, err := suite.handler.GetMesosMasterHostPort(context.Background(), &hostsvc.MesosMasterHostPortRequest{})
	suite.NotNil(err)
	suite.Contains(err.Error(), "unable to fetch leader mesos master hostname & port")

	suite.mesosDetector.EXPECT().HostPort().Return("master:5050")
	mesosMasterHostPortResponse, err = suite.handler.GetMesosMasterHostPort(context.Background(), &hostsvc.MesosMasterHostPortRequest{})
	suite.Nil(err)
	suite.Equal(mesosMasterHostPortResponse.Hostname, "master")
	suite.Equal(mesosMasterHostPortResponse.Port, "5050")
}

func (suite *HostMgrHandlerTestSuite) TestServiceHandlerGetDrainingHosts() {
	defer suite.ctrl.Finish()

	req := &hostsvc.GetDrainingHostsRequest{
		Limit:   1,
		Timeout: 1000,
	}
	testHost := "testhost"

	suite.maintenanceQueue.EXPECT().Dequeue(gomock.Any()).Return(testHost, nil)
	resp, err := suite.handler.GetDrainingHosts(context.Background(), req)
	suite.Equal(1, len(resp.GetHostnames()))
	suite.Equal(testHost, resp.GetHostnames()[0])
	suite.NoError(err)

	suite.maintenanceQueue.EXPECT().
		Dequeue(gomock.Any()).
		Return("", fmt.Errorf("fake Dequeue error"))
	resp, err = suite.handler.GetDrainingHosts(context.Background(), req)
	suite.Error(err)
	suite.Nil(resp)

	// Request with limit set to 0
	req = &hostsvc.GetDrainingHostsRequest{
		Limit:   0,
		Timeout: 1000,
	}
	suite.maintenanceQueue.EXPECT().Length().Return(2)
	suite.maintenanceQueue.EXPECT().
		Dequeue(gomock.Any()).
		Return(testHost, nil)
	suite.maintenanceQueue.EXPECT().
		Dequeue(gomock.Any()).
		Return("", queue.DequeueTimeOutError{})
	resp, err = suite.handler.GetDrainingHosts(context.Background(), req)
	suite.Equal(1, len(resp.GetHostnames()))
	suite.Equal(testHost, resp.GetHostnames()[0])
	suite.NoError(err)
}

func (suite *HostMgrHandlerTestSuite) TestServiceHandlerMarkHostsDrained() {
	defer suite.ctrl.Finish()

	hostInfos := []*hpb.HostInfo{
		{
			Hostname: "testhost",
			Ip:       "0.0.0.0",
			State:    hpb.HostState_HOST_STATE_DRAINING,
		},
	}

	var drainingMachines []*mesos_maintenance.ClusterStatus_DrainingMachine
	for _, hostInfo := range hostInfos {
		machineID := &mesos.MachineID{
			Hostname: &hostInfo.Hostname,
			Ip:       &hostInfo.Ip,
		}
		drainingMachines = append(drainingMachines,
			&mesos_maintenance.ClusterStatus_DrainingMachine{
				Id: machineID,
			})
	}

	gomock.InOrder(
		suite.maintenanceHostInfoMap.EXPECT().
			GetDrainingHostInfos([]string{}).
			Return(hostInfos),

		suite.masterOperatorClient.EXPECT().
			StartMaintenance(gomock.Any()).
			Return(nil).
			Do(func(machineIds []*mesos.MachineID) {
				for i := range drainingMachines {
					suite.Exactly(drainingMachines[i].Id, machineIds[i])
				}
			}),
	)

	for _, hostInfo := range hostInfos {
		suite.maintenanceHostInfoMap.EXPECT().UpdateHostState(
			hostInfo.GetHostname(),
			hpb.HostState_HOST_STATE_DRAINING,
			hpb.HostState_HOST_STATE_DOWN)
	}

	var hostnames []string
	for _, hostInfo := range hostInfos {
		hostnames = append(hostnames, hostInfo.GetHostname())
	}
	resp, err := suite.handler.MarkHostsDrained(
		context.Background(),
		&hostsvc.MarkHostsDrainedRequest{
			Hostnames: hostnames,
		})
	suite.NoError(err)
	suite.NotNil(resp)

	// Test host-not-DRAINING
	suite.maintenanceHostInfoMap.EXPECT().
		GetDrainingHostInfos([]string{}).
		Return([]*hpb.HostInfo{})

	resp, err = suite.handler.MarkHostsDrained(
		context.Background(),
		&hostsvc.MarkHostsDrainedRequest{
			Hostnames: hostnames,
		})
	suite.Nil(resp.GetMarkedHosts())

	// Test StartMaintenance error
	suite.maintenanceHostInfoMap.EXPECT().
		GetDrainingHostInfos([]string{}).
		Return([]*hpb.HostInfo{
			{
				Hostname: "host1",
				Ip:       "0.0.0.0",
				State:    hpb.HostState_HOST_STATE_DRAINING,
			},
		})

	suite.masterOperatorClient.EXPECT().
		StartMaintenance(gomock.Any()).
		Return(fmt.Errorf("fake StartMaintenance error"))
	resp, err = suite.handler.MarkHostsDrained(
		context.Background(),
		&hostsvc.MarkHostsDrainedRequest{
			Hostnames: []string{"host1"},
		})
	suite.Error(err)
	suite.Nil(resp.GetMarkedHosts())
}

func getAcquireHostOffersRequest() *hostsvc.AcquireHostOffersRequest {
	return &hostsvc.AcquireHostOffersRequest{
		Filter: &hostsvc.HostFilter{
			Quantity: &hostsvc.QuantityControl{
				MaxHosts: uint32(1),
			},
			ResourceConstraint: &hostsvc.ResourceConstraint{
				Minimum: &task.ResourceConfig{
					CpuLimit:    _perHostCPU,
					MemLimitMb:  _perHostMem,
					DiskLimitMb: _perHostDisk,
				},
			},
		},
	}
}

func createHostReserveOperation() *hostsvc.OfferOperation {
	reserveOperation := &hostsvc.OfferOperation{
		Type: hostsvc.OfferOperation_RESERVE,
		Reserve: &hostsvc.OfferOperation_Reserve{
			Resources: []*mesos.Resource{
				util.NewMesosResourceBuilder().
					WithName("cpus").
					WithValue(_perHostCPU).
					Build(),
				util.NewMesosResourceBuilder().
					WithName("mem").
					WithValue(11.0).
					Build(),
				util.NewMesosResourceBuilder().
					WithName("disk").
					WithValue(12.0).
					Build(),
			},
		},
		ReservationLabels: createReservationLabels(),
	}
	return reserveOperation
}

func createHostCreateOperation() *hostsvc.OfferOperation {
	createOperation := &hostsvc.OfferOperation{
		Type: hostsvc.OfferOperation_CREATE,
		Create: &hostsvc.OfferOperation_Create{
			Volume: &hostsvc.Volume{
				Resource: util.NewMesosResourceBuilder().
					WithName("disk").
					WithValue(1.0).
					Build(),
				ContainerPath: "test",
				Id: &peloton.VolumeID{
					Value: "volumeid",
				},
			},
		},
		ReservationLabels: createReservationLabels(),
	}
	return createOperation
}

func createHostLaunchOperation() *hostsvc.OfferOperation {
	launchOperation := &hostsvc.OfferOperation{
		Type: hostsvc.OfferOperation_LAUNCH,
		Launch: &hostsvc.OfferOperation_Launch{
			Tasks: generateLaunchableTasks(1),
		},
		ReservationLabels: createReservationLabels(),
	}
	return launchOperation
}

func createReservationLabels() *mesos.Labels {
	return reservation.CreateReservationLabels(_testJobID, 0, "hostname-0")
}

func makeAgentsResponse(numAgents int) *mesos_master.Response_GetAgents {
	response := &mesos_master.Response_GetAgents{
		Agents: []*mesos_master.Response_GetAgents_Agent{},
	}
	for i := 0; i < numAgents; i++ {
		resVal := float64(_defaultResourceValue)
		tmpID := fmt.Sprintf("id-%d", i)
		resources := []*mesos.Resource{
			util.NewMesosResourceBuilder().
				WithName(_cpuName).
				WithValue(resVal).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(_memName).
				WithValue(resVal).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(_diskName).
				WithValue(resVal).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(_gpuName).
				WithValue(resVal).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(_cpuName).
				WithValue(resVal).
				WithRevocable(&mesos.Resource_RevocableInfo{}).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(_memName).
				WithValue(resVal).
				WithRevocable(&mesos.Resource_RevocableInfo{}).
				Build(),
		}
		getAgent := &mesos_master.Response_GetAgents_Agent{
			AgentInfo: &mesos.AgentInfo{
				Hostname:  &tmpID,
				Resources: resources,
			},
			TotalResources: resources,
		}
		response.Agents = append(response.Agents, getAgent)
	}

	return response
}

func createEvent(_uuid string, offset int) *pb_eventstream.Event {
	state := mesos.TaskState_TASK_STARTING
	status := &mesos.TaskStatus{
		TaskId: &mesos.TaskID{
			Value: &_uuid,
		},
		State: &state,
		Uuid:  []byte{201, 117, 104, 168, 54, 76, 69, 143, 185, 116, 159, 95, 198, 94, 162, 38},
		AgentId: &mesos.AgentID{
			Value: &_uuid,
		},
	}

	taskStatusUpdate := &sched.Event{
		Update: &sched.Event_Update{
			Status: status,
		},
	}

	return &pb_eventstream.Event{
		Offset:          uint64(offset),
		Type:            pb_eventstream.Event_MESOS_TASK_STATUS,
		MesosTaskStatus: taskStatusUpdate.GetUpdate().GetStatus(),
	}
}

// TestGetHosts tests the hosts, which meets the criteria
func (suite *HostMgrHandlerTestSuite) TestGetHosts() {
	defer suite.ctrl.Finish()
	numAgents := 2
	suite.InitializeHosts(numAgents)
	// Matching constraint.
	acquireReq := &hostsvc.GetHostsRequest{
		Filter: &hostsvc.HostFilter{
			Quantity: &hostsvc.QuantityControl{
				MaxHosts: uint32(2),
			},
			ResourceConstraint: &hostsvc.ResourceConstraint{
				Minimum: &task.ResourceConfig{
					CpuLimit:    1,
					MemLimitMb:  1,
					DiskLimitMb: 1,
				},
			},
		},
	}
	// Calling the GetHosts calls
	acquiredResp, err := suite.handler.GetHosts(
		rootCtx,
		acquireReq,
	)
	// There should not be any error
	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	// Both the hosts should return
	suite.Equal(len(acquiredResp.Hosts), 2)
}

// Tests disable kill tasks request to mesos master
func (suite *HostMgrHandlerTestSuite) TestDisableKillTasks() {
	defer suite.ctrl.Finish()

	_, err := suite.handler.DisableKillTasks(rootCtx, &hostsvc.DisableKillTasksRequest{})
	suite.NoError(err)
	suite.True(suite.handler.disableKillTasks.Load())
}

// TestGetHostsInvalidFilters tests if the filter is invalid it would return error
func (suite *HostMgrHandlerTestSuite) TestGetHostsInvalidFilters() {
	defer suite.ctrl.Finish()
	numAgents := 2
	suite.InitializeHosts(numAgents)

	acquireReq := &hostsvc.GetHostsRequest{
		// Passing the HostFilter Nil
		Filter: nil,
	}

	acquiredResp, err := suite.handler.GetHosts(
		rootCtx,
		acquireReq,
	)
	suite.Error(err)
	// It should return error
	suite.NotNil(acquiredResp.GetError())
	// Error message should be compared if that's the right error
	suite.Equal(acquiredResp.GetError().InvalidHostFilter.Message,
		"Empty host filter")

	// Constraint which does not match.
	acquireReq = &hostsvc.GetHostsRequest{
		Filter: &hostsvc.HostFilter{
			Quantity: &hostsvc.QuantityControl{
				MaxHosts: uint32(2),
			},
			// We are passing resources wich are more then host capacity
			// So it should return error.
			ResourceConstraint: &hostsvc.ResourceConstraint{
				Minimum: &task.ResourceConfig{
					CpuLimit:    2,
					MemLimitMb:  2,
					DiskLimitMb: 1,
				},
			},
		},
	}
	acquiredResp, err = suite.handler.GetHosts(
		rootCtx,
		acquireReq,
	)

	suite.Error(err)
	// It should return error
	suite.NotNil(acquiredResp.GetError())
	// Error message should be compared if that's the right error
	suite.Equal(acquiredResp.GetError().Failure.Message,
		"could not return matching hosts INSUFFICIENT_RESOURCES")
}

// TestReserveHosts tests to reserve the hosts
func (suite *HostMgrHandlerTestSuite) TestReserveHosts() {
	defer suite.ctrl.Finish()
	reserveReq := &hostsvc.ReserveHostsRequest{
		Reservation: &hostsvc.Reservation{
			Task:  nil,
			Hosts: nil,
		},
	}
	resp, err := suite.handler.ReserveHosts(context.Background(), reserveReq)
	suite.NoError(err)
	suite.Nil(resp.Error)
}

// TestReserveHosts tests to reserve the hosts
func (suite *HostMgrHandlerTestSuite) TestReserveHostsError() {
	defer suite.ctrl.Finish()
	reserveReq := &hostsvc.ReserveHostsRequest{
		Reservation: nil,
	}
	resp, err := suite.handler.ReserveHosts(context.Background(), reserveReq)
	suite.NoError(err)
	suite.Equal(resp.Error.Failed.Message, errNilReservation.Error())
}

// TestEnqueueReservationError tests to reserve the hosts
// And check the error case
func (suite *HostMgrHandlerTestSuite) TestEnqueueReservationError() {
	ctrl := gomock.NewController(suite.T())
	handler := &ServiceHandler{}
	mockReserver := reserver_mocks.NewMockReserver(ctrl)
	handler.reserver = mockReserver
	mockReserver.EXPECT().EnqueueReservation(gomock.Any(), gomock.Any()).
		Return(errors.New("error"))
	reservations, err := handler.ReserveHosts(
		context.Background(),
		&hostsvc.ReserveHostsRequest{
			Reservation: &hostsvc.Reservation{},
		})
	suite.NoError(err)
	suite.NotNil(reservations.GetError().GetFailed())
	suite.Equal(reservations.GetError().GetFailed().GetMessage(),
		errReservationNotFound.Error())
}

func TestHostManagerTestSuite(t *testing.T) {
	suite.Run(t, new(HostMgrHandlerTestSuite))
}

// InitializeHosts adds the hosts to host map
func (suite *HostMgrHandlerTestSuite) InitializeHosts(numAgents int) {
	mockMaintenanceMap := hm.NewMockMaintenanceHostInfoMap(suite.ctrl)
	loader := &host.Loader{
		OperatorClient:         suite.masterOperatorClient,
		Scope:                  suite.testScope,
		SlackResourceTypes:     []string{"cpus"},
		MaintenanceHostInfoMap: mockMaintenanceMap,
	}
	response := makeAgentsResponse(numAgents)
	gomock.InOrder(
		suite.masterOperatorClient.EXPECT().Agents().Return(response, nil),
		mockMaintenanceMap.EXPECT().
			GetDrainingHostInfos(gomock.Any()).
			Return([]*hpb.HostInfo{}).Times(len(response.GetAgents())),
	)
	loader.Load(nil)
}

// TestGetCompletedReservations tests the completed reservations
func (suite *HostMgrHandlerTestSuite) TestGetCompletedReservations() {
	ctrl := gomock.NewController(suite.T())
	handler := &ServiceHandler{}
	mockReserver := reserver_mocks.NewMockReserver(ctrl)
	handler.reserver = mockReserver
	mockReserver.EXPECT().DequeueCompletedReservation(
		gomock.Any(), gomock.Any()).Return(
		[]*hostsvc.CompletedReservation{
			{},
		}, nil)
	reservations, err := handler.GetCompletedReservations(
		context.Background(),
		&hostsvc.GetCompletedReservationRequest{})

	suite.Equal(mockReserver, handler.GetReserver())
	suite.NoError(err)
	suite.NotNil(reservations.CompletedReservations)
	suite.Equal(len(reservations.CompletedReservations), 1)
}

// TestGetCompletedReservationsError tests the error condition
// in completed reservations
func (suite *HostMgrHandlerTestSuite) TestGetCompletedReservationsError() {
	ctrl := gomock.NewController(suite.T())
	handler := &ServiceHandler{}
	mockReserver := reserver_mocks.NewMockReserver(ctrl)
	handler.reserver = mockReserver
	mockReserver.EXPECT().DequeueCompletedReservation(gomock.Any(),
		gomock.Any()).
		Return(nil, errors.New("error"))
	reservations, err := handler.GetCompletedReservations(context.Background(),
		&hostsvc.GetCompletedReservationRequest{})

	suite.Equal(mockReserver, handler.GetReserver())
	suite.NoError(err)
	suite.Nil(reservations.CompletedReservations)
	suite.Equal(reservations.GetError().GetNotFound().Message, "error")
}

// Test OfferOperations errors
func (suite *HostMgrHandlerTestSuite) TestOfferOperationsError() {
	launchOperation := &hostsvc.OfferOperation{
		Type: hostsvc.OfferOperation_LAUNCH,
		Launch: &hostsvc.OfferOperation_Launch{
			Tasks: generateLaunchableTasks(1),
		},
	}

	// Test offer pool ClaimForLaunch error
	hostname := "test-host"
	operationReq := &hostsvc.OfferOperationsRequest{
		Hostname: hostname,
		Operations: []*hostsvc.OfferOperation{
			launchOperation,
		},
	}

	operationResp, err := suite.handler.OfferOperations(
		rootCtx,
		operationReq,
	)
	suite.NotNil(operationResp.GetError().GetInvalidOffers())
	suite.Equal(
		operationResp.GetError().GetInvalidOffers().GetMessage(),
		"cannot find input hostname test-host")
	suite.Nil(err)

	// Test scheduler Call error
	numHosts := 1
	acquiredResp, err := suite.acquireHostOffers(numHosts)

	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	acquiredHostOffers := acquiredResp.GetHostOffers()
	suite.Equal(1, len(acquiredHostOffers))

	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["acquire_host_offers+"].Value())

	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(numHosts, "placing")

	operationReq.Hostname = acquiredHostOffers[0].GetHostname()
	errString := "Fake scheduler call error"
	gomock.InOrder(
		suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
			suite.frameworkID),
		suite.provider.EXPECT().GetMesosStreamID(context.Background()).
			Return(_streamID),
		suite.schedulerClient.EXPECT().
			Call(
				gomock.Eq(_streamID),
				gomock.Any(),
			).Return(fmt.Errorf(errString)),
	)

	operationReq = &hostsvc.OfferOperationsRequest{
		Hostname: acquiredHostOffers[0].GetHostname(),
		Operations: []*hostsvc.OfferOperation{
			launchOperation,
		},
		Id: acquiredHostOffers[0].GetId(),
	}

	operationResp, err = suite.handler.OfferOperations(
		rootCtx,
		operationReq,
	)
	suite.NoError(err)
	suite.NotNil(operationResp.GetError().GetFailure())
	suite.Equal(errString, operationResp.GetError().GetFailure().GetMessage())
}

func (suite *HostMgrHandlerTestSuite) withHostOffers(numHosts int) []*hostsvc.
	HostOffer {
	acquiredResp, err := suite.acquireHostOffers(numHosts)
	suite.NoError(err)
	suite.Nil(acquiredResp.GetError())
	acquiredHostOffers := acquiredResp.GetHostOffers()
	suite.Equal(1, len(acquiredHostOffers))
	suite.Equal(
		int64(1),
		suite.testScope.Snapshot().Counters()["acquire_host_offers+"].Value())
	suite.checkResourcesGauges(0, "ready")
	suite.checkResourcesGauges(numHosts, "placing")
	return acquiredHostOffers
}

// Test LaunchTasks errors
func (suite *HostMgrHandlerTestSuite) TestLaunchTasksInvalidOfferError() {
	acquiredHostOffers := suite.withHostOffers(1)

	tt := []struct {
		test string
		req  *hostsvc.LaunchTasksRequest
		err  *hostsvc.InvalidOffers
	}{
		{test: "test invalid host name",
			req: &hostsvc.LaunchTasksRequest{
				Hostname: "test-host",
				AgentId:  acquiredHostOffers[0].GetAgentId(),
				Tasks:    generateLaunchableTasks(1),
				Id:       acquiredHostOffers[0].GetId(),
			},
			err: &hostsvc.InvalidOffers{
				Message: "cannot find input hostname test-host",
			},
		},
		{
			test: "test invalid host offer id",
			req: &hostsvc.LaunchTasksRequest{
				Hostname: acquiredHostOffers[0].GetHostname(),
				AgentId:  acquiredHostOffers[0].GetAgentId(),
				Tasks:    generateLaunchableTasks(1),
				Id:       &peloton.HostOfferID{Value: uuid.New()},
			},
			err: &hostsvc.InvalidOffers{
				Message: "host offer id does not match",
			},
		},
	}

	for _, t := range tt {
		// Test InvalidOffer error
		launchResp, err := suite.handler.LaunchTasks(
			rootCtx,
			t.req,
		)
		suite.Error(err, t.test)
		suite.NotNil(launchResp.GetError().GetInvalidOffers(), t.test)
		suite.Equal(
			t.err.GetMessage(),
			launchResp.GetError().GetInvalidOffers().GetMessage(), t.test)
	}
}

func (suite *HostMgrHandlerTestSuite) TestLaunchTasksInvalidArgError() {
	acquiredHostOffers := suite.withHostOffers(1)
	tt := []struct {
		test string
		req  *hostsvc.LaunchTasksRequest
		err  *hostsvc.InvalidArgument
	}{
		{test: "test empty tasks",
			req: &hostsvc.LaunchTasksRequest{
				Hostname: acquiredHostOffers[0].GetHostname(),
				AgentId:  acquiredHostOffers[0].GetAgentId(),
				Tasks:    nil,
				Id:       &peloton.HostOfferID{Value: uuid.New()},
			},
			err: &hostsvc.InvalidArgument{
				Message: errEmptyTaskList.Error(),
			},
		},
		{
			test: "test empty agent id",
			req: &hostsvc.LaunchTasksRequest{
				Hostname: acquiredHostOffers[0].GetHostname(),
				AgentId:  nil,
				Tasks:    generateLaunchableTasks(1),
				Id:       &peloton.HostOfferID{Value: uuid.New()},
			},
			err: &hostsvc.InvalidArgument{
				Message: errEmptyAgentID.Error(),
			},
		},
		{
			test: "test empty hostname",
			req: &hostsvc.LaunchTasksRequest{
				Hostname: "",
				AgentId:  acquiredHostOffers[0].GetAgentId(),
				Tasks:    generateLaunchableTasks(1),
				Id:       &peloton.HostOfferID{Value: uuid.New()},
			},
			err: &hostsvc.InvalidArgument{
				Message: errEmptyHostName.Error(),
			},
		},
		{
			test: "test empty host offer id",
			req: &hostsvc.LaunchTasksRequest{
				Hostname: acquiredHostOffers[0].GetHostname(),
				AgentId:  acquiredHostOffers[0].GetAgentId(),
				Tasks:    generateLaunchableTasks(1),
				Id:       nil,
			},
			err: &hostsvc.InvalidArgument{
				Message: errEmptyHostOfferID.Error(),
			},
		},
	}
	for _, t := range tt {
		// Test InvalidArgument error
		launchResp, err := suite.handler.LaunchTasks(
			rootCtx,
			t.req,
		)
		suite.Error(err, t.test)
		suite.NotNil(launchResp.GetError().GetInvalidArgument(), t.test)
		suite.Contains(
			launchResp.GetError().GetInvalidArgument().GetMessage(),
			t.err.GetMessage(),
			t.test)
	}
}

func (suite *HostMgrHandlerTestSuite) TestLaunchTasksSchedulerError() {
	acquiredHostOffers := suite.withHostOffers(1)

	// Test framework client error
	errString := "fake scheduler call error"
	gomock.InOrder(
		// Set expectations on provider
		suite.provider.EXPECT().GetFrameworkID(context.Background()).Return(
			suite.frameworkID),
		// Set expectations on provider
		suite.provider.EXPECT().GetMesosStreamID(context.Background()).Return(_streamID),
		// Set expectations on scheduler schedulerClient
		suite.schedulerClient.EXPECT().
			Call(
				gomock.Eq(_streamID),
				gomock.Any(),
			).Return(fmt.Errorf(errString)),
	)

	launchResp, err := suite.handler.LaunchTasks(
		rootCtx,
		&hostsvc.LaunchTasksRequest{
			Hostname: acquiredHostOffers[0].GetHostname(),
			AgentId:  acquiredHostOffers[0].GetAgentId(),
			Tasks:    generateLaunchableTasks(1),
			Id:       acquiredHostOffers[0].GetId(),
		},
	)

	suite.Error(err)
	suite.NotNil(launchResp.GetError().GetLaunchFailure())
	suite.Equal(
		launchResp.GetError().GetLaunchFailure().GetMessage(),
		errString)
}

func (suite *HostMgrHandlerTestSuite) TestReleaseHostsHeldForTasks() {
	defer suite.ctrl.Finish()

	numOffers := 2
	offers := suite.pool.AddOffers(context.Background(), generateOffers(numOffers))

	host1 := offers[0].GetHostname()
	host2 := offers[1].GetHostname()
	tasks := []*peloton.TaskID{
		{Value: "task0"},
		{Value: "task1"},
		{Value: "task2"},
		{Value: "task3"},
	}

	suite.NoError(suite.pool.HoldForTasks(host1, tasks[:2]))
	suite.NoError(suite.pool.HoldForTasks(host2, tasks[2:]))

	resp, err := suite.handler.ReleaseHostsHeldForTasks(
		context.Background(),
		&hostsvc.ReleaseHostsHeldForTasksRequest{
			Ids: []*peloton.TaskID{tasks[0], tasks[2]},
		},
	)

	suite.NoError(err)
	suite.Nil(resp.GetError())

	// host should be released from held
	suite.Empty(suite.pool.GetHostHeldForTask(tasks[0]))
	suite.Empty(suite.pool.GetHostHeldForTask(tasks[2]))

	suite.Equal(suite.pool.GetHostHeldForTask(tasks[1]), host1)
	suite.Equal(suite.pool.GetHostHeldForTask(tasks[3]), host2)
}

// Helper type to implement sorting on the slice
type AgentSlice []*mesos_master.Response_GetAgents_Agent

// Needed to use Sort() on AgentSlice
func (a AgentSlice) Len() int {
	return len(a)
}

// Needed to use Sort() on AgentSlice
func (a AgentSlice) Less(i, j int) bool {
	return *a[i].AgentInfo.Hostname < *a[j].AgentInfo.Hostname
}

// Needed to use Sort() on AgentSlice
func (a AgentSlice) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

// Test GetMesosAgentInfo with and without hostname specified
func (suite *HostMgrHandlerTestSuite) TestGetMesosAgentInfo() {

	response := makeAgentsResponse(3)
	agentInfo := AgentSlice(response.GetAgents())
	sort.Sort(agentInfo)
	suite.masterOperatorClient.EXPECT().Agents().Return(response, nil)
	suite.maintenanceHostInfoMap.EXPECT().GetDrainingHostInfos(gomock.Any()).Return([]*hpb.HostInfo{}).Times(len(response.GetAgents()))
	loader := &host.Loader{
		OperatorClient:         suite.masterOperatorClient,
		Scope:                  suite.testScope,
		MaintenanceHostInfoMap: suite.maintenanceHostInfoMap,
	}
	loader.Load(nil)

	testcases := []struct {
		name     string
		hostname string
		result   []*mesos_master.Response_GetAgents_Agent
		err      *hostsvc.GetMesosAgentInfoResponse_Error
	}{
		{
			name:     "specific agent",
			hostname: "id-0",
			result:   agentInfo[0:1],
		},
		{
			name:   "all agents",
			result: agentInfo,
		},
		{
			name:     "unknown agent",
			hostname: "foo",
			err: &hostsvc.GetMesosAgentInfoResponse_Error{
				HostNotFound: &hostsvc.HostNotFound{
					Message: "host not found",
				},
			},
		},
	}
	for _, tc := range testcases {
		resp, err := suite.handler.GetMesosAgentInfo(rootCtx,
			&hostsvc.GetMesosAgentInfoRequest{Hostname: tc.hostname})
		suite.NoError(err, tc.name)
		if tc.err == nil {
			suite.Nil(resp.GetError(), tc.name)
			actualAgentInfo := AgentSlice(resp.GetAgents())
			sort.Sort(actualAgentInfo)
			suite.EqualValues(tc.result, actualAgentInfo, tc.name)
		} else {
			suite.Equal(tc.err, resp.GetError(), tc.name)
		}
	}
}

// Test toHostStatus to convert HostStatus to string
func (suite *HostMgrHandlerTestSuite) TestToHostStatus() {
	statuses := []summary.HostStatus{
		summary.ReadyHost,
		summary.PlacingHost,
		summary.ReservedHost,
		summary.HeldHost,
		100,
	}
	expectedStatuses := []string{
		"ready",
		"placing",
		"reserved",
		"held",
		"unknown",
	}

	for i, status := range statuses {
		suite.Equal(expectedStatuses[i], toHostStatus(status))
	}
}

// TestWatchEvent sets up a watch client, and verifies the responses
// are streamed back correctly based on the input, finally the
// test cancels the watch stream.
func (suite *HostMgrHandlerTestSuite) TestWatchEvent() {
	watchID := watchevent.NewWatchID()
	eventClient := &watchevent.EventClient{
		// do not set buffer size for input to make sure the
		// tests sends all the events before sending stop
		// signal
		Input:  make(chan *pb_eventstream.Event),
		Signal: make(chan watchevent.StopSignal, 1),
	}

	suite.watchProcessor.EXPECT().NewEventClient().
		Return(watchID, eventClient, nil)
	suite.watchProcessor.EXPECT().StopEventClient(watchID)

	event := &pb_eventstream.Event{}

	suite.watchEventServer.EXPECT().
		Send(&hostsvc.WatchEventResponse{
			WatchId: watchID,
			Events:  nil,
		}).
		Return(nil)
	suite.watchEventServer.EXPECT().
		Send(&hostsvc.WatchEventResponse{
			WatchId: watchID,
			Events:  []*pb_eventstream.Event{event},
		}).
		Return(nil)

	req := &hostsvc.WatchEventRequest{}

	go func() {
		eventClient.Input <- event
		// cancelling  watch event
		eventClient.Signal <- watchevent.StopSignalCancel
	}()

	err := suite.handler.WatchEvent(req, suite.watchEventServer)
	suite.Error(err)
	suite.True(yarpcerrors.IsCancelled(err))
}

// TestWatchEvent_MaxClientReached checks Watch will return resource-exhausted
// error when NewEventClient reached max client.
func (suite *HostMgrHandlerTestSuite) TestWatchEvent_MaxClientReached() {
	suite.watchProcessor.EXPECT().NewEventClient().
		Return("", nil, yarpcerrors.ResourceExhaustedErrorf("max client reached"))

	req := &hostsvc.WatchEventRequest{}
	err := suite.handler.WatchEvent(req, suite.watchEventServer)
	suite.Error(err)
	suite.True(yarpcerrors.IsResourceExhausted(err))
}

// TestWatchEvent_InitSendError tests for error case of initial response.
func (suite *HostMgrHandlerTestSuite) TestWatchEvent_InitSendError() {
	watchID := watchevent.NewWatchID()
	eventClient := &watchevent.EventClient{
		// do not set buffer size for input to make sure the
		// tests sends all the events before sending stop
		// signal
		Input:  make(chan *pb_eventstream.Event),
		Signal: make(chan watchevent.StopSignal, 1),
	}

	suite.watchProcessor.EXPECT().NewEventClient().
		Return(watchID, eventClient, nil)
	suite.watchProcessor.EXPECT().StopEventClient(watchID)

	sendErr := errors.New("message:transport is closing")

	// initial response
	suite.watchEventServer.EXPECT().
		Send(&hostsvc.WatchEventResponse{
			WatchId: watchID,
			Events:  nil,
		}).
		Return(sendErr)

	req := &hostsvc.WatchEventRequest{}

	err := suite.handler.WatchEvent(req, suite.watchEventServer)
	suite.Error(err)
	suite.Equal(sendErr, err)
}

// TestWatchEvent_InitSendError tests for error case of subsequent response
// after initial one.
func (suite *HostMgrHandlerTestSuite) TestWatchEvent_SendError() {
	watchID := watchevent.NewWatchID()
	eventClient := &watchevent.EventClient{
		// do not set buffer size for input to make sure the
		// tests sends all the events before sending stop
		// signal
		Input:  make(chan *pb_eventstream.Event),
		Signal: make(chan watchevent.StopSignal, 1),
	}

	suite.watchProcessor.EXPECT().NewEventClient().
		Return(watchID, eventClient, nil)
	suite.watchProcessor.EXPECT().StopEventClient(watchID)

	// initial response
	event := &pb_eventstream.Event{}

	suite.watchEventServer.EXPECT().
		Send(&hostsvc.WatchEventResponse{
			WatchId: watchID,
			Events:  nil,
		}).
		Return(nil)

	sendErr := errors.New("message:transport is closing")

	// subsequent response
	suite.watchEventServer.EXPECT().
		Send(&hostsvc.WatchEventResponse{
			WatchId: watchID,
			Events:  []*pb_eventstream.Event{event},
		}).
		Return(sendErr)

	req := &hostsvc.WatchEventRequest{}

	go func() {
		eventClient.Input <- event
		eventClient.Signal <- watchevent.StopSignalCancel
	}()

	err := suite.handler.WatchEvent(req, suite.watchEventServer)
	suite.Error(err)
	suite.Equal(sendErr, err)
}

// TestCancelWatchEvent tests Cancel request are proxied to watch processor correctly.
func (suite *HostMgrHandlerTestSuite) TestCancel() {
	watchID := watchevent.NewWatchID()

	suite.watchProcessor.EXPECT().StopEventClient(watchID).Return(nil)

	resp, err := suite.handler.CancelWatchEvent(suite.ctx, &hostsvc.CancelWatchRequest{
		WatchId: watchID,
	})
	suite.NotNil(resp)
	suite.NoError(err)
}

// TestCancelWatchEvent_NotFoundTaskEvent tests Cancel response returns not-found error, when
// an invalid  watch-id is passed in.
func (suite *HostMgrHandlerTestSuite) TestCancel_NotFoundTask() {
	watchID := watchevent.NewWatchID()

	err := yarpcerrors.NotFoundErrorf("watch_id %s not exist for task watch client", watchID)

	suite.watchProcessor.EXPECT().
		StopEventClient(watchID).
		Return(err)

	resp, err := suite.handler.CancelWatchEvent(suite.ctx, &hostsvc.CancelWatchRequest{
		WatchId: watchID,
	})
	suite.Nil(resp)
	suite.Error(err)
	suite.True(yarpcerrors.IsNotFound(err))
}
