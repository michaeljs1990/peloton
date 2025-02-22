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

package recovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	pb_job "github.com/uber/peloton/.gen/peloton/api/v0/job"
	"github.com/uber/peloton/.gen/peloton/api/v0/peloton"
	pb_task "github.com/uber/peloton/.gen/peloton/api/v0/task"
	"github.com/uber/peloton/.gen/peloton/private/models"

	"github.com/uber/peloton/pkg/storage/cassandra"
	store_mocks "github.com/uber/peloton/pkg/storage/mocks"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc/yarpcerrors"
)

var (
	csStore        *cassandra.Store
	receivedJobIDs []string
	count          int
)

var mutex = &sync.Mutex{}
var scope = tally.Scope(tally.NoopScope)

func init() {
	conf := cassandra.MigrateForTest()
	var err error
	csStore, err = cassandra.NewStore(conf, scope)
	if err != nil {
		log.Fatal(err)
	}
}

func createJob(
	ctx context.Context,
	state pb_job.JobState,
	goalState pb_job.JobState,
) (*peloton.JobID, error) {
	var jobID = &peloton.JobID{Value: uuid.New()}
	var sla = pb_job.SlaConfig{
		Priority:                22,
		MaximumRunningInstances: 3,
		Preemptible:             false,
	}
	var taskConfig = pb_task.TaskConfig{
		Resource: &pb_task.ResourceConfig{
			CpuLimit:    0.8,
			MemLimitMb:  800,
			DiskLimitMb: 1500,
		},
	}

	now := time.Now()
	var jobConfig = pb_job.JobConfig{
		Name:          "TestValidatorWithStore",
		OwningTeam:    "team6",
		LdapGroups:    []string{"money", "team6", "gsg9"},
		SLA:           &sla,
		DefaultConfig: &taskConfig,
		InstanceCount: 2,
		ChangeLog: &peloton.ChangeLog{
			CreatedAt: uint64(now.UnixNano()),
			UpdatedAt: uint64(now.UnixNano()),
			Version:   1,
		},
	}
	configAddOn := &models.ConfigAddOn{}

	initialJobRuntime := pb_job.RuntimeInfo{
		State:        pb_job.JobState_INITIALIZED,
		CreationTime: now.Format(time.RFC3339Nano),
		TaskStats:    make(map[string]uint32),
		GoalState:    goalState,
		Revision: &peloton.ChangeLog{
			CreatedAt: uint64(now.UnixNano()),
			UpdatedAt: uint64(now.UnixNano()),
			Version:   1,
		},
		ConfigurationVersion: jobConfig.GetChangeLog().GetVersion(),
	}

	if err := csStore.AddActiveJob(ctx, jobID); err != nil {
		return nil, err
	}

	err := csStore.CreateJobConfig(ctx, jobID, &jobConfig, configAddOn, 1, "gsg9")
	if err != nil {
		return nil, err
	}

	err = csStore.CreateJobRuntime(ctx, jobID, &initialJobRuntime)
	if err != nil {
		return nil, err
	}

	jobRuntime, err := csStore.GetJobRuntime(ctx, jobID.GetValue())
	if err != nil {
		return nil, err
	}

	jobRuntime.State = state
	jobRuntime.GoalState = goalState
	err = csStore.UpdateJobRuntime(ctx, jobID, jobRuntime)
	if err != nil {
		return nil, err
	}
	return jobID, nil
}

func recoverAllTask(
	ctx context.Context,
	jobID string,
	jobConfig *pb_job.JobConfig,
	configAddOn *models.ConfigAddOn,
	jobRuntime *pb_job.RuntimeInfo,
	batch TasksBatch,
	errChan chan<- error) {
	mutex.Lock()
	receivedJobIDs = append(receivedJobIDs, jobID)
	mutex.Unlock()
	return
}

func recoverFailTaskRandomly(
	ctx context.Context,
	jobID string,
	jobConfig *pb_job.JobConfig,
	configAddOn *models.ConfigAddOn,
	jobRuntime *pb_job.RuntimeInfo,
	batch TasksBatch,
	errChan chan<- error) {
	mutex.Lock()
	if count%9 == 0 {
		errChan <- errors.New("Task Recovery Failed")
	}
	count++
	mutex.Unlock()

	return
}

