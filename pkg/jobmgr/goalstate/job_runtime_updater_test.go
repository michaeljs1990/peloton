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

package goalstate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uber/peloton/.gen/peloton/api/v0/job"
	pbjob "github.com/uber/peloton/.gen/peloton/api/v0/job"
	"github.com/uber/peloton/.gen/peloton/api/v0/peloton"
	pbtask "github.com/uber/peloton/.gen/peloton/api/v0/task"
	"github.com/uber/peloton/.gen/peloton/private/models"
	"github.com/uber/peloton/.gen/peloton/private/resmgrsvc"
	resmocks "github.com/uber/peloton/.gen/peloton/private/resmgrsvc/mocks"

	goalstatemocks "github.com/uber/peloton/pkg/common/goalstate/mocks"
	cachedmocks "github.com/uber/peloton/pkg/jobmgr/cached/mocks"
	jobmgrtask "github.com/uber/peloton/pkg/jobmgr/task"
	storemocks "github.com/uber/peloton/pkg/storage/mocks"

	"github.com/uber/peloton/pkg/jobmgr/cached"
	jobmgrcommon "github.com/uber/peloton/pkg/jobmgr/common"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc/yarpcerrors"
)

const (
	jobStartTime      = "2017-01-02T15:04:05.456789016Z"
	jobCompletionTime = "2017-01-03T18:04:05.987654447Z"
)

type JobRuntimeUpdaterTestSuite struct {
	suite.Suite

	ctrl                          *gomock.Controller
	jobStore                      *storemocks.MockJobStore
	taskStore                     *storemocks.MockTaskStore
	updateStore                   *storemocks.MockUpdateStore
	jobGoalStateEngine            *goalstatemocks.MockEngine
	taskGoalStateEngine           *goalstatemocks.MockEngine
	updateGoalStateEngine         *goalstatemocks.MockEngine
	jobFactory                    *cachedmocks.MockJobFactory
	cachedJob                     *cachedmocks.MockJob
	cachedConfig                  *cachedmocks.MockJobConfigCache
	cachedTask                    *cachedmocks.MockTask
	goalStateDriver               *driver
	resmgrClient                  *resmocks.MockResourceManagerServiceYARPCClient
	jobID                         *peloton.JobID
	jobEnt                        *jobEntity
	lastUpdateTs                  float64
	jobRuntimeCalculationViaCache bool
}

func TestJobRuntimeUpdater(t *testing.T) {
	suite.Run(t, new(JobRuntimeUpdaterTestSuite))
}

func (suite *JobRuntimeUpdaterTestSuite) SetupTest() {
	suite.ctrl = gomock.NewController(suite.T())
	suite.jobStore = storemocks.NewMockJobStore(suite.ctrl)
	suite.taskStore = storemocks.NewMockTaskStore(suite.ctrl)
	suite.updateStore = storemocks.NewMockUpdateStore(suite.ctrl)

	suite.resmgrClient = resmocks.NewMockResourceManagerServiceYARPCClient(suite.ctrl)
	suite.jobGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.taskGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.updateGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.jobFactory = cachedmocks.NewMockJobFactory(suite.ctrl)
	suite.cachedJob = cachedmocks.NewMockJob(suite.ctrl)
	suite.cachedTask = cachedmocks.NewMockTask(suite.ctrl)
	suite.cachedConfig = cachedmocks.NewMockJobConfigCache(suite.ctrl)
	suite.jobRuntimeCalculationViaCache = false
	suite.goalStateDriver = &driver{
		jobEngine:                     suite.jobGoalStateEngine,
		taskEngine:                    suite.taskGoalStateEngine,
		updateEngine:                  suite.updateGoalStateEngine,
		jobStore:                      suite.jobStore,
		taskStore:                     suite.taskStore,
		updateStore:                   suite.updateStore,
		jobFactory:                    suite.jobFactory,
		resmgrClient:                  suite.resmgrClient,
		mtx:                           NewMetrics(tally.NoopScope),
		cfg:                           &Config{},
		jobRuntimeCalculationViaCache: suite.jobRuntimeCalculationViaCache,
	}
	suite.jobID = &peloton.JobID{Value: uuid.NewRandom().String()}
	suite.jobEnt = &jobEntity{
		id:     suite.jobID,
		driver: suite.goalStateDriver,
	}
	suite.lastUpdateTs = float64(
		time.Now().Add(time.Duration(-1) * time.Hour).Unix())
	suite.goalStateDriver.cfg.normalize()
	suite.cachedJob.EXPECT().
		GetResourceUsage().Return(
		jobmgrtask.CreateEmptyResourceUsageMap()).AnyTimes()
}

func (suite *JobRuntimeUpdaterTestSuite) TearDownTest() {
	suite.ctrl.Finish()
}

// TestJobEvaluateMaxRunningInstancesSLANoConfi tests when getting config failed
func (suite *JobRuntimeUpdaterTestSuite) TestJobEvaluateMaxRunningInstancesSLANoConfig() {
	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(nil, errors.New(""))
	err := JobEvaluateMaxRunningInstancesSLA(context.Background(), suite.jobEnt)
	suite.Error(err)
}

// TestJobRuntimeUpdaterNoRunTime tests when geting runtime failed
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdaterNoRunTime() {
	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(nil, errors.New(""))
	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.Error(err)
}

// TestJobRuntimeUpdaterNoConfig tests getting jobConfig failed
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdaterNoConfig() {
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_KILLED,
		GoalState: pbjob.JobState_SUCCEEDED,
	}
	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)
	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(nil, errors.New(""))
	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.Error(err)
}

// Verify that completion time of a completed job shouldn't be empty.
func (suite *JobRuntimeUpdaterTestSuite) TestJobCompletionTimeNotEmpty() {
	instanceCount := uint32(100)
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_KILLED,
		GoalState: pbjob.JobState_SUCCEEDED,
	}
	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// Simulate KILLED job which never ran
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_KILLED.String()] = instanceCount

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	// Because the job never ran, GetLastTaskUpdateTime will return 0
	// Mock it to return 0 here
	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(float64(0))

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.NotEqual(jobInfo.Runtime.CompletionTime, "")
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()
	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

