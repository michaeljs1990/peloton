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

package offerpool

import (
	"context"
	"fmt"
	"testing"
	"time"

	mesos "github.com/uber/peloton/.gen/mesos/v1"

	"github.com/uber/peloton/pkg/common/util"
	"github.com/uber/peloton/pkg/hostmgr/binpacking"
	"github.com/uber/peloton/pkg/hostmgr/scalar"
	"github.com/uber/peloton/pkg/hostmgr/summary"
	hmutil "github.com/uber/peloton/pkg/hostmgr/util"

	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
)

type RefreshTestSuite struct {
	suite.Suite
	defragRanker binpacking.Ranker
	offerIndex   map[string]summary.HostSummary
	pool         Pool
}

func TestRefreshTestSuite(t *testing.T) {
	binpacking.Init()
	suite.Run(t, new(RefreshTestSuite))
}

func (suite *RefreshTestSuite) SetupTest() {
	suite.defragRanker = binpacking.GetRankerByName(binpacking.DeFrag)
	suite.offerIndex = CreateOfferIndex()
	suite.pool = &offerPool{
		hostOfferIndex:   suite.offerIndex,
		offerHoldTime:    1 * time.Minute,
		metrics:          NewMetrics(tally.NoopScope),
		binPackingRanker: suite.defragRanker,
	}
	suite.defragRanker.RefreshRanking(nil)
}

func (suite *RefreshTestSuite) TestRefresh() {
	refresher := NewRefresher(suite.pool)
	refresher.Refresh(nil)
	sortedList := suite.defragRanker.GetRankedHostList(suite.offerIndex)
	suite.EqualValues(hmutil.GetResourcesFromOffers(
		sortedList[0].(summary.HostSummary).GetOffers(summary.All)),
		scalar.Resources{CPU: 1, Mem: 1, Disk: 1, GPU: 1})
	suite.EqualValues(hmutil.GetResourcesFromOffers(
		sortedList[1].(summary.HostSummary).GetOffers(summary.All)),
		scalar.Resources{CPU: 3, Mem: 3, Disk: 3, GPU: 2})
	suite.EqualValues(hmutil.GetResourcesFromOffers(
		sortedList[2].(summary.HostSummary).GetOffers(summary.All)),
		scalar.Resources{CPU: 3, Mem: 3, Disk: 3, GPU: 2})
	suite.EqualValues(hmutil.GetResourcesFromOffers(
		sortedList[3].(summary.HostSummary).GetOffers(summary.All)),
		scalar.Resources{CPU: 1, Mem: 1, Disk: 1, GPU: 4})
	suite.EqualValues(hmutil.GetResourcesFromOffers(
		sortedList[4].(summary.HostSummary).GetOffers(summary.All)),
		scalar.Resources{CPU: 2, Mem: 2, Disk: 2, GPU: 4})

	AddHostToIndex(5, suite.offerIndex)
	sortedListNew := suite.defragRanker.GetRankedHostList(suite.offerIndex)
	suite.EqualValues(len(sortedListNew), 5)
	// Refresh the ranker
	refresher.Refresh(nil)
	sortedListNew = suite.defragRanker.GetRankedHostList(suite.offerIndex)
	suite.EqualValues(len(sortedListNew), 6)
	suite.EqualValues(hmutil.GetResourcesFromOffers(
		sortedListNew[5].(summary.HostSummary).GetOffers(summary.All)),
		scalar.Resources{CPU: 5, Mem: 5, Disk: 5, GPU: 5})
}

func CreateOfferIndex() map[string]summary.HostSummary {
	offerIndex := make(map[string]summary.HostSummary)
	hostName0 := "hostname0"
	offer0 := CreateOffer(hostName0, scalar.Resources{CPU: 1, Mem: 1, Disk: 1, GPU: 1})
	summry0 := summary.New(nil, nil, hostName0, nil, time.Duration(30*time.Second))
	summry0.AddMesosOffers(context.Background(), []*mesos.Offer{offer0})
	offerIndex[hostName0] = summry0

	hostName1 := "hostname1"
	offer1 := CreateOffer(hostName1, scalar.Resources{CPU: 1, Mem: 1, Disk: 1, GPU: 4})
	summry1 := summary.New(nil, nil, hostName1, nil, time.Duration(30*time.Second))
	summry1.AddMesosOffers(context.Background(), []*mesos.Offer{offer1})
	offerIndex[hostName1] = summry1

	hostName2 := "hostname2"
	offer2 := CreateOffer(hostName2, scalar.Resources{CPU: 2, Mem: 2, Disk: 2, GPU: 4})
	summry2 := summary.New(nil, nil, hostName2, nil, time.Duration(30*time.Second))
	summry2.AddMesosOffers(context.Background(), []*mesos.Offer{offer2})
	offerIndex[hostName2] = summry2

	hostName3 := "hostname3"
	offer3 := CreateOffer(hostName3, scalar.Resources{CPU: 3, Mem: 3, Disk: 3, GPU: 2})
	summry3 := summary.New(nil, nil, hostName3, nil, time.Duration(30*time.Second))
	summry3.AddMesosOffers(context.Background(), []*mesos.Offer{offer3})
	offerIndex[hostName3] = summry3

	hostName4 := "hostname4"
	offer4 := CreateOffer(hostName4, scalar.Resources{CPU: 3, Mem: 3, Disk: 3, GPU: 2})
	summry4 := summary.New(nil, nil, hostName4, nil, time.Duration(30*time.Second))
	summry4.AddMesosOffers(context.Background(), []*mesos.Offer{offer4})
	offerIndex[hostName4] = summry4
	return offerIndex
}

func AddHostToIndex(id int, offerIndex map[string]summary.HostSummary) {
	hostName := fmt.Sprintf("hostname%d", id)
	offer := CreateOffer(hostName, scalar.Resources{CPU: 5, Mem: 5, Disk: 5, GPU: 5})
	summry := summary.New(nil, nil, hostName, nil, time.Duration(30*time.Second))
	summry.AddMesosOffers(context.Background(), []*mesos.Offer{offer})
	offerIndex[hostName] = summry
}

func CreateOffer(
	hostName string,
	resource scalar.Resources) *mesos.Offer {
	offerID := fmt.Sprintf("%s-%d", hostName, 1)
	agentID := fmt.Sprintf("%s-%d", hostName, 1)
	return &mesos.Offer{
		Id: &mesos.OfferID{
			Value: &offerID,
		},
		AgentId: &mesos.AgentID{
			Value: &agentID,
		},
		Hostname: &hostName,
		Resources: []*mesos.Resource{
			util.NewMesosResourceBuilder().
				WithName("cpus").
				WithValue(resource.CPU).
				Build(),
			util.NewMesosResourceBuilder().
				WithName("mem").
				WithValue(resource.Mem).
				Build(),
			util.NewMesosResourceBuilder().
				WithName("disk").
				WithValue(resource.Disk).
				Build(),
			util.NewMesosResourceBuilder().
				WithName("gpus").
				WithValue(resource.GPU).
				Build(),
		},
	}
}