func TestJobRecoveryWithStore(t *testing.T) {
	var err error

	ctx := context.Background()

	pendingJobID, err := createJob(
		ctx,
		pb_job.JobState_PENDING,
		pb_job.JobState_SUCCEEDED,
	)
	assert.NoError(t, err)

	runningJobID, err := createJob(
		ctx,
		pb_job.JobState_RUNNING,
		pb_job.JobState_SUCCEEDED,
	)
	assert.NoError(t, err)

	receivedJobIDs = nil
	err = RecoverActiveJobs(
		ctx, scope, csStore, recoverAllTask)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(receivedJobIDs))

	// Delete these jobIDs from active jobs table to clear the state for
	// next tests
	err = csStore.DeleteActiveJob(ctx, pendingJobID)
	assert.NoError(t, err)
	err = csStore.DeleteActiveJob(ctx, runningJobID)
	assert.NoError(t, err)
}

// TestRecoveryMissingJobRuntime tests recovery when one of the active jobs
// doesn't exist in job_runtime
func TestRecoveryMissingJobRuntime(t *testing.T) {
	var missingJobID = &peloton.JobID{Value: uuid.New()}
	var pendingJobID = &peloton.JobID{Value: uuid.New()}
	var jobRuntime = pb_job.RuntimeInfo{
		State: pb_job.JobState_PENDING,
	}
	var jobConfig = pb_job.JobConfig{
		Type:          pb_job.JobType_BATCH,
		Name:          "TestValidatorWithStore",
		OwningTeam:    "team6",
		LdapGroups:    []string{"money", "team6", "gsg9"},
		InstanceCount: 2,
	}

	ctrl := gomock.NewController(t)
	ctx := context.Background()
	mockJobStore := store_mocks.NewMockJobStore(ctrl)

	// recoverJobsBatch should pass even if there is no job_id present in
	// job_runtime. It should just skip over to a new job.
	mockJobStore.EXPECT().
		GetActiveJobs(ctx).
		Return([]*peloton.JobID{missingJobID}, nil)

	// missingJobID doesn't have job_runtime
	mockJobStore.EXPECT().
		GetJobRuntime(ctx, missingJobID.GetValue()).
		Return(
			nil,
			yarpcerrors.NotFoundErrorf(
				"Cannot find job wth jobID %v", missingJobID.GetValue()),
		)
	mockJobStore.EXPECT().
		GetJobRuntime(ctx, pendingJobID.GetValue()).
		Return(&jobRuntime, nil)

	mockJobStore.EXPECT().
		GetJobConfig(ctx, pendingJobID.GetValue()).
		Return(&jobConfig, &models.ConfigAddOn{}, nil).AnyTimes()

	mockJobStore.EXPECT().DeleteActiveJob(ctx, missingJobID).Return(nil)

	err := RecoverActiveJobs(
		ctx,
		scope,
		mockJobStore,
		recoverAllTask,
	)
	assert.NoError(t, err)
}

// TestRecoveryMissingJobConfig tests recovery when one of the active jobs
// doesn't exist in job_config
func TestRecoveryMissingJobConfig(t *testing.T) {
	var missingJobID = &peloton.JobID{Value: uuid.New()}
	var pendingJobID = &peloton.JobID{Value: uuid.New()}
	var err error
	var jobRuntime = pb_job.RuntimeInfo{
		State: pb_job.JobState_PENDING,
	}
	var jobConfig = pb_job.JobConfig{
		Type:          pb_job.JobType_BATCH,
		Name:          "TestValidatorWithStore",
		OwningTeam:    "team6",
		LdapGroups:    []string{"money", "team6", "gsg9"},
		InstanceCount: 2,
	}

	ctrl := gomock.NewController(t)
	ctx := context.Background()
	mockJobStore := store_mocks.NewMockJobStore(ctrl)

	// recoverJobsBatch should pass even if there is no job_id present in
	// job_config. It should just skip over to a new job.
	mockJobStore.EXPECT().
		GetActiveJobs(ctx).
		Return([]*peloton.JobID{missingJobID, pendingJobID}, nil)

	mockJobStore.EXPECT().
		GetJobRuntime(ctx, missingJobID.GetValue()).
		Return(&jobRuntime, nil)

	mockJobStore.EXPECT().
		GetJobRuntime(ctx, pendingJobID.GetValue()).
		Return(&jobRuntime, nil)

	// missingJobID doesn't have job_config
	mockJobStore.EXPECT().
		GetJobConfig(ctx, missingJobID.GetValue()).
		Return(
			nil,
			&models.ConfigAddOn{},
			yarpcerrors.NotFoundErrorf(
				"Cannot find job wth jobID %v", missingJobID.GetValue()),
		)

	mockJobStore.EXPECT().
		GetJobConfig(ctx, pendingJobID.GetValue()).
		Return(&jobConfig, &models.ConfigAddOn{}, nil).AnyTimes()

	mockJobStore.EXPECT().DeleteActiveJob(ctx, missingJobID).Return(nil)

	err = RecoverActiveJobs(
		ctx,
		scope,
		mockJobStore,
		recoverAllTask,
	)
	assert.NoError(t, err)
}