//TestJobRuntimeUpdater_Batch_RUNNING tests updating a RUNNING batch job
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_Batch_RUNNING() {
	instanceCount := uint32(100)
	updateID := &peloton.UpdateID{Value: uuid.NewRandom().String()}
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_PENDING,
		GoalState: pbjob.JobState_SUCCEEDED,
		UpdateID:  updateID,
	}
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	// Simulate RUNNING job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_PENDING.String()] = instanceCount / 4
	stateCounts[pbtask.TaskState_RUNNING.String()] = instanceCount / 4
	stateCounts[pbtask.TaskState_LAUNCHED.String()] = instanceCount / 4
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 4

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(false)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(float64(0))

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			instanceCount := uint32(100)
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_RUNNING)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_PENDING.String()], instanceCount/4)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_RUNNING.String()], instanceCount/4)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_LAUNCHED.String()], instanceCount/4)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_SUCCEEDED.String()], instanceCount/4)
		}).
		Return(nil)

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_Batch_RUNNING tests updating a SUCCEED batch job
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_Batch_SUCCEED() {
	instanceCount := uint32(100)
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_PENDING,
		GoalState: pbjob.JobState_SUCCEEDED,
	}
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	// Simulate SUCCEEDED job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)
	endTime, _ := time.Parse(time.RFC3339Nano, jobCompletionTime)
	endTimeUnix := float64(endTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(endTimeUnix)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			instanceCount := uint32(100)
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_SUCCEEDED)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_SUCCEEDED.String()], instanceCount)
			suite.Equal(jobInfo.Runtime.StartTime, jobStartTime)
			suite.Equal(jobInfo.Runtime.CompletionTime, jobCompletionTime)
		}).
		Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_Batch_PENDING test updating a PENDING batch job
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_Batch_PENDING() {
	instanceCount := uint32(100)
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_PENDING,
		GoalState: pbjob.JobState_SUCCEEDED,
	}

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// Simulate PENDING job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_PENDING.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(false)

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			instanceCount := uint32(100)
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_PENDING)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_SUCCEEDED.String()], instanceCount/2)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_PENDING.String()], instanceCount/2)
		}).Return(nil)

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_Batch_FAILED tests updating a failed batch job
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_Batch_FAILED() {
	instanceCount := uint32(100)
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_PENDING,
		GoalState: pbjob.JobState_SUCCEEDED,
	}

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)
	endTime, _ := time.Parse(time.RFC3339Nano, jobCompletionTime)
	endTimeUnix := float64(endTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// Simulate FAILED job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_FAILED.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(endTimeUnix)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			instanceCount := uint32(100)
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_FAILED)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_SUCCEEDED.String()], instanceCount/2)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_FAILED.String()], instanceCount/2)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_Batch_FAILED tests updating a batch job with lost tasks
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_Batch_LOST() {
	instanceCount := uint32(100)
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_PENDING,
		GoalState: pbjob.JobState_SUCCEEDED,
	}

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)
	endTime, _ := time.Parse(time.RFC3339Nano, jobCompletionTime)
	endTimeUnix := float64(endTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// Simulate FAILED job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_LOST.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(endTimeUnix)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			instanceCount := uint32(100)
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_FAILED)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_SUCCEEDED.String()], instanceCount/2)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_LOST.String()], instanceCount/2)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_Batch_KILLING tests update a KILLING batch job
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_Batch_KILLING() {
	instanceCount := uint32(100)
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_KILLING,
		GoalState: pbjob.JobState_KILLED,
	}
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(false)

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// Simulate KILLING job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_KILLING.String()] = instanceCount / 4
	stateCounts[pbtask.TaskState_KILLED.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 4

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			instanceCount := uint32(100)
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_KILLING)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_SUCCEEDED.String()], instanceCount/4)
			stateCounts[pbtask.TaskState_KILLING.String()] = instanceCount / 4
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_KILLED.String()], instanceCount/2)
			suite.Empty(jobInfo.Runtime.GetCompletionTime())
		}).Return(nil)

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_Batch_KILLED tests updating a KILLED batch job
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_Batch_KILLED() {
	instanceCount := uint32(100)
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_PENDING,
		GoalState: pbjob.JobState_SUCCEEDED,
	}

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)
	endTime, _ := time.Parse(time.RFC3339Nano, jobCompletionTime)
	endTimeUnix := float64(endTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// Simulate KILLED job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_FAILED.String()] = instanceCount / 4
	stateCounts[pbtask.TaskState_LOST.String()] = instanceCount / 4
	stateCounts[pbtask.TaskState_KILLED.String()] = instanceCount / 4
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 4

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(endTimeUnix)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			instanceCount := uint32(100)
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_KILLED)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_SUCCEEDED.String()], instanceCount/4)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_FAILED.String()], instanceCount/4)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_KILLED.String()], instanceCount/4)
			suite.Equal(jobInfo.Runtime.TaskStats[pbtask.TaskState_LOST.String()], instanceCount/4)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_DBError tests when updating meets DB error
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_DBError() {
	instanceCount := uint32(100)
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_PENDING,
		GoalState: pbjob.JobState_SUCCEEDED,
	}

	// Simulate fake DB error
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount
	jobRuntime = pbjob.RuntimeInfo{
		State:     pbjob.JobState_RUNNING,
		GoalState: pbjob.JobState_SUCCEEDED,
	}

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)
	endTime, _ := time.Parse(time.RFC3339Nano, jobCompletionTime)
	endTimeUnix := float64(endTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(endTimeUnix)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Return(fmt.Errorf("fake db error"))

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.Error(err)
}