// TestRecoveryTerminalJobs tests recovery when one of the active jobs
// is actually a terminal job and verifies that the job is deleted from the
// active_jobs table ONLY if it is a BATCH job
func TestRecoveryTerminalJobs(t *testing.T) {
	var err error
	var terminalJobID = &peloton.JobID{Value: uuid.New()}
	var nonTerminalJobID = &peloton.JobID{Value: uuid.New()}
	var jobRuntime = pb_job.RuntimeInfo{
		State:     pb_job.JobState_SUCCEEDED,
		GoalState: pb_job.JobState_SUCCEEDED,
	}
	var nonTerminalJobRuntime = pb_job.RuntimeInfo{
		State:     pb_job.JobState_PENDING,
		GoalState: pb_job.JobState_SUCCEEDED,
	}

	var jobConfig = pb_job.JobConfig{
		Type:          pb_job.JobType_BATCH,
		Name:          "TestValidatorWithStore",
		OwningTeam:    "team6",
		LdapGroups:    []string{"money", "team6", "gsg9"},
		InstanceCount: 2,
	}

	ctrl := gomock.NewController(t)
	ctx := context.Background()
	mockJobStore := store_mocks.NewMockJobStore(ctrl)

	// recoverJobsBatch should pass even if the job to be recovered is terminal
	// and if it is a batch job, it should be delete from the active_jobs table
	mockJobStore.EXPECT().
		GetActiveJobs(ctx).
		Return([]*peloton.JobID{nonTerminalJobID, terminalJobID}, nil)

	mockJobStore.EXPECT().
		GetJobRuntime(ctx, terminalJobID.GetValue()).
		Return(&jobRuntime, nil)
	mockJobStore.EXPECT().
		GetJobRuntime(ctx, nonTerminalJobID.GetValue()).
		Return(&nonTerminalJobRuntime, nil)

	mockJobStore.EXPECT().
		GetJobConfig(ctx, terminalJobID.GetValue()).
		Return(&jobConfig, &models.ConfigAddOn{}, nil)
	mockJobStore.EXPECT().
		GetJobConfig(ctx, nonTerminalJobID.GetValue()).
		Return(&jobConfig, &models.ConfigAddOn{}, nil)

	// Expect this call because this is a terminal BATCH job
	mockJobStore.EXPECT().DeleteActiveJob(ctx, terminalJobID).Return(nil)

	err = RecoverActiveJobs(
		ctx,
		scope,
		mockJobStore,
		recoverAllTask,
	)
	assert.NoError(t, err)

	// Same test for stateless job
	jobConfig = pb_job.JobConfig{
		Type: pb_job.JobType_SERVICE,
	}
	// recoverJobsBatch should pass even if the job to be recovered is terminal
	// and if it is a batch job, it should be delete from the active_jobs table
	mockJobStore.EXPECT().
		GetActiveJobs(ctx).
		Return([]*peloton.JobID{terminalJobID}, nil)

	mockJobStore.EXPECT().
		GetJobRuntime(ctx, terminalJobID.GetValue()).
		Return(&jobRuntime, nil)

	mockJobStore.EXPECT().
		GetJobConfig(ctx, terminalJobID.GetValue()).
		Return(&jobConfig, &models.ConfigAddOn{}, nil)

	// This time, we don't expect a call to DeleteActiveJob

	err = RecoverActiveJobs(
		ctx,
		scope,
		mockJobStore,
		recoverAllTask,
	)
	assert.NoError(t, err)

}

// TestRecoveryErrors tests RecoverActiveJobs errors
func TestRecoveryErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	ctx := context.Background()
	mockJobStore := store_mocks.NewMockJobStore(ctrl)

	//Test GetActiveJobs error
	mockJobStore.EXPECT().
		GetActiveJobs(ctx).
		Return(nil, fmt.Errorf("Fake GetActiveJobs error"))
	err := RecoverActiveJobs(
		ctx,
		scope,
		mockJobStore,
		recoverAllTask,
	)
	assert.Error(t, err)
}

// TestRecoveryWithFailedJobBatches test will create 100 jobs, and eventually 10
// jobByBatches. recoverTaskRandomly will send error for recovering 11 jobs,
// so errChan will have at least 2 messages or max 10 messages if all job
// batches failed, here errChan will be full.
// TODO (varung): Add go routine leak test.
func TestRecoveryWithFailedJobBatches(t *testing.T) {
	var err error
	var jobID *peloton.JobID
	var activeJobs []*peloton.JobID

	count = 0
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		jobID, err = createJob(ctx, pb_job.JobState_PENDING, pb_job.JobState_SUCCEEDED)
		assert.NoError(t, err)
		activeJobs = append(activeJobs, jobID)
	}

	for i := 0; i < 50; i++ {
		jobID, err = createJob(ctx, pb_job.JobState_RUNNING, pb_job.JobState_SUCCEEDED)
		assert.NoError(t, err)
		activeJobs = append(activeJobs, jobID)
	}

	err = RecoverActiveJobs(
		ctx, scope, csStore, recoverFailTaskRandomly)
	assert.Error(t, err)

	for _, jobID := range activeJobs {
		// Delete these jobIDs from active jobs table to clear the state for
		// next tests
		err = csStore.DeleteActiveJob(ctx, jobID)
		assert.NoError(t, err)
	}
}

// TestDeleteActiveJobFailure simulates DeleteActiveJob failure and ensures
// that the recovery still goes through fine
func TestDeleteActiveJobFailure(t *testing.T) {
	var missingJobID = &peloton.JobID{Value: uuid.New()}

	ctrl := gomock.NewController(t)
	ctx := context.Background()
	mockJobStore := store_mocks.NewMockJobStore(ctrl)

	// recoverJobsBatch should pass even if there is no job_id present in
	// job_runtime. It should just skip over to a new job.
	mockJobStore.EXPECT().
		GetActiveJobs(ctx).
		Return([]*peloton.JobID{missingJobID}, nil)

	// missingJobID doesn't have job_runtime
	mockJobStore.EXPECT().
		GetJobRuntime(ctx, missingJobID.GetValue()).
		Return(
			nil,
			yarpcerrors.NotFoundErrorf(
				"Cannot find job wth jobID %v", missingJobID.GetValue()),
		)

	mockJobStore.EXPECT().
		DeleteActiveJob(ctx, missingJobID).
		Return(fmt.Errorf("DeleteActiveJob error"))

	err := RecoverActiveJobs(
		ctx,
		scope,
		mockJobStore,
		recoverAllTask,
	)
	assert.NoError(t, err)
}

// TestDeleteOnlyOnNotFound simulates the case where cassandra error is not
// not found. In this case the job should not be deleted from active_jobs
func TestDeleteOnlyOnNotFound(t *testing.T) {
	var missingJobID = &peloton.JobID{Value: uuid.New()}

	ctrl := gomock.NewController(t)
	ctx := context.Background()
	mockJobStore := store_mocks.NewMockJobStore(ctrl)

	// recoverJobsBatch should pass even if there is no job_id present in
	// job_runtime. It should just skip over to a new job.
	mockJobStore.EXPECT().
		GetActiveJobs(ctx).
		Return([]*peloton.JobID{missingJobID}, nil)

	// missingJobID doesn't have job_runtime
	mockJobStore.EXPECT().
		GetJobRuntime(ctx, missingJobID.GetValue()).
		Return(
			nil,
			yarpcerrors.InternalErrorf(
				"Cannot find job wth jobID %v", missingJobID.GetValue()),
		)

	// DeleteActiveJob should NOT be called in this case because the job
	// doesn't result in NotFound error from DB

	err := RecoverActiveJobs(
		ctx,
		scope,
		mockJobStore,
		recoverAllTask,
	)
	assert.NoError(t, err)
}