// TestJobRuntimeUpdater_IncorrectState tests when uodating job with incorrect states
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_IncorrectState() {
	instanceCount := uint32(100)

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)
	endTime, _ := time.Parse(time.RFC3339Nano, jobCompletionTime)
	endTimeUnix := float64(endTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// Simulate SUCCEEDED job with correct task stats in runtime but incorrect state
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_RUNNING,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(endTimeUnix)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_SUCCEEDED)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_KILLEDWithNoTask tests updating a KILLED job with no tasks
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_KILLEDWithNoTask() {
	instanceCount := uint32(100)

	// Simulate killed job with no tasks created
	stateCounts := make(map[string]uint32)
	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_KILLED,
		GoalState: pbjob.JobState_KILLED,
		TaskStats: stateCounts,
	}

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_PartiallyCreatedJob testing updating a partially created job
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_PartiallyCreatedJob() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// sum of each state is smaller than instanceCount
	// simulate partially created job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_PENDING.String()] = instanceCount/2 - 1
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount/2 - 1

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_PENDING,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().GetType().Return(pbjob.JobType_BATCH)
	suite.cachedJob.EXPECT().GetJobType().Return(pbjob.JobType_BATCH).AnyTimes()

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(true).
		AnyTimes()

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_INITIALIZED)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_InitializedJobWithMoreTasksThanConfigured tests
// INITIALIZED job with more tasks than configured
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_InitializedJobWithMoreTasksThanConfigured() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount - 10).
		AnyTimes()

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// sum of each state is smaller than instanceCount
	// simulate partially created job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_PENDING.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_INITIALIZED,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH).
		AnyTimes()

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(true).
		AnyTimes()

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_PENDING)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_PendingJobWithMoreTasksThanConfigured
// tests updating a PENDING job with more tasks than configured
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_PendingJobWithMoreTasksThanConfigured() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount - 10).
		AnyTimes()

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// sum of each state is more than instanceCount
	// simulate partially created job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_FAILED.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_INITIALIZED,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH).
		AnyTimes()

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(true).
		AnyTimes()

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_PENDING)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_ControllerTaskSucceeded tests
// updating a job with controller task succeeded
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_ControllerTaskSucceeded() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_FAILED.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_INITIALIZED,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().GetType().Return(pbjob.JobType_BATCH)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(true)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(false).
		AnyTimes()

	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), uint32(0)).
		Return(suite.cachedTask, nil)

	suite.cachedTask.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&pbtask.RuntimeInfo{
			State: pbtask.TaskState_SUCCEEDED,
		}, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(float64(0))

	// as long as controller task succeeds, job state is succeeded
	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_SUCCEEDED)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdaterControllerTaskFailToGetTask tests
// updating a job when controller task failed to get task
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdaterControllerTaskFailToGetTask() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_FAILED.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_INITIALIZED,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(true)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(false).
		AnyTimes()

	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), uint32(0)).
		Return(nil, yarpcerrors.UnavailableErrorf("test error"))

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.Error(err)
}

// TestJobRuntimeUpdaterControllerTaskFailToGetRuntime tests
// updating a job when controller task failed to get runtime
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdaterControllerTaskFailToGetRuntime() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_FAILED.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_INITIALIZED,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(true)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(false).
		AnyTimes()

	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), uint32(0)).
		Return(suite.cachedTask, nil)

	suite.cachedTask.EXPECT().
		GetRuntime(gomock.Any()).
		Return(nil, yarpcerrors.UnavailableErrorf("test error"))

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.Error(err)
}

// TestJobRuntimeUpdater_ControllerTaskFailed tests
// updating a job  when the controller task failed
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_ControllerTaskFailed() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_FAILED.String()] = 1
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount - 1

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_INITIALIZED,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(true)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(false).
		AnyTimes()

	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), uint32(0)).
		Return(suite.cachedTask, nil)

	suite.cachedTask.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&pbtask.RuntimeInfo{
			State: pbtask.TaskState_FAILED,
		}, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	// as long as controller task failed, job state is failed
	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_FAILED)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_ControllerTaskLost tests
// updating a job  when the controller task is lost
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_ControllerTaskLost() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_LOST.String()] = 1
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount - 1

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_INITIALIZED,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(true)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(false).
		AnyTimes()

	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), uint32(0)).
		Return(suite.cachedTask, nil)

	suite.cachedTask.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&pbtask.RuntimeInfo{
			State: pbtask.TaskState_LOST,
		}, nil)

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	// as long as controller task failed, job state is failed
	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_FAILED)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestJobRuntimeUpdater_ControllerTaskRunning tests
// updating a job with controller task running
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_ControllerTaskRunning() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_RUNNING.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_INITIALIZED,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(true)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(false).
		AnyTimes()

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	// even if controller task finishes, still wait for all tasks
	// finish before entering terminal state
	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_RUNNING)
		}).Return(nil)

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// Tests partially created batch job which has an update for adding more instances.
func (suite *JobRuntimeUpdaterTestSuite) TestJobRuntimeUpdater_UpdateAddingInstancesToJob() {
	instanceCount := uint32(100)
	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	startTime, _ := time.Parse(time.RFC3339Nano, jobStartTime)
	startTimeUnix := float64(startTime.UnixNano()) / float64(time.Second/time.Nanosecond)

	// sum of each state is smaller than instanceCount
	// simulate partially created job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_PENDING.String()] = instanceCount/2 - 1
	stateCounts[pbtask.TaskState_RUNNING.String()] = instanceCount/2 - 1

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_PENDING,
		GoalState: pbjob.JobState_SUCCEEDED,
		TaskStats: stateCounts,
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.taskStore.EXPECT().
		GetTaskStateSummaryForJob(gomock.Any(), suite.jobID).
		Return(stateCounts, nil)

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_BATCH)

	suite.cachedJob.EXPECT().
		GetJobType().
		Return(pbjob.JobType_BATCH).
		AnyTimes()

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(true).
		AnyTimes()

	suite.cachedJob.EXPECT().
		GetFirstTaskUpdateTime().
		Return(startTimeUnix)

	suite.cachedJob.EXPECT().
		GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context,
			jobInfo *pbjob.JobInfo,
			_ *models.ConfigAddOn,
			_ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.State, pbjob.JobState_INITIALIZED)
		}).Return(nil)

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := JobRuntimeUpdater(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// Test partially created service job whose desired state is terminal
func (suite *JobRuntimeUpdaterTestSuite) TestJobStateDeterminer_PartiallyCreatedServiceJob() {
	instanceCount := uint32(100)
	configVersion := uint64(4)

	testTable := []struct {
		expectedState    job.JobState
		actualState      job.JobState
		desiredGoalState job.JobState
		hasKilled        bool
		msg              string
	}{
		{
			expectedState:    job.JobState_FAILED,
			actualState:      job.JobState_PENDING,
			desiredGoalState: job.JobState_DELETED,
			hasKilled:        false,
			msg:              "desired goal is deleted and no tasks are killed, so expected state is failed",
		},
		{
			expectedState:    job.JobState_KILLED,
			actualState:      job.JobState_PENDING,
			desiredGoalState: job.JobState_DELETED,
			hasKilled:        true,
			msg:              "desired state is deleted and few tasks are killed, so expected state is killed",
		},
		{
			expectedState:    job.JobState_FAILED,
			actualState:      job.JobState_PENDING,
			desiredGoalState: job.JobState_KILLED,
			hasKilled:        false,
			msg:              "desired goal is killed and no tasks are killed, so expected state is failed",
		},
		{
			expectedState:    job.JobState_KILLED,
			actualState:      job.JobState_PENDING,
			desiredGoalState: job.JobState_KILLED,
			hasKilled:        true,
			msg:              "desired state is killed and few tasks are killed, so expected state is killed",
		},
		{
			expectedState:    job.JobState_PENDING,
			actualState:      job.JobState_PENDING,
			desiredGoalState: job.JobState_RUNNING,
			hasKilled:        true,
			msg:              "desired state is running, so defaulting to service job actual state pending",
		},
	}

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_SERVICE).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(true).
		AnyTimes()

	suite.cachedJob.EXPECT().
		GetJobType().
		Return(job.JobType_SERVICE).
		AnyTimes()

	for _, tt := range testTable {
		// sum of each state is smaller than instanceCount
		// simulate partially created job
		stateCounts := make(map[string]uint32)
		stateCounts[pbtask.TaskState_LOST.String()] = instanceCount / 3
		stateCounts[pbtask.TaskState_FAILED.String()] = instanceCount / 3

		taskStates := []pbtask.TaskState{pbtask.TaskState_LOST, pbtask.TaskState_FAILED}
		if tt.hasKilled {
			stateCounts[pbtask.TaskState_KILLED.String()] = instanceCount / 3
			taskStates = append(taskStates, pbtask.TaskState_KILLED)
		}

		first := uint32(0)
		last := instanceCount / 3
		cachedTasks := make(map[uint32]cached.Task)
		for _, state := range taskStates {
			for i := first; i < last; i++ {
				cachedTasks[i] = suite.cachedTask

				suite.cachedTask.EXPECT().
					CurrentState().
					Return(cached.TaskStateVector{
						State: state,
					})

				suite.cachedTask.EXPECT().
					GetRuntime(gomock.Any()).
					Return(&pbtask.RuntimeInfo{
						State:         state,
						ConfigVersion: configVersion,
					}, nil)
			}
			first = last
			last = first + instanceCount/3
		}

		suite.cachedJob.EXPECT().
			GetAllTasks().
			Return(cachedTasks)

		jobRuntime := &pbjob.RuntimeInfo{
			State:     tt.actualState,
			GoalState: tt.desiredGoalState,
			UpdateID:  &peloton.UpdateID{Value: uuid.NewRandom().String()},
		}

		jobState, _, _, _, err := determineJobRuntimeStateAndCounts(context.Background(),
			jobRuntime, stateCounts, suite.cachedConfig, suite.goalStateDriver, suite.cachedJob)
		suite.NoError(err)
		suite.Equal(jobState, tt.expectedState)
	}
}

func (suite *JobRuntimeUpdaterTestSuite) TestJobStateDeterminer_NoInstancesCreatedServiceJob() {
	instanceCount := uint32(100)
	stateCounts := make(map[string]uint32)
	jobRuntime := &pbjob.RuntimeInfo{
		State:     job.JobState_KILLED,
		GoalState: job.JobState_DELETED,
		UpdateID:  &peloton.UpdateID{Value: uuid.NewRandom().String()},
	}

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_SERVICE).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedJob.EXPECT().
		IsPartiallyCreated(gomock.Any()).
		Return(true).
		AnyTimes()

	suite.cachedJob.EXPECT().
		GetJobType().
		Return(job.JobType_SERVICE).
		AnyTimes()

	suite.cachedJob.EXPECT().
		GetAllTasks().
		Return(nil)

	jobState, _, _, _, err := determineJobRuntimeStateAndCounts(context.Background(),
		jobRuntime, stateCounts, suite.cachedConfig, suite.goalStateDriver, suite.cachedJob)
	suite.NoError(err)
	suite.Equal(job.JobState_KILLED, jobState)
}

// TestDetermineJobRuntimeStateStaleJob tests determining job runtime state
// for a stale active job with out of sync materialized view
func (suite *JobRuntimeUpdaterTestSuite) TestDetermineJobRuntimeStateStaleJob() {
	instanceCount := uint32(10)

	// State count from MV shows that half of tasks are in RUNNING state
	stateCounts := map[string]uint32{
		pbtask.TaskState_SUCCEEDED.String(): instanceCount / 2,
		pbtask.TaskState_RUNNING.String():   instanceCount / 2,
	}

	// State count form cache shows that all the tasks are in Succeed state
	stateCountsFromCache := map[string]uint32{
		pbtask.TaskState_SUCCEEDED.String(): instanceCount,
	}

	suite.initTaskStats(stateCountsFromCache)
	cachedTasks := make(map[uint32]cached.Task)
	mockTasks := make(map[uint32]*cachedmocks.MockTask)
	for i := uint32(0); i < instanceCount; i++ {
		cachedTask := cachedmocks.NewMockTask(suite.ctrl)
		mockTasks[i] = cachedTask
		cachedTasks[i] = cachedTask
	}

	// Simulate that MV is out of sync with base table by setting ALL of cached
	// tasks to SUCCEEDED state
	suite.cachedJob.EXPECT().GetAllTasks().Return(cachedTasks)
	for i := uint32(0); i < instanceCount; i++ {
		mockTasks[i].EXPECT().CurrentState().Return(cached.TaskStateVector{
			State: pbtask.TaskState_SUCCEEDED,
		})
	}

	// Set the current job state to RUNNING
	jobRuntime := &pbjob.RuntimeInfo{
		State: pbjob.JobState_RUNNING,
	}
	suite.cachedConfig.EXPECT().GetType().Return(pbjob.JobType_BATCH).AnyTimes()
	suite.cachedConfig.EXPECT().GetInstanceCount().
		Return(instanceCount).AnyTimes()
	suite.cachedConfig.EXPECT().HasControllerTask().Return(false).AnyTimes()
	suite.cachedJob.EXPECT().IsPartiallyCreated(false).AnyTimes()
	suite.cachedJob.EXPECT().GetLastTaskUpdateTime().
		Return(float64(time.Now().AddDate(0, 0, -5).Second())).AnyTimes()
	suite.cachedJob.EXPECT().ID().Return(
		&peloton.JobID{Value: uuid.New()}).Times(2)
	jobState, currentStateCounts, _, _, err := determineJobRuntimeStateAndCounts(
		context.Background(), jobRuntime, stateCounts, suite.cachedConfig,
		suite.goalStateDriver, suite.cachedJob)
	suite.NoError(err)
	suite.Equal(jobState, pbjob.JobState_SUCCEEDED)
	suite.Equal(currentStateCounts, stateCountsFromCache)

	// Simulate that some of cached tasks have UNKNOWN state, in this case
	// the original RUNNING state should be returned
	suite.cachedJob.EXPECT().GetAllTasks().Return(cachedTasks)
	for i := uint32(0); i < instanceCount; i++ {
		mockTasks[i].EXPECT().CurrentState().Return(cached.TaskStateVector{
			State: pbtask.TaskState_UNKNOWN,
		})
	}

	jobState, currentStateCounts, _, _, err = determineJobRuntimeStateAndCounts(
		context.Background(), jobRuntime, stateCounts, suite.cachedConfig,
		suite.goalStateDriver, suite.cachedJob)
	suite.NoError(err)
	suite.Equal(jobState, pbjob.JobState_RUNNING)
	suite.Equal(currentStateCounts, stateCounts)
}

// TestDetermineJobRuntimeFlagOn tests determining job runtime state
// jobRuntimeCalculationViaCache flag for this Peloton cluster is on.
// Verify job runtime state will be calculated from cache
func (suite *JobRuntimeUpdaterTestSuite) TestDetermineJobRuntimeFlagOn() {
	suite.determineJobRuntimeHelper(true)
}

// TestDetermineJobRuntimeFlagOff  tests determining job runtime state
// with total instance count derived from MV is greater than what configured
// which indicates MV is out of sync
// and jobRuntimeCalculationViaCache flag for this Peloton cluster is Off
// Verify job runtime state will not be calculated from cache
func (suite *JobRuntimeUpdaterTestSuite) TestDetermineJobRuntimeFlagOff() {
	suite.determineJobRuntimeHelper(false)
}

func (suite *JobRuntimeUpdaterTestSuite) determineJobRuntimeHelper(flag bool) {
	suite.goalStateDriver.jobRuntimeCalculationViaCache = flag
	instanceCount := uint32(10)
	// Simulate mismatch of instances count between MV and configuration,
	// 10 still in KILLING stage, 10 in KILLED stage
	stateCounts := map[string]uint32{
		pbtask.TaskState_KILLING.String(): instanceCount,
		pbtask.TaskState_KILLED.String():  instanceCount,
	}

	// State counts from cache shows all the tasks in KILLED state
	stateCountsFromCache := map[string]uint32{
		pbtask.TaskState_KILLED.String(): instanceCount,
	}

	suite.initTaskStats(stateCountsFromCache)

	// Set the current job state to KILLED
	jobRuntime := &pbjob.RuntimeInfo{
		State: pbjob.JobState_KILLED,
	}
	if flag {
		cachedTasks := make(map[uint32]cached.Task)
		mockTasks := make(map[uint32]*cachedmocks.MockTask)
		for i := uint32(0); i < instanceCount; i++ {
			cachedTask := cachedmocks.NewMockTask(suite.ctrl)
			mockTasks[i] = cachedTask
			cachedTasks[i] = cachedTask
		}

		// Setting ALL of cached tasks to KILLED state
		for i := uint32(0); i < instanceCount; i++ {
			mockTasks[i].EXPECT().CurrentState().Return(cached.TaskStateVector{
				State: pbtask.TaskState_KILLED,
			})
		}
		suite.cachedConfig.EXPECT().GetType().Return(pbjob.JobType_BATCH).Times(3)
		suite.cachedJob.EXPECT().GetAllTasks().Return(cachedTasks)
		suite.cachedConfig.EXPECT().GetInstanceCount().
			Return(instanceCount).Times(4)
	} else {
		suite.cachedConfig.EXPECT().GetType().Return(pbjob.JobType_BATCH).Times(2)
		suite.cachedConfig.EXPECT().GetInstanceCount().
			Return(instanceCount).Times(2)
	}
	suite.cachedConfig.EXPECT().HasControllerTask().Return(false).AnyTimes()
	suite.cachedJob.EXPECT().IsPartiallyCreated(false).AnyTimes()
	suite.cachedJob.EXPECT().GetLastTaskUpdateTime().
		Return(suite.lastUpdateTs).Times(1)
	jobState, currentStateCounts, _, _, err := determineJobRuntimeStateAndCounts(
		context.Background(), jobRuntime, stateCounts, suite.cachedConfig,
		suite.goalStateDriver, suite.cachedJob)
	suite.NoError(err)

	if flag {
		suite.Equal(jobState, pbjob.JobState_KILLED)
		suite.Equal(currentStateCounts, stateCountsFromCache)
	} else {
		suite.Equal(jobState, pbjob.JobState_PENDING)
		suite.Equal(currentStateCounts, stateCounts)
	}
}

// TestDetermineBatchJobRuntimeState tests determining JobRuntimeState for batch jobs
func (suite *JobRuntimeUpdaterTestSuite) TestDetermineBatchJobRuntimeState() {
	var instanceCount uint32 = 100
	tests := []struct {
		stateCounts             map[string]uint32
		configuredInstanceCount uint32
		currentState            pbjob.JobState
		expectedState           pbjob.JobState
		message                 string
	}{
		{
			map[string]uint32{
				pbtask.TaskState_FAILED.String():    instanceCount / 2,
				pbtask.TaskState_SUCCEEDED.String(): instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_FAILED,
			"Batch job terminated with failed task should be FAILED",
		},
		{
			map[string]uint32{
				pbtask.TaskState_RUNNING.String():   instanceCount / 2,
				pbtask.TaskState_SUCCEEDED.String(): instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_RUNNING,
			"Batch job with tasks running should be RUNNING",
		},
		{
			map[string]uint32{
				pbtask.TaskState_SUCCEEDED.String(): instanceCount,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_SUCCEEDED,
			"Batch job with all tasks succeed should be SUCCEEDED",
		},
		{
			map[string]uint32{
				pbtask.TaskState_SUCCEEDED.String(): instanceCount / 2,
				pbtask.TaskState_PENDING.String():   instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_PENDING,
			"Batch job with tasks pending should be PENDING",
		},
		{
			map[string]uint32{
				pbtask.TaskState_SUCCEEDED.String(): instanceCount / 2,
				pbtask.TaskState_KILLING.String():   instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_KILLING,
			pbjob.JobState_KILLING,
			"Batch job with killing state should be KILLING",
		},
		{
			map[string]uint32{
				pbtask.TaskState_FAILED.String(): instanceCount / 2,
				pbtask.TaskState_KILLED.String(): instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_KILLED,
			"Batch job with terminated with killed task should be KILLED",
		},
		{
			map[string]uint32{
				pbtask.TaskState_FAILED.String(): instanceCount/2 - 1,
				pbtask.TaskState_KILLED.String(): instanceCount/2 - 1,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_INITIALIZED,
			"Batch job partially created should be INITIALIZED",
		},
	}

	for index, test := range tests {
		ctrl := gomock.NewController(suite.T())
		jobRuntime := &pbjob.RuntimeInfo{
			State: test.currentState,
		}
		cachedConfig := cachedmocks.NewMockJobConfigCache(ctrl)
		cachedJob := cachedmocks.NewMockJob(ctrl)

		cachedConfig.EXPECT().GetType().Return(pbjob.JobType_BATCH).AnyTimes()
		cachedJob.EXPECT().GetJobType().Return(pbjob.JobType_BATCH).AnyTimes()

		cachedConfig.EXPECT().GetInstanceCount().
			Return(test.configuredInstanceCount).AnyTimes()

		cachedConfig.EXPECT().HasControllerTask().Return(false).AnyTimes()

		cachedJob.EXPECT().IsPartiallyCreated(gomock.Any()).
			Return(getTotalInstanceCount(test.stateCounts) <
				test.configuredInstanceCount).AnyTimes()
		cachedJob.EXPECT().GetLastTaskUpdateTime().
			Return(suite.lastUpdateTs).AnyTimes()

		jobState, currentStateCount, _, _, _ := determineJobRuntimeStateAndCounts(
			context.Background(),
			jobRuntime,
			test.stateCounts,
			cachedConfig,
			suite.goalStateDriver,
			cachedJob,
		)

		suite.Equal(jobState, test.expectedState, "Test %d: %s", index, test.message)
		suite.Equal(currentStateCount, test.stateCounts)

		ctrl.Finish()
	}
}

// TestDetermineServiceJobRuntimeState tests determining JobRuntimeState for service jobs
func (suite *JobRuntimeUpdaterTestSuite) TestDetermineServiceJobRuntimeState() {
	var instanceCount uint32 = 100
	var configVersion uint64 = 4

	tests := []struct {
		stateCounts             map[pbtask.TaskState]uint32
		configuredInstanceCount uint32
		currentState            pbjob.JobState
		expectedState           pbjob.JobState
		message                 string
	}{
		{
			map[pbtask.TaskState]uint32{
				pbtask.TaskState_FAILED:    instanceCount / 2,
				pbtask.TaskState_SUCCEEDED: instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_FAILED,
			"Service job completed with task FAILED should be FAILED",
		},
		{
			map[pbtask.TaskState]uint32{
				pbtask.TaskState_RUNNING:   instanceCount / 2,
				pbtask.TaskState_SUCCEEDED: instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_RUNNING,
			"Service job with tasks running should be RUNNING",
		},
		{
			map[pbtask.TaskState]uint32{
				pbtask.TaskState_SUCCEEDED: instanceCount,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_SUCCEEDED,
			"Service job with all tasks SUCCEEDED should be SUCCEEDED",
		},
		{
			map[pbtask.TaskState]uint32{
				pbtask.TaskState_SUCCEEDED: instanceCount / 2,
				pbtask.TaskState_PENDING:   instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_PENDING,
			"Service job with tasks pending should be PENDING",
		},
		{
			map[pbtask.TaskState]uint32{
				pbtask.TaskState_SUCCEEDED: instanceCount / 2,
				pbtask.TaskState_KILLING:   instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_KILLING,
			pbjob.JobState_KILLING,
			"Service job with killing state should be KILLING",
		},
		{
			map[pbtask.TaskState]uint32{
				pbtask.TaskState_FAILED: instanceCount / 2,
				pbtask.TaskState_KILLED: instanceCount / 2,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_KILLED,
			"Service job terminated with tasks KILLED should be KILLED",
		},
		{
			map[pbtask.TaskState]uint32{
				pbtask.TaskState_FAILED: instanceCount/2 - 1,
				pbtask.TaskState_KILLED: instanceCount/2 - 1,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_PENDING,
			"Service job partially created should be PENDING",
		},
		{
			map[pbtask.TaskState]uint32{
				pbtask.TaskState_KILLED: instanceCount,
			},
			instanceCount,
			pbjob.JobState_PENDING,
			pbjob.JobState_KILLED,
			"Service job with all tasks killed should be KILLED",
		},
	}

	for index, test := range tests {
		ctrl := gomock.NewController(suite.T())
		jobRuntime := &pbjob.RuntimeInfo{
			State: test.currentState,
		}
		cachedConfig := cachedmocks.NewMockJobConfigCache(ctrl)
		cachedJob := cachedmocks.NewMockJob(ctrl)
		cachedTasks := make(map[uint32]cached.Task)
		taskStateCounts := make(map[string]uint32)

		for _, taskStatus := range allTaskStates {
			taskStateCounts[taskStatus.String()] = 0
		}

		count := 0
		instCount := uint32(0)
		for state, numInstances := range test.stateCounts {
			cachedTask := cachedmocks.NewMockTask(ctrl)
			cachedTask.EXPECT().
				CurrentState().
				Return(cached.TaskStateVector{
					State: state,
				}).
				AnyTimes()

			cachedTask.EXPECT().
				GetRuntime(gomock.Any()).
				Return(&pbtask.RuntimeInfo{
					State:         state,
					ConfigVersion: configVersion,
				}, nil).
				AnyTimes()

			for j := uint32(0); j < numInstances; j++ {
				cachedTasks[instCount] = cachedTask
				instCount++
			}
			taskStateCounts[state.String()] = numInstances
			count++
		}

		cachedConfig.EXPECT().GetType().Return(pbjob.JobType_SERVICE).AnyTimes()
		cachedJob.EXPECT().GetJobType().Return(pbjob.JobType_SERVICE).AnyTimes()

		cachedConfig.EXPECT().GetInstanceCount().
			Return(test.configuredInstanceCount).AnyTimes()

		cachedConfig.EXPECT().HasControllerTask().Return(false).AnyTimes()

		cachedJob.EXPECT().
			GetAllTasks().
			Return(cachedTasks)

		cachedJob.EXPECT().IsPartiallyCreated(gomock.Any()).
			Return(getTotalInstanceCount(taskStateCounts) <
				test.configuredInstanceCount).AnyTimes()

		jobState, currentStateCount, _, _, _ := determineJobRuntimeStateAndCounts(
			context.Background(),
			jobRuntime,
			taskStateCounts,
			cachedConfig,
			suite.goalStateDriver,
			cachedJob,
		)

		suite.Equal(jobState, test.expectedState, "Test %d: %s", index, test.message)
		suite.Equal(currentStateCount, taskStateCounts)

		ctrl.Finish()
	}
}

func (suite *JobRuntimeUpdaterTestSuite) TestDetermineStatelessJobRuntimeState() {
	suite.goalStateDriver.jobRuntimeCalculationViaCache = true
	instanceCount := uint32(100)
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_RUNNING.String()] = instanceCount
	jobRuntime := &pbjob.RuntimeInfo{
		State: pbjob.JobState_RUNNING,
	}
	configVersion := uint64(4)

	cachedTasks := make(map[uint32]cached.Task)
	for i := uint32(0); i < instanceCount; i++ {
		cachedTasks[i] = suite.cachedTask
	}

	suite.cachedConfig.EXPECT().
		GetType().
		Return(pbjob.JobType_SERVICE).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		GetInstanceCount().
		Return(instanceCount).
		AnyTimes()

	suite.cachedConfig.EXPECT().
		HasControllerTask().
		Return(false).
		AnyTimes()

	suite.cachedJob.EXPECT().
		GetAllTasks().
		Return(cachedTasks)

	suite.cachedTask.EXPECT().
		CurrentState().
		Return(cached.TaskStateVector{
			State: pbtask.TaskState_RUNNING,
		}).
		Times(int(instanceCount))

	suite.cachedTask.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&pbtask.RuntimeInfo{
			State:         pbtask.TaskState_RUNNING,
			ConfigVersion: configVersion,
		}, nil).
		Times(int(instanceCount))

	jobState, _, configVersionStateStats, _, _ := determineJobRuntimeStateAndCounts(
		context.Background(),
		jobRuntime,
		stateCounts,
		suite.cachedConfig,
		suite.goalStateDriver,
		suite.cachedJob,
	)

	suite.Equal(jobState, pbjob.JobState_RUNNING)
	suite.Equal(len(configVersionStateStats), 1)
	suite.Equal(
		configVersionStateStats[configVersion].GetStateStats()[pbtask.TaskState_RUNNING.String()],
		instanceCount)
}

// TestJobEvaluateMaxRunningInstances tests
// evaluating max running instances
func (suite *JobRuntimeUpdaterTestSuite) TestJobEvaluateMaxRunningInstances() {
	instanceCount := uint32(100)
	maxRunningInstances := uint32(10)
	jobConfig := pbjob.JobConfig{
		OwningTeam:    "team6",
		LdapGroups:    []string{"team1", "team2", "team3"},
		InstanceCount: instanceCount,
		Type:          pbjob.JobType_BATCH,
		SLA: &pbjob.SlaConfig{
			MaximumRunningInstances: maxRunningInstances,
		},
	}

	jobRuntime := pbjob.RuntimeInfo{
		State:     pbjob.JobState_RUNNING,
		GoalState: pbjob.JobState_SUCCEEDED,
	}

	suite.cachedConfig.EXPECT().
		GetSLA().
		Return(jobConfig.SLA).AnyTimes()

	// Simulate RUNNING job
	stateCounts := make(map[string]uint32)
	stateCounts[pbtask.TaskState_INITIALIZED.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_SUCCEEDED.String()] = instanceCount / 2
	jobRuntime.TaskStats = stateCounts

	var initializedTasks []uint32
	for i := uint32(0); i < instanceCount/2; i++ {
		initializedTasks = append(initializedTasks, i)
	}

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil).
		Times(2)

	suite.jobStore.EXPECT().
		GetJobConfig(gomock.Any(), suite.jobID.GetValue()).
		Return(&jobConfig, &models.ConfigAddOn{}, nil)

	suite.taskStore.EXPECT().
		GetTaskIDsForJobAndState(gomock.Any(), suite.jobID, pbtask.TaskState_INITIALIZED.String()).
		Return(initializedTasks, nil)

	for i := uint32(0); i < jobConfig.SLA.MaximumRunningInstances; i++ {
		suite.taskStore.EXPECT().
			GetTaskRuntime(gomock.Any(), suite.jobID, gomock.Any()).
			Return(&pbtask.RuntimeInfo{
				State: pbtask.TaskState_INITIALIZED,
			}, nil)
		suite.cachedJob.EXPECT().
			GetTask(gomock.Any()).Return(suite.cachedTask)
		suite.taskGoalStateEngine.EXPECT().
			IsScheduled(gomock.Any()).
			Return(false)
	}

	suite.resmgrClient.EXPECT().
		EnqueueGangs(gomock.Any(), gomock.Any()).
		Return(&resmgrsvc.EnqueueGangsResponse{}, nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Do(func(ctx context.Context, runtimeDiffs map[uint32]jobmgrcommon.RuntimeDiff) {
			suite.Equal(uint32(len(runtimeDiffs)), jobConfig.SLA.MaximumRunningInstances)
			for _, runtimeDiff := range runtimeDiffs {
				suite.Equal(runtimeDiff[jobmgrcommon.StateField], pbtask.TaskState_PENDING)
			}
		}).
		Return(nil)

	err := JobEvaluateMaxRunningInstancesSLA(context.Background(), suite.jobEnt)
	suite.NoError(err)

	// Simulate when max running instances are already running
	stateCounts = make(map[string]uint32)
	stateCounts[pbtask.TaskState_INITIALIZED.String()] = instanceCount - maxRunningInstances
	stateCounts[pbtask.TaskState_RUNNING.String()] = maxRunningInstances
	jobRuntime.TaskStats = stateCounts

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.jobStore.EXPECT().
		GetJobConfig(gomock.Any(), suite.jobID.GetValue()).
		Return(&jobConfig, &models.ConfigAddOn{}, nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(suite.cachedConfig, nil)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	err = JobEvaluateMaxRunningInstancesSLA(context.Background(), suite.jobEnt)
	suite.NoError(err)

	// Simulate error when scheduled instances is greater than maximum running instances
	stateCounts = make(map[string]uint32)
	stateCounts[pbtask.TaskState_INITIALIZED.String()] = instanceCount / 2
	stateCounts[pbtask.TaskState_RUNNING.String()] = instanceCount / 2
	jobRuntime.TaskStats = stateCounts

	suite.jobFactory.EXPECT().
		AddJob(suite.jobID).
		Return(suite.cachedJob)

	suite.jobStore.EXPECT().
		GetJobConfig(gomock.Any(), suite.jobID.GetValue()).
		Return(&jobConfig, &models.ConfigAddOn{}, nil)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&jobRuntime, nil)

	err = JobEvaluateMaxRunningInstancesSLA(context.Background(), suite.jobEnt)
	suite.NoError(err)
}

// TestShouldRecalculateJobStateNonBatch tests shouldRecalculateJobStateFromCache
// function for a service job
func (suite *JobRuntimeUpdaterTestSuite) TestShouldRecalculateJobStateNonBatch() {
	suite.True(shouldRecalculateJobStateFromCache(
		suite.cachedJob, pbjob.JobType_SERVICE, pbjob.JobState_RUNNING,
		suite.goalStateDriver.jobRuntimeCalculationViaCache))
}

// TestshouldRecalculateJobStateTerminalJob tests shouldRecalculateJobStateFromCache
// function for a terminal job
func (suite *JobRuntimeUpdaterTestSuite) TestshouldRecalculateJobStateTerminalJob() {
	suite.False(shouldRecalculateJobStateFromCache(
		suite.cachedJob, pbjob.JobType_BATCH, pbjob.JobState_SUCCEEDED,
		suite.goalStateDriver.jobRuntimeCalculationViaCache))
}

// TestShouldRecalculateJobState tests shouldRecalculateJobStateFromCache function to
// determine if the job state is stale and needs to be recalculated
func (suite *JobRuntimeUpdaterTestSuite) TestShouldRecalculateJobState() {
	// assume last update to the job state was 5 days ago
	lastUpdateTs := float64(time.Now().AddDate(0, 0, -5).Unix())
	suite.cachedJob.EXPECT().GetLastTaskUpdateTime().Return(lastUpdateTs)
	suite.cachedJob.EXPECT().ID().Return(
		&peloton.JobID{Value: uuid.New()}).Times(1)
	suite.True(shouldRecalculateJobStateFromCache(
		suite.cachedJob, pbjob.JobType_BATCH, pbjob.JobState_PENDING,
		suite.goalStateDriver.jobRuntimeCalculationViaCache))

	suite.cachedJob.EXPECT().GetLastTaskUpdateTime().Return(suite.lastUpdateTs)
	suite.False(shouldRecalculateJobStateFromCache(
		suite.cachedJob, pbjob.JobType_BATCH, pbjob.JobState_PENDING,
		suite.goalStateDriver.jobRuntimeCalculationViaCache))
}

// TestShouldRecalculateJobStateFlagOnMVDiverged tests
// shouldRecalculateJobStateFromCache function
// when MV diverged and jobRuntimeCalculationViaCache flag is on
func (suite *JobRuntimeUpdaterTestSuite) TestShouldRecalculateJobStateFlagOnMVDiverged() {
	suite.goalStateDriver.jobRuntimeCalculationViaCache = true
	suite.cachedJob.EXPECT().GetLastTaskUpdateTime().Return(suite.lastUpdateTs)
	suite.True(shouldRecalculateJobStateFromCache(
		suite.cachedJob, pbjob.JobType_BATCH, pbjob.JobState_PENDING,
		suite.goalStateDriver.jobRuntimeCalculationViaCache))
}

// TestShouldRecalculateJobStateFlagOffMVDiverged tests
// shouldRecalculateJobStateFromCache function
// when MV diverged and jobRuntimeCalculationViaCache flag is Off
func (suite *JobRuntimeUpdaterTestSuite) TestShouldRecalculateJobStateFlagOffMVDiverged() {
	suite.goalStateDriver.jobRuntimeCalculationViaCache = false
	suite.cachedJob.EXPECT().GetLastTaskUpdateTime().Return(suite.lastUpdateTs)
	suite.False(shouldRecalculateJobStateFromCache(
		suite.cachedJob, pbjob.JobType_BATCH, pbjob.JobState_PENDING,
		suite.goalStateDriver.jobRuntimeCalculationViaCache))
}

func (suite *JobRuntimeUpdaterTestSuite) initTaskStats(
	stateCountsFromCache map[string]uint32) {
	for _, taskState := range allTaskStates {
		if _, ok := stateCountsFromCache[taskState.String()]; !ok {
			stateCountsFromCache[taskState.String()] = 0
		}
	}
}
